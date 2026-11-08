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

	// Bans and strikes are per IP: a peer that connects to us comes from a
	// random port, so banning one address:port wouldn't keep it out.
	// exact (tests only) bans the exact address:port instead, because every
	// test peer shares 127.0.0.1.
	strikes map[netip.AddrPort]int
	banned  map[netip.AddrPort]bool
	exact   bool
}

type candidate struct {
	state    candState
	failures int       // consecutive failed or useless connections
	nextTry  time.Time // don't dial before this
}

type candState int

const (
	idle candState = iota
	dialing
	connected
)

func newSwarm(base time.Duration) *swarm {
	return &swarm{
		base:    base,
		addrs:   make(map[netip.AddrPort]*candidate),
		strikes: make(map[netip.AddrPort]int),
		banned:  make(map[netip.AddrPort]bool),
	}
}

// banKey is the IP (port zeroed), or the full address in exact mode.
func (w *swarm) banKey(a netip.AddrPort) netip.AddrPort {
	if w.exact {
		return a
	}
	return netip.AddrPortFrom(a.Addr(), 0)
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

// addSome is add for less trusted sources (PEX): new addresses are only
// taken while the swarm knows fewer than limit, so peers can't make it
// grow without bound.
func (w *swarm) addSome(addrs []netip.AddrPort, limit int) {
	for _, a := range addrs {
		if len(w.addrs) >= limit {
			return
		}
		w.add([]netip.AddrPort{a})
	}
}

// next returns up to n addresses ready to dial and marks them dialing.
func (w *swarm) next(n int, now time.Time) []netip.AddrPort {
	var out []netip.AddrPort
	for a, c := range w.addrs { // map order: a cheap random-ish spread
		if len(out) >= n {
			break
		}
		if c.state == idle && !now.Before(c.nextTry) && !w.isBanned(a) {
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
	c.state = idle
	c.failures++
	c.nextTry = now.Add(w.backoff(c.failures))
}

func (w *swarm) connected(a netip.AddrPort) { w.get(a).state = connected }

// disconnected makes a peer dialable again. A peer that gave us data is
// retried soon with its failure count reset; a useless one backs off.
func (w *swarm) disconnected(a netip.AddrPort, useful bool) {
	c := w.get(a)
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
	w.strikes[w.banKey(a)]++
	return w.strikes[w.banKey(a)]
}

func (w *swarm) ban(a netip.AddrPort) { w.banned[w.banKey(a)] = true }

func (w *swarm) isBanned(a netip.AddrPort) bool { return w.banned[w.banKey(a)] }

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

// maintain opens new connections while we're below the peer limit. Seeds
// keep dialing too (peers behind NAT can't connect to us), but connections
// to other seeds are dropped as soon as we see their bitfield.
func (s *Session) maintain(ctx context.Context) {
	if ctx.Err() != nil || (s.m.Complete() && s.cfg.NoSeed) {
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
	if s.swarm.isBanned(r.addr) || s.peers[r.addr] != nil || len(s.peers) >= s.cfg.MaxPeers {
		r.conn.Close()
		if s.peers[r.addr] == nil {
			s.swarm.disconnected(r.addr, false)
		}
		return
	}
	s.addPeer(ctx, r.conn)
}
