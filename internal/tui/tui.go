// Package tui is Denco's terminal dashboard (`denco tui`): a live list of
// torrents with details, files and peers, controlled from the keyboard.
//
// It is only a client: everything goes through the daemon's API (the same
// one the CLI uses), so there is no second engine, and quitting the TUI
// never stops the daemon. Standard library only (DECISIONS D55).
//
// Files:
//
//	tui.go            – Run: the event loop, refreshing, running operations
//	model.go          – the state and how events change it (no I/O)
//	render*.go        – drawing the dashboard, details and help
//	keys.go           – decoding keyboard input
//	screen.go         – drawing frames without flicker
//	text.go           – cell widths, truncation, colors
//	term*.go          – raw mode, size and resizes per OS
//
// Concurrency: Run's goroutine owns the model. An input goroutine, one
// refresh goroutine (so refreshes never overlap) and one goroutine per
// daemon operation send it events; none of them touch the model.
package tui

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// Client is the daemon API the TUI uses: *ipc.Client's methods, plus
// AddTorrent, which turns what the user typed (a path, magnet or URL) into
// an add exactly as `denco add` does.
type Client interface {
	Status(ctx context.Context) (ipc.Status, error)
	List(ctx context.Context) ([]engine.Torrent, error)
	Get(ctx context.Context, ref string) (engine.Torrent, error)
	Files(ctx context.Context, ref string) ([]engine.File, error)
	Peers(ctx context.Context, ref string) ([]engine.Peer, error)
	Pause(ctx context.Context, ref string) (engine.Torrent, error)
	Resume(ctx context.Context, ref string) (engine.Torrent, error)
	ForceStart(ctx context.Context, ref string) (engine.Torrent, error)
	MoveInQueue(ctx context.Context, ref, move string) (engine.Torrent, error)
	Remove(ctx context.Context, ref string, deleteFiles bool) (engine.Removed, error)
	AddTorrent(ctx context.Context, arg string) (engine.Torrent, error)
}

// Config configures Run.
type Config struct {
	Client Client // connected to the daemon
	// Reconnect finds the daemon again after the connection was lost (it
	// may have been restarted, with a new address). It must not start a
	// daemon: if the user stopped it, the TUI just says so.
	Reconnect func(ctx context.Context) (Client, error)
	Refresh   time.Duration // how often to refresh; default 1s
	Color     bool          // use colors (off for NO_COLOR)
}

const (
	requestTimeout = 10 * time.Second
	actionTimeout  = 2 * time.Minute // removing waits for the torrent to stop
)

// inputClosed: the keyboard went away (EOF), so nobody can quit any more.
type inputClosed struct{}

// Run shows the dashboard until the user quits, input ends or ctx is
// cancelled (signals). It leaves the alternate screen before returning,
// also on panics; the caller restores the terminal mode (Terminal.Restore).
func Run(ctx context.Context, term Terminal, cfg Config) error {
	if cfg.Refresh <= 0 {
		cfg.Refresh = time.Second
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer wg.Wait() // after cancel (defers run last-in, first-out)
	defer cancel()

	if _, err := io.WriteString(term, enterScreen); err != nil {
		return err
	}
	defer io.WriteString(term, leaveScreen)

	events := make(chan any, 64)
	send := func(ev any) bool {
		select {
		case events <- ev:
			return true
		case <-ctx.Done():
			return false
		}
	}
	// Not waited for: a blocked terminal read can't be interrupted, and the
	// process exits after Run anyway.
	go readInput(term, term.Raw(), send)

	lnk := &link{c: cfg.Client, reconnect: cfg.Reconnect}
	var detailID atomic.Int64
	kick := make(chan struct{}, 1)
	refreshNow := func() {
		select {
		case kick <- struct{}{}:
		default: // one is already queued
		}
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		fetchLoop(ctx, lnk, &detailID, kick, cfg.Refresh, send)
	}()

	w, h := term.Size()
	m := newModel(w, h, !term.Raw(), cfg.Color, time.Now())
	scr := newScreen(term)
	tick := time.NewTicker(time.Second) // clock, uptime, expiring messages
	defer tick.Stop()

	for {
		if err := scr.draw(m.render()); err != nil {
			return fmt.Errorf("drawing: %w", err)
		}
		if m.quit {
			return nil
		}
		var ev any
		select {
		case <-ctx.Done():
			return nil
		case ev = <-events:
		case <-term.Resized():
			w, h := term.Size()
			ev = resizeEvent{w, h}
			scr.invalidate()
		case now := <-tick.C:
			ev = tickEvent{now}
		}
		m.now = time.Now()
		if _, ok := ev.(inputClosed); ok {
			m.quit = true
		}
		before := m.detailID
		for _, a := range m.update(ev) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if send(perform(ctx, lnk, a)) {
					refreshNow() // show the effect at once
				}
			}()
		}
		if m.view == viewDetails {
			detailID.Store(int64(m.detailID))
		} else {
			detailID.Store(0)
		}
		if m.detailID != before && m.detailID != 0 {
			refreshNow() // fetch files and peers without waiting a second
		}
	}
}

