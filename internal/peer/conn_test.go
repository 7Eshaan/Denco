package peer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/ratelimit"
)

var testInfoHash = metainfo.Hash{0xde, 0xc0}

// pair returns a connected (dialer, acceptor) pair over loopback TCP.
func pair(t *testing.T, dialOpts, acceptOpts Options) (*Conn, *Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	type result struct {
		c   *Conn
		err error
	}
	accepted := make(chan result, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			accepted <- result{nil, err}
			return
		}
		c, err := Accept(nc, func(h metainfo.Hash) bool { return h == testInfoHash }, acceptOpts)
		accepted <- result{c, err}
	}()

	addr := netip.MustParseAddrPort(ln.Addr().String())
	d, err := Dial(context.Background(), addr, testInfoHash, dialOpts)
	if err != nil {
		t.Fatal(err)
	}
	r := <-accepted
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { d.Close(); r.c.Close() })
	return d, r.c
}

func recv(t *testing.T, c *Conn) *Message {
	t.Helper()
	select {
	case m, ok := <-c.Incoming():
		if !ok {
			t.Fatalf("connection closed: %v", c.Err())
		}
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for message")
		return nil
	}
}

func TestDialAcceptExchange(t *testing.T) {
	a := Options{PeerID: peerid.New(), Extensions: true, DHT: true}
	b := Options{PeerID: peerid.New()}
	d, acc := pair(t, a, b)

	// Each side sees the other's handshake.
	if d.Peer.PeerID != b.PeerID || acc.Peer.PeerID != a.PeerID {
		t.Fatal("peer IDs not exchanged")
	}
	if !acc.Peer.SupportsExtensions() || !acc.Peer.SupportsDHT() || d.Peer.SupportsExtensions() {
		t.Fatal("reserved bits wrong")
	}
	if !d.Outgoing || acc.Outgoing {
		t.Fatal("Outgoing flag wrong")
	}
	if !acc.Remote.Addr().Is4() {
		t.Fatalf("remote should be plain IPv4, got %s", acc.Remote)
	}

	// Several messages in a row arrive in order (exercises write batching).
	msgs := []*Message{Interested(), Have(5), Request(Block{1, 0, BlockSize}), Piece(1, 0, []byte("data"))}
	for _, m := range msgs {
		if err := d.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range msgs {
		got := recv(t, acc)
		if got.ID != want.ID || string(got.Payload) != string(want.Payload) {
			t.Fatalf("got %s, want %s", got, want)
		}
	}

	// And the other direction.
	if err := acc.Send(Unchoke()); err != nil {
		t.Fatal(err)
	}
	if got := recv(t, d); got.ID != MsgUnchoke {
		t.Fatalf("got %s", got)
	}

	if d.BytesWritten() == 0 || acc.BytesRead() == 0 {
		t.Error("byte counters not updated")
	}
}

func TestCloseEndsBothSides(t *testing.T) {
	d, acc := pair(t, Options{PeerID: peerid.New()}, Options{PeerID: peerid.New()})
	d.Close()
	d.Close() // idempotent

	select {
	case <-d.Done():
	case <-time.After(time.Second):
		t.Fatal("Done not closed")
	}
	if d.Err() != nil {
		t.Errorf("local close should have nil Err, got %v", d.Err())
	}
	if err := d.Send(Choke()); !errors.Is(err, ErrClosed) {
		t.Errorf("Send after close = %v", err)
	}

	// Remote side notices the disconnect and Incoming closes.
	select {
	case _, ok := <-acc.Incoming():
		if ok {
			t.Fatal("unexpected message")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("remote did not notice close")
	}
	if acc.Err() == nil {
		t.Error("remote Err should explain the disconnect")
	}
}

func TestKeepAliveKeepsConnectionOpen(t *testing.T) {
	fast := Options{
		PeerID:            peerid.New(),
		KeepAliveInterval: 20 * time.Millisecond,
		IdleTimeout:       150 * time.Millisecond,
	}
	other := fast
	other.PeerID = peerid.New()
	d, acc := pair(t, fast, other)

	// Nobody sends real messages for well over IdleTimeout; keep-alives must
	// keep both sides alive.
	time.Sleep(500 * time.Millisecond)
	select {
	case <-d.Done():
		t.Fatalf("dialer closed: %v", d.Err())
	case <-acc.Done():
		t.Fatalf("acceptor closed: %v", acc.Err())
	default:
	}
}

func TestIdleTimeout(t *testing.T) {
	// The acceptor never sends keep-alives (huge interval); the dialer has a
	// short idle timeout and must drop it.
	d, _ := pair(t,
		Options{PeerID: peerid.New(), IdleTimeout: 100 * time.Millisecond},
		Options{PeerID: peerid.New(), KeepAliveInterval: time.Hour},
	)
	select {
	case <-d.Done():
		if !errors.Is(d.Err(), ErrIdle) {
			t.Fatalf("Err = %v, want ErrIdle", d.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle peer not dropped")
	}
}

// rawPeer listens and runs fn on the first accepted connection.
func rawPeer(t *testing.T, fn func(net.Conn)) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		defer nc.Close()
		fn(nc)
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func TestDialErrors(t *testing.T) {
	ctx := context.Background()
	me := Options{PeerID: peerid.New(), HandshakeTimeout: 300 * time.Millisecond}

	t.Run("info hash mismatch", func(t *testing.T) {
		addr := rawPeer(t, func(nc net.Conn) {
			ReadHandshake(nc)
			(&Handshake{InfoHash: metainfo.Hash{9}, PeerID: peerid.New()}).WriteTo(nc)
		})
		if _, err := Dial(ctx, addr, testInfoHash, me); !errors.Is(err, ErrInfoHashMismatch) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("self connection", func(t *testing.T) {
		addr := rawPeer(t, func(nc net.Conn) {
			ReadHandshake(nc)
			(&Handshake{InfoHash: testInfoHash, PeerID: me.PeerID}).WriteTo(nc)
		})
		if _, err := Dial(ctx, addr, testInfoHash, me); !errors.Is(err, ErrSelfConnection) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("not bittorrent", func(t *testing.T) {
		addr := rawPeer(t, func(nc net.Conn) {
			// Read our handshake first, as a real server reads a request:
			// closing with unread data makes Windows reset the connection,
			// and the reset discards the reply before we read it.
			io.ReadFull(nc, make([]byte, HandshakeLen))
			nc.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
		})
		if _, err := Dial(ctx, addr, testInfoHash, me); !errors.Is(err, ErrBadHandshake) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("silent peer times out", func(t *testing.T) {
		addr := rawPeer(t, func(nc net.Conn) { time.Sleep(2 * time.Second) })
		start := time.Now()
		if _, err := Dial(ctx, addr, testInfoHash, me); err == nil {
			t.Fatal("expected timeout")
		}
		if time.Since(start) > 1500*time.Millisecond {
			t.Fatal("handshake timeout not applied")
		}
	})
	t.Run("context cancel during handshake", func(t *testing.T) {
		addr := rawPeer(t, func(nc net.Conn) { time.Sleep(2 * time.Second) })
		cctx, cancel := context.WithCancel(ctx)
		time.AfterFunc(50*time.Millisecond, cancel)
		slow := me
		slow.HandshakeTimeout = 5 * time.Second
		if _, err := Dial(cctx, addr, testInfoHash, slow); !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := netip.MustParseAddrPort(ln.Addr().String())
		ln.Close()
		if _, err := Dial(ctx, addr, testInfoHash, me); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestAcceptUnknownTorrent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errc := make(chan error, 1)
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			errc <- err
			return
		}
		_, err = Accept(nc, func(metainfo.Hash) bool { return false }, Options{PeerID: peerid.New()})
		errc <- err
	}()
	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	(&Handshake{InfoHash: testInfoHash, PeerID: peerid.New()}).WriteTo(nc)
	if err := <-errc; !errors.Is(err, ErrUnknownTorrent) {
		t.Fatalf("got %v", err)
	}
}

func TestOversizedMessageDropsPeer(t *testing.T) {
	addr := rawPeer(t, func(nc net.Conn) {
		h, _ := ReadHandshake(nc)
		(&Handshake{InfoHash: h.InfoHash, PeerID: peerid.New()}).WriteTo(nc)
		nc.Write([]byte{0xff, 0xff, 0xff, 0xff}) // 4 GiB message claim
		time.Sleep(time.Second)
	})
	c, err := Dial(context.Background(), addr, testInfoHash, Options{PeerID: peerid.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-c.Done():
		if !errors.Is(c.Err(), ErrMessageTooLarge) {
			t.Fatalf("Err = %v", c.Err())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer not dropped")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 50; i++ {
		d, acc := pair(t, Options{PeerID: peerid.New()}, Options{PeerID: peerid.New()})
		d.Send(Interested())
		d.Close()
		acc.Close()
		<-d.Done()
		<-acc.Done()
	}
	// Goroutines exit asynchronously; give them a moment.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestTrySend(t *testing.T) {
	d, _ := pair(t, Options{PeerID: peerid.New(), QueueSize: 2}, Options{PeerID: peerid.New()})
	// With a live writer the "queue full" case is timing-dependent, so here we
	// only check open → true and closed → false (full queue: next test).
	if !d.TrySend(Interested()) {
		t.Fatal("TrySend on open conn failed")
	}
	d.Close()
	<-d.Done()
	if d.TrySend(Interested()) {
		t.Fatal("TrySend after close succeeded")
	}
}

func TestTrySendFullQueue(t *testing.T) {
	// A Conn whose writer goroutine never runs: build it by hand.
	c := &Conn{out: make(chan *Message, 1), done: make(chan struct{})}
	if !c.TrySend(Choke()) {
		t.Fatal("first send should fit")
	}
	if c.TrySend(Choke()) {
		t.Fatal("second send should report full queue")
	}
}

func TestReadLimiterThrottlesAndStopsOnClose(t *testing.T) {
	lim := ratelimit.New(1 << 20) // 1 MiB/s, burst 256 KiB
	d, acc := pair(t, Options{PeerID: peerid.New()}, Options{PeerID: peerid.New(), ReadLimiter: lim})

	// 48 blocks of 16 KiB = 768 KiB: about 0.5s beyond the burst.
	block := make([]byte, BlockSize)
	go func() {
		for i := 0; i < 48; i++ {
			d.Send(Piece(0, uint32(i*BlockSize), block))
		}
	}()
	start := time.Now()
	for i := 0; i < 48; i++ {
		recv(t, acc)
	}
	if got := time.Since(start); got < 350*time.Millisecond {
		t.Fatalf("768 KiB at 1 MiB/s arrived in %v; the limit wasn't applied", got)
	}

	// A reader stuck on a tiny limit must still shut down promptly.
	lim.SetRate(1000)
	go d.Send(Piece(0, 0, block))
	time.Sleep(50 * time.Millisecond)
	start = time.Now()
	acc.Close()
	select {
	case <-acc.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked")
	}
	for range acc.Incoming() { // closed once the reader exits
	}
	if time.Since(start) > time.Second {
		t.Fatalf("reader took %v to stop", time.Since(start))
	}
}

// Time we spend waiting on our own download limit isn't the peer being
// idle: the idle timeout covers socket reads only (D46). Before the fix
// one message slower to read than IdleTimeout dropped the peer.
func TestThrottledReadIsNotIdle(t *testing.T) {
	lim := ratelimit.New(100_000) // burst 32 KiB; then 100 KB/s
	// The sender keeps the line busy (keep-alives every 50 ms), so the only
	// way to time out is counting our throttling as the peer's silence.
	d, acc := pair(t, Options{PeerID: peerid.New(), KeepAliveInterval: 50 * time.Millisecond},
		Options{PeerID: peerid.New(), ReadLimiter: lim, IdleTimeout: 200 * time.Millisecond})
	big := make([]byte, 400_000) // ≈ 3.7 s to read at the limit
	go d.Send(Piece(0, 0, big))
	m := recv(t, acc)
	if _, _, data, err := ParsePiece(m); err != nil || len(data) != len(big) {
		t.Fatalf("got %v, %v", m, err)
	}
	time.Sleep(300 * time.Millisecond) // keep-alives only: still alive
	select {
	case <-acc.Done():
		t.Fatalf("connection closed: %v", acc.Err())
	default:
	}
}
