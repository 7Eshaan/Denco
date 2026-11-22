package utp

import (
	"io"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"
)

// LEDBAT and timer constants (BEP 29 where it gives them).
const (
	target          = 100 * time.Millisecond // queuing delay LEDBAT aims for
	maxCwndIncrease = 3000                   // bytes per RTT at most, when far below target
	minCwnd         = maxPayload             // never below one packet
	maxCwnd         = 4 << 20
	initialCwnd     = 4 * maxPayload
	minRTO          = 500 * time.Millisecond
	maxRTO          = 30 * time.Second
	initialRTO      = time.Second
	deadAfter       = 30 * time.Second // nothing heard while data is unacked → ErrTimeout
	keepAlive       = 29 * time.Second // NAT mappings expire after ~30 s of silence
	linger          = 10 * time.Second // after Close, how long to wait for our FIN's ack
	maxOOO          = 1024             // packets ahead of the ack we keep
)

type connState int

const (
	stateSynSent connState = iota
	stateConnected
	stateClosed
)

// Conn is one uTP connection, a net.Conn. One goroutine may read while
// another writes; the Socket's goroutines deliver packets and run timers.
type Conn struct {
	s              *Socket
	remote         netip.AddrPort
	recvID, sendID uint16
	connected      chan struct{} // Dial: closed when connected or failed
	connOnce       sync.Once

	mu      sync.Mutex
	changed chan struct{} // closed and replaced on every change: wakes Read/Write
	state   connState
	err     error // why it ended (reads return it after the buffered data)
	closing bool  // Close was called

	// Sending.
	seq           uint16 // next sequence number
	sendBuf       []byte // written, not yet in a packet (from soff)
	soff          int
	inflight      []*outPacket // sent, unacked, in sequence order
	inflightBytes int
	lostBytes     int // of which marked for resending (after a timeout)
	finSent       bool
	peerWnd       int
	lastAck       uint16
	dupAcks       int
	lastSend      time.Time
	synSentAt     time.Time
	synTries      int
	closeBy       time.Time

	// Congestion control (LEDBAT).
	cwnd, ssthresh float64
	slowStart      bool
	cwndLimited    bool // the window, not the application, limited the last flush
	rtt, rttVar    time.Duration
	rto            time.Duration
	lastCut        time.Time
	base           [2]uint32 // lowest delay sample this minute and the last
	baseSet        [2]bool
	baseMinute     int64
	recent         [3]uint32 // the last delay samples (their minimum is used)
	nRecent        int

	// Receiving.
	ack        uint16 // last sequence number received in order
	gotData    bool
	rbuf       []byte // received in order, not yet read (from roff)
	roff       int
	ooo        map[uint16][]byte // received out of order (nil payload: a FIN)
	oooBytes   int
	finRecv    bool
	finSeq     uint16
	eof        bool
	ackPending int  // in-order packets not yet acked
	ackNow     bool // send an ack at the end of this packet's handling
	advertised int  // the receive window we last told the peer
	lastTsDiff uint32
	lastRecv   time.Time

	readDeadline, writeDeadline time.Time

	// Counters, for tests and diagnosis.
	nTimeouts, nFastResends, nPackets, nProbes int
	ackRTT                                     time.Duration // processAck's RTT sample, -1 = none
}

type outPacket struct {
	typ        byte
	seq        uint16
	payload    []byte
	sentAt     time.Time
	sends      int
	acked      bool
	lost       bool // to be resent when the window allows (after a timeout)
	fastResent bool
	probed     bool // the tail-loss probe resent it
}

func newConn(s *Socket, remote netip.AddrPort, recvID, sendID uint16, outgoing bool) *Conn {
	now := time.Now()
	c := &Conn{
		s: s, remote: remote, recvID: recvID, sendID: sendID,
		connected: make(chan struct{}), changed: make(chan struct{}),
		cwnd: initialCwnd, ssthresh: maxCwnd, slowStart: true, rto: initialRTO,
		peerWnd: maxPayload * 4, ooo: map[uint16][]byte{}, lastRecv: now, lastSend: now,
	}
	if outgoing {
		c.state = stateSynSent
		c.seq = 2 // the SYN is 1
	} else {
		c.state = stateConnected
		c.seq = randomID()
		c.connOnce.Do(func() { close(c.connected) })
	}
	return c
}

