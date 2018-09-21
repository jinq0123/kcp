// Package kcp - A Fast and Reliable ARQ Protocol
package kcp

import (
	"io"
	"sync/atomic"
	"time"

	assert "github.com/arl/assertgo"
	"github.com/jinq0123/kcp/internal"
	"github.com/jinq0123/kcp/internal/bsc"
	"github.com/jinq0123/kcp/internal/endec"
	"github.com/jinq0123/kcp/internal/pktpl"
	"github.com/jinq0123/kcp/internal/seg"
	"github.com/jinq0123/kcp/internal/util"
)

// KCP defines a single KCP connection
// non-goroutine-safe
type KCP struct {
	conv, mtu, mss, state                  uint32
	snd_una, snd_nxt, rcv_nxt              uint32
	ssthresh                               uint32
	rx_rttvar, rx_srtt                     int32
	rx_rto, rx_minrto                      uint32
	snd_wnd, rcv_wnd, rmt_wnd, cwnd, probe uint32
	interval, ts_flush                     uint32
	nodelay, updated                       uint32
	ts_probe, probe_wait                   uint32
	dead_link, incr                        uint32

	fastresend     int32
	nocwnd, stream int32

	snd_queue []seg.Segment
	rcv_queue []seg.Segment
	snd_buf   []seg.Segment
	rcv_buf   []seg.Segment

	acklist []ackItem

	buffer []byte

	output io.Writer // KCP output, not nil
}

type ackItem struct {
	sn uint32
	ts uint32
}

// NewKCPWithOutput creates a new kcp control object and sets output writer.
// 'conversationID' must be equal in two endpoints from the same connection.
func NewKCPWithOutput(conversationID uint32, output io.Writer) *KCP {
	kcp := NewKCP(conversationID)
	kcp.SetOutput(output)
	return kcp
}

// NewKCP creates a new kcp control object.
// 'conversationID' must be equal in two endpoints from the same connection.
func NewKCP(conversationID uint32) *KCP {
	kcp := new(KCP)
	kcp.conv = conversationID
	kcp.snd_wnd = bsc.WND_SND
	kcp.rcv_wnd = bsc.WND_RCV
	kcp.rmt_wnd = bsc.WND_RCV
	kcp.mtu = bsc.MTU_DEF
	kcp.mss = kcp.mtu - bsc.OVERHEAD
	kcp.buffer = make([]byte, (kcp.mtu+bsc.OVERHEAD)*3)
	kcp.rx_rto = bsc.RTO_DEF
	kcp.rx_minrto = bsc.RTO_MIN
	kcp.interval = bsc.INTERVAL
	kcp.ts_flush = bsc.INTERVAL
	kcp.ssthresh = bsc.THRESH_INIT
	kcp.dead_link = bsc.DEADLINK
	kcp.output = internal.NewDummyWriter()
	return kcp
}

// newSegment creates a KCP segment
func (kcp *KCP) newSegment(size int) (seg seg.Segment) {
	seg.Data = pktpl.PacketPool.Get().([]byte)[:size]
	return
}

// delSegment recycles a KCP segment
func (kcp *KCP) delSegment(seg seg.Segment) {
	pktpl.PacketPool.Put(seg.Data)
}

// PeekSize checks the size of next message in the recv queue
func (kcp *KCP) PeekSize() (length int) {
	if len(kcp.rcv_queue) == 0 {
		return -1
	}

	seg := &kcp.rcv_queue[0]
	if seg.Frg == 0 {
		return len(seg.Data)
	}

	if len(kcp.rcv_queue) < int(seg.Frg+1) {
		return -1
	}

	for k := range kcp.rcv_queue {
		seg := &kcp.rcv_queue[k]
		length += len(seg.Data)
		if seg.Frg == 0 {
			break
		}
	}
	return
}

