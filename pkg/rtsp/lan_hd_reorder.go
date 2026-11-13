package rtsp

import (
	"time"

	"github.com/pion/rtp"

	"tuya-ipc-terminal/pkg/core"
)

const (
	lanHDReorderWait   = 250 * time.Millisecond
	lanHDReorderWindow = 256
)

type pendingRTPPacket struct {
	packet   *rtp.Packet
	received time.Time
}

// rtpReorder holds later packets briefly when an earlier sequence is missing.
type rtpReorder struct {
	started bool
	next    uint16
	ssrc    uint32
	pending map[uint16]pendingRTPPacket
	warned  bool
}

func (r *rtpReorder) push(
	packet *rtp.Packet, now time.Time, wait time.Duration, window uint16, label string,
) []*rtp.Packet {
	seq := packet.SequenceNumber
	if !r.started || packet.SSRC != r.ssrc {
		*r = rtpReorder{started: true, next: seq, ssrc: packet.SSRC}
	}
	if int16(seq-r.next) < 0 {
		return nil
	}
	if len(r.pending) == 0 && seq == r.next {
		r.next++
		return []*rtp.Packet{packet}
	}
	if r.pending == nil {
		r.pending = make(map[uint16]pendingRTPPacket)
	}
	if _, exists := r.pending[seq]; !exists {
		r.pending[seq] = pendingRTPPacket{packet: packet, received: now}
	}
	return r.drain(now, wait, window, label)
}

func (r *rtpReorder) deadline(wait time.Duration) time.Time {
	var oldest time.Time
	for _, pending := range r.pending {
		if oldest.IsZero() || pending.received.Before(oldest) {
			oldest = pending.received
		}
	}
	if oldest.IsZero() {
		return time.Time{}
	}
	return oldest.Add(wait)
}

func (r *rtpReorder) drain(
	now time.Time, wait time.Duration, window uint16, label string,
) []*rtp.Packet {
	var out []*rtp.Packet
	for len(r.pending) != 0 {
		if next, exists := r.pending[r.next]; exists {
			delete(r.pending, r.next)
			r.next++
			out = append(out, next.packet)
			continue
		}
		nearest, farthest := uint16(65535), uint16(0)
		oldest := now
		for seq, pending := range r.pending {
			distance := seq - r.next
			nearest = min(nearest, distance)
			farthest = max(farthest, distance)
			if pending.received.Before(oldest) {
				oldest = pending.received
			}
		}
		if now.Sub(oldest) < wait && farthest < window {
			break
		}
		if !r.warned {
			r.warned = true
			core.Logger.Warn().Msgf("%s: skipping missing video RTP packets after reorder limit",
				label)
		}
		r.next += nearest
	}
	return out
}
