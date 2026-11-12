package rtsp

import (
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func renewalPacket(seq uint16, ts, ssrc uint32, marker bool, payload ...byte) *rtp.Packet {
	return &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: ssrc, Marker: marker}, Payload: payload}
}

func newRenewalForwarder(t *testing.T) (*RTPForwarder, *net.UDPConn) {
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

func readRenewalPacket(t *testing.T, peer *net.UDPConn) *rtp.Packet {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65536)
	n, _, err := peer.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	p := &rtp.Packet{}
	if err := p.Unmarshal(buf[:n]); err != nil {
		t.Fatal(err)
	}
	return p
}

func noRenewalPacket(t *testing.T, peer *net.UDPConn) {
	t.Helper()
	if err := peer.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := peer.ReadFromUDP(make([]byte, 1500)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("unexpected packet or read error: %v", err)
	}
}

func TestRenewalContinuitySeparateMediaAndWrap(t *testing.T) {
	f, peer := newRenewalForwarder(t)
	for _, p := range []*rtp.Packet{
		renewalPacket(65534, 0xffffe890, 11, false, 1),
		renewalPacket(65535, 0xfffff448, 11, false, 1),
		renewalPacket(0, 0xfffff448, 11, true, 1),
	} {
		f.ForwardVideoPacket(p)
		got := readRenewalPacket(t, peer)
		if got.SequenceNumber != p.SequenceNumber || got.Timestamp != p.Timestamp || got.SSRC != p.SSRC {
			t.Fatalf("pre-renewal packet changed: %+v", got.Header)
		}
	}
	audio := renewalPacket(9, 0xfffffff0, 22, true, make([]byte, 32)...)
	f.ForwardAudioPacket(audio)
	readRenewalPacket(t, peer)

	f.RenewSource(false)
	video := renewalPacket(900, 100, 33, true, 1)
	f.ForwardVideoPacket(video)
	got := readRenewalPacket(t, peer)
	if got.SequenceNumber != 1 || got.Timestamp != 0 || got.SSRC != 11 {
		t.Fatalf("new video header: %+v", got.Header)
	}
	if video.SequenceNumber != 900 || video.Timestamp != 100 || video.SSRC != 33 {
		t.Fatal("input video modified")
	}
	audio = renewalPacket(9, 500, 22, true, make([]byte, 20)...)
	f.ForwardAudioPacket(audio)
	got = readRenewalPacket(t, peer)
	if got.SequenceNumber != 10 || got.Timestamp != 16 || got.SSRC != 22 {
		t.Fatalf("new audio header: %+v", got.Header)
	}
	f.ForwardAudioPacket(audio)
	noRenewalPacket(t, peer)
	video2 := renewalPacket(902, 700, 33, true, 1)
	f.ForwardVideoPacket(video2)
	got = readRenewalPacket(t, peer)
	if got.SequenceNumber != 3 || got.Timestamp != 600 {
		t.Fatalf("offset did not preserve source gap: %+v", got.Header)
	}
}

func TestRenewalHEVCGateFragmentLossAndCompleteFrame(t *testing.T) {
	f, peer := newRenewalForwarder(t)
	f.ForwardVideoPacket(renewalPacket(10, 100, 42, true, 1))
	readRenewalPacket(t, peer)
	f.RenewSource(true)
	// Fragmented VPS loses its end; it must not count as a parameter set.
	for _, p := range []*rtp.Packet{
		renewalPacket(1, 200, 99, false, 98, 1, 0x80|32, 1),
		renewalPacket(3, 200, 99, false, 98, 1, 0x40|32, 2),
		renewalPacket(4, 201, 99, false, 66, 1, 1),
		renewalPacket(5, 202, 99, false, 68, 1, 1),
		renewalPacket(6, 203, 99, true, 38, 1, 0x80),
	} {
		f.ForwardVideoPacket(p)
	}
	noRenewalPacket(t, peer)
	// Full VPS, then an IRAP FU which loses a fragment.
	for _, p := range []*rtp.Packet{
		renewalPacket(7, 204, 99, false, 98, 1, 0x80|32, 1),
		renewalPacket(8, 204, 99, true, 98, 1, 0x40|32, 2),
		renewalPacket(9, 205, 99, false, 98, 1, 0x80|19, 0x80),
		renewalPacket(11, 205, 99, true, 98, 1, 0x40|19, 2),
	} {
		f.ForwardVideoPacket(p)
	}
	noRenewalPacket(t, peer)
	start := renewalPacket(12, 206, 99, false, 98, 1, 0x80|19, 0x80)
	f.ForwardVideoPacket(start)
	start.Payload[3] = 0
	noRenewalPacket(t, peer)
	f.ForwardVideoPacket(renewalPacket(13, 206, 99, true, 98, 1, 0x40|19, 3))
	for i := 0; i < 6; i++ {
		got := readRenewalPacket(t, peer)
		if got.SequenceNumber != uint16(11+i) || got.SSRC != 42 {
			t.Fatalf("released packet %d: %+v", i, got.Header)
		}
		if i == 0 && got.Timestamp != 6100 {
			t.Fatalf("fallback timestamp = %d", got.Timestamp)
		}
		if i == 4 && got.Payload[3] != 0x80 {
			t.Fatal("buffered input was not cloned")
		}
	}
	f.ForwardVideoPacket(renewalPacket(14, 207, 99, true, 2, 1, 1))
	got := readRenewalPacket(t, peer)
	if got.SequenceNumber != 17 {
		t.Fatalf("post-gate sequence = %d", got.SequenceNumber)
	}
}

