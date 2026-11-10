package rtsp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	pion "github.com/pion/webrtc/v4"

	"tuya-ipc-terminal/pkg/storage"
	"tuya-ipc-terminal/pkg/tuya"
)

func TestLANHDPeer(t *testing.T) {
	tests := []struct {
		name string
		pair *pion.ICECandidatePair
		ok   bool
	}{
		{"missing pair", nil, false},
		{"missing remote", &pion.ICECandidatePair{}, false},
		{"private", lanHDPair("192.168.1.77", pion.ICECandidateTypeHost), true},
		{"mapped", lanHDPair("::ffff:10.1.2.3", pion.ICECandidateTypeHost), true},
		{"private v6", lanHDPair("fd00::1", pion.ICECandidateTypeHost), true},
		{"link local v4", lanHDPair("169.254.1.2", pion.ICECandidateTypeHost), true},
		{"link local v6", lanHDPair("fe80::1", pion.ICECandidateTypeHost), true},
		{"relay", lanHDPair("192.168.1.77", pion.ICECandidateTypeRelay), false},
		{"public", lanHDPair("152.32.227.120", pion.ICECandidateTypeHost), false},
		{"public v6", lanHDPair("2001:4860:4860::8888", pion.ICECandidateTypeHost), false},
		{"loopback", lanHDPair("127.0.0.1", pion.ICECandidateTypeHost), false},
		{"hostname", lanHDPair("camera.local", pion.ICECandidateTypeHost), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := requireLANHDPeer(tt.pair); (err == nil) != tt.ok {
				t.Fatalf("requireLANHDPeer() = %v, want success %t", err, tt.ok)
			}
		})
	}
}

func lanHDPair(address string, kind pion.ICECandidateType) *pion.ICECandidatePair {
	return &pion.ICECandidatePair{Remote: &pion.ICECandidate{Address: address, Typ: kind}}
}

func TestSwitchLANHD(t *testing.T) {
	pairError := errors.New("pair failed")
	sendError := errors.New("send failed")
	for _, tt := range []struct {
		name       string
		pair       *pion.ICECandidatePair
		pairErr    error
		sendErr    error
		cancel     bool
		wantErr    string
		wantCalls  int
		wantSelect int
	}{
		{"private", lanHDPair("192.168.1.77", pion.ICECandidateTypeHost), nil, nil, false, "", 1, 1},
		{"public", lanHDPair("152.32.227.120", pion.ICECandidateTypeHost), nil, nil, false, "requires a private", 0, 1},
		{"no pair", nil, nil, nil, false, "selected ICE peer", 0, 1},
		{"pair error", nil, pairError, nil, false, "pair failed", 0, 1},
		{"send error", lanHDPair("192.168.1.77", pion.ICECandidateTypeHost), nil, sendError, false, "send failed", 1, 1},
		{"cancelled", nil, nil, nil, true, "canceled", 0, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.cancel {
				cancel()
			}
			calls, selects := 0, 0
			err := switchLANHD(ctx, 0, func() (*pion.ICECandidatePair, error) {
				selects++
				return tt.pair, tt.pairErr
			}, func(resolution int) error {
				calls++
				if resolution != 0 {
					t.Errorf("resolution = %d, want HD (0)", resolution)
				}
				return tt.sendErr
			})
			if (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("switchLANHD() = %v, want %q", err, tt.wantErr)
			}
			if calls != tt.wantCalls || selects != tt.wantSelect {
				t.Fatalf("send calls = %d, selections = %d", calls, selects)
			}
		})
	}
}

func TestSwitchLANHDCancelledDuringPairLookup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := switchLANHD(ctx, 0, func() (*pion.ICECandidatePair, error) {
		cancel()
		return lanHDPair("192.168.1.77", pion.ICECandidateTypeHost), nil
	}, func(int) error {
		t.Fatal("sent a resolution request after cancellation")
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("switchLANHD() = %v, want cancellation", err)
	}
}

