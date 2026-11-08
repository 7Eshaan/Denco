// Package engine runs many torrents at once. It owns everything the daemon
// shares between torrents: the peer port (one torrent.Listener), our peer
// ID, the resume store, and the download queue. Each started torrent gets a
// goroutine that opens its storage, checks its saved progress, and runs a
// torrent.Session with a resume.Saver until it is paused, removed, fails,
// or the engine closes.
//
// Files:
//
//	engine.go   – Engine, Open/Close, the queue and the per-torrent run goroutine
//	commands.go – Add, Pause, Resume, Remove and torrent lookup
//	view.go     – read-only views (Torrent, File, Peer, Overview) for the UI
//	magnet.go   – magnet links: AddMagnet and fetching metadata before the first run
//
// Concurrency: one mutex guards the torrent table and every entry's fields.
// Methods may be called from any goroutine (the IPC server calls them
// concurrently). Nothing slow (disk checks, network) runs under the mutex
// except small resume-file writes. See DECISIONS D29.
package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/dht"
	"github.com/7Eshaan/Denco/internal/metadata"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/ratelimit"
	"github.com/7Eshaan/Denco/internal/resume"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/torrent"
)

// Config configures an Engine. StateDir and DownloadDir are required.
type Config struct {
	StateDir    string // resume store directory
	DownloadDir string // default download directory for Add
	ListenAddr  string // peer port, e.g. ":6881" (default); falls back to a random port if busy
	MaxActive   int    // torrents downloading at once; seeding ones don't count; default 5
	SaveEvery   time.Duration
	// Total speed limits in bytes/s for all torrents together; 0 = unlimited.
	// Changeable later with SetLimits.
	DownloadLimit, UploadLimit int64
	// EnableDHT runs a DHT node (UDP, on the peer port's number if free)
	// shared by all torrents; DHTBootstrap are its routers. Its state is
	// kept in StateDir/dht.json.
	EnableDHT    bool
	DHTBootstrap []string
	// Seeding limits (D45): a complete torrent stops seeding ("finished")
	// at this ratio or after this much seeding time; 0 = no limit. A
	// torrent's own seeding mode ("on"/"off") overrides them. Changeable
	// later with SetSeedLimits. SeedCheckEvery is how often they are
	// checked (default 5s; tests shrink it).
	SeedRatio      float64
	SeedTime       time.Duration
	SeedCheckEvery time.Duration
	// Session is the template for every torrent session (timeouts, peer
	// limits, announcer). PeerID, Port, Logger, NoSeed and the rate
	// limiters are set by the engine.
	Session torrent.Config
	Logger  *slog.Logger // default: discard
}

// Engine manages all torrents of one daemon.
type Engine struct {
	cfg     Config
	log     *slog.Logger
	store   *resume.Store
	ln      *torrent.Listener
	peerID  peerid.ID
	started time.Time
	down    *ratelimit.Limiter // shared by every connection
	up      *ratelimit.Limiter
	dht     *dht.Server // nil when DHT is off

	mu        sync.Mutex
	seedRatio float64       // current limits (Config values, then SetSeedLimits)
	seedTime  time.Duration //
	byID      map[int]*entry
	byHash    map[metainfo.Hash]*entry
	nextID    int
	closed    bool
	finished  struct{ up, down int64 } // payload moved by sessions that have ended

	wg        sync.WaitGroup // run goroutines
	closeOnce sync.Once
}

// entry is one torrent. All fields are guarded by Engine.mu. hash never
// changes; mi and layout are set once — at creation, or for a magnet link
// when its run goroutine receives the metadata — and never change after.
type entry struct {
	id     int
	hash   metainfo.Hash
	mi     *metainfo.MetaInfo // nil: magnet link without metadata yet
	layout *storage.Layout    // nil with mi

	state resume.State       // last persisted state (while running: as of start)
	have  *bitfield.Bitfield // pieces believed good while not running

	// The user's per-torrent choices; the entry owns them and every save
	// records them (flagsTo).
	paused        bool
	uploadsPaused bool   // choke every peer; downloading goes on
	seedMode      string // "" = global limits, SeedOn, SeedOff
	finished      bool   // stopped seeding (limit or mode off); not restarted on its own

	removing bool  // Remove is waiting for the run goroutine
	err      error // why it last stopped; cleared by Resume

	// Set while the run goroutine is alive.
	running  bool
	checking bool // opening storage / checking saved data
	cancel   context.CancelFunc
	done     chan struct{} // closed when the run goroutine has exited
	sess     *torrent.Session
	saver    *resume.Saver          // set with sess
	snapshot func() resume.Snapshot // set with sess: what saves record
	seeded   atomic.Int64           // ns spent seeding during this run
	fetcher  *metadata.Fetcher      // set while fetching a magnet's metadata
	// bootstrap: peers the metadata fetcher found, handed to the first
	// session (a magnet's x.pe peers are known nowhere else).
	bootstrap []netip.AddrPort
}