// readInput turns terminal input into key events (raw mode) or line
// events (fallback), until input ends.
func readInput(r io.Reader, raw bool, send func(any) bool) {
	defer send(inputClosed{})
	if !raw {
		sc := bufio.NewScanner(r)
		for sc.Scan() {
			if !send(lineEvent{strings.TrimRight(sc.Text(), "\r")}) {
				return
			}
		}
		return
	}
	buf := make([]byte, 1024) // a pasted magnet link usually comes in one read
	for {
		n, err := r.Read(buf)
		for _, k := range parseKeys(buf[:n]) {
			if !send(keyEvent{k}) {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// --- talking to the daemon -----------------------------------------------------------------

// link holds the current daemon client. Only the refresh loop reconnects;
// operations use whatever is connected.
type link struct {
	mu        sync.Mutex
	c         Client
	reconnect func(ctx context.Context) (Client, error)
}

var errNotConnected = errors.New("not connected to the daemon")

func (l *link) current() Client {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.c
}

func (l *link) set(c Client) {
	l.mu.Lock()
	l.c = c
	l.mu.Unlock()
}

// connected returns the client, reconnecting first if the last refresh
// lost it.
func (l *link) connected(ctx context.Context) (Client, error) {
	if c := l.current(); c != nil {
		return c, nil
	}
	if l.reconnect == nil {
		return nil, errNotConnected
	}
	c, err := l.reconnect(ctx)
	if err != nil {
		return nil, err
	}
	l.set(c)
	return c, nil
}

// fetchLoop refreshes now, then every interval or when kicked, one
// refresh at a time.
func fetchLoop(ctx context.Context, lnk *link, detailID *atomic.Int64, kick <-chan struct{}, every time.Duration, send func(any) bool) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		snap, err := fetch(ctx, lnk, int(detailID.Load()))
		if ctx.Err() != nil || !send(dataEvent{snap, err}) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-kick:
		}
	}
}

// fetch gets the overview, the torrent list and, in the details view, one
// torrent with its files and peers. A failed status or list call means the
// daemon is gone: the client is dropped, and the next refresh reconnects.
func fetch(ctx context.Context, lnk *link, detailID int) (snapshot, error) {
	var snap snapshot
	c, err := lnk.connected(ctx)
	if err != nil {
		return snap, err
	}
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	if snap.status, err = c.Status(ctx); err == nil {
		snap.torrents, err = c.List(ctx)
	}
	if err != nil {
		lnk.set(nil)
		return snap, err
	}
	if detailID == 0 {
		return snap, nil
	}
	ref := strconv.Itoa(detailID)
	snap.detailID = detailID
	if snap.detail, snap.detailErr = c.Get(ctx, ref); snap.detailErr != nil {
		return snap, nil
	}
	if snap.detail.HasMetadata {
		snap.files, snap.detailErr = c.Files(ctx, ref)
	}
	if snap.detailErr == nil {
		snap.peers, snap.detailErr = c.Peers(ctx, ref)
	}
	return snap, nil
}

// perform runs one operation and describes the outcome.
func perform(ctx context.Context, lnk *link, a action) resultEvent {
	c := lnk.current()
	if c == nil {
		return resultEvent{err: errNotConnected}
	}
	ctx, cancel := context.WithTimeout(ctx, actionTimeout)
	defer cancel()
	ref := strconv.Itoa(a.id)
	switch a.kind {
	case actPause:
		t, err := c.Pause(ctx, ref)
		return result(err, "Paused #%d %s", t.ID, clean(t.Name))
	case actResume:
		t, err := c.Resume(ctx, ref)
		return result(err, "Resumed #%d %s (%s)", t.ID, clean(t.Name), strings.ToLower(stateLabel(t.State)))
	case actForce:
		t, err := c.ForceStart(ctx, ref)
		return result(err, "Force-started #%d %s (ignores max_active_downloads; f again queues it)", t.ID, clean(t.Name))
	case actUnforce:
		t, err := c.Resume(ctx, ref)
		return result(err, "#%d %s follows the queue again (position %d of %d)", t.ID, clean(t.Name), t.Queue, t.QueueLength)
	case actMove:
		t, err := c.MoveInQueue(ctx, ref, a.move)
		return result(err, "Moved #%d %s to queue position %d of %d", t.ID, clean(t.Name), t.Queue, t.QueueLength)
	case actRemove:
		r, err := c.Remove(ctx, ref, a.deleteFiles)
		what := "files kept"
		switch {
		case r.FilesDeleted:
			what = "files deleted"
		case a.deleteFiles:
			what = "nothing deleted: it never started, so no files there are its own"
		}
		return result(err, "Removed #%d %s (%s)", r.ID, clean(r.Name), what)
	default: // actAdd
		t, err := c.AddTorrent(ctx, a.arg)
		return result(err, "Added #%d %s → %s (%s)", t.ID, clean(t.Name), clean(t.Dir), strings.ToLower(stateLabel(t.State)))
	}
}

func result(err error, format string, args ...any) resultEvent {
	if err != nil {
		return resultEvent{err: err}
	}
	return resultEvent{text: fmt.Sprintf(format, args...)}
}