// --- net.Conn ---------------------------------------------------------------

func (c *Conn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if c.closing {
			return 0, net.ErrClosed
		}
		if n := len(c.rbuf) - c.roff; n > 0 {
			n = copy(p, c.rbuf[c.roff:])
			c.roff += n
			if c.roff == len(c.rbuf) {
				c.rbuf, c.roff = c.rbuf[:0], 0
			}
			c.windowUpdate()
			return n, nil
		}
		if c.eof {
			return 0, io.EOF
		}
		if c.err != nil {
			return 0, c.err
		}
		if err := c.wait(c.readDeadline); err != nil {
			return 0, err
		}
	}
}

func (c *Conn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	total := 0
	for len(p) > 0 {
		if c.closing {
			return total, net.ErrClosed
		}
		if c.err != nil {
			return total, c.err
		}
		if room := c.s.cfg.SendBuffer - c.unsent() - c.inflightBytes; room > 0 {
			n := min(room, len(p))
			if c.soff > 0 && c.soff >= len(c.sendBuf)/2 {
				c.sendBuf = append(c.sendBuf[:0], c.sendBuf[c.soff:]...)
				c.soff = 0
			}
			c.sendBuf = append(c.sendBuf, p[:n]...)
			p, total = p[n:], total+n
			c.flush(time.Now())
			continue
		}
		if err := c.wait(c.writeDeadline); err != nil {
			return total, err
		}
	}
	return total, nil
}

// Close sends what is buffered, then a FIN; it doesn't wait. Reads and
// writes fail with net.ErrClosed from now on.
func (c *Conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return nil
	}
	c.closing = true
	switch c.state {
	case stateSynSent:
		c.die(net.ErrClosed)
	case stateConnected:
		c.closeBy = time.Now().Add(linger)
		c.flush(time.Now())
	}
	c.broadcast()
	return nil
}

func (c *Conn) LocalAddr() net.Addr  { return c.s.pc.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return net.UDPAddrFromAddrPort(c.remote) }

func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline, c.writeDeadline = t, t
	c.broadcast()
	c.mu.Unlock()
	return nil
}

func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadline = t
	c.broadcast()
	c.mu.Unlock()
	return nil
}

func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadline = t
	c.broadcast()
	c.mu.Unlock()
	return nil
}

// broadcast wakes every waiter (mu held).
func (c *Conn) broadcast() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// wait sleeps until the next change or the deadline (mu held; released
// while waiting).
func (c *Conn) wait(deadline time.Time) error {
	if !deadline.IsZero() && !time.Now().Before(deadline) {
		return os.ErrDeadlineExceeded
	}
	ch := c.changed
	c.mu.Unlock()
	defer c.mu.Lock()
	if deadline.IsZero() {
		<-ch
		return nil
	}
	t := time.NewTimer(time.Until(deadline))
	defer t.Stop()
	select {
	case <-ch:
		return nil
	case <-t.C:
		return os.ErrDeadlineExceeded
	}
}

// die ends the connection with err (mu held).
func (c *Conn) die(err error) {
	if c.state == stateClosed {
		return
	}
	c.state = stateClosed
	if c.err == nil {
		c.err = err
	}
	c.connOnce.Do(func() { close(c.connected) })
	c.broadcast()
	c.s.remove(c)
}

// abort ends the connection, telling the peer with a RESET if asked.
func (c *Conn) abort(err error, reset bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if reset && c.state == stateConnected {
		h := header{typ: stReset, connID: c.sendID, ts: c.s.now(), seq: c.seq, ack: c.ack}
		c.s.send(h.marshal(nil, nil), c.remote)
	}
	c.die(err)
}

// --- sending ----------------------------------------------------------------

func (c *Conn) unsent() int { return len(c.sendBuf) - c.soff }

