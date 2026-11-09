package torrent

import (
	"sync/atomic"
	"time"

	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/ratelimit"
)

// Pacing under a download limit (DECISIONS D46). A global limit is shared
// by every peer we download from, across all torrents. With a low limit
// and many peers, each one may deliver less than a block per
// RequestTimeout — not because it snubs us, but because *we* read its
// socket slowly. So the snub timeout grows to a few times the time one
// block takes at a peer's fair share, and the request pipeline shrinks to
// a few seconds' worth of that share (deep queues would only make blocks
// arrive minutes after they were asked for).

const (
	paceSlack      = 3                // timeout = this × one block's time at the fair share
	maxPacedWait   = 10 * time.Minute // never wait longer than this for a block
	paceQueueTime  = 5                // seconds of the fair share to keep requested
	minPacedQueue  = 2                // …but always at least this many blocks
	pacedQueueSlop = 1                // plus one, so a block is always on its way
)

// Pacer is shared by all sessions under one download limit. Safe for
// concurrent use; a nil *Pacer means "unlimited".
type Pacer struct {
	lim   *ratelimit.Limiter
	peers atomic.Int64 // peers being downloaded from, all sessions together
}

// NewPacer returns a pacer for downloads limited by lim (nil = unlimited).
func NewPacer(lim *ratelimit.Limiter) *Pacer { return &Pacer{lim: lim} }

// PerPeer is each downloading peer's fair share of the limit in bytes/s,
// or 0 when downloads are unlimited.
func (p *Pacer) PerPeer() int64 {
	if p == nil {
		return 0
	}
	r := p.lim.Rate()
	if r <= 0 {
		return 0
	}
	return max(1, r/max(1, p.peers.Load()))
}

func (p *Pacer) add(n int64) {
	if p != nil && n != 0 {
		p.peers.Add(n)
	}
}

// requestTimeout is how long a peer with outstanding requests may go
// without delivering a block before it counts as snubbing us.
func (s *Session) requestTimeout() time.Duration {
	t := s.cfg.RequestTimeout
	if share := s.cfg.Pacer.PerPeer(); share > 0 {
		need := time.Duration(paceSlack * float64(peer.BlockSize) / float64(share) * float64(time.Second))
		t = max(t, min(need, maxPacedWait))
	}
	return t
}

// pipeline is how many requests to keep outstanding per peer.
func (s *Session) pipeline() int {
	q := s.cfg.RequestQueue
	if share := s.cfg.Pacer.PerPeer(); share > 0 {
		q = min(q, max(minPacedQueue, int(share*paceQueueTime/peer.BlockSize)+pacedQueueSlop))
	}
	return q
}

// updatePace reports how many of our peers we are downloading from to the
// shared Pacer (loop goroutine, every tick).
func (s *Session) updatePace() {
	n := int64(0)
	for _, p := range s.peers {
		if !p.peerChoking && p.amInterested {
			n++
		}
	}
	s.cfg.Pacer.add(n - s.paced)
	s.paced = n
}
