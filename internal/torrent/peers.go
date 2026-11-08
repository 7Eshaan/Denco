package torrent

import (
	"context"
	"net/netip"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// peerState is everything the session knows about one connected peer.
// Owned by the loop goroutine.
type peerState struct {
	addr netip.AddrPort
	conn *peer.Conn
	has  *bitfield.Bitfield

	peerChoking    bool // they choke us (initially true)
	amInterested   bool // we told them we're interested
	amChoking      bool // we choke them (initially true)
	peerInterested bool // they're interested in us

	// Download side.
	pending      map[peer.Block]bool // our outstanding requests to them
	lastProgress time.Time           // last useful block, or when requests started
	downloaded   int64               // payload bytes received from them
	rate         rate                // their upload rate to us

	// Upload side (upload.go).
	served   map[peer.Block]bool // their requests we've accepted and not yet sent
	uploadQ  chan peer.Block     // blocks for the uploader goroutine to read
	uploaded int64               // payload bytes sent to them
	upRate   rate                // our upload rate to them

	// Extension protocol (extensions.go): the peer's message IDs for
	// ut_metadata and ut_pex, 0 if it doesn't support them (or hasn't said
	// yet), and the listening port from its extension handshake.
	extMetadata int
	extPEX      int
	listenPort  int

	// Peer exchange (pex.go).
	pexSent   map[netip.AddrPort]bool // addresses we have told it about
	lastPEXIn time.Time               // when it last sent us a PEX message
	nextPEX   time.Time               // when we may send it the next one

	connectedAt time.Time
}

// addPeer registers a freshly handshaken connection.
func (s *Session) addPeer(ctx context.Context, c *peer.Conn) {
	p := &peerState{
		addr:        c.Remote,
		conn:        c,
		has:         bitfield.New(s.info.NumPieces()),
		peerChoking: true,
		amChoking:   true,
		pending:     make(map[peer.Block]bool),
		served:      make(map[peer.Block]bool),
		pexSent:     make(map[netip.AddrPort]bool),
		uploadQ:     make(chan peer.Block, maxQueuedUploads),
		connectedAt: time.Now(),
	}
	s.peers[p.addr] = p
	if c.Outgoing {
		s.swarm.connected(p.addr)
	}
	s.log.Debug("peer connected", "peer", p.addr, "client", peerid.ClientName(c.Peer.PeerID))

	// Our bitfield must be the first message after the handshake.
	if have := s.m.Have(); have.Count() > 0 {
		s.sendTo(p, peer.BitfieldMsg(have.Bytes()))
	}
	s.sendExtHandshake(p)

	s.startUploader(ctx, p)
	// Forward this peer's messages into the loop.
	s.goHelper(func() {
		for m := range c.Incoming() {
			select {
			case s.events <- msgEvent{p, m}:
			case <-ctx.Done():
				return
			}
		}
		s.send(ctx, closedEvent{p})
	})
}

// onPeerClosed cleans up after a disconnect.
func (s *Session) onPeerClosed(p *peerState) {
	if s.peers[p.addr] != p {
		return
	}
	delete(s.peers, p.addr)
	s.m.PeerRequestsDropped(p.addr)
	s.m.PeerGoneBitfield(p.has)
	// Incoming peers use a random source port: nothing to redial.
	if p.conn.Outgoing {
		s.swarm.disconnected(p.addr, p.downloaded > 0)
	}
	if s.choke.optimistic == p.addr {
		s.choke.optimistic = netip.AddrPort{}
	}
	if !p.amChoking {
		s.rechoke(time.Now(), false) // hand its slot to someone else now
	}
	s.log.Debug("peer disconnected", "peer", p.addr, "err", p.conn.Err())
}

// sendTo queues m for p without blocking; a peer that can't keep up is dropped.
func (s *Session) sendTo(p *peerState, m *peer.Message) {
	if !p.conn.TrySend(m) {
		p.conn.Close()
	}
}

// drop disconnects a misbehaving peer.
func (s *Session) drop(p *peerState, reason string) {
	s.log.Debug("dropping peer", "peer", p.addr, "reason", reason)
	p.conn.Close()
}

func (s *Session) onMessage(p *peerState, m *peer.Message) {
	switch m.ID {
	case peer.MsgChoke:
		p.peerChoking = true
		// Without the fast extension, a choke discards all our requests.
		s.m.PeerRequestsDropped(p.addr)
		clear(p.pending)

	case peer.MsgUnchoke:
		p.peerChoking = false
		p.lastProgress = time.Now()
		s.fillRequests(p)

	case peer.MsgInterested:
		p.peerInterested = true
		s.onInterestChanged(p)
	case peer.MsgNotInterested:
		p.peerInterested = false
		s.onInterestChanged(p)

	case peer.MsgHave:
		idx, err := peer.ParseHave(m)
		if err != nil || int(idx) >= s.info.NumPieces() {
			s.drop(p, "bad have")
			return
		}
		if !p.has.Has(int(idx)) {
			p.has.Set(int(idx))
			s.m.PeerHave(int(idx))
		}
		if s.bothSeeds(p) {
			return
		}
		s.updateInterest(p)

	case peer.MsgBitfield:
		bf, err := bitfield.FromBytes(m.Payload, s.info.NumPieces())
		if err != nil {
			s.drop(p, "bad bitfield")
			return
		}
		s.m.PeerGoneBitfield(p.has) // in case of a (non-standard) repeat
		p.has = bf
		s.m.PeerBitfield(bf)
		if s.bothSeeds(p) {
			return
		}
		s.updateInterest(p)

	case peer.MsgPiece:
		s.onPiece(p, m)

	case peer.MsgRequest:
		s.onRequest(p, m)
	case peer.MsgCancel:
		s.onCancel(p, m)

	case peer.MsgExtended:
		s.onExtended(p, m)

	default:
		// Port and unknown messages: ignored.
	}
}

func (s *Session) onPiece(p *peerState, m *peer.Message) {
	index, begin, data, err := peer.ParsePiece(m)
	if err != nil {
		s.drop(p, "bad piece message")
		return
	}
	delete(p.pending, peer.Block{Index: index, Begin: begin, Length: uint32(len(data))})

	r := s.m.BlockReceived(p.addr, index, begin, data)
	if r.Accepted {
		n := int64(len(data))
		p.downloaded += n
		p.rate.add(n)
		s.downRate.add(n)
		s.downloaded.Add(n)
		p.lastProgress = time.Now()
	}
	// Endgame: tell the other peers we no longer need this block.
	for _, other := range r.Cancel {
		if op := s.peers[other]; op != nil {
			b := peer.Block{Index: index, Begin: begin, Length: uint32(len(data))}
			delete(op.pending, b)
			s.sendTo(op, peer.Cancel(b))
		}
	}
	if r.Complete {
		s.verifyAndWrite(s.ctx(), int(index), r.Data, r.Contributors)
	}
	s.fillRequests(p)
}

// bothSeeds drops p if we and it are both complete: neither can give the
// other anything, and the slot is better used for a leecher.
func (s *Session) bothSeeds(p *peerState) bool {
	if s.m.Complete() && p.has.Full() {
		s.drop(p, "both seeds")
		return true
	}
	return false
}

// updateInterest tells p whether we want anything it has.
func (s *Session) updateInterest(p *peerState) {
	want := s.m.Interesting(p.has)
	if want == p.amInterested {
		if want {
			s.fillRequests(p)
		}
		return
	}
	p.amInterested = want
	if want {
		s.sendTo(p, peer.Interested())
		s.fillRequests(p)
	} else {
		s.sendTo(p, peer.NotInterested())
	}
}

// fillRequests tops up p's request pipeline.
func (s *Session) fillRequests(p *peerState) {
	if p.peerChoking || !p.amInterested {
		return
	}
	room := s.cfg.RequestQueue - len(p.pending)
	if room <= 0 {
		return
	}
	if len(p.pending) == 0 {
		p.lastProgress = time.Now() // start the timeout clock
	}
	for _, b := range s.m.NextRequests(p.addr, p.has, room) {
		p.pending[b] = true
		s.sendTo(p, peer.Request(b))
	}
}

// checkTimeouts drops peers that stopped answering our requests ("snubbed"),
// freeing their blocks for other peers.
func (s *Session) checkTimeouts() {
	now := time.Now()
	for _, p := range s.peers {
		if len(p.pending) > 0 && now.Sub(p.lastProgress) > s.cfg.RequestTimeout {
			s.drop(p, "request timeout")
		}
	}
}
