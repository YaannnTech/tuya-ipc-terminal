package rtsp

import (
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func timestampPacket(seq uint16, ts, ssrc uint32) *rtp.Packet {
	return &rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc},
		Payload: []byte{1},
	}
}

func TestVideoTimestampAutomaticDetection(t *testing.T) {
	for _, tc := range []struct {
		name    string
		step    uint32
		divisor uint32
	}{
		{"inflated", 6000000, 1000},
		{"normal", 6000, 1},
		{"other rate", 60000, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := videoTimestampScaler{}
			start := time.Unix(1, 0)
			base := uint32(0xff000000)
			var received []*rtp.Packet
			// At 15 fps, packet 8 (533 ms) is the first one after the sampling period.
			for i := 0; i <= 8; i++ {
				p := timestampPacket(uint16(i), base+uint32(i)*tc.step, 1)
				packets := s.push(p, start.Add(time.Duration(i)*time.Second/15))
				if i < 8 && len(packets) != 0 {
					t.Fatal("released packets before the sampling period")
				}
				received = append(received, packets...)
			}
			if s.divisor != tc.divisor || len(received) != 9 {
				t.Fatalf("divisor = %d, packets = %d", s.divisor, len(received))
			}
			for i, p := range received {
				want := base + uint32(i)*tc.step/tc.divisor
				if p.Timestamp != want {
					t.Fatalf("packet %d timestamp = %d, want %d", i, p.Timestamp, want)
				}
			}
			next := s.push(timestampPacket(9, base+9*tc.step, 1), start.Add(time.Second))
			want := base + 9*tc.step/tc.divisor
			if len(next) != 1 || next[0].Timestamp != want {
				t.Fatalf("next packet = %v, want timestamp %d", next, want)
			}
		})
	}
}

func TestVideoTimestampOverrideWrapAndReordering(t *testing.T) {
	s := videoTimestampScaler{override: 1000, divisor: 1000}
	now := time.Unix(1, 0)
	base := uint32(0xfff00000)
	for _, tc := range []struct {
		seq    uint16
		offset uint32
		want   uint32
	}{
		{65534, 0, 0},
		{65535, 6000000, 6000},
		{0, 12000000, 12000},
		{65535, 6000000, 6000}, // Late packet must not move the anchor.
		{1, 18000000, 18000},
		{2, 18000000, 18000}, // Multiple packets in one frame.
		{3, 24000500, 24000},
		{4, 30000000, 30000}, // Preserve fractional ticks across packets.
	} {
		p := timestampPacket(tc.seq, base+tc.offset, 1)
		got := s.push(p, now)
		if len(got) != 1 || got[0].Timestamp != base+tc.want {
			t.Fatalf("seq %d: timestamp = %v, want %d", tc.seq, got, base+tc.want)
		}
		if p.Timestamp != base+tc.offset {
			t.Fatal("input packet was modified")
		}
	}
}

func TestVideoTimestampResetAndDiscontinuity(t *testing.T) {
	s := videoTimestampScaler{override: 1000, divisor: 1000}
	now := time.Unix(1, 0)
	s.push(timestampPacket(1, 400000000, 1), now)
	s.push(timestampPacket(2, 406000000, 1), now)
	got := s.push(timestampPacket(3, 1000000, 1), now)
	if got[0].Timestamp != 400006000 {
		t.Fatalf("discontinuity timestamp = %d", got[0].Timestamp)
	}
	got = s.push(timestampPacket(4, 7000000, 1), now)
	if got[0].Timestamp != 400012000 {
		t.Fatalf("timestamp after discontinuity = %d", got[0].Timestamp)
	}
	got = s.push(timestampPacket(1, 42, 2), now)
	if got[0].Timestamp != 42 {
		t.Fatalf("new source timestamp = %d", got[0].Timestamp)
	}
	s.reset()
	if s.started || s.divisor != 1000 {
		t.Fatalf("reset lost override or kept source state: %+v", s)
	}
}

func TestVideoTimestampDetectionLowFrameRate(t *testing.T) {
	// At 5 fps, the sampling period ends after two frames; detection waits for four.
	s := videoTimestampScaler{}
	start := time.Unix(1, 0)
	for i := 0; i <= 4; i++ {
		out := s.push(timestampPacket(uint16(i), uint32(i)*18000000, 1), start.Add(time.Duration(i)*time.Second/5))
		if i < 4 && len(out) != 0 {
			t.Fatalf("released packets after %d frames", i)
		}
		if i == 4 && len(out) != 5 {
			t.Fatalf("released %d packets after four frames, want 5", len(out))
		}
	}
	if s.divisor != 1000 {
		t.Fatalf("divisor = %d, want 1000", s.divisor)
	}
}