func (c *Conn) window() int { return min(int(c.cwnd), c.peerWnd) }

// flush sends what the window allows: packets lost to a timeout first,
// then new data, then the FIN once everything is out (mu held).
func (c *Conn) flush(now time.Time) {
	if c.state != stateConnected {
		return
	}
	for _, p := range c.inflight {
		if !p.lost {
			continue
		}
		if c.inflightBytes-c.lostBytes+len(p.payload) > c.window() && c.inflightBytes > c.lostBytes {
			c.cwndLimited = true
			return
		}
		c.lostBytes -= len(p.payload)
		p.lost = false
		c.transmit(p, now)
	}
	for c.unsent() > 0 {
		n := min(c.unsent(), maxPayload)
		// One packet may always be in flight, so a tiny window (or a zero
		// window, which this probes) can't stall the connection.
		if len(c.inflight) > 0 && c.inflightBytes+n > c.window() {
			c.cwndLimited = true
			return
		}
		p := &outPacket{typ: stData, seq: c.seq, payload: append([]byte(nil), c.sendBuf[c.soff:c.soff+n]...)}
		c.soff += n
		c.seq++
		c.inflight = append(c.inflight, p)
		c.inflightBytes += n
		c.transmit(p, now)
	}
	c.cwndLimited = false
	if c.soff == len(c.sendBuf) {
		c.sendBuf, c.soff = c.sendBuf[:0], 0
	}
	if c.closing && !c.finSent {
		p := &outPacket{typ: stFin, seq: c.seq}
		c.seq++
		c.finSent = true
		c.inflight = append(c.inflight, p)
		c.transmit(p, now)
	}
}

// transmit sends (or resends) p with fresh header fields.
func (c *Conn) transmit(p *outPacket, now time.Time) {
	p.sentAt = now
	p.sends++
	c.nPackets++
	c.sendPacket(p.typ, p.seq, p.payload, now)
}

func (c *Conn) sendPacket(typ byte, seq uint16, payload []byte, now time.Time) {
	room := c.recvRoom()
	h := header{typ: typ, connID: c.sendID, ts: c.s.now(), tsDiff: c.lastTsDiff, wnd: uint32(room),
		seq: seq, ack: c.ack, sack: c.sackBits()}
	c.s.send(h.marshal(make([]byte, 0, maxDatagram), payload), c.remote)
	c.ackPending, c.ackNow, c.advertised, c.lastSend = 0, false, room, now
}

// sendState sends a pure ack. Its sequence number is the next one we'll
// use; it doesn't consume it.
func (c *Conn) sendState() { c.sendPacket(stState, c.seq, nil, time.Now()) }

// sendSyn sends (or repeats) the SYN. Its connection ID is our recvID —
// the one the peer will send to — and its sequence number 1.
func (c *Conn) sendSyn() {
	now := time.Now()
	h := header{typ: stSyn, connID: c.recvID, ts: c.s.now(), wnd: uint32(c.recvRoom()), seq: c.seq - 1}
	c.s.send(h.marshal(nil, nil), c.remote)
	c.synSentAt, c.lastSend = now, now
	c.synTries++
}

// windowUpdate tells the peer when reading freed a good part of the
// receive buffer, so a sender that filled it doesn't wait for a timeout.
func (c *Conn) windowUpdate() {
	if c.state == stateConnected && c.recvRoom()-c.advertised >= c.s.cfg.RecvBuffer/4 {
		c.sendState()
	}
}

// --- receiving --------------------------------------------------------------

func (c *Conn) recvRoom() int {
	return max(0, c.s.cfg.RecvBuffer-(len(c.rbuf)-c.roff)-c.oooBytes)
}

// sackBits describes the packets held out of order past ack+1.
func (c *Conn) sackBits() []byte {
	if len(c.ooo) == 0 {
		return nil
	}
	var bits [maxSACK]byte
	hi := -1
	for seq := range c.ooo {
		i := int(seq - c.ack - 2)
		if i >= 0 && i < maxSACK*8 {
			bits[i/8] |= 1 << (i % 8)
			hi = max(hi, i)
		}
	}
	if hi < 0 {
		return nil
	}
	return bits[:(hi/32+1)*4]
}

