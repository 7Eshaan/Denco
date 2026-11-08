package torrent

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
)

// maxPendingHandshakes bounds concurrent incoming handshakes so a flood of
// connections can't create unbounded goroutines.
const maxPendingHandshakes = 64

// Listener accepts incoming peer connections on one TCP port for all
// torrents. The peer speaks first, so after reading its handshake we know
// which torrent it wants and hand the connection to that Session.
type Listener struct {
	ln   net.Listener
	opts peer.Options
	log  *slog.Logger

	mu       sync.Mutex
	sessions map[metainfo.Hash]*Session
	closed   bool

	sem chan struct{}
	wg  sync.WaitGroup
}

// Listen opens a TCP listener on addr (e.g. ":6881"). opts must use the same
// peer ID as the sessions it serves.
func Listen(addr string, opts peer.Options, log *slog.Logger) (*Listener, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	l := &Listener{
		ln:       ln,
		opts:     opts,
		log:      log,
		sessions: make(map[metainfo.Hash]*Session),
		sem:      make(chan struct{}, maxPendingHandshakes),
	}
	l.wg.Add(1)
	go l.serve()
	return l, nil
}

// Port returns the port actually listened on (useful with ":0").
func (l *Listener) Port() uint16 {
	if a, ok := l.ln.Addr().(*net.TCPAddr); ok {
		return uint16(a.Port)
	}
	return 0
}

// Addr returns the listening address.
func (l *Listener) Addr() netip.AddrPort {
	ap, _ := netip.ParseAddrPort(l.ln.Addr().String())
	return ap
}

// Add makes s reachable by incoming peers.
func (l *Listener) Add(s *Session) {
	l.mu.Lock()
	l.sessions[s.mi.InfoHash] = s
	l.mu.Unlock()
}

// Remove stops routing incoming peers to s.
func (l *Listener) Remove(s *Session) {
	l.mu.Lock()
	if l.sessions[s.mi.InfoHash] == s {
		delete(l.sessions, s.mi.InfoHash)
	}
	l.mu.Unlock()
}

func (l *Listener) lookup(h metainfo.Hash) *Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.sessions[h]
}

// Close stops accepting and waits for in-flight handshakes to finish.
func (l *Listener) Close() error {
	l.mu.Lock()
	l.closed = true
	l.mu.Unlock()
	err := l.ln.Close()
	l.wg.Wait()
	return err
}

func (l *Listener) isClosed() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.closed
}

func (l *Listener) serve() {
	defer l.wg.Done()
	for {
		nc, err := l.ln.Accept()
		if err != nil {
			if l.isClosed() || errors.Is(err, net.ErrClosed) {
				return
			}
			l.log.Warn("accept failed", "err", err)
			time.Sleep(100 * time.Millisecond) // e.g. out of file descriptors
			continue
		}
		select {
		case l.sem <- struct{}{}:
		default:
			nc.Close() // too many handshakes in flight
			continue
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			defer func() { <-l.sem }()
			l.handshake(nc)
		}()
	}
}

func (l *Listener) handshake(nc net.Conn) {
	c, err := peer.Accept(nc, func(h metainfo.Hash) bool { return l.lookup(h) != nil }, l.opts)
	if err != nil {
		l.log.Debug("incoming handshake failed", "err", err)
		return
	}
	s := l.lookup(c.Peer.InfoHash)
	if s == nil { // removed while we were handshaking
		c.Close()
		return
	}
	s.addIncoming(c)
}
