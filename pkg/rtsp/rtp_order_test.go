package rtsp

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtp"
)

type testVideoTrack struct {
	packets  chan *rtp.Packet
	deadline time.Time
}

func (t *testVideoTrack) SetReadDeadline(deadline time.Time) error {
	t.deadline = deadline
	return nil
}

func (t *testVideoTrack) ReadRTP() (*rtp.Packet, interceptor.Attributes, error) {
	var timeout <-chan time.Time
	var timer *time.Timer
	if !t.deadline.IsZero() {
		timer = time.NewTimer(time.Until(t.deadline))
		defer timer.Stop()
		timeout = timer.C
	}
	select {
	case packet, ok := <-t.packets:
		if !ok {
			return nil, nil, io.EOF
		}
		return packet, nil, nil
	case <-timeout:
		return nil, nil, os.ErrDeadlineExceeded
	}
}

func startTestVideoTrack(t *testing.T, wait time.Duration, window uint16) (*testVideoTrack, <-chan *rtp.Packet) {
	t.Helper()
	oldWait, oldWindow := videoReorderWait, videoReorderWindow
	videoReorderWait, videoReorderWindow = wait, window
	track := &testVideoTrack{packets: make(chan *rtp.Packet, 32)}
	out := make(chan *rtp.Packet, 32)
	done := make(chan struct{})
	go func() {
		defer close(done)
		forwardVideoRTPTrack(context.Background(), track, func(p *rtp.Packet) { out <- p })
	}()
	t.Cleanup(func() {
		close(track.packets)
		<-done
		videoReorderWait, videoReorderWindow = oldWait, oldWindow
	})
	return track, out
}

func readOrderedPacket(t *testing.T, out <-chan *rtp.Packet, seq uint16, ts uint32) {
	t.Helper()
	select {
	case p := <-out:
		if p.SequenceNumber != seq || p.Timestamp != ts {
			t.Fatalf("got sequence %d timestamp %d, want %d timestamp %d",
				p.SequenceNumber, p.Timestamp, seq, ts)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for sequence %d", seq)
	}
}

func assertNoOrderedPacket(t *testing.T, out <-chan *rtp.Packet) {
	t.Helper()
	select {
	case p := <-out:
		t.Fatalf("unexpected packet: sequence %d", p.SequenceNumber)
	case <-time.After(20 * time.Millisecond):
	}
}

func TestRTPVideoReordersLateRetransmission(t *testing.T) {
	track, out := startTestVideoTrack(t, time.Second, 1024)
	track.packets <- timestampPacket(10, 100, 1)
	readOrderedPacket(t, out, 10, 100)
	track.packets <- timestampPacket(12, 300, 1)
	track.packets <- timestampPacket(13, 400, 1)
	assertNoOrderedPacket(t, out)
	track.packets <- timestampPacket(11, 200, 1)
	for i := uint16(11); i <= 13; i++ {
		readOrderedPacket(t, out, i, uint32(i-9)*100)
	}
}

func TestRTPVideoSkipsMissingPacketAfterWait(t *testing.T) {
	track, out := startTestVideoTrack(t, 60*time.Millisecond, 1024)
	track.packets <- timestampPacket(1, 100, 1)
	readOrderedPacket(t, out, 1, 100)
	track.packets <- timestampPacket(3, 300, 1)
	assertNoOrderedPacket(t, out)
	readOrderedPacket(t, out, 3, 300)
	track.packets <- timestampPacket(2, 200, 1)
	track.packets <- timestampPacket(4, 400, 1)
	readOrderedPacket(t, out, 4, 400)
	assertNoOrderedPacket(t, out)
}

func TestRTPVideoSequenceWrap(t *testing.T) {
	track, out := startTestVideoTrack(t, time.Second, 1024)
	track.packets <- timestampPacket(65534, 100, 0)
	readOrderedPacket(t, out, 65534, 100)
	track.packets <- timestampPacket(0, 300, 0)
	assertNoOrderedPacket(t, out)
	track.packets <- timestampPacket(65535, 200, 0)
	readOrderedPacket(t, out, 65535, 200)
	readOrderedPacket(t, out, 0, 300)
}

func TestRTPVideoSequenceWindowAndNewSource(t *testing.T) {
	track, out := startTestVideoTrack(t, time.Second, 4)
	track.packets <- timestampPacket(10, 100, 1)
	readOrderedPacket(t, out, 10, 100)
	track.packets <- timestampPacket(12, 200, 1)
	track.packets <- timestampPacket(12, 200, 1)
	track.packets <- timestampPacket(15, 300, 1)
	readOrderedPacket(t, out, 12, 200)
	track.packets <- timestampPacket(1, 400, 2)
	readOrderedPacket(t, out, 1, 400)
	assertNoOrderedPacket(t, out)
}

func udpForwarder(t *testing.T) (*RTPForwarder, *net.UDPConn) {
	t.Helper()
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "1")
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := NewRTPForwarder()
	addr := peer.LocalAddr().(*net.UDPAddr)
	if err := f.AddUDPClient("test", addr.IP.String(), addr.Port, addr.Port); err != nil {
		peer.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Stop(); peer.Close() })
	return f, peer
}

func readUDPOrderPacket(t *testing.T, peer *net.UDPConn, seq uint16, ts uint32) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	var packet rtp.Packet
	if err := packet.Unmarshal(buf[:n]); err != nil {
		t.Fatal(err)
	}
	if packet.SequenceNumber != seq || packet.Timestamp != ts {
		t.Fatalf("got sequence %d timestamp %d, want %d timestamp %d",
			packet.SequenceNumber, packet.Timestamp, seq, ts)
	}
}

func TestRTPVideoBFrameTimestampsUnchanged(t *testing.T) {
	f, peer := udpForwarder(t)
	track, out := startTestVideoTrack(t, time.Second, 1024)
	track.packets <- timestampPacket(1, 29790, 1)
	track.packets <- timestampPacket(3, 37350, 1)
	track.packets <- timestampPacket(2, 44820, 1)
	track.packets <- timestampPacket(4, 59850, 1)
	for i, ts := range []uint32{29790, 44820, 37350, 59850} {
		select {
		case p := <-out:
			f.ForwardVideoPacket(p)
			readUDPOrderPacket(t, peer, uint16(i+1), ts)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for video packet")
		}
	}
}

func TestRTPAudioDropsLateAndDuplicate(t *testing.T) {
	f, peer := udpForwarder(t)
	for _, p := range []*rtp.Packet{
		timestampPacket(65534, 1000, 1), timestampPacket(0, 900, 1),
		timestampPacket(65535, 950, 1), timestampPacket(0, 900, 1),
		timestampPacket(1, 800, 1), timestampPacket(9, 50, 2),
	} {
		f.ForwardAudioPacket(p)
	}
	for _, want := range []struct {
		seq uint16
		ts  uint32
	}{{65534, 1000}, {0, 900}, {1, 800}, {9, 50}} {
		readUDPOrderPacket(t, peer, want.seq, want.ts)
	}
	if err := peer.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := peer.ReadFromUDP(make([]byte, 1500)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("unexpected extra audio packet or read error: %v", err)
	}
}
