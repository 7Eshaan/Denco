package utp

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// listen opens a loopback UDP socket.
func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	return pc
}

func addrOf(s *Socket) netip.AddrPort { return s.pc.LocalAddr().(*net.UDPAddr).AddrPort() }

// lossy wraps a socket: outgoing datagrams are dropped, duplicated or
// delayed (so reordered) at random.
type lossy struct {
	*net.UDPConn
	drop, dup, delay float64
	mu               sync.Mutex
	rnd              *mrand.Rand
}

func (l *lossy) WriteToUDPAddrPort(b []byte, to netip.AddrPort) (int, error) {
	l.mu.Lock()
	drop, dup, late := l.rnd.Float64() < l.drop, l.rnd.Float64() < l.dup, l.rnd.Float64() < l.delay
	wait := time.Duration(l.rnd.IntN(30)) * time.Millisecond
	l.mu.Unlock()
	if drop {
		return len(b), nil
	}
	if late {
		b = bytes.Clone(b)
		time.AfterFunc(wait, func() { l.UDPConn.WriteToUDPAddrPort(b, to) })
	} else {
		l.UDPConn.WriteToUDPAddrPort(b, to)
	}
	if dup {
		l.UDPConn.WriteToUDPAddrPort(b, to)
	}
	return len(b), nil
}

func newLossy(t *testing.T, drop, dup, delay float64, seed uint64) *lossy {
	return &lossy{UDPConn: listen(t), drop: drop, dup: dup, delay: delay, rnd: mrand.New(mrand.NewPCG(seed, 1))}
}

func socket(t *testing.T, pc PacketConn, cfg Config) *Socket {
	t.Helper()
	s := NewSocket(pc, cfg)
	t.Cleanup(func() { s.Close() })
	return s
}