// handle processes one packet for this connection (Socket reader).
func (c *Conn) handle(h header, payload []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state == stateClosed {
		return
	}
	now := time.Now()
	c.lastRecv = now
	c.lastTsDiff = c.s.now() - h.ts
	if h.typ == stReset {
		c.die(ErrReset)
		return
	}
	if c.state == stateSynSent {
		if h.typ != stState {
			return
		}
		// The STATE's sequence number is the acceptor's next one; nothing
		// before it exists.
		c.ack = h.seq - 1
		c.lastAck = h.ack
		c.state = stateConnected
		c.peerWnd = int(h.wnd)
		if c.synTries == 1 {
			c.rttSample(now.Sub(c.synSentAt))
		}
		c.connOnce.Do(func() { close(c.connected) })
		c.broadcast()
		return
	}
	oldWnd := c.peerWnd
	c.peerWnd = int(h.wnd)
	c.processAck(h, now)
	if oldWnd < maxPayload && c.peerWnd >= maxPayload && len(c.inflight) > 0 {
		c.transmit(c.inflight[0], now) // the window reopened: resend the probe now
	}
	if h.typ == stData || h.typ == stFin {
		c.receive(h, payload)
	}
	c.flush(now)
	if c.ackNow || c.ackPending >= 2 {
		c.sendState()
	}
	c.finishIfDone()
	c.broadcast()
}

// receive files a data or FIN packet (mu held).
func (c *Conn) receive(h header, payload []byte) {
	c.gotData = true
	if c.eof || (c.finRecv && seqLess(c.finSeq, h.seq)) {
		c.ackNow = true
		return // nothing comes after the FIN
	}
	if h.typ == stFin && !c.finRecv {
		c.finRecv, c.finSeq = true, h.seq
	}
	switch {
	case h.seq == c.ack+1:
		if !c.take(payload) {
			c.ackNow = true // no room: say so (the window) without acking it
			return
		}
		c.ack++
		for {
			p, ok := c.ooo[c.ack+1]
			if !ok {
				break
			}
			delete(c.ooo, c.ack+1)
			c.oooBytes -= len(p)
			c.take(p) // fits: its bytes just left oooBytes
			c.ack++
		}
		if c.finRecv && c.ack == c.finSeq {
			c.eof = true
		}
		c.ackPending++
		if len(c.ooo) > 0 || c.eof {
			c.ackNow = true
		}
	case !seqLess(c.ack, h.seq): // at or before ack: a duplicate
		c.ackNow = true
	default:
		if uint16(h.seq-c.ack) <= maxOOO {
			if _, dup := c.ooo[h.seq]; !dup && len(payload) <= c.recvRoom() {
				c.ooo[h.seq] = append([]byte(nil), payload...)
				c.oooBytes += len(payload)
			}
		}
		c.ackNow = true
	}
}

// take appends in-order data to the read buffer if it fits.
func (c *Conn) take(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	if len(b) > c.recvRoom() {
		return false
	}
	if c.roff > 0 && c.roff >= len(c.rbuf)/2 {
		c.rbuf = append(c.rbuf[:0], c.rbuf[c.roff:]...)
		c.roff = 0
	}
	c.rbuf = append(c.rbuf, b...)
	return true
}

