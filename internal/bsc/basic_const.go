// Package bsc defines basic const values for KCP.
package bsc

// KCP BASIC
const (
	RTO_NDL     = 30  // no delay min rto
	RTO_MIN     = 100 // normal min rto
	RTO_DEF     = 200
	RTO_MAX     = 60000
	CMD_PUSH    = 81 // cmd: push data
	CMD_ACK     = 82 // cmd: ack
	CMD_WASK    = 83 // cmd: window probe (ask)
	CMD_WINS    = 84 // cmd: window size (tell)
	ASK_SEND    = 1  // need to send IKCP_CMD_WASK
	ASK_TELL    = 2  // need to send IKCP_CMD_WINS
	WND_SND     = 32
	WND_RCV     = 128 // must >= max fragment size
	MTU_DEF     = 1400
	ACK_FAST    = 3
	INTERVAL    = 100
	OVERHEAD    = 24 // KCP header size
	DEADLINK    = 20
	THRESH_INIT = 2
	THRESH_MIN  = 2
	PROBE_INIT  = 7000   // 7 secs to probe window size
	PROBE_LIMIT = 120000 // up to 120 secs to probe window
)