// connect dials b from a and returns both ends.
func connect(t *testing.T, a, b *Socket) (*Conn, net.Conn) {
	t.Helper()
	acc := make(chan net.Conn, 1)
	go func() {
		c, err := b.Accept()
		if err != nil {
			t.Error(err)
		}
		acc <- c
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := a.Dial(ctx, addrOf(b))
	if err != nil {
		t.Fatal(err)
	}
	return c, <-acc
}

// exchange sends size random bytes each way at once and checks them.
func exchange(t *testing.T, x, y net.Conn, size int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	x.SetDeadline(deadline)
	y.SetDeadline(deadline)
	data := make([]byte, size)
	rand.Read(data)
	errs := make(chan error, 4)
	for _, w := range []net.Conn{x, y} {
		go func() {
			for off := 0; off < size; off += 7000 {
				if _, err := w.Write(data[off:min(off+7000, size)]); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for _, r := range []net.Conn{x, y} {
		go func() {
			got := make([]byte, size)
			if _, err := io.ReadFull(r, got); err != nil {
				errs <- err
				return
			}
			if !bytes.Equal(got, data) {
				errs <- errors.New("data corrupted")
				return
			}
			errs <- nil
		}()
	}
	for range 4 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

func TestHeaderRoundTrip(t *testing.T) {
	h := header{typ: stData, connID: 0xbeef, ts: 1, tsDiff: 2, wnd: 3, seq: 0xfffe, ack: 7, sack: []byte{1, 0, 0, 0x80}}
	b := h.marshal(nil, []byte("payload"))
	got, payload, err := parse(b)
	if err != nil || string(payload) != "payload" || got.connID != 0xbeef || got.seq != 0xfffe ||
		got.ack != 7 || got.wnd != 3 || got.tsDiff != 2 || !bytes.Equal(got.sack, h.sack) {
		t.Fatalf("%+v %q %v", got, payload, err)
	}
	// An unknown extension before the SACK is skipped.
	raw := (&header{typ: stState}).marshal(nil, nil)
	raw[1] = 9
	raw = append(raw, extSACK, 3, 'x', 'y', 'z', 0, 4, 0xff, 0, 0, 0)
	got, payload, err = parse(raw)
	if err != nil || len(payload) != 0 || len(got.sack) != 4 || got.sack[0] != 0xff {
		t.Fatalf("extensions: %+v %v", got, err)
	}
	for _, bad := range [][]byte{
		raw[:19],                                     // short
		append([]byte{0x02}, raw[1:20]...),           // version 2
		append([]byte{0x51}, raw[1:20]...),           // type 5
		append(bytes.Clone(raw[:20]), 1, 9),          // extension longer than the packet
		append(bytes.Clone(raw[:20]), 0, 3, 1, 2, 3), // SACK not a multiple of 4
	} {
		bad[1] = max(bad[1], 0)
		if len(bad) > 20 {
			bad[1] = extSACK
		}
		if _, _, err := parse(bad); err == nil {
			t.Errorf("parsed %x", bad)
		}
	}
	if !seqLess(0xfff0, 3) || seqLess(3, 0xfff0) || seqLess(5, 5) {
		t.Error("seqLess wraps wrongly")
	}
}

func TestTransfer(t *testing.T) {
	a, b := socket(t, listen(t), Config{}), socket(t, listen(t), Config{})
	x, y := connect(t, a, b)
	exchange(t, x, y, 4<<20, 30*time.Second)
}

// 10% loss, duplicates and reordering both ways: the data still arrives
// intact.
func TestTransferLossy(t *testing.T) {
	a := socket(t, newLossy(t, 0.10, 0.05, 0.2, 1), Config{})
	b := socket(t, newLossy(t, 0.10, 0.05, 0.2, 2), Config{})
	x, y := connect(t, a, b)
	start := time.Now()
	exchange(t, x, y, 1<<20, 60*time.Second)
	for _, c := range []*Conn{x, y.(*Conn)} {
		c.mu.Lock()
		t.Logf("%v: %d packets, %d timeouts, %d fast resends, %d probes, cwnd %.0f rtt %v rto %v", time.Since(start), c.nPackets, c.nTimeouts, c.nFastResends, c.nProbes, c.cwnd, c.rtt, c.rto)
		c.mu.Unlock()
	}
}

func TestManyConnections(t *testing.T) {
	a, b := socket(t, listen(t), Config{}), socket(t, listen(t), Config{})
	var wg sync.WaitGroup
	for range 20 {
		x, y := connect(t, a, b)
		wg.Add(1)
		go func() {
			defer wg.Done()
			exchange(t, x, y, 100_000, 30*time.Second)
		}()
	}
	wg.Wait()
}

func TestDialUnanswered(t *testing.T) {
	silent := listen(t) // a UDP socket that never answers
	defer silent.Close()
	a := socket(t, listen(t), Config{ConnectTimeout: 300 * time.Millisecond})
	start := time.Now()
	_, err := a.Dial(context.Background(), silent.LocalAddr().(*net.UDPAddr).AddrPort())
	if !errors.Is(err, ErrConnect) || time.Since(start) > 2*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	// The SYN was sent three times.
	silent.SetReadDeadline(time.Now().Add(time.Second))
	n := 0
	for buf := make([]byte, 100); ; n++ {
		if _, err := silent.Read(buf); err != nil {
			break
		}
	}
	if n != 3 {
		t.Errorf("%d SYNs, want 3", n)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.conns) != 0 {
		t.Error("the failed connection is still registered")
	}
}

// Close sends everything written before it, then the peer reads EOF.
func TestCloseDeliversEverything(t *testing.T) {
	a := socket(t, newLossy(t, 0.05, 0, 0.1, 3), Config{})
	b := socket(t, listen(t), Config{})
	x, y := connect(t, a, b)
	data := make([]byte, 300_000)
	rand.Read(data)
	if _, err := x.Write(data); err != nil {
		t.Fatal(err)
	}
	x.Close()
	if _, err := x.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("write after close: %v", err)
	}
	y.SetReadDeadline(time.Now().Add(20 * time.Second))
	got, err := io.ReadAll(y)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("read %d of %d bytes, %v", len(got), len(data), err)
	}
	// Once both have closed and their FINs are acked, both sides forget
	// the connection — well before the linger time.
	start := time.Now()
	y.Close()
	waitFor(t, func() bool { return a.count() == 0 && b.count() == 0 })
	if time.Since(start) > linger/2 {
		t.Errorf("closing took %v", time.Since(start))
	}
}

func (s *Socket) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(15 * time.Second); !cond(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
	}
}

func TestDeadlines(t *testing.T) {
	a := socket(t, listen(t), Config{SendBuffer: 64 << 10})
	b := socket(t, listen(t), Config{RecvBuffer: 64 << 10})
	x, y := connect(t, a, b)
	y.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, err := y.Read(make([]byte, 10))
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("read: %v", err)
	}
	// Nobody reads y: x's buffers fill and a write deadline ends the write.
	x.SetWriteDeadline(time.Now().Add(500 * time.Millisecond))
	_, err = x.Write(make([]byte, 1<<20))
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("write: %v", err)
	}
	// A past deadline cleared: reading works again.
	y.SetReadDeadline(time.Time{})
	if _, err := io.ReadFull(y, make([]byte, 1000)); err != nil {
		t.Fatal(err)
	}
	// A deadline set while a Read waits wakes it.
	go func() { time.Sleep(50 * time.Millisecond); x.SetReadDeadline(time.Now()) }()
	if _, err := x.Read(make([]byte, 1)); !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("woken read: %v", err)
	}
}

