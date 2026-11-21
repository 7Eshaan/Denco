package tui

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// fakeTerm is a terminal whose keyboard is a pipe and whose screen is a buffer.
type fakeTerm struct {
	keys *io.PipeWriter
	in   *io.PipeReader
	raw  bool

	mu      sync.Mutex
	out     bytes.Buffer
	w, h    int
	resized chan struct{}
}

func newFakeTerm(w, h int) *fakeTerm {
	r, wr := io.Pipe()
	return &fakeTerm{keys: wr, in: r, raw: true, w: w, h: h, resized: make(chan struct{}, 1)}
}

func (f *fakeTerm) Read(p []byte) (int, error) { return f.in.Read(p) }
func (f *fakeTerm) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.Write(p)
}
func (f *fakeTerm) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, f.h
}
func (f *fakeTerm) Resized() <-chan struct{} { return f.resized }
func (f *fakeTerm) Raw() bool                { return f.raw }
func (f *fakeTerm) Restore() error           { return nil }

func (f *fakeTerm) output() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.out.String()
}

func (f *fakeTerm) type_(t *testing.T, s string) {
	t.Helper()
	if _, err := f.keys.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
}

// fakeClient is an in-memory daemon.
type fakeClient struct {
	mu        sync.Mutex
	torrents  []engine.Torrent
	calls     []string
	down      bool          // every call fails like a stopped daemon
	block     chan struct{} // if set, Status waits for it
	inStatus  atomic.Int32
	maxStatus atomic.Int32
}

var errGone = errors.New("talking to the daemon: connection refused")

func (c *fakeClient) record(call string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, call)
	if c.down {
		return errGone
	}
	return nil
}

func (c *fakeClient) called(call string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, x := range c.calls {
		if x == call {
			return true
		}
	}
	return false
}

func (c *fakeClient) Status(ctx context.Context) (ipc.Status, error) {
	n := c.inStatus.Add(1)
	defer c.inStatus.Add(-1)
	for m := c.maxStatus.Load(); n > m && !c.maxStatus.CompareAndSwap(m, n); m = c.maxStatus.Load() {
	}
	if c.block != nil {
		select {
		case <-c.block:
		case <-ctx.Done():
			return ipc.Status{}, ctx.Err()
		}
	}
	st := ipc.Status{PID: 7}
	st.StartedAt = time.Now()
	return st, c.record("status")
}

func (c *fakeClient) List(context.Context) ([]engine.Torrent, error) {
	err := c.record("list")
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]engine.Torrent(nil), c.torrents...), err
}

func (c *fakeClient) find(ref string) (engine.Torrent, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, t := range c.torrents {
		if ref == strconv.Itoa(t.ID) {
			return t, nil
		}
	}
	return engine.Torrent{}, &ipc.Error{Code: ipc.CodeNotFound, Message: "no such torrent"}
}

func (c *fakeClient) setState(ref string, st engine.State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.torrents {
		if strconv.Itoa(c.torrents[i].ID) == ref {
			c.torrents[i].State = st
		}
	}
}