// ErrClosed is returned by methods called after Close.
var ErrClosed = errors.New("engine: closed")

// Open starts an engine: it opens the resume store and the peer port, loads
// every saved torrent, and starts those that aren't paused (up to MaxActive
// downloading).
func Open(cfg Config) (*Engine, error) {
	if cfg.StateDir == "" || cfg.DownloadDir == "" {
		return nil, errors.New("engine: StateDir and DownloadDir are required")
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":6881"
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = 5
	}
	if cfg.SaveEvery <= 0 {
		cfg.SaveEvery = 30 * time.Second
	}
	if cfg.SeedCheckEvery <= 0 {
		cfg.SeedCheckEvery = 5 * time.Second
	}
	if cfg.Session.MaxPeers <= 0 {
		cfg.Session.MaxPeers = 50 // same default as torrent.Config; set here so Overview can show it
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	store, err := resume.OpenStore(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	id := cfg.Session.PeerID
	if id == (peerid.ID{}) {
		id = peerid.New()
	}
	down, up := ratelimit.New(cfg.DownloadLimit), ratelimit.New(cfg.UploadLimit)
	ln, err := listen(cfg.ListenAddr, peer.Options{PeerID: id, ReadLimiter: down, Extensions: true, DHT: cfg.EnableDHT}, log)
	if err != nil {
		return nil, fmt.Errorf("engine: peer port: %w", err)
	}
	e := &Engine{
		cfg: cfg, log: log, store: store, ln: ln, peerID: id, started: time.Now(), down: down, up: up,
		byID: map[int]*entry{}, byHash: map[metainfo.Hash]*entry{}, nextID: 1,
		seedRatio: cfg.SeedRatio, seedTime: cfg.SeedTime,
	}
	if cfg.EnableDHT {
		if e.dht, err = openDHT(cfg, ln.Port(), log); err != nil {
			ln.Close()
			return nil, err
		}
	}
	if err := e.load(); err != nil {
		e.closeShared()
		return nil, err
	}
	e.mu.Lock()
	e.scheduleLocked()
	e.mu.Unlock()
	return e, nil
}

// listen opens the peer port, falling back to a random one if it's taken
// (e.g. another client is running).
func listen(addr string, opts peer.Options, log *slog.Logger) (*torrent.Listener, error) {
	ln, err := torrent.Listen(addr, opts, log)
	host, port, serr := net.SplitHostPort(addr)
	if err == nil || serr != nil || port == "0" {
		return ln, err
	}
	log.Warn("peer port unavailable; using a random port", "addr", addr, "err", err)
	return torrent.Listen(net.JoinHostPort(host, "0"), opts, log)
}

// openDHT starts the DHT node on UDP port `port` (the peer port's number,
// as most clients do), or a random one if that's taken.
func openDHT(cfg Config, port uint16, log *slog.Logger) (*dht.Server, error) {
	host, _, _ := net.SplitHostPort(cfg.ListenAddr)
	dcfg := dht.Config{
		Addr:      net.JoinHostPort(host, strconv.Itoa(int(port))),
		Bootstrap: cfg.DHTBootstrap,
		StateFile: filepath.Join(cfg.StateDir, "dht.json"),
		Logger:    log,
	}
	d, err := dht.Open(dcfg)
	if err != nil {
		log.Warn("DHT port unavailable; using a random port", "addr", dcfg.Addr, "err", err)
		dcfg.Addr = net.JoinHostPort(host, "0")
		d, err = dht.Open(dcfg)
	}
	if err != nil {
		return nil, fmt.Errorf("engine: %w", err)
	}
	return d, nil
}

// closeShared closes what all torrents share, once none is running.
func (e *Engine) closeShared() {
	if e.dht != nil {
		e.dht.Close() // saves its routing table for the next start
	}
	e.ln.Close()
}

// load restores every torrent in the resume store. Broken entries are
// logged and skipped (their files are left alone).
func (e *Engine) load() error {
	list, err := e.store.List()
	if err != nil {
		return err
	}
	var needID []*entry
	for _, en := range list {
		if en.Err != nil {
			e.log.Warn("skipping unreadable resume entry", "err", en.Err)
			continue
		}
		t, err := e.restoreEntry(en)
		if err != nil {
			e.log.Warn("skipping saved torrent", "name", en.State.Name, "err", err)
			continue
		}
		e.byHash[t.hash] = t
		if id := en.State.ID; id > 0 && e.byID[id] == nil {
			t.id = id
			e.byID[id] = t
			e.nextID = max(e.nextID, id+1)
		} else {
			needID = append(needID, t) // e.g. saved by `denco download`
		}
	}
	// List is oldest first, so new IDs follow the order torrents were added.
	for _, t := range needID {
		t.id = e.nextID
		e.nextID++
		e.byID[t.id] = t
		t.state.ID = t.id
		if err := e.store.Save(&t.state, t.raw()); err != nil {
			e.log.Warn("saving torrent ID", "name", t.state.Name, "err", err)
		}
	}
	return nil
}

// restoreEntry rebuilds a torrent from its saved state (and .torrent, if
// it has metadata).
func (e *Engine) restoreEntry(en resume.Entry) (*entry, error) {
	st := en.State
	t := &entry{state: *st, paused: st.Paused, uploadsPaused: st.UploadsPaused, seedMode: st.Seed, finished: st.Finished}
	if en.Meta == nil { // magnet link still waiting for metadata
		h, err := hashFromHex(en.State.InfoHash)
		t.hash = h
		return t, err
	}
	layout, err := storage.NewLayout(en.State.DownloadDir, &en.Meta.Info)
	if err != nil {
		return nil, err
	}
	t.hash, t.mi, t.layout = en.Meta.InfoHash, en.Meta, layout
	t.have = savedHave(&t.state, &en.Meta.Info)
	return t, nil
}

// peerOpts are the connection options for sessions, the listener and
// metadata fetchers.
func (e *Engine) peerOpts() peer.Options {
	o := e.cfg.Session.PeerOpts
	o.PeerID, o.Extensions, o.ReadLimiter, o.DHT = e.peerID, true, e.down, e.dht != nil
	return o
}

// savedHave decodes a state's bitfield; a broken one counts as nothing.
func savedHave(st *resume.State, info *metainfo.Info) *bitfield.Bitfield {
	if b, err := bitfield.FromBytes(st.Have, info.NumPieces()); err == nil {
		return b
	}
	return bitfield.New(info.NumPieces())
}

// Close stops every torrent (each makes a final clean save, so they resume
// on the next start), then closes the peer port. Safe to call twice.
func (e *Engine) Close() error {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		for _, t := range e.byID {
			if t.running {
				t.cancel()
			}
		}
		e.mu.Unlock()
		e.wg.Wait()
		e.closeShared()
	})
	return nil
}

