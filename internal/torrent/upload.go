package torrent

import (
	"context"
	"math/rand/v2"
	"net/netip"
	"sort"
	"time"

	"github.com/7Eshaan/Denco/internal/peer"
)

const (
	// maxQueuedUploads caps how many of a peer's requests we hold at once,
	// and is what we advertise as "reqq". libtorrent keeps up to 500
	// requests outstanding per peer whatever reqq says, and never
	// re-requests one we silently ignore, so its download stalls (D69).
	// 2000 is libtorrent's own limit; a peer going past it is dropped.
	maxQueuedUploads = 2000
	// maxRequestLen is the largest block we serve. 16 KiB is the norm; a
	// little leeway avoids dropping slightly unusual clients.
	maxRequestLen = 2 * peer.BlockSize
)

// --- serving requests ----------------------------------------------------------

// startUploader runs a goroutine that performs disk reads for p's requests,
// so the loop never waits on the disk. The loop hands it one block at a
// time (feedUploader) and decides afterwards whether the block is still
// wanted (the peer may have cancelled or been choked). Before each read it
// waits for room in the connection's send queue, then for the upload
// limit, so a slow or throttled peer slows the reads down instead of
// overflowing the queue and getting disconnected (D69).
func (s *Session) startUploader(ctx context.Context, p *peerState) {
	pctx, cancel := context.WithCancel(ctx) // ends when the peer goes away
	s.goHelper(func() {
		select {
		case <-p.conn.Done():
		case <-pctx.Done():
		}
		cancel()
	})
	s.goHelper(func() {
		defer cancel()
		for {
			select {
			case b := <-p.uploadQ:
				// Room to send it, then the torrent's own limit, then the
				// global one (D59).
				if p.conn.WaitRoom(pctx) != nil ||
					s.cfg.TorrentUpLimiter.WaitN(pctx, int(b.Length)) != nil ||
					s.cfg.UploadLimiter.WaitN(pctx, int(b.Length)) != nil {
					return // peer or session gone
				}
				data := make([]byte, b.Length)
				off := int64(b.Index)*s.info.PieceLength + int64(b.Begin)
				_, err := s.st.ReadAt(data, off)
				select {
				case s.events <- uploadRead{p: p, b: b, data: data, err: err}:
				case <-ctx.Done():
					return
				}
			case <-p.conn.Done():
				return
			case <-ctx.Done():
				return
			}
		}
	})
}

func (s *Session) onRequest(p *peerState, m *peer.Message) {
	b, err := peer.ParseBlock(m)
	if err != nil {
		s.drop(p, "bad request")
		return
	}
	idx := int(b.Index)
	switch {
	case p.amChoking:
		return // allowed by BEP 3: requests from choked peers are discarded
	case idx >= s.info.NumPieces():
		s.drop(p, "request for nonexistent piece")
		return
	case b.Length == 0 || b.Length > maxRequestLen ||
		int64(b.Begin)+int64(b.Length) > s.info.PieceSize(idx):
		s.drop(p, "bad request range")
		return
	case !s.m.HasPiece(idx), p.served[b]:
		return // don't have it yet / already queued: one answer covers both
	case len(p.served) >= maxQueuedUploads:
		// Ignoring it would leave the peer waiting for it forever (D69).
		s.drop(p, "too many queued requests")
		return
	}
	p.served[b] = true
	p.upOrder = append(p.upOrder, b)
	if len(p.upOrder) > 2*maxQueuedUploads {
		p.compactUpOrder() // many cancelled entries: bound the memory
	}
	s.feedUploader(p)
}

func (s *Session) onCancel(p *peerState, m *peer.Message) {
	if b, err := peer.ParseBlock(m); err == nil {
		// Its upOrder entry is skipped later; if the read is in flight,
		// onUploadRead doesn't send it.
		delete(p.served, b)
	}
}

// feedUploader hands p's uploader the oldest request still wanted, unless
// it is busy with one already. Handing over one block at a time keeps
// cancelled requests out of any channel: they cost nothing (D69).
func (s *Session) feedUploader(p *peerState) {
	if p.upBusy {
		return
	}
	for len(p.upOrder) > 0 {
		b := p.upOrder[0]
		p.upOrder = p.upOrder[1:]
		if !p.served[b] {
			continue // cancelled, or a duplicate entry already answered
		}
		select {
		case p.uploadQ <- b:
			p.upBusy = true
		default: // can't happen: the uploader takes one block at a time
			s.drop(p, "uploader out of step")
		}
		return
	}
	p.upOrder = nil // let the emptied backing array go
}

// compactUpOrder drops cancelled and duplicate entries, keeping the order.
func (p *peerState) compactUpOrder() {
	seen := make(map[peer.Block]bool, len(p.served))
	kept := make([]peer.Block, 0, len(p.served))
	for _, b := range p.upOrder {
		if p.served[b] && !seen[b] {
			seen[b] = true
			kept = append(kept, b)
		}
	}
	p.upOrder = kept
}

