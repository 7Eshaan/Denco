// Package torrent runs one torrent: it finds peers through trackers,
// connects to them, downloads and verifies pieces, writes them to disk, and
// uploads to other peers (seeding once complete).
//
// Concurrency model (see DECISIONS D23): a single event-loop goroutine owns
// all mutable state — the pieces.Manager, every peer's state, the swarm of
// known addresses. Helper goroutines (one forwarder per peer connection, dial
// attempts, the tracker announcer, hash/write workers) never touch that
// state; they send events to the loop over one channel. The loop never
// blocks on the network or disk: it uses peer.Conn.TrySend and drops peers
// whose send queue is full, and per-peer uploader goroutines do disk reads.
//
// Files:
//
//	session.go  – Session, Config, Run and the event loop
//	peers.go    – per-peer state and wire message handling (download side)
//	upload.go   – serving requests and the choking algorithm
//	extensions.go – BEP 10 extension handshake; serving metadata (BEP 9)
//	pex.go      – peer exchange (BEP 11)
//	listener.go – one TCP listener routing incoming peers to sessions
//	swarm.go    – known addresses, dialing, backoff, bans
//	announce.go – tracker announce loop
//	dht.go      – DHT lookups and announces, PORT messages
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
	"github.com/7Eshaan/Denco/internal/ratelimit"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/tracker"
)

// Config tunes a session. Zero values use the defaults in withDefaults.
type Config struct {
	PeerID peerid.ID // required; must match the Listener's
	Port   uint16    // port reported to trackers (the Listener's port)

	MaxPeers     int // connected (incl. incoming) + dialing; default 50
	DialParallel int // simultaneous dial attempts; default 10
	RequestQueue int // outstanding block requests per peer; default 32
	UploadSlots  int // peers unchoked by rate (plus 1 optimistic); default 4

	// NoSeed makes Run return as soon as the download completes instead of
	// staying online to upload. Default false: seed until stopped.
	NoSeed bool
	// UploadsPaused starts the session with uploads paused (see SetUploadsPaused).
	UploadsPaused bool

	// PEX enables peer exchange (BEP 11) with peers that support it. It is
	// never used for private torrents, whatever this says.
	PEX bool
	// DHT, if set, is asked for peers and told about us (BEP 5); never for
	// private torrents. PeerOpts.DHT should be set with it.
	DHT PeerFinder

	// UploadLimiter, if set, caps upload speed (shared by all sessions for
	// a global limit). The download limit lives in PeerOpts.ReadLimiter.
	UploadLimiter *ratelimit.Limiter

	Announcer tracker.Announcer // default &tracker.Client{}
	Pieces    pieces.Config
	PeerOpts  peer.Options // PeerID is filled in from PeerID above
	Logger    *slog.Logger // default: discard

	// Timing knobs (tests shrink these).
	Tick            time.Duration // housekeeping interval; default 1s
	RequestTimeout  time.Duration // drop a peer that sends nothing back for this long; default 60s
	RetryBase       time.Duration // first reconnect backoff; doubles per failure; default 30s
	AnnounceRetry   time.Duration // first retry after all trackers failed; default 1m
	ChokeInterval   time.Duration // re-evaluate who is unchoked; default 10s
	OptimisticEvery time.Duration // rotate the optimistic unchoke; default 30s
	PEXInterval     time.Duration // how often PEX messages are sent; default 60s
	DHTInterval     time.Duration // DHT lookup + announce; default 15m
	DHTRetry        time.Duration // first retry after a lookup found no one; default 1m

	banExactAddr bool // tests only: ban address:port, not the whole IP (see swarm)
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
	def(&c.UploadSlots, 4)
	defD(&c.Tick, time.Second)
	defD(&c.RequestTimeout, 60*time.Second)
	defD(&c.RetryBase, 30*time.Second)
	defD(&c.AnnounceRetry, time.Minute)
	defD(&c.ChokeInterval, 10*time.Second)
	defD(&c.OptimisticEvery, 30*time.Second)
	defD(&c.PEXInterval, time.Minute)
	defD(&c.DHTInterval, 15*time.Minute)
	defD(&c.DHTRetry, time.Minute)
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

// Session downloads and seeds one torrent.
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
	dhtPeers int // peers the last DHT lookup found
	choke    chokeState

	snapMu sync.Mutex
	snap   snapshot

	extraMu  sync.Mutex
	extra    []netip.AddrPort // queued by AddPeers
	incoming []*peer.Conn     // queued by addIncoming (Listener)
	running  bool             // Run is active; when false, incoming conns are refused

	uploadsPaused atomic.Bool
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
	s.uploadsPaused.Store(cfg.UploadsPaused)
	s.swarm.exact = cfg.banExactAddr
	if s.m.Complete() {
		s.startedComplete = true
		s.markComplete()
	}
	s.publish()
	return s
}

// Completed is closed when every piece has been verified.
func (s *Session) Completed() <-chan struct{} { return s.completed }

// InfoHash identifies the torrent.
func (s *Session) InfoHash() metainfo.Hash { return s.mi.InfoHash }