// Recv is user/upper level recv: returns size, returns below zero for EAGAIN
func (kcp *KCP) Recv(buffer []byte) (n int) {
	if len(kcp.rcv_queue) == 0 {
		return -1
	}

	peeksize := kcp.PeekSize()
	if peeksize < 0 {
		return -2
	}

	if peeksize > len(buffer) {
		return -3
	}

	var fast_recover bool
	if len(kcp.rcv_queue) >= int(kcp.rcv_wnd) {
		fast_recover = true
	}

	// merge fragment
	count := 0
	for k := range kcp.rcv_queue {
		seg := &kcp.rcv_queue[k]
		copy(buffer, seg.Data)
		buffer = buffer[len(seg.Data):]
		n += len(seg.Data)
		count++
		kcp.delSegment(*seg)
		if seg.Frg == 0 {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = kcp.remove_front(kcp.rcv_queue, count)
	}

	// move available data from rcv_buf -> rcv_queue
	count = 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.Sn == kcp.rcv_nxt && len(kcp.rcv_queue) < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}

	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}

	// fast recover
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) && fast_recover {
		// ready to send back bsc.CMD_WINS in ikcp_flush
		// tell remote my window size
		kcp.probe |= bsc.ASK_TELL
	}
	return
}

// Send is user/upper level send, returns below zero for error
func (kcp *KCP) Send(buffer []byte) int {
	var count int
	if len(buffer) == 0 {
		return -1
	}

	// append to previous segment in streaming mode (if possible)
	if kcp.stream != 0 {
		n := len(kcp.snd_queue)
		if n > 0 {
			seg := &kcp.snd_queue[n-1]
			if len(seg.Data) < int(kcp.mss) {
				capacity := int(kcp.mss) - len(seg.Data)
				extend := capacity
				if len(buffer) < capacity {
					extend = len(buffer)
				}

				// grow slice, the underlying cap is guaranteed to
				// be larger than kcp.mss
				oldlen := len(seg.Data)
				seg.Data = seg.Data[:oldlen+extend]
				copy(seg.Data[oldlen:], buffer)
				buffer = buffer[extend:]
			}
		}

		if len(buffer) == 0 {
			return 0
		}
	}

	if len(buffer) <= int(kcp.mss) {
		count = 1
	} else {
		count = (len(buffer) + int(kcp.mss) - 1) / int(kcp.mss)
	}

	if count > 255 {
		return -2
	}

	if count == 0 {
		count = 1
	}

	for i := 0; i < count; i++ {
		var size int
		if len(buffer) > int(kcp.mss) {
			size = int(kcp.mss)
		} else {
			size = len(buffer)
		}
		seg := kcp.newSegment(size)
		copy(seg.Data, buffer[:size])
		if kcp.stream == 0 { // message mode
			seg.Frg = uint8(count - i - 1)
		} else { // stream mode
			seg.Frg = 0
		}
		kcp.snd_queue = append(kcp.snd_queue, seg)
		buffer = buffer[size:]
	}
	return 0
}

func (kcp *KCP) update_ack(rtt int32) {
	// https://tools.ietf.org/html/rfc6298
	var rto uint32
	if kcp.rx_srtt == 0 {
		kcp.rx_srtt = rtt
		kcp.rx_rttvar = rtt >> 1
	} else {
		delta := rtt - kcp.rx_srtt
		kcp.rx_srtt += delta >> 3
		if delta < 0 {
			delta = -delta
		}
		if rtt < kcp.rx_srtt-kcp.rx_rttvar {
			// if the new RTT sample is below the bottom of the range of
			// what an RTT measurement is expected to be.
			// give an 8x reduced weight versus its normal weighting
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 5
		} else {
			kcp.rx_rttvar += (delta - kcp.rx_rttvar) >> 2
		}
	}
	rto = uint32(kcp.rx_srtt) + util.Max(kcp.interval, uint32(kcp.rx_rttvar)<<2)
	kcp.rx_rto = util.Bound(kcp.rx_minrto, rto, bsc.RTO_MAX)
}

func (kcp *KCP) shrink_buf() {
	if len(kcp.snd_buf) > 0 {
		seg := &kcp.snd_buf[0]
		kcp.snd_una = seg.Sn
	} else {
		kcp.snd_una = kcp.snd_nxt
	}
}

