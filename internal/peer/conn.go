package peer

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/ratelimit"
)

// Options configures connections. Zero durations use the defaults below.
type Options struct {
	PeerID     peerid.ID // our peer ID
	Extensions bool      // advertise BEP 10 extension protocol
	DHT        bool      // advertise BEP 5 DHT

	DialTimeout       time.Duration // TCP connect; default 10s
	HandshakeTimeout  time.Duration // whole handshake exchange; default 10s
	IdleTimeout       time.Duration // drop peer after this long with no data; default 2m
	KeepAliveInterval time.Duration // send keep-alive when idle this long; default 90s
	WriteTimeout      time.Duration // one flush must finish within this; default 60s

	QueueSize int // buffered messages in each direction; default 256

	// ReadLimiter, if set, caps how fast this connection reads (shared by
	// all connections for a global download limit). Reading slower makes
	// TCP slow the peer down; nothing is dropped.
	ReadLimiter *ratelimit.Limiter
}

func (o *Options) withDefaults() Options {
	c := *o
	def := func(d *time.Duration, v time.Duration) {
		if *d <= 0 {
			*d = v
		}
	}
	def(&c.DialTimeout, 10*time.Second)
	def(&c.HandshakeTimeout, 10*time.Second)
	def(&c.IdleTimeout, 2*time.Minute)
	def(&c.KeepAliveInterval, 90*time.Second)
	def(&c.WriteTimeout, 60*time.Second)
	if c.QueueSize <= 0 {
		c.QueueSize = 256
	}
	return c
}

func (o *Options) handshake(infoHash metainfo.Hash) *Handshake {
	h := &Handshake{InfoHash: infoHash, PeerID: o.PeerID}
	if o.Extensions {
		h.SetExtensions()
	}
	if o.DHT {
		h.SetDHT()
	}
	return h
}

var (
	// ErrInfoHashMismatch: the peer answered for a different torrent.
	ErrInfoHashMismatch = errors.New("peer: info hash mismatch")
	// ErrUnknownTorrent: an incoming peer asked for a torrent we don't have.
	ErrUnknownTorrent = errors.New("peer: unknown torrent")
	// ErrSelfConnection: we connected to ourselves (our own IP from a tracker).
	ErrSelfConnection = errors.New("peer: connected to self")
	// ErrClosed is returned by Send after the connection has closed.
	ErrClosed = errors.New("peer: connection closed")
	// ErrIdle: the peer sent nothing for IdleTimeout.
	ErrIdle = errors.New("peer: idle timeout")
)

// Conn is an established, handshaken connection to one peer.
//
// Concurrency: Conn owns two goroutines. The reader delivers messages on
// Incoming(); the writer sends what Send() queues and emits keep-alives.
// When either fails, or Close is called, the connection shuts down once:
// Done() closes, Incoming() closes, and Err() reports the first error.
type Conn struct {
	Remote   netip.AddrPort
	Peer     Handshake // the handshake the peer sent us
	Outgoing bool      // true if we dialed, false if they connected to us

	nc   net.Conn
	opts Options

	in   chan *Message
	out  chan *Message
	done chan struct{}
	// ctx is cancelled at shutdown, so a reader waiting on the rate
	// limiter stops at once.
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
	errMu     sync.Mutex
	err       error

	bytesRead    atomic.Int64
	bytesWritten atomic.Int64
}

