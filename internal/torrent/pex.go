package torrent

import (
	"net/netip"
	"time"

	"github.com/7Eshaan/Denco/internal/peer"
)

// Peer exchange (BEP 11, DECISIONS D41). A peer that supports ut_pex gets
// its first message right after its extension handshake (so even a magnet
// user who stays only a few seconds hears about the swarm), then at most one
// per PEXInterval, each listing which peers we connected to and lost since
// the previous one. Peers it tells us about join the swarm.
//
// Rules:
//   - never for private torrents (BEP 27: peers only via the tracker);
//   - we advertise only peers that accept connections at a known address:
//     ones we dialed, and incoming ones that told us their listening port;
//   - per message at most 50 added and 50 dropped (peer.MaxPEXPeers);
//   - from one peer we accept one message per half interval and at most
//     50 new addresses per message, and PEX can grow the swarm only up to
//     maxKnownFromPEX addresses.

// maxKnownFromPEX caps the swarm size PEX can push us to (tracker peers
// aren't limited; they come from a source we chose).
const maxKnownFromPEX = 2000

// pexEnabled reports whether peer exchange is allowed for this torrent.
func (s *Session) pexEnabled() bool {
	return s.cfg.PEX && s.cfg.PeerOpts.Extensions && !s.info.Private
}

// listenAddr is where other peers can connect to p, if we know it.
func (p *peerState) listenAddr() (netip.AddrPort, bool) {
	if p.conn.Outgoing {
		return p.addr, true
	}
	if p.listenPort > 0 {
		return netip.AddrPortFrom(p.addr.Addr(), uint16(p.listenPort)), true
	}
	return netip.AddrPort{}, false
}

// maybeSendPEX sends PEX messages to the peers that are due one (loop
// goroutine, every tick).
func (s *Session) maybeSendPEX(now time.Time) {
	if !s.pexEnabled() {
		return
	}
	var current map[netip.AddrPort]byte
	for _, p := range s.peers {
		if p.extPEX == 0 || now.Before(p.nextPEX) {
			continue
		}
		if current == nil {
			current = s.pexPeers()
		}
		if s.sendPEX(p, current) {
			p.nextPEX = now.Add(s.cfg.PEXInterval)
		} // nothing new to say: try again next tick, so news isn't held back
	}
}

// pexPeers maps every advertisable connected peer to its PEX flags.
func (s *Session) pexPeers() map[netip.AddrPort]byte {
	current := map[netip.AddrPort]byte{}
	for _, p := range s.peers {
		if a, ok := p.listenAddr(); ok {
			var f byte
			if p.has.Full() {
				f |= peer.PEXSeed
			}
			if p.conn.Outgoing {
				f |= peer.PEXReachable // we reached it, so others can too
			}
			current[a] = f
		}
	}
	return current
}

// sendPEX tells p what changed since the last message we sent it, and
// reports whether there was anything to send.
func (s *Session) sendPEX(p *peerState, current map[netip.AddrPort]byte) bool {
	self, _ := p.listenAddr()
	var msg peer.PEX
	for a, f := range current {
		if len(msg.Added) >= peer.MaxPEXPeers {
			break // the rest go in later rounds
		}
		if a == self || a == p.addr || p.pexSent[a] {
			continue
		}
		msg.Added = append(msg.Added, a)
		msg.AddedFlags = append(msg.AddedFlags, f)
	}
	for a := range p.pexSent {
		if len(msg.Dropped) >= peer.MaxPEXPeers {
			break
		}
		if _, ok := current[a]; !ok {
			msg.Dropped = append(msg.Dropped, a)
		}
	}
	if msg.Empty() {
		return false
	}
	for _, a := range msg.Added {
		p.pexSent[a] = true
	}
	for _, a := range msg.Dropped {
		delete(p.pexSent, a)
	}
	s.sendTo(p, peer.Extended(uint8(p.extPEX), msg.Encode()))
	return true
}

// onPEX adds the peers p tells us about to the swarm.
func (s *Session) onPEX(p *peerState, body []byte) {
	if !s.pexEnabled() {
		return // we didn't advertise it; ignore rather than trust it
	}
	now := time.Now()
	if !p.lastPEXIn.IsZero() && now.Sub(p.lastPEXIn) < s.cfg.PEXInterval/2 {
		return // too often: ignored, not punished (clocks and timers vary)
	}
	p.lastPEXIn = now
	msg, err := peer.ParsePEX(body)
	if err != nil {
		s.drop(p, "bad ut_pex message")
		return
	}
	seeding := s.m.Complete()
	var addrs []netip.AddrPort
	for i, a := range msg.Added {
		if len(addrs) >= peer.MaxPEXPeers {
			break
		}
		if seeding && msg.Flags(i)&peer.PEXSeed != 0 {
			continue // a seed has nothing to give a seed
		}
		addrs = append(addrs, a)
	}
	s.swarm.addSome(addrs, maxKnownFromPEX)
	if len(addrs) > 0 {
		s.log.Debug("peers from PEX", "peer", p.addr, "added", len(addrs))
	}
}
