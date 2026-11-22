package utp

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"
)

// PacketConn is the UDP socket a Socket runs on: *net.UDPConn or a
// udpmux.Endpoint.
type PacketConn interface {
	ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error)
	WriteToUDPAddrPort(b []byte, addr netip.AddrPort) (int, error)
	LocalAddr() net.Addr
	Close() error
}

var (
	// ErrConnect: the peer didn't answer our SYN (no uTP there, or UDP is
	// blocked). Callers fall back to TCP on it.
	ErrConnect = errors.New("utp: no answer")
	// ErrReset: the peer reset the connection.
	ErrReset = errors.New("utp: connection reset by peer")
	// ErrTimeout: packets went unacknowledged for too long.
	ErrTimeout = errors.New("utp: connection timed out")
)

const (
	maxDatagram = 1232 // fits any path MTU ≥ 1280 (IPv6's minimum), tunnels included
	maxSACK     = 8    // selective ack bytes: 64 packets past the ack
	maxPayload  = maxDatagram - headerLen - 2 - maxSACK
	acceptQueue = 64
	tick        = 10 * time.Millisecond
)

// Config tunes a Socket; zero values are defaults.
type Config struct {
	ConnectTimeout time.Duration // SYN unanswered this long → ErrConnect; default 2s
	RecvBuffer     int           // per connection; default 1 MiB
	SendBuffer     int           // per connection; default 1 MiB
}

// Socket runs uTP connections over one UDP socket. It is a net.Listener
// for incoming ones and dials outgoing ones. Safe for concurrent use.
type Socket struct {
	pc  PacketConn
	cfg Config

	mu      sync.Mutex
	conns   map[connKey]*Conn
	closed  bool
	accept  chan *Conn
	done    chan struct{}
	wg      sync.WaitGroup
	started time.Time // for packet timestamps

	// dropOut, if set (tests), drops an outgoing datagram when it returns true.
	dropOut func(b []byte) bool
}

// connKey is how incoming packets find their connection: the sender and
// the connection ID it uses, which is our recvID.
type connKey struct {
	addr netip.AddrPort
	id   uint16
}

// NewSocket starts serving uTP on pc. Close closes pc too.
func NewSocket(pc PacketConn, cfg Config) *Socket {
	if cfg.ConnectTimeout <= 0 {
		cfg.ConnectTimeout = 2 * time.Second
	}
	if cfg.RecvBuffer <= 0 {
		cfg.RecvBuffer = 1 << 20
	}
	if cfg.SendBuffer <= 0 {
		cfg.SendBuffer = 1 << 20
	}
	s := &Socket{pc: pc, cfg: cfg, conns: map[connKey]*Conn{}, accept: make(chan *Conn, acceptQueue),
		done: make(chan struct{}), started: time.Now()}
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.readLoop() }()
	go func() { defer s.wg.Done(); s.tickLoop() }()
	return s
}

// now is the packet clock: microseconds since the socket started.
func (s *Socket) now() uint32 { return uint32(time.Since(s.started).Microseconds()) }

// Addr is the local UDP address (net.Listener).
func (s *Socket) Addr() net.Addr { return s.pc.LocalAddr() }

// Port is the local UDP port.
func (s *Socket) Port() uint16 {
	if a, ok := s.pc.LocalAddr().(*net.UDPAddr); ok {
		return uint16(a.Port)
	}
	return 0
}

// Accept waits for an incoming connection (net.Listener).
func (s *Socket) Accept() (net.Conn, error) {
	select {
	case c := <-s.accept:
		return c, nil
	case <-s.done:
		return nil, net.ErrClosed
	}
}

// Close resets every connection, stops the socket and closes pc.
func (s *Socket) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	conns := make([]*Conn, 0, len(s.conns))
	for _, c := range s.conns {
		conns = append(conns, c)
	}
	close(s.done)
	s.mu.Unlock()
	for _, c := range conns {
		c.abort(net.ErrClosed, true)
	}
	err := s.pc.Close()
	s.wg.Wait()
	return err
}

