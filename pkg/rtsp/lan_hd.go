package rtsp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/pion/rtp"
	pion "github.com/pion/webrtc/v4"

	"tuya-ipc-terminal/pkg/core"
)

const (
	lanHDResolution = "hd-lan"
	lanHDDelay      = 5 * time.Second
	lanHDMTU        = 1200
	maxLANHDNALSize = 8 << 20
)

func (wb *WebRTCBridge) upstreamResolution() string {
	if wb.resolution == lanHDResolution {
		return "sd"
	}
	return wb.resolution
}

func (wb *WebRTCBridge) lanHDICETransport() *pion.ICETransport {
	// Pion uses the same DTLS/ICE transport for bundled RTP and SCTP.
	return wb.peerConnection.SCTP().Transport().ICETransport()
}

func requireLANHDPeer(pair *pion.ICECandidatePair) error {
	if pair == nil || pair.Remote == nil {
		return errors.New("/hd-lan requires a selected ICE peer")
	}
	addr, err := netip.ParseAddr(pair.Remote.Address)
	if err == nil && pair.Remote.Typ != pion.ICECandidateTypeRelay {
		addr = addr.Unmap()
		if addr.IsPrivate() || addr.IsLinkLocalUnicast() {
			return nil
		}
	}
	return fmt.Errorf("/hd-lan requires a private or link-local non-relay ICE peer, got %q", pair.Remote.Address)
}

func switchLANHD(ctx context.Context, delay time.Duration, selectedPair func() (*pion.ICECandidatePair, error), send func(int) error) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	pair, err := selectedPair()
	if err != nil {
		return fmt.Errorf("/hd-lan: get selected ICE pair: %w", err)
	}
	if err := requireLANHDPeer(pair); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := send(0); err != nil {
		return fmt.Errorf("/hd-lan: request HD: %w", err)
	}
	return nil
}

func (wb *WebRTCBridge) requestLANHD() {
	// The reporter's camera accepts the switch after its SD stream starts.
	err := switchLANHD(wb.ctx, lanHDDelay, wb.lanHDICETransport().GetSelectedCandidatePair, wb.cameraClient.SendResolution)
	if err != nil {
		if wb.ctx.Err() == nil {
			wb.handleError(err)
		}
		return
	}
	wb.lanHDSwitched.Store(true)
	core.Logger.Info().Msg("/hd-lan: requested HD on the local SD session")
}

// lanHDVideo is owned by the video-track reader, including its reorder buffer.
type lanHDVideo struct {
	fragment   []byte
	seq        uint16
	timestamp  uint32
	ssrc       uint32
	params     [3][]byte
	ready      bool
	sequencer  rtp.Sequencer
	reorder    *lanHDReorder
	warnedDrop bool
}

func newLANHDVideo() *lanHDVideo {
	return &lanHDVideo{sequencer: rtp.NewRandomSequencer()}
}

func (v *lanHDVideo) warnDrop() {
	if !v.warnedDrop {
		v.warnedDrop = true
		core.Logger.Warn().Msg("/hd-lan: dropping malformed or incomplete video RTP payload")
	}
}

func (v *lanHDVideo) push(packet *rtp.Packet, now time.Time) []*rtp.Packet {
	if v.reorder == nil {
		out := v.repacketize(packet)
		// The camera can reset its sequence numbers during the switch.
		if v.ready {
			v.reorder = &lanHDReorder{next: packet.SequenceNumber + 1, ssrc: packet.SSRC}
		}
		return out
	}
	var out []*rtp.Packet
	for _, input := range v.reorder.push(packet, now) {
		out = append(out, v.repacketize(input)...)
	}
	return out
}

func (v *lanHDVideo) repacketize(packet *rtp.Packet) []*rtp.Packet {
	var nals [][]byte
	for _, nal := range v.unwrap(packet) {
		// Only single-layer HEVC with a valid temporal ID can be forwarded.
		if len(nal) < 3 || nal[0]&0x81 != 0 || nal[1]&0xf8 != 0 || nal[1]&7 == 0 {
			continue
		}
		kind := (nal[0] >> 1) & 63
		if kind >= 32 && kind <= 34 {
			v.params[kind-32] = append(v.params[kind-32][:0], nal...)
			continue
		}
		if kind > 40 {
			continue
		}
		// Repeat parameters for clients joining an already active stream.
		if kind >= 16 && kind <= 21 && nal[2]&0x80 != 0 &&
			len(v.params[0]) > 0 && len(v.params[1]) > 0 && len(v.params[2]) > 0 {
			v.ready = true
			nals = append(nals, v.params[:]...)
		}
		if v.ready {
			nals = append(nals, nal)
		}
	}
	payloads := packetizeLANHD(nals)
	out := make([]*rtp.Packet, 0, len(payloads))
	for i, payload := range payloads {
		out = append(out, &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    96,
				SequenceNumber: v.sequencer.NextSequenceNumber(),
				Timestamp:      packet.Timestamp,
				SSRC:           packet.SSRC,
				Marker:         packet.Marker && i == len(payloads)-1,
			},
			Payload: payload,
		})
	}
	return out
}