// SetLimits changes the total download and upload limits (bytes/s,
// 0 = unlimited) for every torrent, effective immediately.
func (e *Engine) SetLimits(down, up int64) {
	e.down.SetRate(down)
	e.up.SetRate(up)
	e.log.Info("speed limits changed", "download", down, "upload", up)
}

// --- queue ------------------------------------------------------------------------------

// scheduleLocked starts every torrent that should be running: all
// unpaused complete ones (seeding), and incomplete ones up to MaxActive, in
// ID order. Called whenever something changes. Caller holds e.mu.
func (e *Engine) scheduleLocked() {
	if e.closed {
		return
	}
	active := 0
	for _, t := range e.byID {
		if t.running && !t.completeLocked() {
			active++
		}
	}
	for _, t := range e.sortedLocked() {
		if t.running || t.paused || t.removing || t.err != nil {
			continue
		}
		complete := t.completeLocked()
		if t.finished && !complete { // its selection grew: download again
			t.finished = false
			e.persistLocked(t)
		}
		if t.finished {
			continue
		}
		if complete {
			// Done seeding already (limit reached, mode off)? Then don't
			// start it just to stop it.
			if reason := e.seedStopLocked(t); reason != "" {
				t.finished = true
				e.persistLocked(t)
				e.log.Info("not seeding", "id", t.id, "torrent", t.name(), "reason", reason)
				continue
			}
		}
		if !complete {
			if active >= e.cfg.MaxActive {
				continue // stays queued
			}
			active++
		}
		e.startLocked(t)
	}
}