// Dial opens a connection to addr. It fails with ErrConnect if the SYN
// goes unanswered for ConnectTimeout.
func (s *Socket) Dial(ctx context.Context, addr netip.AddrPort) (*Conn, error) {
	addr = netip.AddrPortFrom(addr.Addr().Unmap(), addr.Port())
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, net.ErrClosed
	}
	var id uint16
	for {
		id = randomID()
		// Our recvID is id; the peer will key us by id+1 on its side.
		if s.conns[connKey{addr, id}] == nil {
			break
		}
	}
	c := newConn(s, addr, id, id+1, true)
	s.conns[connKey{addr, id}] = c
	s.mu.Unlock()

	c.mu.Lock()
	c.sendSyn()
	c.mu.Unlock()

	timer := time.NewTimer(s.cfg.ConnectTimeout)
	defer timer.Stop()
	select {
	case <-c.connected:
		c.mu.Lock()
		err := c.err
		c.mu.Unlock()
		if err != nil {
			return nil, fmt.Errorf("utp %s: %w", addr, err)
		}
		return c, nil
	case <-timer.C:
		c.abort(ErrConnect, false)
		return nil, fmt.Errorf("utp %s: %w", addr, ErrConnect)
	case <-ctx.Done():
		c.abort(ctx.Err(), false)
		return nil, ctx.Err()
	}
}

func randomID() uint16 {
	var b [2]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint16(b[:])
}

func (s *Socket) remove(c *Conn) {
	s.mu.Lock()
	if s.conns[connKey{c.remote, c.recvID}] == c {
		delete(s.conns, connKey{c.remote, c.recvID})
	}
	s.mu.Unlock()
}

// send writes one datagram.
func (s *Socket) send(b []byte, to netip.AddrPort) {
	if s.dropOut != nil && s.dropOut(b) {
		return
	}
	s.pc.WriteToUDPAddrPort(b, to)
}

func (s *Socket) readLoop() {
	buf := make([]byte, 64<<10)
	for {
		n, from, err := s.pc.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			select {
			case <-s.done:
				return
			default:
				continue
			}
		}
		h, payload, err := parse(buf[:n])
		if err != nil {
			continue
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		s.dispatch(h, payload, from)
	}
}

func (s *Socket) dispatch(h header, payload []byte, from netip.AddrPort) {
	if h.typ == stSyn {
		s.onSyn(h, from)
		return
	}
	s.mu.Lock()
	c := s.conns[connKey{from, h.connID}]
	s.mu.Unlock()
	if c != nil {
		c.handle(h, payload)
	}
	// Unknown connection: dropped. (Answering with a reset needs the
	// peer's receive ID, which a stray packet doesn't tell reliably.)
}

// onSyn accepts a new connection, or re-answers a SYN we already took
// (our STATE was lost).
func (s *Socket) onSyn(h header, from netip.AddrPort) {
	key := connKey{from, h.connID + 1}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	if c := s.conns[key]; c != nil {
		s.mu.Unlock()
		c.mu.Lock()
		if !c.gotData {
			c.sendState()
		}
		c.mu.Unlock()
		return
	}
	if len(s.accept) >= cap(s.accept) {
		s.mu.Unlock()
		return // backlog full: let it retry
	}
	c := newConn(s, from, h.connID+1, h.connID, false)
	c.ack = h.seq
	c.peerWnd = int(h.wnd)
	c.lastTsDiff = s.now() - h.ts
	s.conns[key] = c
	s.accept <- c // room checked above, under mu
	s.mu.Unlock()
	c.mu.Lock()
	c.sendState()
	c.mu.Unlock()
}

// tickLoop drives timers: retransmissions, delayed acks, keep-alives,
// lingering closes.
func (s *Socket) tickLoop() {
	t := time.NewTicker(tick)
	defer t.Stop()
	var conns []*Conn
	for {
		select {
		case <-s.done:
			return
		case now := <-t.C:
			s.mu.Lock()
			conns = conns[:0]
			for _, c := range s.conns {
				conns = append(conns, c)
			}
			s.mu.Unlock()
			for _, c := range conns {
				c.tick(now)
			}
		}
	}
}

// Dialer dials over the Socket of the address's family (peer.UTPDialer).
// A nil Socket means no uTP for that family.
type Dialer struct{ V4, V6 *Socket }

// Dial connects to addr over V4 or V6.
func (d *Dialer) Dial(ctx context.Context, addr netip.AddrPort) (net.Conn, error) {
	s := d.V4
	if addr.Addr().Unmap().Is6() {
		s = d.V6
	}
	if s == nil {
		return nil, fmt.Errorf("utp %s: %w (no uTP socket for this address family)", addr, ErrConnect)
	}
	c, err := s.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	return c, nil
}
