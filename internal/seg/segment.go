// Package seg defines Segment class.
package seg

import (
	"encoding/binary"
)

/* encode 8 bits unsigned int */
func ikcp_encode8u(p []byte, c byte) []byte {
	p[0] = c
	return p[1:]
}

/* decode 8 bits unsigned int */
func ikcp_decode8u(p []byte, c *byte) []byte {
	*c = p[0]
	return p[1:]
}

/* encode 16 bits unsigned int (lsb) */
func ikcp_encode16u(p []byte, w uint16) []byte {
	binary.LittleEndian.PutUint16(p, w)
	return p[2:]
}

/* decode 16 bits unsigned int (lsb) */
func ikcp_decode16u(p []byte, w *uint16) []byte {
	*w = binary.LittleEndian.Uint16(p)
	return p[2:]
}

/* encode 32 bits unsigned int (lsb) */
func ikcp_encode32u(p []byte, l uint32) []byte {
	binary.LittleEndian.PutUint32(p, l)
	return p[4:]
}

/* decode 32 bits unsigned int (lsb) */
func ikcp_decode32u(p []byte, l *uint32) []byte {
	*l = binary.LittleEndian.Uint32(p)
	return p[4:]
}

func _imin_(a, b uint32) uint32 {
	if a <= b {
		return a
	}
	return b
}

func _imax_(a, b uint32) uint32 {
	if a >= b {
		return a
	}
	return b
}

func _ibound_(lower, middle, upper uint32) uint32 {
	return _imin_(_imax_(lower, middle), upper)
}

func _itimediff(later, earlier uint32) int32 {
	return (int32)(later - earlier)
}

// Segment defines a KCP segment
type Segment struct {
	ConversationID uint32

	Cmd  uint8 // CMD_PUSH/CMD_ACK/CMD_WASK/CMD_WINS
	Frg  uint8 // fragment
	Wnd  uint16
	Ts   uint32
	Sn   uint32 // Serial number
	Una  uint32
	RTO  uint32 // Retransmission Timeout
	Xmit uint32 // transmitted count

	ResendTs uint32 // resend timestamp
	FastAck  uint32

	Data []byte
}

// Encode a segment into buffer.
func (seg *Segment) Encode(ptr []byte) []byte {
	ptr = ikcp_encode32u(ptr, seg.ConversationID)
	ptr = ikcp_encode8u(ptr, seg.Cmd)
	ptr = ikcp_encode8u(ptr, seg.Frg)
	ptr = ikcp_encode16u(ptr, seg.Wnd)
	ptr = ikcp_encode32u(ptr, seg.Ts)
	ptr = ikcp_encode32u(ptr, seg.Sn)
	ptr = ikcp_encode32u(ptr, seg.Una)
	ptr = ikcp_encode32u(ptr, uint32(len(seg.Data)))
	// XXX atomic.AddUint64(&DefaultSnmp.OutSegs, 1)
	return ptr
}
