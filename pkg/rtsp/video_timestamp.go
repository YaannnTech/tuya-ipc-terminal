package rtsp

import (
	"os"
	"strconv"
	"time"

	"github.com/pion/rtp"

	"tuya-ipc-terminal/pkg/core"
)

const (
	videoTimestampSampleTime = 500 * time.Millisecond
	videoTimestampMaxWait    = 2 * time.Second
	videoTimestampMaxBytes   = 16 << 20
	// A 1000x clock is obvious after a few frames, so detection needs only four.
	videoTimestampMinFrames  = 4
	videoTimestampMaxPackets = 8192
)

type videoTimestampScaler struct {
	override  uint32 // Zero selects automatic detection.
	divisor   uint32
	started   bool
	ssrc      uint32
	lastSeq   uint16
	lastTS    uint32
	unwrapped int64
	origin    int64
	output    uint32

	sampleStart time.Time
	sampleBase  int64
	frames      int
	fastFrames  int
	pending     []*rtp.Packet
	pendingSize int
}

func newVideoTimestampScaler() videoTimestampScaler {
	var divisor uint32
	if value := os.Getenv("TUYA_VIDEO_TS_DIVISOR"); value != "" {
		n, err := strconv.ParseUint(value, 10, 32)
		if err != nil || n == 0 {
			core.Logger.Warn().Msgf("Invalid TUYA_VIDEO_TS_DIVISOR %q; using automatic detection", value)
		} else {
			divisor = uint32(n)
		}
	}
	return videoTimestampScaler{override: divisor, divisor: divisor}
}

func (s *videoTimestampScaler) reset() {
	*s = videoTimestampScaler{override: s.override, divisor: s.override}
}

// unwrap uses sequence order to keep late packets from moving the clock anchor.
func (s *videoTimestampScaler) unwrap(packet *rtp.Packet) int64 {
	delta := int32(packet.Timestamp - s.lastTS)
	value := s.unwrapped + int64(delta)
	if int16(packet.SequenceNumber-s.lastSeq) > 0 {
		s.lastSeq, s.lastTS, s.unwrapped = packet.SequenceNumber, packet.Timestamp, value
	}
	return value
}

func (s *videoTimestampScaler) start(packet *rtp.Packet, now time.Time) {
	s.started = true
	s.ssrc = packet.SSRC
	s.lastSeq = packet.SequenceNumber
	s.lastTS = packet.Timestamp
	s.unwrapped = int64(packet.Timestamp)
	s.origin = s.unwrapped
	s.output = packet.Timestamp
	s.sampleBase = s.unwrapped
	s.sampleStart = now
}

func (s *videoTimestampScaler) scale(packet *rtp.Packet) *rtp.Packet {
	out := *packet
	delta := int32(packet.Timestamp - s.lastTS)
	if int16(packet.SequenceNumber-s.lastSeq) > 0 &&
		int64(delta) < -180000*int64(s.divisor) {
		// A large backwards discontinuity is not ordinary frame reordering.
		s.output += uint32((s.unwrapped - s.origin) / int64(s.divisor))
		s.lastSeq, s.lastTS = packet.SequenceNumber, packet.Timestamp
		s.unwrapped = int64(packet.Timestamp)
		s.origin = s.unwrapped
		out.Timestamp = s.output
		return &out
	}
	value := s.unwrap(packet)
	out.Timestamp = s.output + uint32((value-s.origin)/int64(s.divisor))
	return &out
}

func (s *videoTimestampScaler) push(packet *rtp.Packet, now time.Time) []*rtp.Packet {
	var out []*rtp.Packet
	if s.started && packet.SSRC != s.ssrc {
		// A new RTP source has its own independent timestamp and sequence space.
		out = append(out, s.pending...)
		s.reset()
	}
	if !s.started {
		s.start(packet, now)
	}
	if s.divisor != 0 {
		if s.divisor == 1 {
			return append(out, packet)
		}
		return append(out, s.scale(packet))
	}

	previous := s.unwrapped
	value := s.unwrap(packet)
	if value != previous && value == s.unwrapped {
		s.frames++
		if value-previous >= 900000 && value-previous <= 180000000 {
			s.fastFrames++
		}
	}
	// Copy packets while detecting: Pion can reuse the incoming packet's data.
	s.pending = append(s.pending, packet.Clone())
	s.pendingSize += packet.MarshalSize()
	elapsed := now.Sub(s.sampleStart)
	if elapsed < videoTimestampSampleTime ||
		(s.frames < videoTimestampMinFrames && elapsed < videoTimestampMaxWait) {
		if s.pendingSize < videoTimestampMaxBytes && len(s.pending) < videoTimestampMaxPackets {
			return out
		}
		core.Logger.Warn().Msg("Video timestamp detection buffer limit reached; leaving timestamps unchanged")
	} else if s.fastFrames >= videoTimestampMinFrames && elapsed > 0 {
		rate := float64(s.unwrapped-s.sampleBase) / elapsed.Seconds()
		if rate >= 90000*500 && rate <= 90000*2000 {
			s.divisor = 1000
			core.Logger.Info().Msg("Detected inflated video RTP timestamps; dividing by 1000")
		}
	}
	if s.divisor == 0 {
		s.divisor = 1
	}

	pending := s.pending
	s.pending = nil
	s.pendingSize = 0
	// Replay from the original anchor so the buffered packets and later packets
	// share one continuous timestamp space, even across a source-clock wrap.
	s.started = false
	for _, p := range pending {
		if !s.started {
			s.start(p, now)
		}
		if s.divisor == 1 {
			out = append(out, p)
		} else {
			out = append(out, s.scale(p))
		}
	}
	return out
}