func TestLANHDUsesHostCandidatesOnly(t *testing.T) {
	config := &tuya.WebRTCConfig{P2PConfig: tuya.P2PConfig{
		Ices: []tuya.ICEServer{{Urls: "stun:stun.example.com:3478"}},
	}}
	for _, tt := range []struct {
		resolution string
		servers    int
	}{
		{"hd-lan", 0},
		{"sd", 1},
		{"hd", 1},
	} {
		t.Run(tt.resolution, func(t *testing.T) {
			bridge := NewWebRTCBridge(&storage.CameraInfo{}, tt.resolution, nil, nil)
			if err := bridge.setupPeerConnection(config); err != nil {
				t.Fatal(err)
			}
			defer bridge.Stop()
			if got := len(bridge.peerConnection.GetConfiguration().ICEServers); got != tt.servers {
				t.Fatalf("ICE server count = %d, want %d", got, tt.servers)
			}
			if tt.resolution == lanHDResolution && bridge.lanHDICETransport() == nil {
				t.Fatal("missing bundled ICE transport")
			}
		})
	}
}

func TestLANHDRoutesAndSDP(t *testing.T) {
	camera := &storage.CameraInfo{Skill: `{"videos":[{"streamType":2,"codecType":4,"width":2304,"height":1296},{"streamType":4,"codecType":2,"width":640,"height":360}]}`}
	server := &RTSPServer{}
	for _, tt := range []struct {
		url, path, resolution, upstream, codec string
	}{
		{"rtsp://localhost:8554/door/hd-lan", "/door", "hd-lan", "sd", "H265"},
		{"rtsp://localhost:8554/door/hd", "/door", "hd", "hd", "H265"},
		{"rtsp://localhost:8554/door/sd", "/door", "sd", "sd", "H264"},
		{"rtsp://localhost:8554/door", "/door", "hd", "hd", "H265"},
	} {
		t.Run(tt.url, func(t *testing.T) {
			path, resolution := extractCameraPath(tt.url)
			if path != tt.path || resolution != tt.resolution {
				t.Fatalf("route = %q, %q", path, resolution)
			}
			if got := (&WebRTCBridge{resolution: resolution}).upstreamResolution(); got != tt.upstream {
				t.Fatalf("upstream = %q, want %q", got, tt.upstream)
			}
			sdp := server.generateSDP(camera, tt.url, resolution)
			if !strings.Contains(sdp, "a=rtpmap:96 "+tt.codec+"/90000\r\n") {
				t.Fatalf("wrong SDP: %s", sdp)
			}
		})
	}
}

func TestLANHDPlayOmitsIncorrectRTPInfo(t *testing.T) {
	for _, resolution := range []string{"hd-lan", "hd", "sd"} {
		t.Run(resolution, func(t *testing.T) {
			clientConn, serverConn := net.Pipe()
			defer clientConn.Close()
			defer serverConn.Close()
			client := &RTSPClient{conn: serverConn, session: "test", stream: &CameraStream{resolution: resolution}}
			req := &RTSPRequest{URL: "rtsp://localhost/door/" + resolution, Headers: map[string]string{"Session": "test"}, CSeq: 1}
			go (&RTSPServer{}).handlePlay(client, req)
			clientConn.SetReadDeadline(time.Now().Add(time.Second))
			reader := bufio.NewReader(clientConn)
			var headers strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					t.Fatal(err)
				}
				headers.WriteString(line)
				if line == "\r\n" {
					break
				}
			}
			hasInfo := strings.Contains(headers.String(), "RTP-Info:")
			if hasInfo != (resolution != lanHDResolution) {
				t.Fatalf("unexpected RTP-Info: %s", headers.String())
			}
		})
	}
}

func stap(nals ...[]byte) []byte {
	out := []byte{0x58}
	for _, nal := range nals {
		out = binary.BigEndian.AppendUint16(out, uint16(len(nal)))
		out = append(out, nal...)
	}
	return out
}

