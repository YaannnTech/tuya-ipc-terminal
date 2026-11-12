package rtsp

import "github.com/pion/rtp"

// rtpContinuity is protected by the corresponding media mutex.
type rtpContinuity struct {
	started, pending, mapped bool
	ssrc                     uint32
	seq                      uint16
	timestamp, step          uint32
	duration                 uint32
	seqOffset                uint16
	timeOffset               uint32
}

func (c *rtpContinuity) renew() {
	c.pending = c.started
}

func (c *rtpContinuity) rewrite(packet *rtp.Packet, duration, fallback uint32) *rtp.Packet {
	out := packet
	if c.started && !c.mapped && !c.pending && packet.SSRC != c.ssrc {
		c.started = false
		c.step = 0
	}
	if c.pending {
		step := c.step
		if c.duration != 0 {
			step = c.duration
		}
		if step == 0 {
			step = fallback
		}
		c.seqOffset = c.seq + 1 - packet.SequenceNumber
		c.timeOffset = c.timestamp + step - packet.Timestamp
		c.pending, c.mapped = false, true
	}
	if c.mapped {
		out = packet.Clone()
		out.SequenceNumber += c.seqOffset
		out.Timestamp += c.timeOffset
		out.SSRC = c.ssrc
	}
	if !c.started || !c.mapped {
		c.ssrc = out.SSRC
	}
	if c.started && int16(out.SequenceNumber-c.seq) > 0 {
		delta := int32(out.Timestamp - c.timestamp)
		if delta > 0 {
			c.step = uint32(delta)
		}
	}
	if !c.started || int16(out.SequenceNumber-c.seq) > 0 {
		c.seq, c.timestamp, c.duration = out.SequenceNumber, out.Timestamp, duration
	}
	c.started = true
	return out
}
