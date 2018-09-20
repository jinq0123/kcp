// Package pktpl has a packet pool.
package pktpl

import "sync"

const (
	// maximum packet size must not less than MTU
	MaxPacketSize = 1500
)

var (
	// a system-wide packet buffer shared among sending, receiving and FEC
	// to mitigate high-frequency memory allocation for packets
	PacketPool sync.Pool
)

func init() {
	PacketPool.New = func() interface{} {
		return make([]byte, MaxPacketSize)
	}
}