func TestLANHDRepacketizesCapturedHeaders(t *testing.T) {
	v := newLANHDVideo()
	packet := &rtp.Packet{Header: rtp.Header{SequenceNumber: 65534, Timestamp: 12345, SSRC: 7}}
	packet.Payload = []byte{0x65, 0x88, 0x80}
	if out := v.repacketize(packet); len(out) != 0 {
		t.Fatal("forwarded H.264 before HEVC")
	}
	params := [][]byte{{0x40, 1, 2}, {0x42, 1, 3}, {0x44, 1, 4}}
	packet.Payload = stap(params...)
	if out := v.repacketize(packet); len(out) != 0 {
		t.Fatal("forwarded parameters before a keyframe")
	}
	idr := append([]byte{0x26, 1, 0x80}, bytes.Repeat([]byte{0x55}, 2400)...)
	packet.SequenceNumber = 65535
	packet.Payload = append([]byte{0x3c, 0x86}, idr[1:1201]...)
	if out := v.repacketize(packet); len(out) != 0 {
		t.Fatal("forwarded an incomplete FU")
	}
	packet.SequenceNumber = 0
	packet.Marker = true
	packet.Payload = append([]byte{0x3c, 0x46}, idr[1201:]...)
	out := v.repacketize(packet)
	if !v.ready || len(out) != 4 {
		t.Fatalf("ready = %t, output packets = %d, want AP and 3 FUs", v.ready, len(out))
	}
	wantAP := []byte{0x60, 1}
	for _, nal := range params {
		wantAP = binary.BigEndian.AppendUint16(wantAP, uint16(len(nal)))
		wantAP = append(wantAP, nal...)
	}
	if !bytes.Equal(out[0].Payload, wantAP) {
		t.Fatalf("AP = %x, want %x", out[0].Payload, wantAP)
	}
	rebuilt := append([]byte{}, idr[:2]...)
	for i, p := range out {
		if p.PayloadType != 96 || p.Timestamp != packet.Timestamp || p.SSRC != packet.SSRC ||
			p.Marker != (i == len(out)-1) || (i > 0 && p.SequenceNumber != out[i-1].SequenceNumber+1) {
			t.Fatalf("wrong RTP header %d: %+v", i, p.Header)
		}
		if i == 0 {
			continue
		}
		flags := byte(19)
		if i == 1 {
			flags |= 0x80
		}
		if i == len(out)-1 {
			flags |= 0x40
		}
		if !bytes.Equal(p.Payload[:3], []byte{0x62, 1, flags}) || len(p.Payload) > lanHDMTU {
			t.Fatalf("wrong HEVC FU %d: %x", i, p.Payload[:3])
		}
		rebuilt = append(rebuilt, p.Payload[3:]...)
	}
	if !bytes.Equal(rebuilt, idr) {
		t.Fatal("HEVC IDR changed during repacketization")
	}
	var decoder codecs.H265Depacketizer
	var annexB []byte
	for _, p := range out {
		nals, err := decoder.Unmarshal(p.Payload)
		if err != nil {
			t.Fatalf("Pion rejected HEVC RTP payload: %v", err)
		}
		annexB = append(annexB, nals...)
	}
	var wantAnnexB []byte
	for _, nal := range append(params, idr) {
		wantAnnexB = append(wantAnnexB, 0, 0, 0, 1)
		wantAnnexB = append(wantAnnexB, nal...)
	}
	if !bytes.Equal(annexB, wantAnnexB) {
		t.Fatal("Pion did not reconstruct the original HEVC NAL units")
	}
}

func TestLANHDSampleFUType2(t *testing.T) {
	v := newLANHDVideo()
	v.ready = true
	inputs := [][]byte{{0x1c, 0x82, 1, 0x80}, {0x1c, 0x02, 0xaa}, {0x1c, 0x42, 0xbb}}
	var out []*rtp.Packet
	for i, payload := range inputs {
		out = v.repacketize(&rtp.Packet{Header: rtp.Header{SequenceNumber: uint16(i), Timestamp: 20, SSRC: 8, Marker: i == 2}, Payload: payload})
	}

	if len(out) != 1 || !bytes.Equal(out[0].Payload, []byte{0x02, 1, 0x80, 0xaa, 0xbb}) || !out[0].Marker {
		t.Fatalf("incorrect reconstructed type-2 FU: %+v", out)
	}
}

