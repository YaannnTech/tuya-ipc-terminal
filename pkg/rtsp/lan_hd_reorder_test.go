package rtsp

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/rtp"
)

func TestLANHDReordersFragmentsAcrossWrap(t *testing.T) {
	now := time.Now()
	v := newLANHDVideo()
	v.ready = true
	v.reorder = &rtpReorder{started: true, next: 65534, ssrc: 8}
	start := &rtp.Packet{Header: rtp.Header{SequenceNumber: 65534, Timestamp: 100, SSRC: 8}, Payload: []byte{0x3c, 0x86, 1, 0x80, 0xaa}}
	end := &rtp.Packet{Header: rtp.Header{SequenceNumber: 0, Timestamp: 100, SSRC: 8, Marker: true}, Payload: []byte{0x3c, 0x46, 0xcc}}
	middle := &rtp.Packet{Header: rtp.Header{SequenceNumber: 65535, Timestamp: 100, SSRC: 8}, Payload: []byte{0x3c, 0x06, 0xbb}}
	if out := v.push(start, now); len(out) != 0 {
		t.Fatal("emitted incomplete FU")
	}
	if out := v.push(end, now.Add(time.Millisecond)); len(out) != 0 {
		t.Fatal("emitted FU before missing fragment arrived")
	}
	out := v.push(middle, now.Add(10*time.Millisecond))
	if len(out) != 1 || !bytes.Equal(out[0].Payload, []byte{0x26, 1, 0x80, 0xaa, 0xbb, 0xcc}) || !out[0].Marker {
		t.Fatalf("incorrect reordered FU: %+v", out)
	}
}

func TestLANHDReorderLimitsAndDuplicates(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name  string
		after time.Duration
		seq   uint16
		count int
	}{
		{"time limit", lanHDReorderWait, 12, 2},
		{"sequence limit", time.Millisecond, 10 + lanHDReorderWindow, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := rtpReorder{started: true, next: 10, ssrc: 8}
			first := &rtp.Packet{Header: rtp.Header{SequenceNumber: 11, SSRC: 8}}
			out := r.push(first, now, lanHDReorderWait, lanHDReorderWindow, "/hd-lan")
			if len(out) != 0 {
				t.Fatal("did not wait for missing packet")
			}
			out = r.push(first, now, lanHDReorderWait, lanHDReorderWindow, "/hd-lan")
			if len(out) != 0 {
				t.Fatal("duplicate packet was emitted")
			}
			next := &rtp.Packet{Header: rtp.Header{SequenceNumber: tt.seq, SSRC: 8}}
			out = r.push(next, now.Add(tt.after), lanHDReorderWait, lanHDReorderWindow, "/hd-lan")
			if len(out) != tt.count || out[0] != first || (tt.count == 2 && out[1] != next) {
				t.Fatalf("unexpected packets after gap: %v", out)
			}
			if !r.warned {
				t.Fatal("did not report skipped packets")
			}
			skipped := &rtp.Packet{Header: rtp.Header{SequenceNumber: 10, SSRC: 8}}
			out = r.push(skipped, now.Add(tt.after), lanHDReorderWait, lanHDReorderWindow, "/hd-lan")
			if len(out) != 0 {
				t.Fatal("accepted skipped packet")
			}
			newSSRC := &rtp.Packet{Header: rtp.Header{SequenceNumber: 3, SSRC: 9}}
			out = r.push(newSSRC, now, lanHDReorderWait, lanHDReorderWindow, "/hd-lan")
			if len(out) != 1 || out[0] != newSSRC {
				t.Fatal("did not reset for a changed SSRC")
			}
		})
	}
}