func (kcp *KCP) parse_ack(sn uint32) {
	if util.TimeDiff(sn, kcp.snd_una) < 0 || util.TimeDiff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		sg := &kcp.snd_buf[k] // segment
		if sn == sg.Sn {
			kcp.delSegment(*sg)
			copy(kcp.snd_buf[k:], kcp.snd_buf[k+1:])
			kcp.snd_buf[len(kcp.snd_buf)-1] = seg.Segment{}
			kcp.snd_buf = kcp.snd_buf[:len(kcp.snd_buf)-1]
			break
		}
		if util.TimeDiff(sn, sg.Sn) < 0 {
			break
		}
	}
}

func (kcp *KCP) parse_fastack(sn uint32) {
	if util.TimeDiff(sn, kcp.snd_una) < 0 || util.TimeDiff(sn, kcp.snd_nxt) >= 0 {
		return
	}

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if util.TimeDiff(sn, seg.Sn) < 0 {
			break
		} else if sn != seg.Sn {
			seg.FastAck++
		}
	}
}

func (kcp *KCP) parse_una(una uint32) {
	count := 0
	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		if util.TimeDiff(una, seg.Sn) > 0 {
			kcp.delSegment(*seg)
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.snd_buf = kcp.remove_front(kcp.snd_buf, count)
	}
}

// ack append
func (kcp *KCP) ack_push(sn, ts uint32) {
	kcp.acklist = append(kcp.acklist, ackItem{sn, ts})
}

func (kcp *KCP) parse_data(newseg seg.Segment) {
	sn := newseg.Sn
	if util.TimeDiff(sn, kcp.rcv_nxt+kcp.rcv_wnd) >= 0 ||
		util.TimeDiff(sn, kcp.rcv_nxt) < 0 {
		kcp.delSegment(newseg)
		return
	}

	n := len(kcp.rcv_buf) - 1
	insert_idx := 0
	repeat := false
	for i := n; i >= 0; i-- {
		seg := &kcp.rcv_buf[i]
		if seg.Sn == sn {
			repeat = true
			atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
			break
		}
		if util.TimeDiff(sn, seg.Sn) > 0 {
			insert_idx = i + 1
			break
		}
	}

	if !repeat {
		if insert_idx == n+1 {
			kcp.rcv_buf = append(kcp.rcv_buf, newseg)
		} else {
			kcp.rcv_buf = append(kcp.rcv_buf, seg.Segment{})
			copy(kcp.rcv_buf[insert_idx+1:], kcp.rcv_buf[insert_idx:])
			kcp.rcv_buf[insert_idx] = newseg
		}
	} else {
		kcp.delSegment(newseg)
	}

	// move available data from rcv_buf -> rcv_queue
	count := 0
	for k := range kcp.rcv_buf {
		seg := &kcp.rcv_buf[k]
		if seg.Sn == kcp.rcv_nxt && len(kcp.rcv_queue) < int(kcp.rcv_wnd) {
			kcp.rcv_nxt++
			count++
		} else {
			break
		}
	}
	if count > 0 {
		kcp.rcv_queue = append(kcp.rcv_queue, kcp.rcv_buf[:count]...)
		kcp.rcv_buf = kcp.remove_front(kcp.rcv_buf, count)
	}
}

