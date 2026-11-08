package metadata

import (
	"context"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/tracker"
)

// Config tunes a Fetcher. Zero values use defaults.
type Config struct {
	PeerOpts  peer.Options      // PeerID required; extensions are switched on
	Announcer tracker.Announcer // default &tracker.Client{}
	Port      uint16            // our peer port, reported to trackers and peers
	Parallel  int               // peers talked to at once; default 10
	// Retry is how often trackers are asked again and failed peers retried
	// (default 60s); RequestTimeout drops a peer that stops answering (20s).
	Retry          time.Duration
	RequestTimeout time.Duration
	Logger         *slog.Logger
}

// perPeerRequests is how many pieces we ask one peer for at a time.
const perPeerRequests = 4

// Fetcher downloads one torrent's metadata. Create with NewFetcher, then
// call Run once; Status may be called from any goroutine meanwhile.
//
// Each peer's worker assembles the whole metadata from that peer alone
// (DECISIONS D38). Mixing pieces from several peers would be faster, but if
// the result failed the hash check we couldn't tell who lied, and banning
// everyone involved would ban honest peers too. Metadata is small (usually
// well under 1 MB), so the duplicate downloading is cheap.
//
// Concurrency: Run's goroutine owns the list of addresses and starts one
// worker goroutine per peer (at most Parallel); one announcer goroutine
// feeds it addresses. Workers share only the fields under mu. Run waits for
// all of them before returning.
type Fetcher struct {
	hash  metainfo.Hash
	tiers [][]string
	seed  []netip.AddrPort
	cfg   Config
	log   *slog.Logger

	mu        sync.Mutex
	banned    map[netip.AddrPort]bool // their metadata failed the hash check
	known     map[netip.AddrPort]bool // every address heard of (for Peers)
	peers     int                     // workers past the extension handshake
	bestHave  int                     // most pieces any one worker holds (for Status)
	bestTotal int
	trackErr  string        // last tracker error, if none worked
	result    []byte        // verified metadata
	done      chan struct{} // closed when result is set
	doneOnce  sync.Once
}

// Status is a snapshot of a Fetcher's progress.
type Status struct {
	Peers        int // connected peers that offer metadata
	Have, Total  int // pieces (Total 0 until a peer tells us the size)
	TrackerError string
}

// NewFetcher prepares a fetch for hash, using trackers in tiers and any
// peers already known (magnet x.pe).
func NewFetcher(hash metainfo.Hash, tiers [][]string, peers []netip.AddrPort, cfg Config) *Fetcher {
	if cfg.Parallel <= 0 {
		cfg.Parallel = 10
	}
	if cfg.Retry <= 0 {
		cfg.Retry = time.Minute
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 20 * time.Second
	}
	if cfg.Announcer == nil {
		cfg.Announcer = &tracker.Client{}
	}
	cfg.PeerOpts.Extensions = true
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Fetcher{
		hash: hash, tiers: tiers, seed: peers, cfg: cfg, log: log.With("magnet", hash.String()),
		banned: map[netip.AddrPort]bool{}, known: map[netip.AddrPort]bool{},
		done: make(chan struct{}),
	}
}

// Status returns current progress.
func (f *Fetcher) Status() Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := Status{Peers: f.peers, Have: f.bestHave, Total: f.bestTotal, TrackerError: f.trackErr}
	if len(f.tiers) == 0 && len(f.seed) == 0 {
		s.TrackerError = "the magnet link has no trackers or peers (DHT arrives in Phase 11)"
	}
	return s
}

// Peers returns every peer address the fetcher heard of (magnet x.pe and
// trackers) except those that sent bad metadata — a good start for the
// download that follows.
func (f *Fetcher) Peers() []netip.AddrPort {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []netip.AddrPort
	for a := range f.known {
		if !f.banned[a] {
			out = append(out, a)
		}
	}
	return out
}

// Run fetches until the metadata is verified (returned) or ctx ends.
func (f *Fetcher) Run(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait() // runs after cancel (defers are last-in, first-out)
	defer cancel()

	found := make(chan []netip.AddrPort, 4)
	if len(f.tiers) > 0 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f.announceLoop(ctx, found)
		}()
	}

	known := map[netip.AddrPort]bool{} // every address heard of
	tried := map[netip.AddrPort]bool{} // tried since the last retry round
	active := map[netip.AddrPort]bool{}
	var queue []netip.AddrPort
	enqueue := func(addrs []netip.AddrPort) {
		f.mu.Lock()
		for _, a := range addrs {
			f.known[a] = true
		}
		f.mu.Unlock()
		for _, a := range addrs {
			known[a] = true
			if !tried[a] && !active[a] && !f.isBanned(a) {
				tried[a] = true
				queue = append(queue, a)
			}
		}
	}
	enqueue(f.seed)
	finished := make(chan netip.AddrPort)
	retry := time.NewTicker(f.cfg.Retry)
	defer retry.Stop()

	for {
		for len(active) < f.cfg.Parallel && len(queue) > 0 {
			a := queue[0]
			queue = queue[1:]
			active[a] = true
			wg.Add(1)
			go func() {
				defer wg.Done()
				f.worker(ctx, a)
				select {
				case finished <- a:
				case <-ctx.Done():
				}
			}()
		}
		select {
		case <-f.done:
			return f.result, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		case addrs := <-found:
			enqueue(addrs)
		case a := <-finished:
			delete(active, a)
		case <-retry.C: // give every known peer another chance
			clear(tried)
			all := make([]netip.AddrPort, 0, len(known))
			for a := range known {
				all = append(all, a)
			}
			enqueue(all)
		}
	}
}