// packetizeLANHD emits RFC 7798 single NAL, aggregation and FU packets.
func packetizeLANHD(nals [][]byte) [][]byte {
	var payloads [][]byte
	for i := 0; i < len(nals); {
		nal := nals[i]
		if len(nal) > lanHDMTU {
			for offset := 2; offset < len(nal); {
				end := min(offset+lanHDMTU-3, len(nal))
				fu := []byte{(nal[0] & 0x81) | 49<<1, nal[1], (nal[0] >> 1) & 63}
				if offset == 2 {
					fu[2] |= 0x80
				}
				if end == len(nal) {
					fu[2] |= 0x40
				}
				fu = append(fu, nal[offset:end]...)
				payloads = append(payloads, fu)
				offset = end
			}
			i++
			continue
		}
		end, size := i, 2
		tid := byte(7)
		for end < len(nals) && size+2+len(nals[end]) <= lanHDMTU {
			size += 2 + len(nals[end])
			tid = min(tid, nals[end][1]&7)
			end++
		}
		if end-i < 2 {
			payloads = append(payloads, append([]byte(nil), nal...))
			i++
			continue
		}
		ap := make([]byte, 0, size)
		ap = append(ap, 48<<1, tid)
		for _, unit := range nals[i:end] {
			ap = binary.BigEndian.AppendUint16(ap, uint16(len(unit)))
			ap = append(ap, unit...)
		}
		payloads = append(payloads, ap)
		i = end
	}
	return payloads
}

func (v *lanHDVideo) unwrap(packet *rtp.Packet) [][]byte {
	p := packet.Payload
	if len(p) < 2 {
		v.fragment = nil
		v.warnDrop()
		return nil
	}
	if p[0]&31 == 28 {
		if nal := v.unwrapFragment(packet); nal != nil {
			return [][]byte{nal}
		}
		return nil
	}
	if len(v.fragment) != 0 {
		v.warnDrop()
		v.fragment = nil
	}
	switch kind := p[0] & 31; {
	case kind == 24:
		var nals [][]byte
		for p = p[1:]; len(p) != 0; {
			if len(p) < 2 {
				v.warnDrop()
				return nil
			}
			size := int(binary.BigEndian.Uint16(p))
			p = p[2:]
			if size < 2 || size > len(p) {
				v.warnDrop()
				return nil
			}
			nals = append(nals, p[:size])
			p = p[size:]
		}
		return nals
	case kind < 24:
		return [][]byte{p}
	default:
		v.warnDrop()
		return nil
	}
}

func (v *lanHDVideo) unwrapFragment(packet *rtp.Packet) []byte {
	p := packet.Payload
	start, end := p[1]&0x80 != 0, p[1]&0x40 != 0
	if len(p) < 3 || p[1]&0x20 != 0 || (start && end) {
		v.fragment = nil
		v.warnDrop()
		return nil
	}
	header := p[0]&0xe0 | p[1]&31
	if start {
		if len(v.fragment) != 0 {
			v.warnDrop()
		}
		v.fragment = []byte{header}
		v.timestamp, v.ssrc = packet.Timestamp, packet.SSRC
	} else if len(v.fragment) == 0 || v.fragment[0] != header ||
		v.seq+1 != packet.SequenceNumber || v.timestamp != packet.Timestamp || v.ssrc != packet.SSRC {
		v.fragment = nil
		v.warnDrop()
		return nil
	}
	if len(v.fragment)+len(p)-2 > maxLANHDNALSize {
		v.fragment = nil
		v.warnDrop()
		return nil
	}
	v.fragment = append(v.fragment, p[2:]...)
	v.seq = packet.SequenceNumber
	if !end {
		return nil
	}
	nal := v.fragment
	v.fragment = nil
	return nal
}