// Input when you received a low level packet (eg. UDP packet), call it
// regular indicates a regular packet has received(not from FEC)
func (kcp *KCP) Input(data []byte, regular, ackNoDelay bool) int {
	snd_una := kcp.snd_una
	if len(data) < bsc.OVERHEAD {
		return -1
	}

	var maxack uint32
	var lastackts uint32
	var flag int
	var inSegs uint64

	for {
		var ts, sn, length, una, conv uint32
		var wnd uint16
		var cmd, frg uint8

		if len(data) < int(bsc.OVERHEAD) {
			break
		}

		data = endec.Decode32u(data, &conv)
		if conv != kcp.conv {
			return -1
		}

		data = endec.Decode8u(data, &cmd)
		data = endec.Decode8u(data, &frg)
		data = endec.Decode16u(data, &wnd)
		data = endec.Decode32u(data, &ts)
		data = endec.Decode32u(data, &sn)
		data = endec.Decode32u(data, &una)
		data = endec.Decode32u(data, &length)
		if len(data) < int(length) {
			return -2
		}

		if cmd != bsc.CMD_PUSH && cmd != bsc.CMD_ACK &&
			cmd != bsc.CMD_WASK && cmd != bsc.CMD_WINS {
			return -3
		}

		// only trust window updates from regular packets. i.e: latest update
		if regular {
			kcp.rmt_wnd = uint32(wnd)
		}
		kcp.parse_una(una)
		kcp.shrink_buf()

		if cmd == bsc.CMD_ACK {
			kcp.parse_ack(sn)
			kcp.shrink_buf()
			if flag == 0 {
				flag = 1
				maxack = sn
				lastackts = ts
			} else if util.TimeDiff(sn, maxack) > 0 {
				maxack = sn
				lastackts = ts
			}
		} else if cmd == bsc.CMD_PUSH {
			if util.TimeDiff(sn, kcp.rcv_nxt+kcp.rcv_wnd) < 0 {
				kcp.ack_push(sn, ts)
				if util.TimeDiff(sn, kcp.rcv_nxt) >= 0 {
					seg := kcp.newSegment(int(length))
					seg.ConversationID = conv
					seg.Cmd = cmd
					seg.Frg = frg
					seg.Wnd = wnd
					seg.Ts = ts
					seg.Sn = sn
					seg.Una = una
					copy(seg.Data, data[:length])
					kcp.parse_data(seg)
				} else {
					atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
				}
			} else {
				atomic.AddUint64(&DefaultSnmp.RepeatSegs, 1)
			}
		} else if cmd == bsc.CMD_WASK {
			// ready to send back bsc.CMD_WINS in Ikcp_flush
			// tell remote my window size
			kcp.probe |= bsc.ASK_TELL
		} else if cmd == bsc.CMD_WINS {
			// do nothing
		} else {
			return -3
		}

		inSegs++
		data = data[length:]
	}
	atomic.AddUint64(&DefaultSnmp.InSegs, inSegs)

	if flag != 0 && regular {
		kcp.parse_fastack(maxack)
		current := currentMs()
		if util.TimeDiff(current, lastackts) >= 0 {
			kcp.update_ack(util.TimeDiff(current, lastackts))
		}
	}

	if util.TimeDiff(kcp.snd_una, snd_una) > 0 {
		if kcp.cwnd < kcp.rmt_wnd {
			mss := kcp.mss
			if kcp.cwnd < kcp.ssthresh {
				kcp.cwnd++
				kcp.incr += mss
			} else {
				if kcp.incr < mss {
					kcp.incr = mss
				}
				kcp.incr += (mss*mss)/kcp.incr + (mss / 16)
				if (kcp.cwnd+1)*mss <= kcp.incr {
					kcp.cwnd++
				}
			}
			if kcp.cwnd > kcp.rmt_wnd {
				kcp.cwnd = kcp.rmt_wnd
				kcp.incr = kcp.rmt_wnd * mss
			}
		}
	}

	if ackNoDelay && len(kcp.acklist) > 0 { // ack immediately
		kcp.flush(true)
	}
	return 0
}

func (kcp *KCP) wnd_unused() uint16 {
	if len(kcp.rcv_queue) < int(kcp.rcv_wnd) {
		return uint16(int(kcp.rcv_wnd) - len(kcp.rcv_queue))
	}
	return 0
}