// A reader much slower than the sender: the receive window holds the
// sender back, nothing is lost and the connection doesn't time out.
func TestSlowReader(t *testing.T) {
	a := socket(t, listen(t), Config{})
	b := socket(t, listen(t), Config{RecvBuffer: 32 << 10})
	x, y := connect(t, a, b)
	data := make([]byte, 400_000)
	rand.Read(data)
	go x.Write(data)
	got := make([]byte, 0, len(data))
	buf := make([]byte, 8<<10)
	y.SetReadDeadline(time.Now().Add(30 * time.Second))
	for len(got) < len(data) {
		n, err := y.Read(buf)
		if err != nil {
			t.Fatalf("after %d bytes: %v", len(got), err)
		}
		got = append(got, buf[:n]...)
		time.Sleep(5 * time.Millisecond) // ~1.6 MB/s at most
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data corrupted")
	}
	xc := x
	xc.mu.Lock()
	over := xc.inflightBytes > 32<<10+maxPayload
	xc.mu.Unlock()
	if over {
		t.Error("the sender ignored the receive window")
	}
}

// Closing a socket resets its connections; the peer's reads fail.
func TestSocketCloseResetsPeer(t *testing.T) {
	a, b := socket(t, listen(t), Config{}), socket(t, listen(t), Config{})
	x, y := connect(t, a, b)
	x.Write([]byte("hi"))
	io.ReadFull(y, make([]byte, 2))
	a.Close()
	y.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := y.Read(make([]byte, 1)); !errors.Is(err, ErrReset) {
		t.Fatalf("got %v", err)
	}
	if _, err := x.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("own side: %v", err)
	}
}

// A peer that disappears (all its packets lost) ends the connection with
// ErrTimeout while data is unacked.
func TestDeadPeerTimesOut(t *testing.T) {
	if testing.Short() {
		t.Skip("takes deadAfter")
	}
	a := socket(t, listen(t), Config{})
	bl := newLossy(t, 0, 0, 0, 4)
	b := socket(t, bl, Config{})
	x, _ := connect(t, a, b)
	bl.mu.Lock()
	bl.drop = 1
	bl.mu.Unlock()
	x.Write([]byte("anyone?"))
	x.mu.Lock()
	x.lastRecv = time.Now().Add(-deadAfter) // skip the 30 s wait
	x.mu.Unlock()
	x.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := x.Read(make([]byte, 1)); !errors.Is(err, ErrTimeout) {
		t.Fatalf("got %v", err)
	}
}

// --- LEDBAT, on a bare Conn ---------------------------------------------------

func bareConn() *Conn {
	s := &Socket{cfg: Config{RecvBuffer: 1 << 20, SendBuffer: 1 << 20}, conns: map[connKey]*Conn{}, started: time.Now(),
		dropOut: func([]byte) bool { return true }} // sends go nowhere
	c := newConn(s, netip.MustParseAddrPort("127.0.0.1:1"), 1, 2, false)
	c.slowStart = false
	c.cwnd = 100_000
	c.peerWnd = 1 << 20
	c.cwndLimited = true
	return c
}

// ack feeds n bytes acked with a delay sample of base+queue.
func (c *Conn) feed(base, queue time.Duration, n int) {
	c.ledbat(uint32((base + queue).Microseconds()), n, time.Now())
}