func TestRenewalHEVCAggregationAndMalformedPackets(t *testing.T) {
	f, peer := newRenewalForwarder(t)
	f.RenewSource(true)
	// Invalid AP and a non-IRAP cannot unlock the gate.
	f.ForwardVideoPacket(renewalPacket(1, 10, 1, true, 96, 1, 0, 9, 64, 1, 1))
	f.ForwardVideoPacket(renewalPacket(2, 10, 1, true, 2, 1, 0x80))
	noRenewalPacket(t, peer)
	ap := []byte{96, 1, 0, 3, 64, 1, 1, 0, 3, 66, 1, 1, 0, 3, 68, 1, 1, 0, 3, 42, 1, 0x80}
	f.ForwardVideoPacket(renewalPacket(3, 11, 1, true, ap...))
	got := readRenewalPacket(t, peer)
	if got.SequenceNumber != 3 || len(got.Payload) != len(ap) {
		t.Fatalf("AP not forwarded intact: %+v", got)
	}
}

func TestRenewalResetsScalerAndConcurrentState(t *testing.T) {
	f, _ := newRenewalForwarder(t)
	f.ForwardVideoPacket(renewalPacket(1, 1, 1, true, 1))
	f.RenewSource(false)
	if f.videoTimestamp.started {
		t.Fatal("video timestamp scaler was not reset")
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				switch i {
				case 0:
					f.RenewSource(false)
				case 1:
					f.ForwardVideoPacket(renewalPacket(uint16(j), uint32(j), 1, true, 1))
				case 2:
					f.ForwardAudioPacket(renewalPacket(uint16(j), uint32(j), 2, true, 1))
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestRenewalDiscardsPendingTimestampSamples(t *testing.T) {
	t.Setenv("TUYA_VIDEO_TS_DIVISOR", "")
	f := NewRTPForwarder()
	f.clients["test"] = &RTPClient{}
	f.ForwardVideoPacket(renewalPacket(1, 100, 1, true, 1))
	if len(f.videoTimestamp.pending) != 1 {
		t.Fatal("expected a pending timestamp sample")
	}
	f.RenewSource(false)
	if f.videoTimestamp.started || len(f.videoTimestamp.pending) != 0 {
		t.Fatal("renewal retained old timestamp samples")
	}
	f.ForwardVideoPacket(renewalPacket(1, 200, 1, true, 1))
	if len(f.videoTimestamp.pending) != 1 || f.videoTimestamp.pending[0].Timestamp != 200 {
		t.Fatal("new source did not start a fresh sample")
	}
}

func TestRenewalContinuityRepeatedSources(t *testing.T) {
	f, peer := newRenewalForwarder(t)
	f.ForwardAudioPacket(renewalPacket(65535, 100, 10, true, make([]byte, 160)...))
	readRenewalPacket(t, peer)
	for i := 0; i < 2; i++ {
		f.RenewSource(false)
		f.ForwardAudioPacket(renewalPacket(10, 300, uint32(20+i), true, make([]byte, 160)...))
		got := readRenewalPacket(t, peer)
		if got.SequenceNumber != uint16(i) || got.Timestamp != uint32(260+160*i) || got.SSRC != 10 {
			t.Fatalf("renewal %d: %+v", i, got.Header)
		}
	}
}

func TestRenewalAudioDurationFallback(t *testing.T) {
	f, peer := newRenewalForwarder(t)
	f.ForwardAudioPacket(renewalPacket(1, 100, 1, true))
	readRenewalPacket(t, peer)
	f.RenewSource(false)
	f.ForwardAudioPacket(renewalPacket(50, 500, 2, true, make([]byte, 160)...))
	got := readRenewalPacket(t, peer)
	if got.Timestamp != 260 || got.SequenceNumber != 2 || got.SSRC != 1 {
		t.Fatalf("new audio header: %+v", got.Header)
	}
}

func TestRenewalHEVCDropsIncompleteAccessUnit(t *testing.T) {
	f, peer := newRenewalForwarder(t)
	f.RenewSource(true)
	for i, typ := range []byte{32, 33, 34} {
		f.ForwardVideoPacket(renewalPacket(uint16(i+1), 100, 1, false, typ<<1, 1, 1))
	}
	f.ForwardVideoPacket(renewalPacket(4, 200, 1, false, 38, 1, 0x80))
	// A missing slice makes the marked packet unsafe to release.
	f.ForwardVideoPacket(renewalPacket(6, 200, 1, true, 38, 1, 0))
	noRenewalPacket(t, peer)
	f.ForwardVideoPacket(renewalPacket(7, 300, 1, false, 38, 1, 0x80))
	f.ForwardVideoPacket(renewalPacket(8, 300, 1, true, 38, 1, 0))
	for i := 0; i < 5; i++ {
		readRenewalPacket(t, peer)
	}
}

func TestRenewalHEVCFragmentedSuffixSEI(t *testing.T) {
	f, peer := newRenewalForwarder(t)
	f.RenewSource(true)
	for i, typ := range []byte{32, 33, 34} {
		f.ForwardVideoPacket(renewalPacket(uint16(i+1), 100, 1, false, typ<<1, 1, 1))
	}
	f.ForwardVideoPacket(renewalPacket(4, 200, 1, false, 38, 1, 0x80))
	f.ForwardVideoPacket(renewalPacket(5, 200, 1, false, 98, 1, 0x80|40, 1))
	noRenewalPacket(t, peer)
	f.ForwardVideoPacket(renewalPacket(6, 200, 1, true, 98, 1, 0x40|40, 2))
	for i := 0; i < 6; i++ {
		readRenewalPacket(t, peer)
	}
}