// flush pending data
func (kcp *KCP) flush(ackOnly bool) {
	var seg seg.Segment
	seg.ConversationID = kcp.conv
	seg.Cmd = bsc.CMD_ACK
	seg.Wnd = kcp.wnd_unused()
	seg.Una = kcp.rcv_nxt

	buffer := kcp.buffer
	// flush acknowledges
	ptr := buffer
	for i, ack := range kcp.acklist {
		size := len(buffer) - len(ptr)
		if size+bsc.OVERHEAD > int(kcp.mtu) {
			/*n, err := */ kcp.output.Write(buffer[:size]) // XXX check err
			ptr = buffer
		}
		// filter jitters caused by bufferbloat
		if ack.sn >= kcp.rcv_nxt || len(kcp.acklist)-1 == i {
			seg.Sn, seg.Ts = ack.sn, ack.ts
			ptr = seg.Encode(ptr)
		}
	}
	kcp.acklist = kcp.acklist[0:0]

	if ackOnly { // flash remain ack segments
		size := len(buffer) - len(ptr)
		if size > 0 {
			/* n, err := */ kcp.output.Write(buffer[:size]) // XXX check err
		}
		return
	}

	// probe window size (if remote window size equals zero)
	if kcp.rmt_wnd == 0 {
		current := currentMs()
		if kcp.probe_wait == 0 {
			kcp.probe_wait = bsc.PROBE_INIT
			kcp.ts_probe = current + kcp.probe_wait
		} else {
			if util.TimeDiff(current, kcp.ts_probe) >= 0 {
				if kcp.probe_wait < bsc.PROBE_INIT {
					kcp.probe_wait = bsc.PROBE_INIT
				}
				kcp.probe_wait += kcp.probe_wait / 2
				if kcp.probe_wait > bsc.PROBE_LIMIT {
					kcp.probe_wait = bsc.PROBE_LIMIT
				}
				kcp.ts_probe = current + kcp.probe_wait
				kcp.probe |= bsc.ASK_SEND
			}
		}
	} else {
		kcp.ts_probe = 0
		kcp.probe_wait = 0
	}

	// flush window probing commands
	if (kcp.probe & bsc.ASK_SEND) != 0 {
		seg.Cmd = bsc.CMD_WASK
		size := len(buffer) - len(ptr)
		if size+bsc.OVERHEAD > int(kcp.mtu) {
			/* n, err := */ kcp.output.Write(buffer[:size]) // XXX check err
			ptr = buffer
		}
		ptr = seg.Encode(ptr)
	}

	// flush window probing commands
	if (kcp.probe & bsc.ASK_TELL) != 0 {
		seg.Cmd = bsc.CMD_WINS
		size := len(buffer) - len(ptr)
		if size+bsc.OVERHEAD > int(kcp.mtu) {
			/* n, err := */ kcp.output.Write(buffer[:size]) // XXX check err
			ptr = buffer
		}
		ptr = seg.Encode(ptr)
	}

	kcp.probe = 0

	// calculate window size
	cwnd := util.Min(kcp.snd_wnd, kcp.rmt_wnd)
	if kcp.nocwnd == 0 {
		cwnd = util.Min(kcp.cwnd, cwnd)
	}

	// sliding window, controlled by snd_nxt && sna_una+cwnd
	newSegsCount := 0
	for k := range kcp.snd_queue {
		if util.TimeDiff(kcp.snd_nxt, kcp.snd_una+cwnd) >= 0 {
			break
		}
		newseg := kcp.snd_queue[k]
		newseg.ConversationID = kcp.conv
		newseg.Cmd = bsc.CMD_PUSH
		newseg.Sn = kcp.snd_nxt
		kcp.snd_buf = append(kcp.snd_buf, newseg)
		kcp.snd_nxt++
		newSegsCount++
		kcp.snd_queue[k].Data = nil
	}
	if newSegsCount > 0 {
		kcp.snd_queue = kcp.remove_front(kcp.snd_queue, newSegsCount)
	}

	// calculate resent
	resent := uint32(kcp.fastresend)
	if kcp.fastresend <= 0 {
		resent = 0xffffffff
	}

	// check for retransmissions
	current := currentMs()
	var change, lost, lostSegs, fastRetransSegs, earlyRetransSegs uint64
	for k := range kcp.snd_buf {
		segment := &kcp.snd_buf[k]
		needsend := false
		if segment.Xmit == 0 { // initial transmit
			needsend = true
			segment.RTO = kcp.rx_rto
			segment.ResendTs = current + segment.RTO
		} else if util.TimeDiff(current, segment.ResendTs) >= 0 { // RTO
			needsend = true
			if kcp.nodelay == 0 {
				segment.RTO += kcp.rx_rto
			} else {
				segment.RTO += kcp.rx_rto / 2
			}
			segment.ResendTs = current + segment.RTO
			lost++
			lostSegs++
		} else if segment.FastAck >= resent { // fast retransmit
			needsend = true
			segment.FastAck = 0
			segment.RTO = kcp.rx_rto
			segment.ResendTs = current + segment.RTO
			change++
			fastRetransSegs++
		} else if segment.FastAck > 0 && newSegsCount == 0 { // early retransmit
			needsend = true
			segment.FastAck = 0
			segment.RTO = kcp.rx_rto
			segment.ResendTs = current + segment.RTO
			change++
			earlyRetransSegs++
		}

		if needsend {
			segment.Xmit++
			segment.Ts = current
			segment.Wnd = seg.Wnd
			segment.Una = seg.Una

			size := len(buffer) - len(ptr)
			need := bsc.OVERHEAD + len(segment.Data)

			if size+need > int(kcp.mtu) {
				/* n, err := */ kcp.output.Write(buffer[:size]) // XXX check err
				current = currentMs()                           // time update for a blocking call
				ptr = buffer
			}

			ptr = segment.Encode(ptr)
			copy(ptr, segment.Data)
			ptr = ptr[len(segment.Data):]

			if segment.Xmit >= kcp.dead_link {
				kcp.state = 0xFFFFFFFF
			}
		}
	}

	// flash remain segments
	size := len(buffer) - len(ptr)
	if size > 0 {
		/* n, err := */ kcp.output.Write(buffer[:size]) // XXX check err
	}

	// counter updates
	sum := lostSegs
	if lostSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.LostSegs, lostSegs)
	}
	if fastRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.FastRetransSegs, fastRetransSegs)
		sum += fastRetransSegs
	}
	if earlyRetransSegs > 0 {
		atomic.AddUint64(&DefaultSnmp.EarlyRetransSegs, earlyRetransSegs)
		sum += earlyRetransSegs
	}
	if sum > 0 {
		atomic.AddUint64(&DefaultSnmp.RetransSegs, sum)
	}

	// update ssthresh
	// rate halving, https://tools.ietf.org/html/rfc6937
	if change > 0 {
		inflight := kcp.snd_nxt - kcp.snd_una
		kcp.ssthresh = inflight / 2
		if kcp.ssthresh < bsc.THRESH_MIN {
			kcp.ssthresh = bsc.THRESH_MIN
		}
		kcp.cwnd = kcp.ssthresh + resent
		kcp.incr = kcp.cwnd * kcp.mss
	}

	// congestion control, https://tools.ietf.org/html/rfc5681
	if lost > 0 {
		kcp.ssthresh = cwnd / 2
		if kcp.ssthresh < bsc.THRESH_MIN {
			kcp.ssthresh = bsc.THRESH_MIN
		}
		kcp.cwnd = 1
		kcp.incr = kcp.mss
	}

	if kcp.cwnd < 1 {
		kcp.cwnd = 1
		kcp.incr = kcp.mss
	}
}

