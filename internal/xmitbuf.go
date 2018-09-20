package internal

import "sync"

const (
	// maximum packet size
	mtuLimit = 1500
)

var (
	// a system-wide packet buffer shared among sending, receiving and FEC
	// to mitigate high-frequency memory allocation for packets
	XmitBuf sync.Pool
)

func init() {
	XmitBuf.New = func() interface{} {
		return make([]byte, mtuLimit)
	}
}