func (f *Fetcher) announceLoop(ctx context.Context, found chan<- []netip.AddrPort) {
	list := tracker.NewList(f.tiers)
	req := tracker.Request{
		InfoHash: f.hash, PeerID: f.cfg.PeerOpts.PeerID, Port: f.cfg.Port,
		Left: 1, // size unknown; any non-zero value makes trackers treat us as a leecher
		Key:  rand.Uint32(), Event: tracker.EventStarted,
	}
	for {
		var peers []netip.AddrPort
		var errs []string
		for _, r := range list.AnnounceAll(ctx, f.cfg.Announcer, req) {
			if r.Err != nil {
				errs = append(errs, r.Err.Error())
				continue
			}
			peers = append(peers, r.Response.Peers...)
			req.Event = tracker.EventNone
		}
		f.mu.Lock()
		f.trackErr = ""
		if len(peers) == 0 && len(errs) > 0 {
			f.trackErr = errs[0]
		}
		f.mu.Unlock()
		select {
		case found <- peers:
		case <-ctx.Done():
			return
		}
		t := time.NewTimer(f.cfg.Retry)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return
		}
	}
}

// worker downloads the whole metadata from one peer, until it is done, the
// peer fails or turns out to be lying, or another worker finished first.
func (f *Fetcher) worker(ctx context.Context, addr netip.AddrPort) {
	c, err := peer.Dial(ctx, addr, f.hash, f.cfg.PeerOpts)
	if err != nil {
		f.log.Debug("metadata peer: dial failed", "peer", addr, "err", err)
		return
	}
	defer c.Close()
	if !c.Peer.SupportsExtensions() {
		return
	}
	hs := peer.ExtHandshake{M: map[string]int{peer.ExtMetadata: peer.ExtMetadataID}, Version: "Denco 0.0.1", Port: int(f.cfg.Port)}
	if !c.TrySend(peer.Extended(0, hs.Encode())) {
		return
	}

	var asm *Assembler // this peer's copy; nil until its handshake
	their := 0         // the peer's ID for ut_metadata
	next := 0          // next piece to request
	outstanding := 0
	defer func() {
		if asm != nil {
			f.mu.Lock()
			f.peers--
			f.mu.Unlock()
		}
	}()
	ask := func() bool {
		for outstanding < perPeerRequests && next < asm.NumPieces() {
			if !c.TrySend(peer.Extended(uint8(their), Message{Type: Request, Piece: next}.Encode())) {
				return false
			}
			next++
			outstanding++
		}
		return true
	}

	idle := time.NewTimer(f.cfg.RequestTimeout)
	defer idle.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.done:
			return
		case <-idle.C:
			return // handshake or request unanswered
		case m, ok := <-c.Incoming():
			if !ok {
				return
			}
			if m.ID != peer.MsgExtended {
				continue // bitfield, have, …: irrelevant here
			}
			id, body, err := peer.ParseExtended(m)
			if err != nil {
				return
			}
			switch {
			case id == 0 && asm == nil: // their extension handshake
				h, err := peer.ParseExtHandshake(body)
				if err != nil {
					return
				}
				if their = h.M[peer.ExtMetadata]; their == 0 {
					return // doesn't do ut_metadata
				}
				if asm, err = NewAssembler(f.hash, h.MetadataSize); err != nil {
					return // no (or an absurd) metadata size: it can't help
				}
				f.mu.Lock()
				f.peers++
				f.mu.Unlock()
			case id == peer.ExtMetadataID && asm != nil:
				msg, err := Decode(body)
				if err != nil {
					return
				}
				switch msg.Type {
				case Request: // we have nothing to give yet
					c.TrySend(peer.Extended(uint8(their), Message{Type: Reject, Piece: msg.Piece}.Encode()))
					continue
				case Reject:
					return
				}
				if msg.TotalSize != asm.Size() || asm.Add(msg.Piece, msg.Data) != nil {
					return // contradicts itself: broken or hostile
				}
				outstanding--
				f.progress(asm)
				if asm.Complete() {
					f.complete(addr, asm)
					return
				}
			default:
				continue
			}
			if !ask() {
				return
			}
			idle.Reset(f.cfg.RequestTimeout)
		}
	}
}

func (f *Fetcher) isBanned(a netip.AddrPort) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.banned[a]
}

// progress records the best per-peer progress for Status.
func (f *Fetcher) progress(a *Assembler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if a.Have() > f.bestHave {
		f.bestHave, f.bestTotal = a.Have(), a.NumPieces()
	}
}

// complete checks one peer's full copy: the result if it hashes right,
// otherwise that peer is certainly lying and is never asked again.
func (f *Fetcher) complete(addr netip.AddrPort, a *Assembler) {
	data, ok := a.Verify()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !ok {
		f.log.Warn("peer sent metadata that fails the hash check; ignoring it", "peer", addr)
		f.banned[addr] = true
		return
	}
	if f.result == nil {
		f.result = data
		f.doneOnce.Do(func() { close(f.done) })
	}
}
