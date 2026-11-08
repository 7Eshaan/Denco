package ipc

import (
	"context"
	"crypto/sha1"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/engine"
)

func testTorrent(t *testing.T, name string) []byte {
	t.Helper()
	data := []byte(strings.Repeat(name, 100))
	h := sha1.Sum(data)
	raw, err := bencode.Encode(map[string]any{"info": map[string]any{
		"name": name, "piece length": 16384, "length": len(data), "pieces": string(h[:]),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// setup runs a real engine behind the API and returns a client for it.
func setup(t *testing.T) (*Client, *atomic.Int32, string) {
	t.Helper()
	tmp := t.TempDir()
	e, err := engine.Open(engine.Config{
		StateDir: filepath.Join(tmp, "state"), DownloadDir: filepath.Join(tmp, "dl"),
		ListenAddr: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	var stops atomic.Int32
	token := NewToken()
	srv := httptest.NewServer(NewHandler(e, token, func() { stops.Add(1) }, nil))
	t.Cleanup(srv.Close)
	return NewClient(Info{Addr: srv.Listener.Addr().String(), Token: token}), &stops, tmp
}

func TestAPIRoundTrip(t *testing.T) {
	c, stops, tmp := setup(t)
	ctx := context.Background()

	if list, err := c.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %v, %v", list, err)
	}
	dir := filepath.Join(tmp, "custom")
	added, err := c.Add(ctx, AddRequest{Torrent: testTorrent(t, "one"), Dir: dir, Paused: true})
	if err != nil {
		t.Fatal(err)
	}
	if added.ID != 1 || added.Name != "one" || added.State != engine.StatePaused || added.Dir != dir {
		t.Fatalf("added = %+v", added)
	}

	// Duplicate: 409 with the existing ID.
	_, err = c.Add(ctx, AddRequest{Torrent: testTorrent(t, "one")})
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != CodeDuplicate || apiErr.ID != 1 || apiErr.Status != http.StatusConflict {
		t.Fatalf("duplicate: %#v", err)
	}
	// Invalid torrent: 400.
	if _, err := c.Add(ctx, AddRequest{Torrent: []byte("junk")}); !errors.As(err, &apiErr) || apiErr.Code != CodeInvalid {
		t.Fatalf("invalid: %#v", err)
	}

	got, err := c.Get(ctx, added.InfoHash[:8])
	if err != nil || got.ID != 1 {
		t.Fatalf("get by hash = %+v, %v", got, err)
	}
	if _, err := c.Get(ctx, "#1"); err != nil { // '#' must be escaped in the URL
		t.Fatalf("get #1: %v", err)
	}
	if _, err := c.Get(ctx, "42"); !errors.As(err, &apiErr) || apiErr.Code != CodeNotFound || apiErr.Status != 404 {
		t.Fatalf("missing: %#v", err)
	}

	files, err := c.Files(ctx, "1")
	if err != nil || len(files) != 1 || files[0].Path != "one" || files[0].Size != 300 {
		t.Fatalf("files = %+v, %v", files, err)
	}
	if peers, err := c.Peers(ctx, "1"); err != nil || len(peers) != 0 {
		t.Fatalf("peers = %v, %v", peers, err)
	}

	if r, err := c.Resume(ctx, "1"); err != nil || r.State == engine.StatePaused {
		t.Fatalf("resume = %v, %v", r.State, err)
	}
	if p, err := c.Pause(ctx, "1"); err != nil || p.State != engine.StatePaused {
		t.Fatalf("pause = %v, %v", p.State, err)
	}

	// A magnet link (nobody serves it here, so it waits for metadata).
	hash := strings.Repeat("ab", 20)
	m, err := c.Add(ctx, AddRequest{Magnet: "magnet:?xt=urn:btih:" + hash + "&dn=mag", Paused: true})
	if err != nil || m.Name != "mag" || m.HasMetadata || m.InfoHash != hash {
		t.Fatalf("magnet add = %+v, %v", m, err)
	}
	if _, err := c.Files(ctx, hash[:8]); !errors.As(err, &apiErr) || apiErr.Code != CodeNoMeta {
		t.Fatalf("files of bare magnet: %#v", err)
	}
	if _, err := c.Add(ctx, AddRequest{Magnet: "magnet:?dn=x"}); !errors.As(err, &apiErr) || apiErr.Code != CodeInvalid {
		t.Fatalf("bad magnet: %#v", err)
	}
	if _, err := c.Add(ctx, AddRequest{Magnet: "magnet:?xt=urn:btih:" + hash, Torrent: testTorrent(t, "x")}); !errors.As(err, &apiErr) || apiErr.Code != CodeInvalid {
		t.Fatalf("both: %#v", err)
	}
	c.Remove(ctx, hash[:8], false)

	st, err := c.Status(ctx)
	if err != nil || st.Torrents != 1 || st.PID != os.Getpid() || st.ListenPort == 0 {
		t.Fatalf("status = %+v, %v", st, err)
	}

	if st, err := c.SetLimits(ctx, 2_000_000, 0); err != nil || st.DownLimit != 2_000_000 || st.UpLimit != 0 {
		t.Fatalf("set limits: %+v, %v", st, err)
	}
	if _, err := c.SetLimits(ctx, -1, 0); !errors.As(err, &apiErr) || apiErr.Code != CodeInvalid {
		t.Fatalf("negative limit: %#v", err)
	}

	if _, err := c.Remove(ctx, "1", false); err != nil {
		t.Fatal(err)
	}
	if list, _ := c.List(ctx); len(list) != 0 {
		t.Fatalf("after remove: %v", list)
	}

	if err := c.Shutdown(ctx); err != nil || stops.Load() != 1 {
		t.Fatalf("shutdown: %v (calls %d)", err, stops.Load())
	}
}

func TestAPIRejectsBadToken(t *testing.T) {
	c, _, _ := setup(t)
	c.token = "wrong"
	_, err := c.List(context.Background())
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusUnauthorized || apiErr.Code != CodeAuth {
		t.Fatalf("err = %#v", err)
	}
	c.token = ""
	if _, err := c.Status(context.Background()); !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Fatalf("no token: %#v", err)
	}
}

func TestDaemonFileAndConnect(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "home", DaemonFile)
	ctx := context.Background()

	if _, err := Connect(ctx, path); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("no file: %v", err)
	}

	// A file left by a daemon that died: nobody answers there.
	dead := httptest.NewServer(http.NotFoundHandler())
	addr := dead.Listener.Addr().String()
	dead.Close()
	if err := WriteInfo(path, Info{Addr: addr, Token: "t", PID: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Connect(ctx, path); !errors.Is(err, ErrNoDaemon) {
		t.Fatalf("stale file: %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
			t.Errorf("daemon file mode = %v, want 0600", fi.Mode().Perm())
		}
	}

	// A live daemon.
	c, _, _ := setup(t)
	info := Info{Addr: strings.TrimPrefix(c.base, "http://"), Token: c.token}
	if err := WriteInfo(path, info); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadInfo(path); err != nil || got != info {
		t.Fatalf("ReadInfo = %+v, %v", got, err)
	}
	if _, err := Connect(ctx, path); err != nil {
		t.Fatalf("live: %v", err)
	}

	os.WriteFile(path, []byte("{"), 0o600)
	if _, err := ReadInfo(path); err == nil || errors.Is(err, ErrNoDaemon) {
		t.Fatalf("corrupt file: %v", err)
	}
}

func TestServeIsLoopbackOnly(t *testing.T) {
	srv, addr, err := Serve(http.NotFoundHandler(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	if !strings.HasPrefix(addr.String(), "127.0.0.1:") {
		t.Fatalf("addr = %s", addr)
	}
}

func TestAPIFileSelection(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()
	data := []byte(strings.Repeat("x", 20000))
	h1, h2 := sha1.Sum(data[:16384]), sha1.Sum(data[16384:])
	raw, _ := bencode.Encode(map[string]any{"info": map[string]any{
		"name": "two", "piece length": 16384, "pieces": string(h1[:]) + string(h2[:]),
		"files": []any{
			map[string]any{"length": 16384, "path": []any{"a"}},
			map[string]any{"length": 20000 - 16384, "path": []any{"b"}},
		},
	}})
	added, err := c.Add(ctx, AddRequest{Torrent: raw, Paused: true, Skip: []int{1}})
	if err != nil || added.SkippedFiles != 1 || added.WantedBytes != 16384 {
		t.Fatalf("add = %+v, %v", added, err)
	}
	files, err := c.SetFiles(ctx, "1", []int{1}, true)
	if err != nil || len(files) != 2 || files[1].Skipped {
		t.Fatalf("SetFiles = %+v, %v", files, err)
	}
	var apiErr *Error
	if _, err := c.SetFiles(ctx, "1", []int{0, 1}, false); !errors.As(err, &apiErr) || apiErr.Code != CodeInvalid || apiErr.Status != 400 {
		t.Errorf("skipping every file: %v", err)
	}
	if _, err := c.SetFiles(ctx, "9", []int{0}, false); !errors.As(err, &apiErr) || apiErr.Code != CodeNotFound {
		t.Errorf("unknown torrent: %v", err)
	}
}

func TestAPISeeding(t *testing.T) {
	c, _, _ := setup(t)
	ctx := context.Background()
	if _, err := c.Add(ctx, AddRequest{Torrent: testTorrent(t, "s"), Paused: true}); err != nil {
		t.Fatal(err)
	}
	if v, err := c.SetUploadsPaused(ctx, "1", true); err != nil || !v.UploadsPaused {
		t.Fatalf("SetUploadsPaused = %+v, %v", v, err)
	}
	if v, err := c.SetSeedMode(ctx, "1", "off"); err != nil || v.SeedMode != "off" {
		t.Fatalf("SetSeedMode = %+v, %v", v, err)
	}
	var apiErr *Error
	if _, err := c.SetSeedMode(ctx, "1", "maybe"); !errors.As(err, &apiErr) || apiErr.Code != CodeInvalid {
		t.Errorf("bad mode: %v", err)
	}
	st, err := c.SetSeedLimits(ctx, 2, time.Hour)
	if err != nil || st.SeedRatio != 2 || st.SeedTime != time.Hour {
		t.Fatalf("SetSeedLimits = %+v, %v", st, err)
	}
	if _, err := c.SetSeedLimits(ctx, -1, 0); !errors.As(err, &apiErr) || apiErr.Code != CodeInvalid {
		t.Errorf("negative limit: %v", err)
	}
}