// processAck handles the ack number and selective acks (mu held).
func (c *Conn) processAck(h header, now time.Time) {
	acked := 0
	c.ackRTT = -1
	for len(c.inflight) > 0 && !seqLess(h.ack, c.inflight[0].seq) {
		acked += c.acknowledge(c.inflight[0], now)
		c.inflight = c.inflight[1:]
	}
	sacked := 0
	if h.sack != nil {
		keep := c.inflight[:0]
		for _, p := range c.inflight {
			if i := int(p.seq - h.ack - 2); i >= 0 && i < len(h.sack)*8 && h.sack[i/8]>>(i%8)&1 == 1 {
				acked += c.acknowledge(p, now)
				sacked++
				continue
			}
			keep = append(keep, p)
		}
		clear(c.inflight[len(keep):])
		c.inflight = keep
	}
	// Fast retransmit: the packet after the ack is missing while three
	// later ones arrived (selective acks), or three duplicate acks came.
	// (onLoss halves the window at most once per RTT.)
	if h.ack == c.lastAck && acked == 0 && h.typ == stState && len(c.inflight) > 0 {
		c.dupAcks++
	} else if h.ack != c.lastAck {
		c.dupAcks = 0
	}
	c.lastAck = h.ack
	if len(c.inflight) > 0 {
		first := c.inflight[0]
		// Resent already and still missing two RTTs later while acks keep
		// coming: the resend was lost too; don't wait for the timeout.
		due := !first.fastResent || now.Sub(first.sentAt) > 2*c.rtt+4*tick
		// Early retransmit (RFC 5827): with few packets in flight, fewer
		// later ones can arrive to signal the hole.
		need := min(3, max(1, len(c.inflight)-1))
		if first.seq == h.ack+1 && due && (sacked >= need || c.dupAcks >= need || countBits(h.sack) >= need) {
			first.fastResent = true
			c.nFastResends++
			if first.lost {
				first.lost = false
				c.lostBytes -= len(first.payload)
			}
			c.transmit(first, now)
			c.onLoss(now)
		}
	}
	if c.ackRTT >= 0 {
		c.rttSample(c.ackRTT)
	}
	if acked > 0 {
		c.ledbat(h.tsDiff, acked, now)
	}
}

// acknowledge removes p's bytes from flight. Of the packets one ack
// covers, the most recently sent (sent once: Karn's rule) gives the RTT
// sample: the others may have waited behind a hole.
func (c *Conn) acknowledge(p *outPacket, now time.Time) int {
	if p.acked {
		return 0
	}
	p.acked = true
	c.inflightBytes -= len(p.payload)
	if p.lost {
		c.lostBytes -= len(p.payload)
	}
	if d := now.Sub(p.sentAt); p.sends == 1 && (c.ackRTT < 0 || d < c.ackRTT) {
		c.ackRTT = d
	}
	return max(len(p.payload), 1)
}

func countBits(b []byte) int {
	n := 0
	for _, x := range b {
		for ; x != 0; x &= x - 1 {
			n++
		}
	}
	return n
}

func (c *Conn) rttSample(d time.Duration) {
	if c.rtt == 0 {
		c.rtt, c.rttVar = d, d/2
	} else {
		diff := c.rtt - d
		if diff < 0 {
			diff = -diff
		}
		c.rttVar += (diff - c.rttVar) / 4
		c.rtt += (d - c.rtt) / 8
	}
	c.rto = min(max(c.rtt+4*c.rttVar, minRTO), maxRTO)
}

// --- LEDBAT -----------------------------------------------------------------

// ledbat grows or shrinks the window after acked bytes, from how far the
// queuing delay is from target. tsDiff is the peer's measurement of our
// packets' one-way delay (plus a clock offset, which cancels out against
// the base delay).
func (c *Conn) ledbat(tsDiff uint32, acked int, now time.Time) {
	if tsDiff != 0 {
		c.addDelaySample(tsDiff, now)
	}
	delay := c.queuingDelay()
	if c.slowStart {
		if delay > target/2 || c.cwnd >= c.ssthresh {
			c.slowStart = false
		} else if c.cwndLimited {
			c.cwnd += float64(acked)
		}
	}
	if !c.slowStart {
		off := min(max(float64(target-delay)/float64(target), -1), 1)
		gain := maxCwndIncrease * off * float64(acked) / c.cwnd
		if gain < 0 || c.cwndLimited { // grow only while the window is what limits us
			c.cwnd += gain
		}
	}
	c.cwnd = min(max(c.cwnd, minCwnd), maxCwnd)
}

