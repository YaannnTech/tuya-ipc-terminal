package rtsp

import (
	"encoding/binary"

	"github.com/pion/rtp"
)

const hevcGateMaxBytes = 8 << 20
const hevcGateMaxPackets = 8192

// hevcRenewalGate accepts RFC 7798 packets without DONL, as advertised by SDP.
// The buffered access unit is released only after its marker and all FU ends.
type hevcRenewalGate struct {
	active, compact bool
	offset          uint16
	started         bool
	source          uint32
	params          []*rtp.Packet
	paramMask       uint8
	frame           []*rtp.Packet
	frameSeq        uint16
	frameTS         uint32
	fragment        []*rtp.Packet
	fragmentKind    byte
	fragmentSeq     uint16
	fragmentTS      uint32
	fragmentHeader  [2]byte
	bytes           int
}

func hevcNALKind(nal []byte) (byte, bool) {
	if len(nal) < 3 || nal[0]&0x80 != 0 || nal[1]&7 == 0 {
		return 0, false
	}
	return (nal[0] >> 1) & 63, true
}

func (g *hevcRenewalGate) clearFrame() {
	g.frame = nil
	g.fragment = nil
	g.recount()
}

func (g *hevcRenewalGate) recount() {
	g.bytes = 0
	for _, list := range [][]*rtp.Packet{g.params, g.frame, g.fragment} {
		for _, p := range list {
			g.bytes += p.MarshalSize()
		}
	}
}

func (g *hevcRenewalGate) push(packet *rtp.Packet) []*rtp.Packet {
	if !g.active {
		if !g.compact {
			return []*rtp.Packet{packet}
		}
		out := packet.Clone()
		out.SequenceNumber += g.offset
		return []*rtp.Packet{out}
	}
	if g.started && packet.SSRC != g.source {
		*g = hevcRenewalGate{active: true}
	}
	g.started, g.source = true, packet.SSRC
	if g.bytes+packet.MarshalSize() > hevcGateMaxBytes ||
		len(g.params)+len(g.frame)+len(g.fragment) >= hevcGateMaxPackets {
		*g = hevcRenewalGate{active: true, started: true, source: packet.SSRC}
		if packet.MarshalSize() > hevcGateMaxBytes {
			return nil
		}
	}

	payload := packet.Payload
	kind, valid := hevcNALKind(payload)
	if !valid {
		g.clearFrame()
		return nil
	}
	frameSeq := g.frameSeq
	if len(g.fragment) > 0 {
		frameSeq = g.fragmentSeq
	}
	if len(g.frame) > 0 && (packet.SequenceNumber != frameSeq+1 || packet.Timestamp != g.frameTS) {
		g.clearFrame()
	}
	if len(g.fragment) > 0 && (kind != 49 || packet.SequenceNumber != g.fragmentSeq+1 ||
		packet.Timestamp != g.fragmentTS || [2]byte{payload[0], payload[1]} != g.fragmentHeader) {
		g.clearFrame()
	}
	if kind == 49 {
		return g.pushFragment(packet)
	}
	if len(g.fragment) != 0 {
		g.clearFrame()
		return nil
	}

	mask, irap, first, ok := hevcGatePayload(payload)
	if !ok {
		g.clearFrame()
		return nil
	}
	return g.accept([]*rtp.Packet{packet.Clone()}, mask, irap, first)
}

func hevcGatePayload(payload []byte) (mask uint8, irap, first, ok bool) {
	kind, valid := hevcNALKind(payload)
	if !valid {
		return
	}
	if kind == 48 {
		remaining := payload[2:]
		count := 0
		for len(remaining) > 0 {
			if len(remaining) < 2 {
				return 0, false, false, false
			}
			n := int(binary.BigEndian.Uint16(remaining))
			remaining = remaining[2:]
			if n < 3 || n > len(remaining) || remaining[0]>>1&63 >= 48 {
				return 0, false, false, false
			}
			m, i, f, valid := hevcGatePayload(remaining[:n])
			if !valid || (irap && (m != 0 || f)) {
				return 0, false, false, false
			}
			if i && !irap && !f {
				return 0, false, false, false
			}
			mask |= m
			irap, first = irap || i, first || f
			remaining = remaining[n:]
			count++
		}
		return mask, irap, first, count >= 2
	}
	switch {
	case kind >= 32 && kind <= 34:
		return 1 << (kind - 32), false, false, true
	case kind >= 16 && kind <= 21:
		return 0, true, payload[2]&0x80 != 0, true
	case kind == 35 || kind == 38 || kind == 39 || kind == 40:
		return 0, false, false, true
	default:
		return 0, false, false, false
	}
}

func (g *hevcRenewalGate) pushFragment(packet *rtp.Packet) []*rtp.Packet {
	p := packet.Payload
	if len(p) < 4 {
		g.clearFrame()
		return nil
	}
	kind, start, end := p[2]&63, p[2]&0x80 != 0, p[2]&0x40 != 0
	allowed := kind >= 16 && kind <= 21 || kind >= 32 && kind <= 35 ||
		kind == 38 || kind == 39 || kind == 40
	if !allowed || (start && end) {
		g.clearFrame()
		return nil
	}
	if start {
		if len(g.fragment) != 0 {
			g.clearFrame()
		}
		if kind <= 21 && len(g.frame) == 0 && (g.paramMask != 7 || p[3]&0x80 == 0) {
			return nil
		}
		if kind >= 35 && len(g.frame) == 0 {
			return nil
		}
		g.fragmentKind, g.fragmentTS = kind, packet.Timestamp
		g.fragmentHeader = [2]byte{p[0], p[1]}
	} else if len(g.fragment) == 0 || kind != g.fragmentKind {
		g.clearFrame()
		return nil
	}
	g.fragmentSeq = packet.SequenceNumber
	g.fragment = append(g.fragment, packet.Clone())
	g.bytes += packet.MarshalSize()
	if !end {
		if packet.Marker {
			g.clearFrame()
		}
		return nil
	}
	fragments := g.fragment
	g.fragment = nil
	g.recount()
	if kind >= 32 && kind <= 34 {
		return g.accept(fragments, 1<<(kind-32), false, false)
	}
	return g.accept(fragments, 0, kind <= 21, fragments[0].Payload[3]&0x80 != 0)
}

func (g *hevcRenewalGate) accept(packets []*rtp.Packet, mask uint8, irap, first bool) []*rtp.Packet {
	if len(g.frame) == 0 && irap && (!first || g.paramMask|mask != 7) {
		return nil
	}
	if len(g.frame) > 0 && mask != 0 {
		g.clearFrame()
		return nil
	}
	if len(g.frame) == 0 && !irap {
		if mask == 0 {
			return nil
		}
		g.params = append(g.params, packets...)
		g.paramMask |= mask
		g.recount()
		return nil
	}
	g.frame = append(g.frame, packets...)
	last := packets[len(packets)-1]
	g.frameSeq, g.frameTS = last.SequenceNumber, last.Timestamp
	g.recount()
	if !last.Marker {
		return nil
	}
	out := append(g.params, g.frame...)
	seq := out[0].SequenceNumber
	for _, p := range out {
		original := p.SequenceNumber
		p.SequenceNumber = seq
		g.offset = seq - original
		seq++
	}
	g.params, g.frame = nil, nil
	g.bytes = 0
	g.active, g.compact = false, true
	return out
}
