// Package torrent runs one torrent: it finds peers through trackers,
// connects to them, downloads and verifies pieces, and writes them to disk.
//
// Concurrency model (see DECISIONS D23): a single event-loop goroutine owns
// all mutable state — the pieces.Manager, every peer's state, the swarm of
// known addresses. Helper goroutines (one forwarder per peer connection, dial
// attempts, the tracker announcer, hash/write workers) never touch that
// state; they send events to the loop over one channel. The loop never
// blocks on the network: it uses peer.Conn.TrySend and drops peers whose
// send queue is full.
//
// Files:
//
//	session.go  – Session, Config, Run and the event loop
//	peers.go    – per-peer state and wire message handling
//	swarm.go    – known addresses, dialing, backoff, bans
//	announce.go – tracker announce loop
//	stats.go    – Stats/PeerInfo snapshots and the speed meter
package torrent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/tracker"
)

// Config tunes a session. Zero values use the defaults in withDefaults.
type Config struct {
	PeerID peerid.ID // required
	Port   uint16    // port reported to trackers (listener arrives in Phase 6)

	MaxPeers     int // connected + dialing; default 50
	DialParallel int // simultaneous dial attempts; default 10
	RequestQueue int // outstanding block requests per peer; default 32

	Announcer tracker.Announcer // default &tracker.Client{}
	Pieces    pieces.Config
	PeerOpts  peer.Options // PeerID is filled in from PeerID above
	Logger    *slog.Logger // default: discard

	// Timing knobs (tests shrink these).
	Tick           time.Duration // housekeeping interval; default 1s
	RequestTimeout time.Duration // drop a peer that sends nothing back for this long; default 60s
	RetryBase      time.Duration // first reconnect backoff; doubles per failure; default 30s
	AnnounceRetry  time.Duration // first retry after all trackers failed; default 1m
}

func (c Config) withDefaults() Config {
	def := func(v *int, d int) {
		if *v <= 0 {
			*v = d
		}
	}
	defD := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&c.MaxPeers, 50)
	def(&c.DialParallel, 10)
	def(&c.RequestQueue, 32)
	defD(&c.Tick, time.Second)
	defD(&c.RequestTimeout, 60*time.Second)
	defD(&c.RetryBase, 30*time.Second)
	defD(&c.AnnounceRetry, time.Minute)
	if c.Announcer == nil {
		c.Announcer = &tracker.Client{}
	}
	if c.Logger == nil {
		c.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c.PeerOpts.PeerID = c.PeerID
	return c
}

// ErrStorage wraps fatal disk errors (disk full, permission denied).
var ErrStorage = errors.New("torrent: storage error")

// Session downloads one torrent.
type Session struct {
	mi    *metainfo.MetaInfo
	info  *metainfo.Info
	st    *storage.Storage
	cfg   Config
	log   *slog.Logger
	tiers [][]string

	events    chan any      // everything the loop reacts to
	completed chan struct{} // closed once all pieces are verified
	compOnce  sync.Once
	// startedComplete is fixed in New, so the announcer goroutine can read it
	// without touching loop-owned state.
	startedComplete bool
	wg              sync.WaitGroup // all helper goroutines

	// Counters read by the announcer goroutine (payload bytes).
	downloaded atomic.Int64
	uploaded   atomic.Int64
	left       atomic.Int64

	// Loop-owned state. Only the loop goroutine touches these.
	runCtx   context.Context // the context Run was started with
	m        *pieces.Manager
	peers    map[netip.AddrPort]*peerState
	swarm    *swarm
	hashing  int // pieces being verified/written
	downRate rate
	upRate   rate
	tracker  trackerInfo

	snapMu sync.Mutex
	snap   snapshot

	extraMu sync.Mutex
	extra   []netip.AddrPort // queued by AddPeers
}

// New prepares a session. have is the set of pieces already verified on
// disk (from resume data or a recheck); nil means none.
func New(mi *metainfo.MetaInfo, st *storage.Storage, have *bitfield.Bitfield, cfg Config) *Session {
	cfg = cfg.withDefaults()
	s := &Session{
		mi:        mi,
		info:      &mi.Info,
		st:        st,
		cfg:       cfg,
		log:       cfg.Logger.With("torrent", mi.Info.Name),
		tiers:     tracker.TiersFromMetaInfo(mi),
		events:    make(chan any, 256),
		completed: make(chan struct{}),
		m:         pieces.NewManager(&mi.Info, have, cfg.Pieces),
		peers:     make(map[netip.AddrPort]*peerState),
		swarm:     newSwarm(cfg.RetryBase),
		tracker:   trackerInfo{seeders: -1, leechers: -1},
	}
	s.left.Store(s.m.BytesLeft())
	if s.m.Complete() {
		s.startedComplete = true
		s.markComplete()
	}
	s.publish()
	return s
}

// Completed is closed when every piece has been verified.
func (s *Session) Completed() <-chan struct{} { return s.completed }

// AddPeers hands the session extra peer addresses (magnet x.pe, DHT later).
// Safe to call from any goroutine, before or during Run; never blocks.
func (s *Session) AddPeers(addrs []netip.AddrPort) {
	s.extraMu.Lock()
	s.extra = append(s.extra, addrs...)
	s.extraMu.Unlock()
	select {
	case s.events <- wakeEvent{}:
	default: // loop is busy; it also drains on every tick
	}
}

// drainExtra moves AddPeers addresses into the swarm (loop goroutine only).
func (s *Session) drainExtra() {
	s.extraMu.Lock()
	addrs := s.extra
	s.extra = nil
	s.extraMu.Unlock()
	s.swarm.add(addrs)
}