// Dial connects to addr and performs the handshake for infoHash.
func Dial(ctx context.Context, addr netip.AddrPort, infoHash metainfo.Hash, opts Options) (*Conn, error) {
	o := opts.withDefaults()
	d := net.Dialer{Timeout: o.DialTimeout}
	nc, err := d.DialContext(ctx, "tcp", addr.String())
	if err != nil {
		return nil, fmt.Errorf("peer %s: dial: %w", addr, err)
	}
	stop := cancelOnDone(ctx, nc)
	defer stop()

	nc.SetDeadline(time.Now().Add(o.HandshakeTimeout))
	if _, err := o.handshake(infoHash).WriteTo(nc); err != nil {
		nc.Close()
		return nil, fmt.Errorf("peer %s: sending handshake: %w", addr, ctxErr(ctx, err))
	}
	theirs, err := ReadHandshake(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("peer %s: %w", addr, ctxErr(ctx, err))
	}
	if theirs.InfoHash != infoHash {
		nc.Close()
		return nil, fmt.Errorf("peer %s: %w", addr, ErrInfoHashMismatch)
	}
	if theirs.PeerID == o.PeerID {
		nc.Close()
		return nil, fmt.Errorf("peer %s: %w", addr, ErrSelfConnection)
	}
	nc.SetDeadline(time.Time{})
	return start(nc, addr, *theirs, true, o), nil
}

// Accept performs the handshake on an incoming connection. known reports
// whether we serve a torrent; the peer speaks first, so we learn which
// torrent it wants before replying.
func Accept(nc net.Conn, known func(metainfo.Hash) bool, opts Options) (*Conn, error) {
	o := opts.withDefaults()
	addr := addrPortOf(nc.RemoteAddr())
	nc.SetDeadline(time.Now().Add(o.HandshakeTimeout))

	theirs, err := ReadHandshake(nc)
	if err != nil {
		nc.Close()
		return nil, fmt.Errorf("peer %s: %w", addr, err)
	}
	if !known(theirs.InfoHash) {
		nc.Close()
		return nil, fmt.Errorf("peer %s: %w %s", addr, ErrUnknownTorrent, theirs.InfoHash)
	}
	if theirs.PeerID == o.PeerID {
		nc.Close()
		return nil, fmt.Errorf("peer %s: %w", addr, ErrSelfConnection)
	}
	if _, err := o.handshake(theirs.InfoHash).WriteTo(nc); err != nil {
		nc.Close()
		return nil, fmt.Errorf("peer %s: sending handshake: %w", addr, err)
	}
	nc.SetDeadline(time.Time{})
	return start(nc, addr, *theirs, false, o), nil
}

func start(nc net.Conn, addr netip.AddrPort, theirs Handshake, outgoing bool, o Options) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Conn{
		ctx:      ctx,
		cancel:   cancel,
		Remote:   addr,
		Peer:     theirs,
		Outgoing: outgoing,
		nc:       nc,
		opts:     o,
		in:       make(chan *Message, o.QueueSize),
		out:      make(chan *Message, o.QueueSize),
		done:     make(chan struct{}),
	}
	go c.readLoop()
	go c.writeLoop()
	return c
}

// Incoming delivers messages from the peer (keep-alives are filtered out).
// It is closed when the connection ends.
func (c *Conn) Incoming() <-chan *Message { return c.in }

// Done is closed when the connection ends.
func (c *Conn) Done() <-chan struct{} { return c.done }

// Err returns why the connection ended (nil while it is open, or after a
// local Close with no prior error).
func (c *Conn) Err() error {
	c.errMu.Lock()
	defer c.errMu.Unlock()
	return c.err
}

// BytesRead / BytesWritten count raw wire bytes after the handshake
// (protocol overhead included).
func (c *Conn) BytesRead() int64    { return c.bytesRead.Load() }
func (c *Conn) BytesWritten() int64 { return c.bytesWritten.Load() }

// Send queues m for writing. It blocks while the queue is full and returns
// ErrClosed if the connection ends first.
func (c *Conn) Send(m *Message) error {
	select {
	case <-c.done:
		return ErrClosed
	default:
	}
	select {
	case c.out <- m:
		return nil
	case <-c.done:
		return ErrClosed
	}
}

// TrySend queues m without blocking. It returns false if the queue is full
// or the connection is closed. The torrent session uses this so one slow
// peer can never stall it; a peer whose queue is full gets disconnected.
func (c *Conn) TrySend(m *Message) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.out <- m:
		return true
	default:
		return false
	}
}

// Close shuts the connection down. Safe to call multiple times.
func (c *Conn) Close() error {
	c.fail(nil)
	return nil
}