func (c *fakeClient) Get(_ context.Context, ref string) (engine.Torrent, error) {
	if err := c.record("get " + ref); err != nil {
		return engine.Torrent{}, err
	}
	return c.find(ref)
}
func (c *fakeClient) Files(_ context.Context, ref string) ([]engine.File, error) {
	return []engine.File{{Index: 0, Path: "the-file.bin", Size: 10, Done: 5}}, c.record("files " + ref)
}
func (c *fakeClient) Peers(_ context.Context, ref string) ([]engine.Peer, error) {
	return nil, c.record("peers " + ref)
}
func (c *fakeClient) Pause(_ context.Context, ref string) (engine.Torrent, error) {
	if err := c.record("pause " + ref); err != nil {
		return engine.Torrent{}, err
	}
	c.setState(ref, engine.StatePaused)
	return c.find(ref)
}
func (c *fakeClient) Resume(_ context.Context, ref string) (engine.Torrent, error) {
	if err := c.record("resume " + ref); err != nil {
		return engine.Torrent{}, err
	}
	c.setState(ref, engine.StateDownloading)
	return c.find(ref)
}
func (c *fakeClient) ForceStart(_ context.Context, ref string) (engine.Torrent, error) {
	if err := c.record("force " + ref); err != nil {
		return engine.Torrent{}, err
	}
	return c.find(ref)
}
func (c *fakeClient) MoveInQueue(_ context.Context, ref, move string) (engine.Torrent, error) {
	if err := c.record("queue " + move + " " + ref); err != nil {
		return engine.Torrent{}, err
	}
	return c.find(ref)
}
func (c *fakeClient) SetTorrentLimits(_ context.Context, ref string, down, up int64) (engine.Torrent, error) {
	if err := c.record(fmt.Sprintf("limits %s %d %d", ref, down, up)); err != nil {
		return engine.Torrent{}, err
	}
	return c.find(ref)
}
func (c *fakeClient) Remove(_ context.Context, ref string, del bool) (engine.Removed, error) {
	call := "remove " + ref
	if del {
		call += " -delete"
	}
	if err := c.record(call); err != nil {
		return engine.Removed{}, err
	}
	t, err := c.find(ref)
	c.mu.Lock()
	var keep []engine.Torrent
	for _, x := range c.torrents {
		if x.ID != t.ID {
			keep = append(keep, x)
		}
	}
	c.torrents = keep
	c.mu.Unlock()
	return engine.Removed{Torrent: t, FilesDeleted: del}, err
}
func (c *fakeClient) AddTorrent(_ context.Context, arg string) (engine.Torrent, error) {
	if err := c.record("add " + arg); err != nil {
		return engine.Torrent{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	t := engine.Torrent{ID: len(c.torrents) + 1, Name: "added-by-tui", State: engine.StateDownloading, HasMetadata: true}
	c.torrents = append(c.torrents, t)
	return t, nil
}

// runAsync starts Run and returns a channel with its result.
func runAsync(ctx context.Context, term Terminal, cfg Config) chan error {
	done := make(chan error, 1)
	go func() { done <- Run(ctx, term, cfg) }()
	return done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitDone(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run didn't return")
	}
}

func TestRunShowsLiveDataAndRunsActions(t *testing.T) {
	term := newFakeTerm(120, 24)
	c := &fakeClient{torrents: fakeTorrents(3)}
	done := runAsync(context.Background(), term, Config{Client: c, Refresh: 20 * time.Millisecond, Color: true})

	waitFor(t, "the list on screen", func() bool { return strings.Contains(term.output(), "torrent-01-downlo") })
	if !strings.HasPrefix(term.output(), enterScreen) {
		t.Error("Run should switch to the alternate screen first")
	}

	term.type_(t, "p")
	waitFor(t, "pause #1", func() bool { return c.called("pause 1") })
	waitFor(t, "the paused state on screen", func() bool { return strings.Contains(term.output(), "Paused #1") })

	term.type_(t, "\x1b[B") // down
	term.type_(t, "r")
	waitFor(t, "resume #2", func() bool { return c.called("resume 2") })

	term.type_(t, "d")
	term.type_(t, "y")
	waitFor(t, "remove #2", func() bool { return c.called("remove 2") })

	term.type_(t, "a")
	term.type_(t, "~/some.torrent\r")
	waitFor(t, "add", func() bool { return c.called("add ~/some.torrent") })
	waitFor(t, "the added torrent", func() bool { return strings.Contains(term.output(), "added-by-tui") })

	term.type_(t, "\r") // details of the selected torrent
	waitFor(t, "files fetched for the details view", func() bool { return c.called("files 3") || c.called("files 1") })
	waitFor(t, "a file on screen", func() bool { return strings.Contains(term.output(), "the-file.bin") })

	term.type_(t, "q")
	waitDone(t, done)
	if !strings.HasSuffix(term.output(), leaveScreen) {
		t.Errorf("Run should leave the alternate screen last: %q", term.output()[max(0, len(term.output())-40):])
	}
	if c.called("shutdown") {
		t.Error("quitting must not stop the daemon")
	}
}

func TestRefreshesNeverOverlapAndKeysStayResponsive(t *testing.T) {
	term := newFakeTerm(100, 20)
	c := &fakeClient{torrents: fakeTorrents(2), block: make(chan struct{})}
	done := runAsync(context.Background(), term, Config{Client: c, Refresh: time.Millisecond})

	// The daemon hangs on the first refresh; many refresh intervals pass.
	time.Sleep(50 * time.Millisecond)
	term.type_(t, "?") // the UI still reacts
	waitFor(t, "help while a refresh hangs", func() bool { return strings.Contains(term.output(), "this help") })
	for i := 0; i < 20; i++ {
		c.block <- struct{}{} // let refreshes through one by one
	}
	if n := c.maxStatus.Load(); n != 1 {
		t.Errorf("%d refreshes ran at once", n)
	}
	term.type_(t, "q") // closes help
	term.type_(t, "q")
	close(c.block)
	waitDone(t, done)
}

func TestReconnectsAfterTheDaemonComesBack(t *testing.T) {
	term := newFakeTerm(100, 20)
	old := &fakeClient{torrents: fakeTorrents(1)}
	fresh := &fakeClient{torrents: fakeTorrents(2)}
	var allow atomic.Bool
	var tries atomic.Int32
	reconnect := func(context.Context) (Client, error) {
		tries.Add(1)
		if !allow.Load() {
			return nil, ipc.ErrNoDaemon
		}
		return fresh, nil
	}
	done := runAsync(context.Background(), term, Config{Client: old, Reconnect: reconnect, Refresh: 10 * time.Millisecond})
	waitFor(t, "first data", func() bool { return strings.Contains(term.output(), "torrent-01") })

	old.mu.Lock()
	old.down = true // daemon stopped
	old.mu.Unlock()
	waitFor(t, "disconnected state", func() bool { return strings.Contains(term.output(), "disconnected") })
	waitFor(t, "reconnect attempts", func() bool { return tries.Load() >= 2 })
	term.type_(t, "p")
	waitFor(t, "refusal while disconnected", func() bool { return strings.Contains(term.output(), "Not connected to the daemon") })

	allow.Store(true) // daemon restarted
	waitFor(t, "data from the new daemon", func() bool { return strings.Contains(term.output(), "torrent-02") })
	term.type_(t, "q")
	waitDone(t, done)
}

func TestResizeRedrawsAtTheNewSize(t *testing.T) {
	term := newFakeTerm(100, 20)
	c := &fakeClient{torrents: fakeTorrents(1)}
	done := runAsync(context.Background(), term, Config{Client: c, Refresh: 10 * time.Millisecond})
	waitFor(t, "first frame", func() bool { return strings.Contains(term.output(), "torrent-01") })
	term.mu.Lock()
	term.w, term.h = 20, 5
	term.out.Reset()
	term.mu.Unlock()
	term.resized <- struct{}{}
	waitFor(t, "too-small frame", func() bool { return strings.Contains(term.output(), "Terminal") })
	if !strings.Contains(term.output(), clearScreen) {
		t.Error("a resize should clear and redraw")
	}
	term.type_(t, "q")
	waitDone(t, done)
}

func TestEndOfInputOrCancelEndsRun(t *testing.T) {
	term := newFakeTerm(80, 20)
	done := runAsync(context.Background(), term, Config{Client: &fakeClient{}})
	term.keys.Close() // keyboard gone (EOF)
	waitDone(t, done)

	ctx, cancel := context.WithCancel(context.Background())
	term = newFakeTerm(80, 20)
	done = runAsync(ctx, term, Config{Client: &fakeClient{}})
	cancel() // SIGTERM / SIGHUP
	waitDone(t, done)
	if !strings.HasSuffix(term.output(), leaveScreen) {
		t.Error("cancelled Run should still leave the alternate screen")
	}
}

func TestLineModeTerminal(t *testing.T) {
	term := newFakeTerm(100, 20)
	term.raw = false
	c := &fakeClient{torrents: fakeTorrents(2)}
	done := runAsync(context.Background(), term, Config{Client: c, Refresh: 10 * time.Millisecond})
	waitFor(t, "line-mode hint", func() bool { return strings.Contains(term.output(), "line mode") })
	term.type_(t, "jp\n")
	waitFor(t, "pause #2 from a typed line", func() bool { return c.called("pause 2") })
	term.type_(t, "q\n")
	waitDone(t, done)
}
