package kcp

import "time"

// returns current time in milliseconds
func currentMs() uint32 {
	return uint32(time.Now().UnixNano() / int64(time.Millisecond))
}
