package torrent

import (
	"net/netip"
	"sort"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// State is a session's overall status.
type State string

const (
	StateDownloading State = "downloading"
	StateStalled     State = "stalled"  // incomplete and no connected peers
	StateSeeding     State = "seeding"  // complete and still uploading
	StateComplete    State = "complete" // complete and Run has returned / NoSeed
)

// Stats is a point-in-time summary, safe to read from any goroutine.
type Stats struct {
	State        State
	Name         string
	TotalBytes   int64
	BytesLeft    int64
	Downloaded   int64 // payload bytes downloaded this session
	Uploaded     int64
	DownloadRate int64 // bytes/second, averaged over the last few seconds
	UploadRate   int64
	PiecesHave   int
	PiecesTotal  int

	Peers    int // connected
	Seeds    int // connected peers that have everything
	Leechers int // connected peers that don't
	Known    int // addresses known to the swarm (any state)

	TrackerSeeders  int // -1 if unknown
	TrackerLeechers int
	TrackerError    string // last error when no tracker is working
}

// ETA estimates time to completion; negative means unknown.
func (st Stats) ETA() time.Duration {
	if st.BytesLeft == 0 {
		return 0
	}
	if st.DownloadRate <= 0 {
		return -1
	}
	return time.Duration(float64(st.BytesLeft) / float64(st.DownloadRate) * float64(time.Second))
}

// PeerInfo describes one connected peer.
type PeerInfo struct {
	Addr         netip.AddrPort
	Client       string
	Outgoing     bool
	Progress     float64 // fraction of pieces the peer has
	Downloaded   int64
	DownloadRate int64
	Uploaded     int64
	UploadRate   int64
	Choking      bool // they choke us
	Interested   bool // we're interested in them
	AmChoking    bool // we choke them
	PeerInterest bool // they're interested in us
	Pending      int  // our outstanding requests
}

type snapshot struct {
	stats Stats
	peers []PeerInfo
	have  *bitfield.Bitfield // verified pieces; every one is already written to storage
}

// Stats returns the latest snapshot (refreshed every tick).
func (s *Session) Stats() Stats {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	return s.snap.stats
}

// Have returns a copy of the verified pieces as of the latest snapshot.
// Every piece in it has already been written to storage (though not
// necessarily fsynced: resume savers call Storage.Sync after reading this).
func (s *Session) Have() *bitfield.Bitfield {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	return s.snap.have.Clone()
}

// Peers returns the latest connected-peer list, fastest first.
func (s *Session) Peers() []PeerInfo {
	s.snapMu.Lock()
	defer s.snapMu.Unlock()
	return append([]PeerInfo(nil), s.snap.peers...)
}

// publish rebuilds the snapshot (loop goroutine only).
func (s *Session) publish() {
	have := s.m.Have()
	st := Stats{
		Name:            s.info.Name,
		TotalBytes:      s.info.TotalLength,
		BytesLeft:       s.m.BytesLeft(),
		Downloaded:      s.downloaded.Load(),
		Uploaded:        s.uploaded.Load(),
		DownloadRate:    s.downRate.perSecond(s.cfg.Tick),
		UploadRate:      s.upRate.perSecond(s.cfg.Tick),
		PiecesHave:      have.Count(),
		PiecesTotal:     s.info.NumPieces(),
		Peers:           len(s.peers),
		Known:           len(s.swarm.addrs),
		TrackerSeeders:  s.tracker.seeders,
		TrackerLeechers: s.tracker.leechers,
	}
	if !s.tracker.working {
		st.TrackerError = s.tracker.lastErr
	}
	peers := make([]PeerInfo, 0, len(s.peers))
	for _, p := range s.peers {
		if p.has.Full() {
			st.Seeds++
		} else {
			st.Leechers++
		}
		peers = append(peers, PeerInfo{
			Addr:         p.addr,
			Client:       peerid.ClientName(p.conn.Peer.PeerID),
			Outgoing:     p.conn.Outgoing,
			Progress:     float64(p.has.Count()) / float64(max(1, s.info.NumPieces())),
			Downloaded:   p.downloaded,
			DownloadRate: p.rate.perSecond(s.cfg.Tick),
			Uploaded:     p.uploaded,
			UploadRate:   p.upRate.perSecond(s.cfg.Tick),
			Choking:      p.peerChoking,
			Interested:   p.amInterested,
			AmChoking:    p.amChoking,
			PeerInterest: p.peerInterested,
			Pending:      len(p.pending),
		})
	}
	sort.Slice(peers, func(i, j int) bool {
		return peers[i].DownloadRate+peers[i].UploadRate > peers[j].DownloadRate+peers[j].UploadRate
	})

	switch {
	case s.m.Complete() && (s.cfg.NoSeed || !s.isRunning()):
		st.State = StateComplete
	case s.m.Complete():
		st.State = StateSeeding
	case len(s.peers) == 0:
		st.State = StateStalled
	default:
		st.State = StateDownloading
	}

	s.snapMu.Lock()
	s.snap = snapshot{stats: st, peers: peers, have: have}
	s.snapMu.Unlock()
}

// --- speed meter --------------------------------------------------------------

// rateWindow is how many ticks the speed average covers.
const rateWindow = 5

// rate is a sliding-window byte counter: add() as bytes arrive, tick() once
// per session tick, perSecond() for the average over the window.
type rate struct {
	buckets [rateWindow]int64
	cur     int64
	filled  int
	pos     int
}

func (r *rate) add(n int64) { r.cur += n }

func (r *rate) tick() {
	r.buckets[r.pos] = r.cur
	r.cur = 0
	r.pos = (r.pos + 1) % rateWindow
	r.filled = min(r.filled+1, rateWindow)
}

// perSecond averages over the filled window; tick is the time between ticks.
func (r *rate) perSecond(tick time.Duration) int64 {
	if r.filled == 0 || tick <= 0 {
		return 0
	}
	var sum int64
	for _, b := range r.buckets {
		sum += b
	}
	return int64(float64(sum) / (float64(r.filled) * tick.Seconds()))
}

func (s *Session) isRunning() bool {
	s.extraMu.Lock()
	defer s.extraMu.Unlock()
	return s.running
}
