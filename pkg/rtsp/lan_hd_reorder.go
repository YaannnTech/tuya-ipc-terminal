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

type lanHDPendingPacket struct {
	packet   *rtp.Packet
	received time.Time
}

// lanHDReorder holds later packets briefly when an earlier sequence is missing.
type lanHDReorder struct {
	next    uint16
	ssrc    uint32
	pending map[uint16]lanHDPendingPacket
	warned  bool
}

func (r *lanHDReorder) push(packet *rtp.Packet, now time.Time) []*rtp.Packet {
	seq := packet.SequenceNumber
	if packet.SSRC != r.ssrc {
		*r = lanHDReorder{next: seq, ssrc: packet.SSRC}
	}
	if int16(seq-r.next) < 0 {
		return nil
	}
	if len(r.pending) == 0 && seq == r.next {
		r.next++
		return []*rtp.Packet{packet}
	}
	if r.pending == nil {
		r.pending = make(map[uint16]lanHDPendingPacket)
	}
	if _, exists := r.pending[seq]; !exists {
		r.pending[seq] = lanHDPendingPacket{packet: packet, received: now}
	}

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
		if now.Sub(oldest) < lanHDReorderWait && farthest < lanHDReorderWindow {
			break
		}
		if !r.warned {
			r.warned = true
			core.Logger.Warn().Msg("/hd-lan: skipping missing video RTP packets after reorder limit")
		}
		r.next += nearest
	}
	return out
}