func TestLEDBAT(t *testing.T) {
	const base = 40 * time.Millisecond
	t.Run("grows below target", func(t *testing.T) {
		c := bareConn()
		c.feed(base, 0, 1000) // sets the base
		before := c.cwnd
		for range 100 {
			c.feed(base, 10*time.Millisecond, 1000)
		}
		// 100 KB acked at 90% below target: +3000·0.9·(100 KB / ~100 KB) ≈ 2.7 KB.
		if g := c.cwnd - before; g < 2000 || g > 3000 {
			t.Fatalf("grew by %.0f bytes in one window", g)
		}
	})
	t.Run("shrinks above target", func(t *testing.T) {
		c := bareConn()
		c.feed(base, 0, 1000)
		before := c.cwnd
		for range 100 {
			c.feed(base, 300*time.Millisecond, 1000) // way above: off-target clamped to −1
		}
		if s := before - c.cwnd; s < 2500 || s > 3500 {
			t.Fatalf("shrank by %.0f bytes in one window", s)
		}
	})
	t.Run("no growth when application-limited", func(t *testing.T) {
		c := bareConn()
		c.cwndLimited = false
		c.feed(base, 0, 1000)
		before := c.cwnd
		c.feed(base, 0, 50_000)
		if c.cwnd != before {
			t.Fatalf("grew from %.0f to %.0f", before, c.cwnd)
		}
	})
	t.Run("slow start doubles, ends near target", func(t *testing.T) {
		c := bareConn()
		c.slowStart, c.cwnd = true, initialCwnd
		c.feed(base, 0, int(initialCwnd))
		if c.cwnd != 2*initialCwnd {
			t.Fatalf("cwnd %.0f after a full window acked", c.cwnd)
		}
		for range 3 { // the lowest of the last three samples counts
			c.feed(base, 60*time.Millisecond, 1000) // above target/2
		}
		if c.slowStart {
			t.Fatal("still in slow start with 60 ms of queuing")
		}
	})
	t.Run("base delay is the minimum over two minutes", func(t *testing.T) {
		c := bareConn()
		now := time.Unix(6000, 0)
		c.addDelaySample(uint32(base.Microseconds()), now)
		c.addDelaySample(uint32((base + 50*time.Millisecond).Microseconds()), now.Add(61*time.Second))
		for range 3 {
			c.addDelaySample(uint32((base + 50*time.Millisecond).Microseconds()), now.Add(62*time.Second))
		}
		if d := c.queuingDelay(); d != 50*time.Millisecond {
			t.Fatalf("queuing delay %v, want 50ms (last minute's base kept)", d)
		}
		c.addDelaySample(uint32((base + 50*time.Millisecond).Microseconds()), now.Add(185*time.Second))
		if d := c.queuingDelay(); d != 0 {
			t.Fatalf("queuing delay %v after the old base expired, want 0", d)
		}
	})
	t.Run("loss halves once per RTT", func(t *testing.T) {
		c := bareConn()
		c.rtt = time.Second
		now := time.Now()
		c.onLoss(now)
		c.onLoss(now.Add(100 * time.Millisecond))
		if c.cwnd != 50_000 {
			t.Fatalf("cwnd %.0f", c.cwnd)
		}
		c.onLoss(now.Add(2 * time.Second))
		if c.cwnd != 25_000 {
			t.Fatalf("cwnd %.0f after a second loss an RTT later", c.cwnd)
		}
	})
}