func TestVideoTimestampDetectionFallback(t *testing.T) {
	start := time.Unix(1, 0)
	t.Run("single timestamp jump", func(t *testing.T) {
		s := videoTimestampScaler{}
		for i := 0; i <= 30; i++ {
			ts := uint32(i * 6000)
			if i != 0 {
				ts += 180000000
			}
			s.push(timestampPacket(uint16(i), ts, 1), start.Add(time.Duration(i)*time.Second/15))
		}
		if s.divisor != 1 {
			t.Fatalf("single jump triggered divisor %d", s.divisor)
		}
	})
	t.Run("too few frames", func(t *testing.T) {
		s := videoTimestampScaler{}
		s.push(timestampPacket(1, 100, 1), start)
		out := s.push(timestampPacket(2, 200, 1), start.Add(videoTimestampMaxWait))
		if s.divisor != 1 || len(out) != 2 || out[1].Timestamp != 200 {
			t.Fatalf("fallback divisor = %d, packets = %v", s.divisor, out)
		}
	})
	t.Run("buffer limit", func(t *testing.T) {
		s := videoTimestampScaler{}
		p := timestampPacket(1, 100, 1)
		p.Payload = make([]byte, videoTimestampMaxBytes)
		out := s.push(p, start)
		if s.divisor != 1 || len(out) != 1 {
			t.Fatalf("fallback divisor = %d, packet count = %d", s.divisor, len(out))
		}
	})
	t.Run("source change", func(t *testing.T) {
		s := videoTimestampScaler{}
		s.push(timestampPacket(1, 100, 1), start)
		out := s.push(timestampPacket(1, 200, 2), start)
		if len(out) != 1 || out[0].Timestamp != 100 || len(s.pending) != 1 {
			t.Fatalf("source change: packets = %v, pending = %d", out, len(s.pending))
		}
	})
}

func TestVideoTimestampAutoForwarding(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "")
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	f := NewRTPForwarder()
	addr := peer.LocalAddr().(*net.UDPAddr)
	if err := f.AddUDPClient("test", addr.IP.String(), addr.Port, addr.Port); err != nil {
		t.Fatal(err)
	}
	defer f.Stop()

	first := timestampPacket(0, 100, 1)
	f.ForwardVideoPacket(first)
	first.Payload[0] = 9
	f.ForwardAudioPacket(timestampPacket(0, 777, 2))
	buf := make([]byte, 1500)
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	var packet rtp.Packet
	if err := packet.Unmarshal(buf[:n]); err != nil || packet.Timestamp != 777 {
		t.Fatalf("audio packet = %+v, error = %v", packet, err)
	}

	f.videoTimestamp.sampleStart = time.Now().Add(-videoTimestampSampleTime)
	for i := 1; i <= 8; i++ {
		f.ForwardVideoPacket(timestampPacket(uint16(i), 100+uint32(i)*12000000, 1))
	}
	for i := 0; i <= 8; i++ {
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		if err := packet.Unmarshal(buf[:n]); err != nil {
			t.Fatal(err)
		}
		if packet.Timestamp != 100+uint32(i)*12000 || packet.Payload[0] != 1 {
			t.Fatalf("packet %d = %+v", i, packet)
		}
	}
	f.Stop()
	if f.videoTimestamp.started || f.videoTimestamp.divisor != 0 {
		t.Fatal("Stop did not reset automatic detection")
	}
}

func TestVideoTimestampBufferedPacketsReportDeadClientOnce(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "")
	peer, conn := net.Pipe()
	peer.Close()
	defer conn.Close()
	f := NewRTPForwarder()
	if err := f.AddTCPClient("dead", conn, 0, 2, 4); err != nil {
		t.Fatal(err)
	}
	f.forwardVideo(timestampPacket(0, 100, 1))
	f.videoTimestamp.sampleStart = time.Now().Add(-2 * time.Second)
	for i := 1; i < 8; i++ {
		f.forwardVideo(timestampPacket(uint16(i), 100+uint32(i)*12000000, 1))
	}
	ids, errs := f.forwardVideo(timestampPacket(8, 96000100, 1))
	if len(ids) != 1 || ids[0] != "dead" || len(errs) != 1 {
		t.Fatalf("dead clients = %v, errors = %v", ids, errs)
	}
}

func TestVideoTimestampEnvironment(t *testing.T) {
	for _, tc := range []struct {
		value string
		want  uint32
	}{
		{"", 0}, {"1", 1}, {"1000", 1000}, {"2", 2},
		{"0", 0}, {"-1", 0}, {"abc", 0}, {"4294967296", 0},
	} {
		t.Run(tc.value, func(t *testing.T) {
			t.Setenv("TUYA_VIDEO_TS_DIVISOR", tc.value)
			if got := newVideoTimestampScaler().divisor; got != tc.want {
				t.Fatalf("divisor = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestVideoTimestampOverrideForwardedOverUDP(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "1000")
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	f := NewRTPForwarder()
	addr := peer.LocalAddr().(*net.UDPAddr)
	if err := f.AddUDPClient("test", addr.IP.String(), addr.Port, 0); err != nil {
		t.Fatal(err)
	}
	defer f.Stop()
	for i := 0; i < 2; i++ {
		p := timestampPacket(uint16(i), 100+uint32(i)*6000000, 1)
		f.ForwardVideoPacket(p)
		buf := make([]byte, 1500)
		n, _, err := peer.ReadFromUDP(buf)
		if err != nil {
			t.Fatal(err)
		}
		var got rtp.Packet
		if err := got.Unmarshal(buf[:n]); err != nil {
			t.Fatal(err)
		}
		if got.Timestamp != 100+uint32(i)*6000 || got.PayloadType != 96 {
			t.Fatalf("forwarded header = %+v", got.Header)
		}
	}
}