func TestLANHDStartsReorderingAfterKeyframe(t *testing.T) {
	v := newLANHDVideo()
	now := time.Now()
	params := [][]byte{{0x40, 1, 2}, {0x42, 1, 3}, {0x44, 1, 4}}
	input := &rtp.Packet{Header: rtp.Header{SequenceNumber: 10, SSRC: 8}, Payload: stap(params...)}
	if out := v.push(input, now); len(out) != 0 || v.reorder != nil {
		t.Fatal("started before keyframe")
	}
	input = &rtp.Packet{Header: rtp.Header{SequenceNumber: 11, SSRC: 8, Marker: true}, Payload: []byte{0x26, 1, 0x80}}
	if out := v.push(input, now); len(out) != 1 || v.reorder == nil {
		t.Fatal("did not start at keyframe")
	}
	later := &rtp.Packet{Header: rtp.Header{SequenceNumber: 13, SSRC: 8}, Payload: []byte{2, 1, 0x80}}
	if out := v.push(later, now); len(out) != 0 {
		t.Fatal("did not buffer a packet ahead of a gap")
	}
	missing := &rtp.Packet{Header: rtp.Header{SequenceNumber: 12, SSRC: 8}, Payload: []byte{2, 1, 0x80}}
	if out := v.push(missing, now); len(out) != 2 {
		t.Fatalf("after filling gap got %d packets, want 2", len(out))
	}
}

func TestLANHDRejectsMalformedPackets(t *testing.T) {
	for _, payload := range [][]byte{
		nil, {0x58}, {0x58, 0}, {0x58, 0, 1, 0x40}, {0x58, 0, 4, 0x40, 1},
		{0x7d, 0, 1}, {0x3c, 0x86}, {0x3c, 0xc6, 1}, {0x3c, 0xa6, 1},
	} {
		v := newLANHDVideo()
		if out := v.unwrap(&rtp.Packet{Payload: payload}); len(out) != 0 || !v.warnedDrop {
			t.Fatalf("accepted malformed payload %x: %x", payload, out)
		}
	}
}

func TestLANHDRejectsBrokenFragments(t *testing.T) {
	for _, tt := range []struct {
		name string
		edit func(*rtp.Packet)
	}{
		{"gap", func(p *rtp.Packet) { p.SequenceNumber++ }},
		{"timestamp", func(p *rtp.Packet) { p.Timestamp++ }},
		{"ssrc", func(p *rtp.Packet) { p.SSRC++ }},
		{"header", func(p *rtp.Packet) { p.Payload[1] = 0x42 }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v := newLANHDVideo()
			start := &rtp.Packet{Header: rtp.Header{SequenceNumber: 10, Timestamp: 100, SSRC: 8}, Payload: []byte{0x3c, 0x86, 1, 0x80}}
			v.unwrap(start)
			end := &rtp.Packet{Header: rtp.Header{SequenceNumber: 11, Timestamp: 100, SSRC: 8}, Payload: []byte{0x3c, 0x46, 1}}
			tt.edit(end)
			if out := v.unwrap(end); len(out) != 0 || len(v.fragment) != 0 || !v.warnedDrop {
				t.Fatalf("accepted broken FU: %x", out)
			}
		})
	}
	v := newLANHDVideo()
	v.fragment = make([]byte, maxLANHDNALSize)
	v.fragment[0], v.seq, v.timestamp, v.ssrc = 0x26, 10, 100, 8
	p := &rtp.Packet{Header: rtp.Header{SequenceNumber: 11, Timestamp: 100, SSRC: 8}, Payload: []byte{0x3c, 0x46, 1}}
	if out := v.unwrap(p); len(out) != 0 || len(v.fragment) != 0 || !v.warnedDrop {
		t.Fatal("accepted oversized FU")
	}
}

func TestLANHDStartupErrorUnblocksWaiters(t *testing.T) {
	bridge := NewWebRTCBridge(&storage.CameraInfo{}, lanHDResolution, nil, nil)
	want := errors.New("test error")
	bridge.handleError(want)
	if err := bridge.waitFor(&bridge.waiter, "connection"); !errors.Is(err, want) {
		t.Fatalf("connection error = %v", err)
	}
	if err := bridge.waitFor(&bridge.lanHDReady, "HEVC"); !errors.Is(err, want) {
		t.Fatalf("HEVC error = %v", err)
	}
	bridge.Stop()
}