// Run downloads until ctx is cancelled or a fatal storage error occurs.
// It keeps running after completion (seeding comes in Phase 6); callers
// that only want the download can wait on Completed() and cancel ctx.
// Run returns nil when stopped via ctx.
func (s *Session) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.runCtx = ctx

	s.goHelper(func() { s.announceLoop(ctx) })

	err := s.loop(ctx)

	cancel()
	for _, p := range s.peers {
		p.conn.Close()
	}
	// Drain events so helpers blocked on sending can exit, until all are done.
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	for {
		select {
		case ev := <-s.events:
			if d, ok := ev.(dialResult); ok && d.conn != nil {
				d.conn.Close()
			}
		case <-done:
			s.publish()
			return err
		}
	}
}

// ctx returns Run's context (loop goroutine only).
func (s *Session) ctx() context.Context { return s.runCtx }

func (s *Session) goHelper(f func()) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		f()
	}()
}

// send delivers an event to the loop unless the session is shutting down.
func (s *Session) send(ctx context.Context, ev any) {
	select {
	case s.events <- ev:
	case <-ctx.Done():
	}
}

// --- events -----------------------------------------------------------------

type (
	msgEvent struct {
		p   *peerState
		msg *peer.Message
	}
	closedEvent struct{ p *peerState }
	dialResult  struct {
		addr netip.AddrPort
		conn *peer.Conn
		err  error
	}
	trackerEvent struct {
		peers             []netip.AddrPort
		seeders, leechers int
		ok                bool
		err               error
	}
	hashResult struct {
		index        int
		ok           bool
		writeErr     error
		contributors []pieces.PeerKey
	}
	wakeEvent struct{} // "AddPeers queued something"
)

// loop is the session's single owner goroutine.
func (s *Session) loop(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Tick)
	defer tick.Stop()
	s.drainExtra()
	s.maintain(ctx) // dial immediately if we already know peers

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			s.drainExtra()
			s.downRate.tick()
			s.upRate.tick()
			for _, p := range s.peers {
				p.rate.tick()
				s.fillRequests(p) // safety net: never leave an unchoked peer idle
			}
			s.checkTimeouts()
			s.maintain(ctx)
			s.publish()
		case ev := <-s.events:
			if err := s.handle(ctx, ev); err != nil {
				return err
			}
		}
	}
}

func (s *Session) handle(ctx context.Context, ev any) error {
	switch e := ev.(type) {
	case msgEvent:
		if s.peers[e.p.addr] == e.p { // ignore stragglers from replaced peers
			s.onMessage(e.p, e.msg)
		}
	case closedEvent:
		s.onPeerClosed(e.p)
		s.maintain(ctx)
	case dialResult:
		s.onDialResult(ctx, e)
	case trackerEvent:
		s.onTracker(e)
		s.maintain(ctx)
	case wakeEvent:
		s.drainExtra()
		s.maintain(ctx)
	case hashResult:
		return s.onHashResult(e)
	}
	return nil
}

// --- piece completion -----------------------------------------------------------

// verifyAndWrite hashes a completed piece and, if good, writes it to disk —
// off the loop goroutine, bounded by the number of CPUs.
func (s *Session) verifyAndWrite(ctx context.Context, index int, data []byte, contributors []pieces.PeerKey) {
	s.hashing++
	s.goHelper(func() {
		hashSem <- struct{}{}
		defer func() { <-hashSem }()
		res := hashResult{index: index, contributors: contributors}
		res.ok = pieces.Check(data, s.info.Pieces[index])
		if res.ok {
			if _, err := s.st.WriteAt(data, int64(index)*s.info.PieceLength); err != nil {
				res.ok, res.writeErr = false, err
			}
		}
		s.send(ctx, res)
	})
}

// hashSem bounds concurrent SHA-1 work across all sessions in the process.
var hashSem = make(chan struct{}, max(2, runtime.NumCPU()))

func (s *Session) onHashResult(r hashResult) error {
	s.hashing--
	if r.writeErr != nil {
		return fmt.Errorf("%w: piece %d: %v", ErrStorage, r.index, r.writeErr)
	}
	s.m.PieceVerified(r.index, r.ok)
	if !r.ok {
		s.log.Warn("piece failed hash check", "piece", r.index, "peers", len(r.contributors))
		s.blame(r.contributors)
		// The piece is wanted again. Peers we told "not interested" while it
		// was being verified must hear "interested" again, or the download
		// can stall forever on the last pieces.
		for _, p := range s.peers {
			s.updateInterest(p)
		}
		return nil
	}
	s.left.Store(s.m.BytesLeft())
	have := peer.Have(uint32(r.index))
	for _, p := range s.peers {
		s.sendTo(p, have)
		s.updateInterest(p)
	}
	if s.m.Complete() {
		s.log.Info("download complete")
		// Publish before signalling, so anyone woken by Completed() sees
		// final stats rather than the previous tick's snapshot.
		s.publish()
		s.markComplete()
	}
	return nil
}

func (s *Session) markComplete() {
	s.compOnce.Do(func() { close(s.completed) })
}

// blame records a hash failure against every peer that contributed. A peer
// that alone supplied a bad piece is certainly guilty and is banned at once;
// shared blame bans after repeated failures.
func (s *Session) blame(contributors []pieces.PeerKey) {
	for _, addr := range contributors {
		strikes := s.swarm.strike(addr)
		if len(contributors) == 1 || strikes >= 3 {
			s.log.Warn("banning peer for bad data", "peer", addr)
			s.swarm.ban(addr)
			if p := s.peers[addr]; p != nil {
				p.conn.Close()
			}
		}
	}
}