func (s *Session) onUploadRead(e uploadRead) {
	p := e.p
	if s.peers[p.addr] != p {
		return // peer gone
	}
	p.upBusy = false
	if p.served[e.b] { // else cancelled, or the peer was choked meanwhile
		delete(p.served, e.b)
		if e.err != nil {
			s.log.Warn("reading block for upload failed", "piece", e.b.Index, "err", e.err)
			s.drop(p, "local read error")
			return
		}
		// The uploader waited for room before reading, so this only fails
		// if the connection is closing.
		if !p.conn.TrySend(peer.Piece(e.b.Index, e.b.Begin, e.data)) {
			p.conn.Close()
			return
		}
		n := int64(len(e.data))
		p.uploaded += n
		p.upRate.add(n)
		s.upRate.add(n)
		s.uploaded.Add(n)
	}
	s.feedUploader(p)
}

// --- choking ---------------------------------------------------------------------

// chokeState is the loop-owned choker bookkeeping.
type chokeState struct {
	paused         bool // uploads paused as of the last rechoke
	nextRechoke    time.Time
	nextOptimistic time.Time
	optimistic     netip.AddrPort // current optimistic unchoke (zero = none)
}

// setChoke changes our choke state toward p, telling it only on change.
// Choking discards the peer's queued requests (BEP 3, no fast extension).
func (s *Session) setChoke(p *peerState, choke bool) {
	if p.amChoking == choke {
		return
	}
	p.amChoking = choke
	if choke {
		clear(p.served)
		p.upOrder = nil
		s.sendTo(p, peer.Choke())
	} else {
		s.sendTo(p, peer.Unchoke())
	}
}

func (s *Session) unchokedCount() int {
	n := 0
	for _, p := range s.peers {
		if !p.amChoking {
			n++
		}
	}
	return n
}

// onInterestChanged reacts to interested / not-interested without waiting
// for the next rechoke: a newly interested peer gets a free slot at once.
func (s *Session) onInterestChanged(p *peerState) {
	if !p.peerInterested {
		s.setChoke(p, true)
		return
	}
	if !s.uploadsPaused.Load() && p.amChoking && s.unchokedCount() < s.cfg.UploadSlots+1 {
		s.setChoke(p, false)
	}
}

// maybeRechoke runs the periodic choker (loop goroutine, every tick).
func (s *Session) maybeRechoke(now time.Time) {
	if now.Before(s.choke.nextRechoke) {
		return
	}
	rotate := !now.Before(s.choke.nextOptimistic)
	s.rechoke(now, rotate)
}

// rechoke is the tit-for-tat choker: unchoke the UploadSlots interested peers
// that give us the most (while downloading: their upload rate to us; while
// seeding: our upload rate to them), plus one "optimistic" random peer so
// newcomers get a chance to prove themselves. See DECISIONS D26.
func (s *Session) rechoke(now time.Time, rotate bool) {
	s.choke.nextRechoke = now.Add(s.cfg.ChokeInterval)
	if rotate {
		s.choke.nextOptimistic = now.Add(s.cfg.OptimisticEvery)
	}
	paused := s.uploadsPaused.Load()
	s.choke.paused = paused

	var cands []chokeCand
	if !paused {
		seeding := s.m.Complete()
		for _, p := range s.peers {
			if !p.peerInterested {
				continue
			}
			r := p.rate.perSecond(s.cfg.Tick)
			if seeding {
				r = p.upRate.perSecond(s.cfg.Tick)
			}
			cands = append(cands, chokeCand{addr: p.addr, rate: r})
		}
	}
	unchoke, opt := chooseUnchoked(cands, s.cfg.UploadSlots, s.choke.optimistic, rotate, rand.IntN)
	s.choke.optimistic = opt
	for _, p := range s.peers {
		s.setChoke(p, !unchoke[p.addr])
	}
}

type chokeCand struct {
	addr netip.AddrPort
	rate int64
}

// chooseUnchoked picks who to unchoke: the `slots` fastest candidates, plus
// one optimistic unchoke among the rest. The current optimistic peer is kept
// unless rotate is set (or it's no longer a candidate / now in the top set).
// pick(n) returns a random index in [0,n). Pure function, for testability.
func chooseUnchoked(cands []chokeCand, slots int, current netip.AddrPort, rotate bool, pick func(int) int) (map[netip.AddrPort]bool, netip.AddrPort) {
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].rate != cands[j].rate {
			return cands[i].rate > cands[j].rate
		}
		return cands[i].addr.String() < cands[j].addr.String() // deterministic ties
	})
	unchoke := make(map[netip.AddrPort]bool)
	for i := 0; i < len(cands) && i < slots; i++ {
		unchoke[cands[i].addr] = true
	}
	var rest []netip.AddrPort
	for _, c := range cands {
		if !unchoke[c.addr] {
			rest = append(rest, c.addr)
		}
	}
	if len(rest) == 0 {
		return unchoke, netip.AddrPort{}
	}
	opt := netip.AddrPort{}
	if !rotate {
		for _, a := range rest {
			if a == current {
				opt = a
			}
		}
	}
	if !opt.IsValid() {
		opt = rest[pick(len(rest))]
	}
	unchoke[opt] = true
	return unchoke, opt
}

// --- completion --------------------------------------------------------------------

// onComplete switches the session to seeding: other seeds are useless to us
// (and we to them), and the choker should now rank by our upload rate.
func (s *Session) onComplete() {
	for _, p := range s.peers {
		if p.has.Full() {
			s.drop(p, "both seeds")
		}
	}
	s.rechoke(time.Now(), true)
}