// completeLocked reports whether every piece is verified (or, before a
// check, was at the last save).
func (t *entry) completeLocked() bool {
	if t.sess != nil {
		select {
		case <-t.sess.Completed():
			return true
		default:
			return false
		}
	}
	return t.mi != nil && t.have.Covers(t.wantLocked())
}

// skipLocked is the per-file skip flags (nil = every file wanted).
func (t *entry) skipLocked() []bool {
	if t.mi == nil {
		return nil
	}
	return pieces.SkipSet(t.state.Skip, len(t.mi.Info.Files))
}

// wantLocked is the set of pieces of the wanted files (nil = all).
func (t *entry) wantLocked() *bitfield.Bitfield {
	if t.mi == nil {
		return nil
	}
	return pieces.Wanted(&t.mi.Info, t.skipLocked())
}

func (e *Engine) sortedLocked() []*entry {
	out := make([]*entry, 0, len(e.byID))
	for _, t := range e.byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func (e *Engine) startLocked(t *entry) {
	ctx, cancel := context.WithCancel(context.Background())
	t.running, t.checking = true, true
	t.cancel = cancel
	t.done = make(chan struct{})
	t.err = nil
	t.seeded.Store(0)
	state := t.state // copy: the run goroutine works from this
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(t.done)
		defer cancel()
		res := e.run(ctx, t, state)
		e.finish(ctx, t, res)
	}()
}

// --- running one torrent -------------------------------------------------------------------

type runResult struct {
	state    resume.State       // final saved state; zero if nothing was saved
	have     *bitfield.Bitfield // nil if the session never started
	up, down int64              // payload this run
	err      error
}

// run opens the torrent's storage, works out which pieces are good, then
// runs the session until ctx is cancelled or it fails. It saves resume data
// on start, periodically, on completion, and finally (clean) after the
// session has stopped.
func (e *Engine) run(ctx context.Context, t *entry, state resume.State) runResult {
	// Only this goroutine ever sets t.mi, so reading it here needs no lock.
	if t.mi == nil {
		var err error
		if state, err = e.fetchMetadata(ctx, t, state); err != nil {
			return runResult{err: err}
		}
	}
	st, have, err := e.prepare(ctx, t, state)
	if err != nil {
		return runResult{err: err}
	}
	defer st.Close()

	cfg := e.cfg.Session
	cfg.PeerID, cfg.Port, cfg.Logger, cfg.NoSeed = e.peerID, e.ln.Port(), e.log, false
	cfg.PeerOpts, cfg.UploadLimiter = e.peerOpts(), e.up
	if e.dht != nil { // only when set: a nil *dht.Server in the interface isn't nil
		cfg.DHT = e.dht
	}
	prev := state
	prev.Fresh, prev.Existing, prev.Paused = false, false, false // from now on the files are ours

	// Session and saver are created and registered in one step under the
	// lock, with the file selection as it is *now*: SetFilesWanted may have
	// changed it while the data was being checked.
	e.mu.Lock()
	prev.Skip = append([]int(nil), t.state.Skip...)
	t.flagsTo(&prev)
	cfg.Wanted = t.wantLocked()
	cfg.UploadsPaused = t.uploadsPaused
	sess := torrent.New(t.mi, st, have, cfg)
	sess.AddPeers(t.bootstrap)
	t.bootstrap = nil
	saver := resume.NewSaver(e.store, t.mi, t.layout, st, &prev)
	snapshot := func() resume.Snapshot {
		s := sess.Stats()
		return resume.Snapshot{Have: sess.Have(), Uploaded: s.Uploaded, Downloaded: s.Downloaded,
			SeedingTime: time.Duration(t.seeded.Load())}
	}
	t.sess, t.saver, t.snapshot, t.checking = sess, saver, snapshot, false
	e.mu.Unlock()

	logErr := func(err error) { e.log.Warn("saving resume data", "torrent", t.mi.Info.Name, "err", err) }
	if err := saver.Save(snapshot(), false); err != nil {
		logErr(err)
	}
	e.ln.Add(sess)
	defer e.ln.Remove(sess)

	hctx, stopHelpers := context.WithCancel(ctx)
	var helpers sync.WaitGroup
	helpers.Add(2)
	go func() {
		defer helpers.Done()
		saver.Run(hctx, e.cfg.SaveEvery, snapshot, logErr)
	}()
	go func() { // on completion: save now, let the next download start, then watch seeding
		defer helpers.Done()
		select {
		case <-sess.Completed():
		case <-hctx.Done():
			return
		}
		if err := saver.Save(snapshot(), false); err != nil {
			logErr(err)
		}
		e.mu.Lock()
		e.scheduleLocked()
		e.mu.Unlock()
		e.watchSeeding(hctx, t)
	}()

	runErr := sess.Run(ctx)
	stopHelpers()
	helpers.Wait()

	e.mu.Lock()
	saver.Update(t.flagsTo)
	e.mu.Unlock()
	// Run has returned, so nothing writes any more: this save may be clean.
	final := snapshot()
	if err := saver.Save(final, runErr == nil); err != nil {
		logErr(err)
	}
	return runResult{state: saver.State(), have: final.Have, up: final.Uploaded, down: final.Downloaded, err: runErr}
}