// Update updates state (call it repeatedly, every 10ms-100ms), or you can ask
// ikcp_check when to call it again (without ikcp_input/_send calling).
// 'current' - current timestamp in millisec.
func (kcp *KCP) Update() {
	kcp.UpdateNow(time.Now())
}

// UpdateNow updates state (call it repeatedly, every 10ms-100ms), or you can ask
// ikcp_check when to call it again (without ikcp_input/_send calling).
// 'now' - time.Now().
// Same as Update() but use a external now time.
func (kcp *KCP) UpdateNow(now time.Time) {
	if kcp.updated == 0 {
		kcp.updated = 1
		kcp.ts_flush = current
	}

	slap := util.TimeDiff(current, kcp.ts_flush)
	if slap >= 10000 || slap < -10000 {
		kcp.ts_flush = current
		slap = 0
	}

	if slap >= 0 {
		kcp.ts_flush += kcp.interval
		if util.TimeDiff(current, kcp.ts_flush) >= 0 {
			kcp.ts_flush = current + kcp.interval
		}
		kcp.flush(false)
	}
}

// Check determines when should you invoke ikcp_update:
// returns when you should invoke ikcp_update in millisec, if there
// is no ikcp_input/_send calling. you can call ikcp_update in that
// time, instead of call update repeatly.
// Important to reduce unnacessary ikcp_update invoking. use it to
// schedule ikcp_update (eg. implementing an epoll-like mechanism,
// or optimize ikcp_update when handling massive kcp connections)
func (kcp *KCP) Check() uint32 {
	current := currentMs()
	ts_flush := kcp.ts_flush
	tm_flush := int32(0x7fffffff)
	tm_packet := int32(0x7fffffff)
	minimal := uint32(0)
	if kcp.updated == 0 {
		return current
	}

	if util.TimeDiff(current, ts_flush) >= 10000 ||
		util.TimeDiff(current, ts_flush) < -10000 {
		ts_flush = current
	}

	if util.TimeDiff(current, ts_flush) >= 0 {
		return current
	}

	tm_flush = util.TimeDiff(ts_flush, current)

	for k := range kcp.snd_buf {
		seg := &kcp.snd_buf[k]
		diff := util.TimeDiff(seg.ResendTs, current)
		if diff <= 0 {
			return current
		}
		if diff < tm_packet {
			tm_packet = diff
		}
	}

	minimal = uint32(tm_packet)
	if tm_packet >= tm_flush {
		minimal = uint32(tm_flush)
	}
	if minimal >= kcp.interval {
		minimal = kcp.interval
	}

	return current + minimal
}

