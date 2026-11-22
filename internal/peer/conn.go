package peer

import (
	"bufio"
	"bytes"
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
	"github.com/7Eshaan/Denco/internal/mse"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/ratelimit"
)

// Options configures connections. Zero durations use the defaults below.
type Options struct {
	PeerID     peerid.ID // our peer ID
	Extensions bool      // advertise BEP 10 extension protocol
	DHT        bool      // advertise BEP 5 DHT
	// Encryption is the MSE policy (D63); the zero value is plain
	// BitTorrent only.
	Encryption Encryption
	// UTP, if set, connects over uTP (BEP 29, D64), and Transport says
	// when: the zero value is TCP only.
	UTP       UTPDialer
	Transport Transport

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

// UTPDialer opens uTP connections (utp.Dialer).
type UTPDialer interface {
	Dial(ctx context.Context, addr netip.AddrPort) (net.Conn, error)
}

// Transport says which transports outgoing connections use (D64).
type Transport int

const (
	// TransportTCP: TCP only.
	TransportTCP Transport = iota
	// TransportBoth: uTP first; TCP when the peer doesn't answer uTP.
	TransportBoth
	// TransportUTP: uTP only.
	TransportUTP
)

// String is the config spelling: tcp, both, utp.
func (t Transport) String() string {
	switch t {
	case TransportBoth:
		return "both"
	case TransportUTP:
		return "utp"
	}
	return "tcp"
}

// ParseTransport reads the config spelling.
func ParseTransport(s string) (Transport, error) {
	for _, t := range []Transport{TransportTCP, TransportBoth, TransportUTP} {
		if s == t.String() {
			return t, nil
		}
	}
	return 0, fmt.Errorf("transport must be both, tcp or utp, not %q", s)
}

// Encryption is a policy for Message Stream Encryption (package mse).
type Encryption int

const (
	// EncryptionOff: plain BitTorrent only; encrypted peers are refused.
	EncryptionOff Encryption = iota
	// EncryptionPrefer: dial encrypted, falling back to plain when a peer
	// doesn't answer MSE; accept both.
	EncryptionPrefer
	// EncryptionRequire: encrypted (RC4) connections only, both ways.
	EncryptionRequire
)

// String is the config spelling: disable, prefer, require.
func (e Encryption) String() string {
	switch e {
	case EncryptionPrefer:
		return "prefer"
	case EncryptionRequire:
		return "require"
	}
	return "disable"
}

// ParseEncryption reads the config spelling.
func ParseEncryption(s string) (Encryption, error) {
	for _, e := range []Encryption{EncryptionOff, EncryptionPrefer, EncryptionRequire} {
		if s == e.String() {
			return e, nil
		}
	}
	return 0, fmt.Errorf("encryption must be prefer, require or disable, not %q", s)
}

var (
	// ErrPlaintextRefused: encryption is required and the peer spoke plain
	// BitTorrent (incoming), or didn't answer MSE (outgoing).
	ErrPlaintextRefused = errors.New("peer: encryption required")
	// ErrEncryptedRefused: encryption is disabled and the peer used MSE.
	ErrEncryptedRefused = errors.New("peer: encrypted connection while encryption is disabled")
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
	Remote    netip.AddrPort
	Peer      Handshake // the handshake the peer sent us
	Outgoing  bool      // true if we dialed, false if they connected to us
	Encrypted bool      // MSE with RC4: everything after the handshake is encrypted
	UTP       bool      // over uTP (D64), not TCP

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
	// torrentLim, if set, is the torrent's own download limit, applied on
	// top of Options.ReadLimiter (D59). Set after the handshake, because an
	// incoming peer's torrent is only known then.
	torrentLim atomic.Pointer[ratelimit.Limiter]
}

// SetTorrentLimiter makes reads also wait on lim (nil = none), on top of
// Options.ReadLimiter. It takes effect from the next socket read.
func (c *Conn) SetTorrentLimiter(lim *ratelimit.Limiter) { c.torrentLim.Store(lim) }

// Dial connects to addr and performs the handshake for infoHash.
//
// Transport (D64): with TransportBoth it tries uTP first and, if the peer
// doesn't answer uTP at all, TCP. Encryption (D63): with
// EncryptionPrefer, a peer that doesn't answer the encrypted handshake is
// dialed again in plain BitTorrent over the same transport (a new
// connection: a peer that saw MSE's random bytes has hung up).
func Dial(ctx context.Context, addr netip.AddrPort, infoHash metainfo.Hash, opts Options) (*Conn, error) {
	o := opts.withDefaults()
	if o.UTP != nil && o.Transport != TransportTCP {
		c, connected, err := dialEncrypted(ctx, addr, infoHash, o, true)
		if err == nil || connected || o.Transport == TransportUTP || ctx.Err() != nil {
			return c, err
		}
	}
	c, _, err := dialEncrypted(ctx, addr, infoHash, o, false)
	return c, err
}

// dialEncrypted dials over one transport, applying the encryption
// policy. connected reports whether the transport connection itself was
// made (a later failure isn't worth trying another transport for).
func dialEncrypted(ctx context.Context, addr netip.AddrPort, infoHash metainfo.Hash, o Options, viaUTP bool) (*Conn, bool, error) {
	if o.Encryption == EncryptionOff {
		return dial(ctx, addr, infoHash, o, viaUTP, false)
	}
	c, connected, err := dial(ctx, addr, infoHash, o, viaUTP, true)
	if err != nil && o.Encryption == EncryptionPrefer && errors.Is(err, mse.ErrNotMSE) && ctx.Err() == nil {
		return dial(ctx, addr, infoHash, o, viaUTP, false)
	}
	if errors.Is(err, mse.ErrNotMSE) {
		err = fmt.Errorf("%w: %w", ErrPlaintextRefused, err)
	}
	return c, connected, err
}

func dial(ctx context.Context, addr netip.AddrPort, infoHash metainfo.Hash, o Options, viaUTP, encrypt bool) (*Conn, bool, error) {
	var nc net.Conn
	var err error
	if viaUTP {
		nc, err = o.UTP.Dial(ctx, addr)
	} else {
		d := net.Dialer{Timeout: o.DialTimeout}
		nc, err = d.DialContext(ctx, "tcp", addr.String())
	}
	if err != nil {
		return nil, false, fmt.Errorf("peer %s: dial: %w", addr, err)
	}
	c, err := handshakeOut(ctx, nc, addr, infoHash, o, encrypt)
	return c, true, err
}

func handshakeOut(ctx context.Context, nc net.Conn, addr netip.AddrPort, infoHash metainfo.Hash, o Options, encrypt bool) (*Conn, error) {
	stop := cancelOnDone(ctx, nc)
	defer stop()

	nc.SetDeadline(time.Now().Add(o.HandshakeTimeout))
	ours := o.handshake(infoHash).MarshalBinary()
	encrypted := false
	if encrypt {
		// Our handshake rides along as MSE's initial payload. RC4 only:
		// offering plaintext too would let the peer skip encryption, and
		// a peer that can't do RC4 is retried in plain by Dial anyway.
		mc, _, err := mse.Initiate(nc, infoHash, mse.RC4, ours)
		if err != nil {
			nc.Close()
			return nil, fmt.Errorf("peer %s: %w", addr, ctxErr(ctx, err))
		}
		nc, encrypted = mc, true
	} else if _, err := nc.Write(ours); err != nil {
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
	c := start(nc, addr, *theirs, true, o)
	c.Encrypted = encrypted
	return c, nil
}

// Torrents is what an accepting side serves: torrents by info hash, and
// by mse.Req2 of their info hash, which is all an encrypted peer reveals.
type Torrents interface {
	Has(metainfo.Hash) bool
	FindObfuscated(req2 [20]byte) (metainfo.Hash, bool)
}

// knownFunc serves the torrents known reports, to plain peers only.
type knownFunc func(metainfo.Hash) bool

func (k knownFunc) Has(h metainfo.Hash) bool                    { return k(h) }
func (knownFunc) FindObfuscated([20]byte) (metainfo.Hash, bool) { return metainfo.Hash{}, false }

// Accept performs the handshake on an incoming connection. known reports
// whether we serve a torrent; the peer speaks first, so we learn which
// torrent it wants before replying. Encrypted peers can't be served
// through Accept (their torrent can't be found from known): see
// AcceptFrom.
func Accept(nc net.Conn, known func(metainfo.Hash) bool, opts Options) (*Conn, error) {
	return AcceptFrom(nc, knownFunc(known), opts)
}

// plainPrefix is how every plain BitTorrent handshake starts. An MSE peer
// starts with its random public key instead.
const plainPrefix = "\x13" + protocolID

// AcceptFrom performs the handshake on an incoming connection, plain or
// encrypted as opts.Encryption allows.
func AcceptFrom(nc net.Conn, torrents Torrents, opts Options) (*Conn, error) {
	o := opts.withDefaults()
	addr := addrPortOf(nc.RemoteAddr())
	nc.SetDeadline(time.Now().Add(o.HandshakeTimeout))
	fail := func(err error) (*Conn, error) {
		nc.Close()
		return nil, fmt.Errorf("peer %s: %w", addr, err)
	}

	var prefix [len(plainPrefix)]byte
	if _, err := io.ReadFull(nc, prefix[:1]); err != nil {
		return fail(fmt.Errorf("reading handshake: %w", err))
	}
	// A plain handshake's first byte is 19; with encryption off, anything
	// else is rejected without waiting for more. It is most likely MSE.
	if prefix[0] != plainPrefix[0] && o.Encryption == EncryptionOff {
		return fail(fmt.Errorf("%w (%w: protocol length %d)", ErrEncryptedRefused, ErrBadHandshake, prefix[0]))
	}
	if _, err := io.ReadFull(nc, prefix[1:]); err != nil {
		return fail(fmt.Errorf("reading handshake: %w", err))
	}
	var (
		r         io.Reader = io.MultiReader(bytes.NewReader(prefix[:]), nc)
		encrypted bool
		skey      metainfo.Hash
	)
	switch {
	case string(prefix[:]) == plainPrefix:
		if o.Encryption == EncryptionRequire {
			return fail(ErrPlaintextRefused)
		}
	case o.Encryption == EncryptionOff:
		return fail(fmt.Errorf("%w (%w: not a BitTorrent handshake)", ErrEncryptedRefused, ErrBadHandshake))
	default:
		find := func(req2 [20]byte) ([20]byte, bool) { return torrents.FindObfuscated(req2) }
		mc, h, _, err := mse.Receive(nc, prefix[:], find, o.chooseMethod)
		if errors.Is(err, mse.ErrUnknownTorrent) {
			return fail(fmt.Errorf("%w (encrypted)", ErrUnknownTorrent))
		}
		if err != nil {
			return fail(err)
		}
		nc, r, skey, encrypted = mc, mc, h, mc.Encrypted()
	}
	theirs, err := ReadHandshake(r)
	if err != nil {
		return fail(err)
	}
	if skey != (metainfo.Hash{}) && theirs.InfoHash != skey {
		// The MSE handshake named one torrent and the BitTorrent one another.
		return fail(ErrInfoHashMismatch)
	}
	if !torrents.Has(theirs.InfoHash) {
		return fail(fmt.Errorf("%w %s", ErrUnknownTorrent, theirs.InfoHash))
	}
	if theirs.PeerID == o.PeerID {
		return fail(ErrSelfConnection)
	}
	if _, err := o.handshake(theirs.InfoHash).WriteTo(nc); err != nil {
		return fail(fmt.Errorf("sending handshake: %w", err))
	}
	nc.SetDeadline(time.Time{})
	c := start(nc, addr, *theirs, false, o)
	c.Encrypted = encrypted
	return c, nil
}

// chooseMethod answers an encrypted peer's crypto_provide: RC4 when
// offered, plaintext-after-the-handshake only when encryption is merely
// preferred.
func (o *Options) chooseMethod(provide uint32) uint32 {
	switch {
	case provide&mse.RC4 != 0:
		return mse.RC4
	case provide&mse.Plaintext != 0 && o.Encryption == EncryptionPrefer:
		return mse.Plaintext
	}
	return 0
}

func start(nc net.Conn, addr netip.AddrPort, theirs Handshake, outgoing bool, o Options) *Conn {
	ctx, cancel := context.WithCancel(context.Background())
	_, overUDP := nc.LocalAddr().(*net.UDPAddr)
	c := &Conn{
		UTP:      overUDP,
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
	// The idle deadline is re-armed before every socket read, not once per
	// message: time spent waiting on our own download limit (between
	// reads) must not count as the peer being silent (D46).
	var src io.Reader = countingReader{deadlineReader{c.nc, c.opts.IdleTimeout}, &c.bytesRead}
	src = limitedReader{src, c}
	r := bufio.NewReaderSize(src, 32*1024)
	for {
		m, err := ReadMessage(r)
		if err != nil {
			c.fail(c.readErr(err))
			return
		}
		if m == nil {
			continue // keep-alive: its read re-armed the deadline, all it's for
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

// deadlineReader gives every read idle time to wait for data.
type deadlineReader struct {
	nc   net.Conn
	idle time.Duration
}

func (d deadlineReader) Read(p []byte) (int, error) {
	d.nc.SetReadDeadline(time.Now().Add(d.idle))
	return d.nc.Read(p)
}

// limitedReader charges every read to the connection's rate limiters —
// the torrent's own, then the global one — pausing after reads while a
// limit is exceeded. A nil limiter is unlimited.
type limitedReader struct {
	r io.Reader
	c *Conn
}

func (lr limitedReader) Read(p []byte) (int, error) {
	n, err := lr.r.Read(p)
	if n > 0 {
		for _, lim := range [2]*ratelimit.Limiter{lr.c.torrentLim.Load(), lr.c.opts.ReadLimiter} {
			if werr := lim.WaitN(lr.c.ctx, n); werr != nil {
				if err == nil {
					err = werr // connection closing
				}
				break
			}
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
	switch a := a.(type) {
	case *net.TCPAddr:
		ap := a.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	case *net.UDPAddr: // uTP
		ap := a.AddrPort()
		return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	}
	ap, _ := netip.ParseAddrPort(a.String())
	return ap
}