// fail records err (if it's the first) and tears the connection down once.
func (c *Conn) fail(err error) {
	c.closeOnce.Do(func() {
		c.errMu.Lock()
		c.err = err
		c.errMu.Unlock()
		close(c.done)
		c.cancel()
		c.nc.Close() // unblocks the reader and writer
	})
}

func (c *Conn) readLoop() {
	defer close(c.in)
	var src io.Reader = countingReader{c.nc, &c.bytesRead}
	if c.opts.ReadLimiter != nil {
		src = limitedReader{src, c.opts.ReadLimiter, c.ctx}
	}
	r := bufio.NewReaderSize(src, 32*1024)
	for {
		c.nc.SetReadDeadline(time.Now().Add(c.opts.IdleTimeout))
		m, err := ReadMessage(r)
		if err != nil {
			c.fail(c.readErr(err))
			return
		}
		if m == nil {
			continue // keep-alive: the deadline reset above is all it's for
		}
		select {
		case c.in <- m:
		case <-c.done:
			return
		}
	}
}

func (c *Conn) readErr(err error) error {
	select {
	case <-c.done:
		return nil // we closed it ourselves; not an error
	default:
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ErrIdle
	}
	if errors.Is(err, io.EOF) {
		return fmt.Errorf("peer %s: disconnected", c.Remote)
	}
	return fmt.Errorf("peer %s: read: %w", c.Remote, err)
}

func (c *Conn) writeLoop() {
	w := bufio.NewWriterSize(countingWriter{c.nc, &c.bytesWritten}, 32*1024)
	keepAlive := time.NewTimer(c.opts.KeepAliveInterval)
	defer keepAlive.Stop()

	write := func(m *Message) error {
		c.nc.SetWriteDeadline(time.Now().Add(c.opts.WriteTimeout))
		_, err := w.Write(m.Marshal())
		return err
	}
	for {
		var m *Message
		select {
		case <-c.done:
			return
		case <-keepAlive.C:
			m = nil // keep-alive
		case m = <-c.out:
		}
		if err := write(m); err != nil {
			c.fail(c.writeErr(err))
			return
		}
		// Batch: drain whatever else is already queued before flushing.
	drain:
		for {
			select {
			case m = <-c.out:
				if err := write(m); err != nil {
					c.fail(c.writeErr(err))
					return
				}
			default:
				break drain
			}
		}
		if err := w.Flush(); err != nil {
			c.fail(c.writeErr(err))
			return
		}
		keepAlive.Reset(c.opts.KeepAliveInterval)
	}
}

func (c *Conn) writeErr(err error) error {
	select {
	case <-c.done:
		return nil
	default:
	}
	return fmt.Errorf("peer %s: write: %w", c.Remote, err)
}

// --- helpers --------------------------------------------------------------

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (cr countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	cr.n.Add(int64(n))
	return n, err
}

// limitedReader charges every read to a rate limiter, pausing after reads
// while the limit is exceeded.
type limitedReader struct {
	r   io.Reader
	lim *ratelimit.Limiter
	ctx context.Context
}

func (lr limitedReader) Read(p []byte) (int, error) {
	n, err := lr.r.Read(p)
	if n > 0 {
		if werr := lr.lim.WaitN(lr.ctx, n); werr != nil && err == nil {
			err = werr // connection closing
		}
	}
	return n, err
}

type countingWriter struct {
	w io.Writer
	n *atomic.Int64
}

func (cw countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	cw.n.Add(int64(n))
	return n, err
}

// cancelOnDone makes blocking I/O on nc fail promptly when ctx is cancelled.
// The returned func must be called once the guarded section is over.
func cancelOnDone(ctx context.Context, nc net.Conn) (stop func() bool) {
	return context.AfterFunc(ctx, func() { nc.SetDeadline(time.Now()) })
}

// ctxErr prefers the context's error when it caused the failure.
func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

func addrPortOf(a net.Addr) netip.AddrPort {
	if tcp, ok := a.(*net.TCPAddr); ok {
		ap := tcp.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}