// SetMtu changes MTU size, default is 1400
func (kcp *KCP) SetMTU(mtu int) {
	const minMTU = 50
	assert.True(bsc.OVERHEAD < minMTU)
	assert.True(pktpl.MaxPacketSize > minMTU)

	if mtu < minMTU {
		mtu = minMTU
	} else if mtu > pktpl.MaxPacketSize {
		mtu = pktpl.MaxPacketSize
	}

	kcp.buffer = make([]byte, (mtu+bsc.OVERHEAD)*3)
	kcp.mtu = uint32(mtu)
	kcp.mss = kcp.mtu - bsc.OVERHEAD
}

// NoDelay options
// fastest: ikcp_nodelay(kcp, 1, 20, 2, 1)
// nodelay: 0:disable(default), 1:enable
// interval: internal update timer interval in millisec, default is 100ms
// resend: 0:disable fast resend(default), 1:enable fast resend
// nc: 0:normal congestion control(default), 1:disable congestion control
func (kcp *KCP) NoDelay(nodelay, interval, resend, nc int) int {
	if nodelay >= 0 {
		kcp.nodelay = uint32(nodelay)
		if nodelay != 0 {
			kcp.rx_minrto = bsc.RTO_NDL
		} else {
			kcp.rx_minrto = bsc.RTO_MIN
		}
	}
	if interval >= 0 {
		if interval > 5000 {
			interval = 5000
		} else if interval < 10 {
			interval = 10
		}
		kcp.interval = uint32(interval)
	}
	if resend >= 0 {
		kcp.fastresend = int32(resend)
	}
	if nc >= 0 {
		kcp.nocwnd = int32(nc)
	}
	return 0
}

// WndSize sets maximum window size: sndwnd=32, rcvwnd=32 by default
func (kcp *KCP) WndSize(sndwnd, rcvwnd int) int {
	if sndwnd > 0 {
		kcp.snd_wnd = uint32(sndwnd)
	}
	if rcvwnd > 0 {
		kcp.rcv_wnd = uint32(rcvwnd)
	}
	return 0
}

// WaitSnd gets how many packet is waiting to be sent
func (kcp *KCP) WaitSnd() int {
	return len(kcp.snd_buf) + len(kcp.snd_queue)
}

// remove front n elements from queue
func (kcp *KCP) remove_front(q []seg.Segment, n int) []seg.Segment {
	newn := copy(q, q[n:])
	for i := newn; i < len(q); i++ {
		q[i] = seg.Segment{} // manual set nil for GC
	}
	return q[:newn]
}

// SetOutput sets output writer.
func (kcp *KCP) SetOutput(output io.Writer) {
	if output == nil {
		output = internal.NewDummyWriter()
	}
	kcp.output = output
}
