// Package seg defines Segment class.
package seg

import (
	"github.com/jinq0123/kcp/internal/endec"
)

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
	ptr = endec.Encode32u(ptr, seg.ConversationID)
	ptr = endec.Encode8u(ptr, seg.Cmd)
	ptr = endec.Encode8u(ptr, seg.Frg)
	ptr = endec.Encode16u(ptr, seg.Wnd)
	ptr = endec.Encode32u(ptr, seg.Ts)
	ptr = endec.Encode32u(ptr, seg.Sn)
	ptr = endec.Encode32u(ptr, seg.Una)
	ptr = endec.Encode32u(ptr, uint32(len(seg.Data)))
	// XXX atomic.AddUint64(&DefaultSnmp.OutSegs, 1)
	return ptr
}