// SetUploadsPaused chokes every peer (and keeps them choked) while paused.
// Downloading continues. Safe to call from any goroutine.
func (s *Session) SetUploadsPaused(paused bool) {
	s.uploadsPaused.Store(paused)
	s.wake()
}

func (s *Session) wake() {
	select {
	case s.events <- wakeEvent{}:
	default: // loop is busy; it also checks on every tick
	}
}

// addIncoming queues a connection accepted by the Listener. Never blocks.
func (s *Session) addIncoming(c *peer.Conn) {
	s.extraMu.Lock()
	if !s.running {
		s.extraMu.Unlock()
		c.Close()
		return
	}
	s.incoming = append(s.incoming, c)
	s.extraMu.Unlock()
	s.wake()
}

// AddPeers hands the session extra peer addresses (magnet x.pe, DHT later).
// Safe to call from any goroutine, before or during Run; never blocks.
func (s *Session) AddPeers(addrs []netip.AddrPort) {
	s.extraMu.Lock()
	s.extra = append(s.extra, addrs...)
	s.extraMu.Unlock()
	s.wake()
}

// drainExtra moves queued addresses into the swarm and registers queued
// incoming connections (loop goroutine only).
func (s *Session) drainExtra(ctx context.Context) {
	s.extraMu.Lock()
	addrs, conns := s.extra, s.incoming
	s.extra, s.incoming = nil, nil
	s.extraMu.Unlock()
	s.swarm.add(addrs)
	for _, c := range conns {
		s.onIncoming(ctx, c)
	}
}

// onIncoming accepts a peer that connected to us, if there's room.
func (s *Session) onIncoming(ctx context.Context, c *peer.Conn) {
	switch {
	case ctx.Err() != nil,
		len(s.peers) >= s.cfg.MaxPeers,
		s.swarm.isBanned(c.Remote),
		s.peers[c.Remote] != nil:
		c.Close()
	default:
		s.addPeer(ctx, c)
	}
}

// Run downloads, then seeds, until ctx is cancelled or a fatal storage
// error occurs. With Config.NoSeed it returns as soon as the download is
// complete. Run returns nil when stopped via ctx or by finishing.
func (s *Session) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s.runCtx = ctx
	s.extraMu.Lock()
	s.running = true
	s.extraMu.Unlock()

	s.goHelper(func() { s.announceLoop(ctx) })
	if s.dhtEnabled() {
		s.goHelper(func() { s.dhtLoop(ctx) })
	}

	err := s.loop(ctx)

	cancel()
	s.extraMu.Lock()
	s.running = false
	leftover := s.incoming
	s.incoming = nil
	s.extraMu.Unlock()
	for _, c := range leftover {
		c.Close()
	}
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
	dhtEvent     struct{ peers []netip.AddrPort }
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
	uploadRead struct { // an uploader goroutine finished reading a block
		p    *peerState
		b    peer.Block
		data []byte
		err  error
	}
	wakeEvent struct{} // AddPeers/addIncoming queued something, or pause toggled
)

// loop is the session's single owner goroutine.
func (s *Session) loop(ctx context.Context) error {
	tick := time.NewTicker(s.cfg.Tick)
	defer tick.Stop()
	if s.m.Complete() && s.cfg.NoSeed {
		return nil // nothing to download and told not to seed
	}
	s.drainExtra(ctx)
	s.maintain(ctx) // dial immediately if we already know peers

	for {
		select {
		case <-ctx.Done():
			return nil
		case now := <-tick.C:
			s.drainExtra(ctx)
			s.downRate.tick()
			s.upRate.tick()
			for _, p := range s.peers {
				p.rate.tick()
				p.upRate.tick()
				s.fillRequests(p) // safety net: never leave an unchoked peer idle
			}
			s.checkTimeouts()
			s.maybeRechoke(now)
			s.maybeSendPEX(now)
			s.maintain(ctx)
			s.publish()
		case ev := <-s.events:
			if err := s.handle(ctx, ev); err != nil {
				return err
			}
		}
		if s.m.Complete() && s.cfg.NoSeed && s.hashing == 0 {
			return nil
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
	case uploadRead:
		s.onUploadRead(e)
	case dialResult:
		s.onDialResult(ctx, e)
	case trackerEvent:
		s.onTracker(e)
		s.maintain(ctx)
	case dhtEvent:
		s.dhtPeers = len(e.peers)
		s.swarm.add(e.peers)
		s.maintain(ctx)
	case wakeEvent:
		s.drainExtra(ctx)
		if s.uploadsPaused.Load() != s.choke.paused {
			s.rechoke(time.Now(), false)
		}
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
		if !p.has.Has(r.index) { // no point telling a peer about what it has
			s.sendTo(p, have)
		}
		s.updateInterest(p)
	}
	if s.m.Complete() {
		s.log.Info("download complete")
		s.onComplete()
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
			for _, p := range s.peers { // the IP may have several connections
				if p.addr.Addr() == addr.Addr() {
					p.conn.Close()
				}
			}
		}
	}
}
