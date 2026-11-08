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
	// maxQueuedUploads caps how many of a peer's requests we hold at once.
	// Extra requests are ignored; the peer will re-request after a timeout.
	maxQueuedUploads = 256
	// maxRequestLen is the largest block we serve. 16 KiB is the norm; a
	// little leeway avoids dropping slightly unusual clients.
	maxRequestLen = 2 * peer.BlockSize
)

// --- serving requests ----------------------------------------------------------

// startUploader runs a goroutine that performs disk reads for p's requests,
// so the loop never waits on the disk. The loop decides afterwards whether
// the block is still wanted (the peer may have cancelled or been choked).
func (s *Session) startUploader(ctx context.Context, p *peerState) {
	s.goHelper(func() {
		for {
			select {
			case b := <-p.uploadQ:
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
	case !s.m.HasPiece(idx), p.served[b], len(p.served) >= maxQueuedUploads:
		return // don't have it yet / duplicate / too many queued: ignore
	}
	select {
	case p.uploadQ <- b:
		p.served[b] = true
	default: // uploader backlog full (cancelled entries still queued): ignore
	}
}

func (s *Session) onCancel(p *peerState, m *peer.Message) {
	if b, err := peer.ParseBlock(m); err == nil {
		delete(p.served, b) // if the read is in flight, onUploadRead skips it
	}
}

func (s *Session) onUploadRead(e uploadRead) {
	p := e.p
	if s.peers[p.addr] != p || !p.served[e.b] {
		return // peer gone, block cancelled, or peer choked meanwhile
	}
	delete(p.served, e.b)
	if e.err != nil {
		s.log.Warn("reading block for upload failed", "piece", e.b.Index, "err", e.err)
		s.drop(p, "local read error")
		return
	}
	if !p.conn.TrySend(peer.Piece(e.b.Index, e.b.Begin, e.data)) {
		p.conn.Close() // can't keep up with what it asked for
		return
	}
	n := int64(len(e.data))
	p.uploaded += n
	p.upRate.add(n)
	s.upRate.add(n)
	s.uploaded.Add(n)
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
