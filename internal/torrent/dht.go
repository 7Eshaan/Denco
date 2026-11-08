package torrent

import (
	"context"
	"net/netip"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
)

// PeerFinder is the DHT as a session sees it (*dht.Server implements it;
// tests use fakes).
type PeerFinder interface {
	// GetPeers looks up peers for hash and, with port > 0, announces that
	// we accept peers on that TCP port.
	GetPeers(ctx context.Context, hash metainfo.Hash, port uint16) ([]netip.AddrPort, error)
	// AddNode is a hint that a DHT node may run at addr (a PORT message).
	AddNode(addr netip.AddrPort)
	// Port is the DHT's UDP port, told to peers in PORT messages.
	Port() uint16
}

// dhtLookupTimeout bounds one lookup + announce.
const dhtLookupTimeout = 2 * time.Minute

// dhtEnabled: a DHT is configured and the torrent isn't private (BEP 27).
func (s *Session) dhtEnabled() bool { return s.cfg.DHT != nil && !s.info.Private }

// dhtLoop asks the DHT for peers (announcing us) at the start and every
// DHTInterval. A lookup that finds no one is retried sooner, backing off
// from DHTRetry — at daemon start the routing table is often still empty.
func (s *Session) dhtLoop(ctx context.Context) {
	failures := 0
	for {
		lctx, cancel := context.WithTimeout(ctx, dhtLookupTimeout)
		peers, err := s.cfg.DHT.GetPeers(lctx, s.mi.InfoHash, s.cfg.Port)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			s.log.Debug("dht lookup failed", "err", err)
		}
		s.send(ctx, dhtEvent{peers: peers})
		wait := s.cfg.DHTInterval
		if len(peers) == 0 {
			failures++
			wait = min(retryDelay(s.cfg.DHTRetry, failures), s.cfg.DHTInterval)
		} else {
			failures = 0
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// sendDHTPort tells a peer that also runs a DHT node where ours listens
// (BEP 5 PORT message), so it can add us to its routing table.
func (s *Session) sendDHTPort(p *peerState) {
	if s.dhtEnabled() && p.conn.Peer.SupportsDHT() {
		s.sendTo(p, peer.Port(s.cfg.DHT.Port()))
	}
}

// onDHTPort passes a peer's DHT port to our node, which pings it.
func (s *Session) onDHTPort(p *peerState, m *peer.Message) {
	port, err := peer.ParsePort(m)
	if err != nil {
		s.drop(p, "bad port message")
		return
	}
	if s.dhtEnabled() && port != 0 {
		s.cfg.DHT.AddNode(netip.AddrPortFrom(p.addr.Addr(), port))
	}
}
