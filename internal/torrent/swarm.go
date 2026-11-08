package torrent

import (
	"context"
	"net/netip"
	"time"

	"github.com/7Eshaan/Denco/internal/peer"
)

// maxBackoff caps how long we wait before retrying a peer.
const maxBackoff = 30 * time.Minute

// swarm remembers every peer address we've heard of and decides whom to
// dial next. Owned by the loop goroutine.
type swarm struct {
	base  time.Duration
	addrs map[netip.AddrPort]*candidate
}

type candidate struct {
	state    candState
	failures int       // consecutive failed or useless connections
	nextTry  time.Time // don't dial before this
	strikes  int       // hash failures this peer contributed to
}

type candState int

const (
	idle candState = iota
	dialing
	connected
	banned
)

func newSwarm(base time.Duration) *swarm {
	return &swarm{base: base, addrs: make(map[netip.AddrPort]*candidate)}
}

// add records addresses (from trackers, magnet links, ...). Known ones are
// left alone so their backoff state survives repeated announces.
func (w *swarm) add(addrs []netip.AddrPort) {
	for _, a := range addrs {
		a = netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
		if !a.IsValid() || a.Port() == 0 || a.Addr().IsUnspecified() {
			continue
		}
		if _, ok := w.addrs[a]; !ok {
			w.addrs[a] = &candidate{}
		}
	}
}

// next returns up to n addresses ready to dial and marks them dialing.
func (w *swarm) next(n int, now time.Time) []netip.AddrPort {
	var out []netip.AddrPort
	for a, c := range w.addrs { // map order: a cheap random-ish spread
		if len(out) >= n {
			break
		}
		if c.state == idle && !now.Before(c.nextTry) {
			c.state = dialing
			out = append(out, a)
		}
	}
	return out
}

func (w *swarm) get(a netip.AddrPort) *candidate {
	c, ok := w.addrs[a]
	if !ok {
		c = &candidate{}
		w.addrs[a] = c
	}
	return c
}

// dialFailed schedules a retry with exponential backoff.
func (w *swarm) dialFailed(a netip.AddrPort, now time.Time) {
	c := w.get(a)
	if c.state == banned {
		return
	}
	c.state = idle
	c.failures++
	c.nextTry = now.Add(w.backoff(c.failures))
}

func (w *swarm) connected(a netip.AddrPort) {
	if c := w.get(a); c.state != banned {
		c.state = connected
	}
}

// disconnected makes a peer dialable again. A peer that gave us data is
// retried soon with its failure count reset; a useless one backs off.
func (w *swarm) disconnected(a netip.AddrPort, useful bool) {
	c := w.get(a)
	if c.state == banned {
		return
	}
	c.state = idle
	if useful {
		c.failures = 0
		c.nextTry = time.Now().Add(w.base)
	} else {
		c.failures++
		c.nextTry = time.Now().Add(w.backoff(c.failures))
	}
}

func (w *swarm) strike(a netip.AddrPort) int {
	c := w.get(a)
	c.strikes++
	return c.strikes
}

func (w *swarm) ban(a netip.AddrPort) { w.get(a).state = banned }

func (w *swarm) isBanned(a netip.AddrPort) bool {
	c, ok := w.addrs[a]
	return ok && c.state == banned
}

func (w *swarm) backoff(failures int) time.Duration {
	d := w.base
	for i := 1; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	return min(d, maxBackoff)
}

func (w *swarm) count(st candState) int {
	n := 0
	for _, c := range w.addrs {
		if c.state == st {
			n++
		}
	}
	return n
}

// --- dialing (session side) --------------------------------------------------------

// maintain opens new connections while we're below the peer limit.
// A finished download stops dialing (seeding connections: Phase 6).
func (s *Session) maintain(ctx context.Context) {
	if s.m.Complete() || ctx.Err() != nil {
		return
	}
	dialing := s.swarm.count(dialing)
	room := min(s.cfg.MaxPeers-len(s.peers)-dialing, s.cfg.DialParallel-dialing)
	if room <= 0 {
		return
	}
	for _, addr := range s.swarm.next(room, time.Now()) {
		s.goHelper(func() {
			c, err := peer.Dial(ctx, addr, s.mi.InfoHash, s.cfg.PeerOpts)
			s.send(ctx, dialResult{addr: addr, conn: c, err: err})
			if ctx.Err() != nil && c != nil {
				c.Close() // shutdown raced with a successful dial
			}
		})
	}
}

func (s *Session) onDialResult(ctx context.Context, r dialResult) {
	if r.err != nil {
		s.log.Debug("dial failed", "peer", r.addr, "err", r.err)
		s.swarm.dialFailed(r.addr, time.Now())
		return
	}
	if s.swarm.isBanned(r.addr) || s.peers[r.addr] != nil || s.m.Complete() {
		r.conn.Close()
		if !s.swarm.isBanned(r.addr) && s.peers[r.addr] == nil {
			s.swarm.disconnected(r.addr, false)
		}
		return
	}
	s.addPeer(ctx, r.conn)
}