func (c *Conn) addDelaySample(d uint32, now time.Time) {
	if m := now.Unix() / 60; m != c.baseMinute {
		c.base[1], c.baseSet[1] = c.base[0], c.baseSet[0] && m == c.baseMinute+1
		c.baseSet[0], c.baseMinute = false, m
	}
	if !c.baseSet[0] || int32(d-c.base[0]) < 0 {
		c.base[0], c.baseSet[0] = d, true
	}
	c.recent[c.nRecent%len(c.recent)] = d
	c.nRecent++
}

// queuingDelay is the lowest recent delay sample above the base delay
// (the lowest over the last two minutes), 0 if unknown.
func (c *Conn) queuingDelay() time.Duration {
	if c.nRecent == 0 {
		return 0
	}
	base := c.base[0]
	if c.baseSet[1] && int32(c.base[1]-base) < 0 {
		base = c.base[1]
	}
	low := c.recent[0]
	for i := 1; i < min(c.nRecent, len(c.recent)); i++ {
		if int32(c.recent[i]-low) < 0 {
			low = c.recent[i]
		}
	}
	return time.Duration(max(int32(low-base), 0)) * time.Microsecond
}

// onLoss halves the window, at most once per RTT.
func (c *Conn) onLoss(now time.Time) {
	if !c.lastCut.IsZero() && now.Sub(c.lastCut) < max(c.rtt, minRTO) {
		return
	}
	c.lastCut = now
	c.cwnd = max(c.cwnd/2, minCwnd)
	c.ssthresh, c.slowStart = c.cwnd, false
}

// --- timers -----------------------------------------------------------------

func (c *Conn) tick(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.state {
	case stateSynSent:
		if c.synTries < 3 && now.Sub(c.synSentAt) >= c.s.cfg.ConnectTimeout/3 {
			c.sendSyn()
		}
	case stateConnected:
		if len(c.inflight) > 0 {
			if now.Sub(c.lastRecv) > deadAfter {
				c.die(ErrTimeout)
				return
			}
			first := c.inflight[0]
			switch {
			case !first.lost && now.Sub(first.sentAt) >= c.rto:
				c.onTimeout(now)
			case !first.lost && !first.probed && c.rtt > 0 && now.Sub(first.sentAt) >= max(2*c.rtt, 5*tick):
				// Tail-loss probe: nothing acked for two RTTs, maybe the
				// last packets of a burst were lost and nothing will come
				// to signal it. Resend the oldest before the full timeout.
				first.probed = true
				c.nProbes++
				c.transmit(first, now)
			}
		}
		if c.ackPending > 0 {
			c.sendState() // delayed ack: at most a tick late
		} else if now.Sub(c.lastSend) >= keepAlive {
			c.sendState()
		}
		if c.closing && now.After(c.closeBy) {
			h := header{typ: stReset, connID: c.sendID, ts: c.s.now(), seq: c.seq, ack: c.ack}
			c.s.send(h.marshal(nil, nil), c.remote)
			c.die(net.ErrClosed)
		}
	}
}

// onTimeout: nothing was acked for an RTO. Everything in flight counts as
// lost and is resent as the window — back to one packet — allows; the
// RTO doubles until something is acked.
func (c *Conn) onTimeout(now time.Time) {
	c.nTimeouts++
	c.ssthresh = max(c.cwnd/2, 2*minCwnd)
	c.cwnd, c.slowStart = minCwnd, true
	c.rto = min(c.rto*2, maxRTO)
	for _, p := range c.inflight {
		if !p.lost {
			p.lost = true
			c.lostBytes += len(p.payload)
		}
	}
	first := c.inflight[0]
	first.lost = false
	c.lostBytes -= len(first.payload)
	c.transmit(first, now)
	c.flush(now)
}

// finishIfDone ends a closed connection once our FIN is acked and the
// peer has closed its side too (we acked its FIN). A peer that never
// closes is reset when the linger time runs out.
func (c *Conn) finishIfDone() {
	if c.closing && c.finSent && len(c.inflight) == 0 && c.eof {
		c.die(net.ErrClosed)
	}
}