// A timeout drops the window to one packet and marks everything in flight
// lost; acks then let the lost packets out again in order.
func TestTimeoutResends(t *testing.T) {
	a := socket(t, listen(t), Config{})
	bl := newLossy(t, 0, 0, 0, 5)
	b := socket(t, bl, Config{})
	x, y := connect(t, a, b)
	var sent []uint16
	var mu sync.Mutex
	a.dropOut = func(p []byte) bool {
		h, _, _ := parse(p)
		mu.Lock()
		defer mu.Unlock()
		if h.typ == stData {
			sent = append(sent, h.seq)
			return len(sent) <= 5 // the first five data packets are lost
		}
		return false
	}
	data := make([]byte, 5*maxPayload)
	rand.Read(data)
	x.Write(data)
	y.SetReadDeadline(time.Now().Add(15 * time.Second))
	got := make([]byte, len(data))
	if _, err := io.ReadFull(y, got); err != nil || !bytes.Equal(got, data) {
		t.Fatalf("%v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(sent) < 10 || sent[5] != sent[0] {
		t.Fatalf("sent %v: the first packet should be resent first", sent)
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.rto < 2*minRTO && x.rtt > 50*time.Millisecond {
		t.Errorf("rto %v", x.rto)
	}
}

// inflightSeqs lists the unacked packets' sequence numbers.
func (c *Conn) inflightSeqs() []uint16 {
	var out []uint16
	for _, p := range c.inflight {
		out = append(out, p.seq)
	}
	return out
}

// sendData queues n full packets on a bare conn (mu held by the caller
// isn't needed: nothing else runs).
func (c *Conn) sendData(n int) {
	c.sendBuf = append(c.sendBuf, make([]byte, n*maxPayload)...)
	c.flush(time.Now())
}

func TestSelectiveAckRemovesPackets(t *testing.T) {
	c := bareConn()
	c.seq = 10
	c.sendData(5) // 10…14
	// Ack 9 (nothing), SACK bits 0 and 1 = packets 11 and 12.
	c.processAck(header{typ: stState, ack: 9, sack: []byte{0b11, 0, 0, 0}, wnd: 1 << 20}, time.Now())
	if got := c.inflightSeqs(); len(got) != 3 || got[0] != 10 || got[1] != 13 || got[2] != 14 {
		t.Fatalf("in flight %v, want [10 13 14]", got)
	}
	if c.inflightBytes != 3*maxPayload {
		t.Fatalf("%d bytes in flight", c.inflightBytes)
	}
}

func TestSenderRespectsReceiveWindow(t *testing.T) {
	c := bareConn()
	c.peerWnd = 3 * maxPayload
	c.sendData(10)
	if len(c.inflight) != 3 || c.unsent() != 7*maxPayload {
		t.Fatalf("%d packets in flight, %d bytes waiting; window is 3 packets", len(c.inflight), c.unsent())
	}
	// A zero window still lets one packet out (the probe) — no more.
	c = bareConn()
	c.peerWnd = 0
	c.sendData(3)
	if len(c.inflight) != 1 {
		t.Fatalf("%d packets in flight into a zero window", len(c.inflight))
	}
}

func TestTimeoutCollapsesWindow(t *testing.T) {
	c := bareConn()
	c.cwnd = 50 * maxPayload
	c.sendData(5)
	c.onTimeout(time.Now())
	if c.cwnd != minCwnd || !c.slowStart || c.rto != 2*initialRTO {
		t.Fatalf("cwnd %.0f slow start %v rto %v", c.cwnd, c.slowStart, c.rto)
	}
	// The first is resent at once; the other four wait as lost.
	if c.inflight[0].sends != 2 || c.lostBytes != 4*maxPayload {
		t.Fatalf("first sent %d times, %d bytes marked lost", c.inflight[0].sends, c.lostBytes)
	}
}

func TestTailLossProbe(t *testing.T) {
	c := bareConn()
	c.rtt, c.rto = 20*time.Millisecond, 5*time.Second
	c.sendData(1)
	c.inflight[0].sentAt = time.Now().Add(-100 * time.Millisecond) // 5 RTTs, well before the RTO
	c.tick(time.Now())
	if p := c.inflight[0]; p.sends != 2 || !p.probed || c.nTimeouts != 0 {
		t.Fatalf("sends %d probed %v timeouts %d", p.sends, p.probed, c.nTimeouts)
	}
	c.inflight[0].sentAt = time.Now().Add(-100 * time.Millisecond)
	c.tick(time.Now())
	if c.inflight[0].sends != 2 {
		t.Fatal("probed twice: the RTO handles it from here")
	}
}

// A read deadline on an idle connection fires on time — nothing else
// (no traffic) has to wake the reader.
func TestReadDeadlineOnIdleConn(t *testing.T) {
	a, b := socket(t, listen(t), Config{}), socket(t, listen(t), Config{})
	_, y := connect(t, a, b)
	time.Sleep(50 * time.Millisecond) // the handshake's acks are done
	done := make(chan error, 1)
	start := time.Now()
	y.SetReadDeadline(start.Add(200 * time.Millisecond))
	go func() { _, err := y.Read(make([]byte, 1)); done <- err }()
	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() || time.Since(start) < 150*time.Millisecond {
			t.Fatalf("%v after %v", err, time.Since(start))
		}
	case <-time.After(3 * time.Second):
		y.Close()
		t.Fatal("the read deadline never fired")
	}
}

// Both sides know each other's receive window from the handshake.
func TestWindowsFromHandshake(t *testing.T) {
	a := socket(t, listen(t), Config{RecvBuffer: 300_000})
	b := socket(t, listen(t), Config{RecvBuffer: 500_000})
	x, y := connect(t, a, b)
	yc := y.(*Conn)
	x.mu.Lock()
	yc.mu.Lock()
	defer x.mu.Unlock()
	defer yc.mu.Unlock()
	if x.peerWnd != 500_000 || yc.peerWnd != 300_000 {
		t.Fatalf("dialer sees %d, acceptor sees %d", x.peerWnd, yc.peerWnd)
	}
}
