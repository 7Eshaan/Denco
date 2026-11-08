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
	"sort"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/pieces"
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
	// Session is the template for every torrent session (timeouts, peer
	// limits, announcer). PeerID, Port, Logger and NoSeed are set by the engine.
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

	mu       sync.Mutex
	byID     map[int]*entry
	byHash   map[metainfo.Hash]*entry
	nextID   int
	closed   bool
	finished struct{ up, down int64 } // payload moved by sessions that have ended

	wg        sync.WaitGroup // run goroutines
	closeOnce sync.Once
}

// entry is one torrent. All fields are guarded by Engine.mu; mi and layout
// never change after creation.
type entry struct {
	id     int
	mi     *metainfo.MetaInfo
	layout *storage.Layout

	state resume.State       // last persisted state (while running: as of start)
	have  *bitfield.Bitfield // pieces believed good while not running

	paused   bool
	removing bool  // Remove is waiting for the run goroutine
	err      error // why it last stopped; cleared by Resume

	// Set while the run goroutine is alive.
	running  bool
	checking bool // opening storage / checking saved data
	cancel   context.CancelFunc
	done     chan struct{} // closed when the run goroutine has exited
	sess     *torrent.Session
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
	ln, err := listen(cfg.ListenAddr, peer.Options{PeerID: id}, log)
	if err != nil {
		return nil, fmt.Errorf("engine: peer port: %w", err)
	}
	e := &Engine{
		cfg: cfg, log: log, store: store, ln: ln, peerID: id, started: time.Now(),
		byID: map[int]*entry{}, byHash: map[metainfo.Hash]*entry{}, nextID: 1,
	}
	if err := e.load(); err != nil {
		ln.Close()
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
		layout, err := storage.NewLayout(en.State.DownloadDir, &en.Meta.Info)
		if err != nil {
			e.log.Warn("skipping torrent with unusable layout", "name", en.State.Name, "err", err)
			continue
		}
		t := &entry{mi: en.Meta, layout: layout, state: *en.State, paused: en.State.Paused}
		t.have = savedHave(&t.state, &en.Meta.Info)
		e.byHash[en.Meta.InfoHash] = t
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
		if err := e.store.Save(&t.state, t.mi.Raw); err != nil {
			e.log.Warn("saving torrent ID", "name", t.state.Name, "err", err)
		}
	}
	return nil
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
		e.ln.Close()
	})
	return nil
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
		if !t.completeLocked() {
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
	return t.have.Full()
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
	st, have, err := e.prepare(ctx, t, state)
	if err != nil {
		return runResult{err: err}
	}
	defer st.Close()

	cfg := e.cfg.Session
	cfg.PeerID, cfg.Port, cfg.Logger, cfg.NoSeed = e.peerID, e.ln.Port(), e.log, false
	sess := torrent.New(t.mi, st, have, cfg)

	prev := state
	prev.Fresh, prev.Existing, prev.Paused = false, false, false // from now on the files are ours
	saver := resume.NewSaver(e.store, t.mi, t.layout, st, &prev)
	snapshot := func() resume.Snapshot {
		s := sess.Stats()
		return resume.Snapshot{Have: sess.Have(), Uploaded: s.Uploaded, Downloaded: s.Downloaded}
	}
	logErr := func(err error) { e.log.Warn("saving resume data", "torrent", t.mi.Info.Name, "err", err) }
	if err := saver.Save(snapshot(), false); err != nil {
		logErr(err)
	}

	e.mu.Lock()
	t.sess, t.checking = sess, false
	e.mu.Unlock()
	e.ln.Add(sess)
	defer e.ln.Remove(sess)

	hctx, stopHelpers := context.WithCancel(ctx)
	var helpers sync.WaitGroup
	helpers.Add(2)
	go func() {
		defer helpers.Done()
		saver.Run(hctx, e.cfg.SaveEvery, snapshot, logErr)
	}()
	go func() { // on completion: save now, and let the next download start
		defer helpers.Done()
		select {
		case <-sess.Completed():
			if err := saver.Save(snapshot(), false); err != nil {
				logErr(err)
			}
			e.mu.Lock()
			e.scheduleLocked()
			e.mu.Unlock()
		case <-hctx.Done():
		}
	}()

	runErr := sess.Run(ctx)
	stopHelpers()
	helpers.Wait()

	e.mu.Lock()
	saver.SetPaused(t.paused)
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
	st, err := storage.Open(t.layout, storage.Options{AllowExisting: allow})
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
	t.running, t.checking, t.sess, t.cancel = false, false, nil, nil
	if r.state.InfoHash != "" {
		t.state = r.state
	}
	if r.have != nil {
		t.have = r.have
	}
	e.finished.up += r.up
	e.finished.down += r.down
	if r.err != nil && ctx.Err() == nil {
		t.err = r.err
		e.log.Error("torrent stopped", "torrent", t.mi.Info.Name, "err", r.err)
	}
	// Paused (or resumed) before the session started: nothing saved it yet.
	if t.state.Paused != t.paused && !t.removing {
		e.persistLocked(t)
	}
	e.scheduleLocked()
}

// persistLocked writes t's state with its current paused flag. Used for
// torrents that aren't running (a running one's Saver does this).
func (e *Engine) persistLocked(t *entry) {
	t.state.Paused = t.paused
	if err := e.store.Save(&t.state, t.mi.Raw); err != nil {
		e.log.Warn("saving torrent state", "torrent", t.mi.Info.Name, "err", err)
	}
}