// prepare opens storage and decides which pieces can be trusted.
func (e *Engine) prepare(ctx context.Context, t *entry, state resume.State) (*storage.Storage, *bitfield.Bitfield, error) {
	info := &t.mi.Info
	// A torrent that never ran must not adopt files it didn't create,
	// unless the user said the data there is theirs to reuse.
	allow := !state.Fresh || state.Existing
	e.mu.Lock()
	skip := t.skipLocked()
	e.mu.Unlock()
	st, err := storage.Open(t.layout, storage.Options{AllowExisting: allow, Skipped: skip})
	if errors.Is(err, storage.ErrExists) {
		return nil, nil, fmt.Errorf("%w (remove the torrent and add it again with -existing to reuse that data)", err)
	}
	if err != nil {
		return nil, nil, err
	}
	var have *bitfield.Bitfield
	switch {
	case state.Fresh && state.Existing:
		have, err = pieces.Recheck(ctx, st, info, nil)
	case state.Fresh:
		have = nil // nothing downloaded yet
	default:
		var rep resume.Report
		have, rep, err = resume.Restore(ctx, &state, t.mi, t.layout, st)
		for _, n := range rep.Notes {
			e.log.Info("resume check", "torrent", info.Name, "note", n)
		}
		if errors.Is(err, resume.ErrMismatch) {
			e.log.Warn("saved progress doesn't match; checking all data", "torrent", info.Name)
			have, err = pieces.Recheck(ctx, st, info, nil)
		}
	}
	if err != nil {
		st.Close()
		return nil, nil, err
	}
	return st, have, nil
}

// finish records the outcome of a run and lets the queue move on.
func (e *Engine) finish(ctx context.Context, t *entry, r runResult) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t.running, t.checking, t.sess, t.saver, t.snapshot, t.cancel = false, false, nil, nil, nil, nil
	if r.state.InfoHash != "" {
		skip := t.state.Skip // the live selection wins, even if the last save failed
		t.state = r.state
		t.state.Skip = skip
		t.seeded.Store(0) // now part of state.SeedSeconds
	}
	if r.have != nil {
		t.have = r.have
	}
	e.finished.up += r.up
	e.finished.down += r.down
	if r.err != nil && ctx.Err() == nil {
		t.err = r.err
		e.log.Error("torrent stopped", "torrent", t.name(), "err", r.err)
	}
	// A choice made before the session started (or after its final save)
	// isn't in the saved state yet.
	if t.flagsDiffer() && !t.removing {
		e.persistLocked(t)
	}
	e.scheduleLocked()
}

// persistLocked writes t's state with its current paused flag. Used for
// torrents that aren't running (a running one's Saver does this).
func (e *Engine) persistLocked(t *entry) {
	t.flagsTo(&t.state)
	if err := e.store.Save(&t.state, t.raw()); err != nil {
		e.log.Warn("saving torrent state", "torrent", t.name(), "err", err)
	}
}
