package engine

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/resume"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/torrent"
)

// --- fixtures ----------------------------------------------------------------------

type fixture struct {
	raw  []byte
	data []byte
	mi   *metainfo.MetaInfo
}

// newFixture builds a multi-file torrent with random content. tracker may be "".
func newFixture(t *testing.T, name, tracker string, pieceLen int, sizes ...int) *fixture {
	t.Helper()
	total := 0
	for _, n := range sizes {
		total += n
	}
	data := make([]byte, total)
	r := rand.New(rand.NewPCG(uint64(total), uint64(len(name))))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	var hashes []byte
	for off := 0; off < total; off += pieceLen {
		h := sha1.Sum(data[off:min(off+pieceLen, total)])
		hashes = append(hashes, h[:]...)
	}
	var files []any
	for i, n := range sizes {
		files = append(files, map[string]any{"length": n, "path": []any{"sub", "file" + strconv.Itoa(i)}})
	}
	root := map[string]any{"info": map[string]any{
		"name": name, "piece length": pieceLen, "pieces": string(hashes), "files": files,
	}}
	if tracker != "" {
		root["announce"] = tracker
	}
	raw, err := bencode.Encode(root)
	if err != nil {
		t.Fatal(err)
	}
	mi, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{raw: raw, data: data, mi: mi}
}

func (f *fixture) path(dir string, i int) string {
	return filepath.Join(append([]string{dir, f.mi.Info.Name}, f.mi.Info.Files[i].Path...)...)
}

// writeData puts the complete torrent content under dir.
func (f *fixture) writeData(t *testing.T, dir string) {
	t.Helper()
	for i, file := range f.mi.Info.Files {
		p := f.path(dir, i)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, f.data[file.Offset:file.Offset+file.Length], 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func (f *fixture) checkData(t *testing.T, dir string) {
	t.Helper()
	for i, file := range f.mi.Info.Files {
		got, err := os.ReadFile(f.path(dir, i))
		if err != nil || !bytes.Equal(got, f.data[file.Offset:file.Offset+file.Length]) {
			t.Fatalf("file %d wrong on disk (err=%v)", i, err)
		}
	}
}

// newTracker is an HTTP tracker that returns every peer that announced
// for the same info hash (as 127.0.0.1:<announced port>).
func newTracker(t *testing.T) string {
	var mu sync.Mutex
	swarms := map[string][]netip.AddrPort{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		port, _ := strconv.Atoi(q.Get("port"))
		me := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(port))
		mu.Lock()
		h := q.Get("info_hash")
		if q.Get("event") != "stopped" && !slices.Contains(swarms[h], me) {
			swarms[h] = append(swarms[h], me)
		}
		var compact []byte
		for _, p := range swarms[h] {
			ip := p.Addr().As4()
			compact = binary.BigEndian.AppendUint16(append(compact, ip[:]...), p.Port())
		}
		mu.Unlock()
		body, _ := bencode.Encode(map[string]any{"interval": 60, "peers": string(compact)})
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/announce"
}

func testConfig(stateDir, dlDir string) Config {
	return Config{
		StateDir:    stateDir,
		DownloadDir: dlDir,
		ListenAddr:  "127.0.0.1:0",
		SaveEvery:   50 * time.Millisecond,
		Session: torrent.Config{
			Tick:           20 * time.Millisecond,
			RetryBase:      50 * time.Millisecond,
			AnnounceRetry:  50 * time.Millisecond,
			RequestTimeout: 2 * time.Second,
		},
	}
}

func openEngine(t *testing.T, cfg Config) *Engine {
	t.Helper()
	e, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// waitState waits until torrent ref is in one of the given states.
func waitState(t *testing.T, e *Engine, ref string, want ...State) Torrent {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, err := e.Get(ref)
		if err == nil && slices.Contains(want, v.State) {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("torrent %s: state %q (err %v, %q), want %v", ref, v.State, err, v.Error, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// waitStopped waits until no run goroutine is active for torrent ref.
func waitStopped(t *testing.T, e *Engine, ref string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		en, err := e.resolveLocked(ref)
		running := err == nil && en.running
		e.mu.Unlock()
		if !running {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("torrent %s still running", ref)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// --- tests --------------------------------------------------------------------------

func TestAddValidatesAndDetectsDuplicates(t *testing.T) {
	tmp := t.TempDir()
	dl := filepath.Join(tmp, "new", "downloads") // created on demand
	e := openEngine(t, testConfig(filepath.Join(tmp, "state"), dl))
	f := newFixture(t, "alpha", "", 16384, 20000, 5000)

	v, err := e.Add(f.raw, AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != 1 || v.Name != "alpha" || v.TotalBytes != 25000 || v.PiecesTotal != 2 || v.Files != 2 {
		t.Fatalf("view = %+v", v)
	}
	if v.Dir != dl {
		t.Errorf("dir = %q, want %q", v.Dir, dl)
	}
	if fi, err := os.Stat(dl); err != nil || !fi.IsDir() {
		t.Fatal("download directory not created")
	}

	var dup *DuplicateError
	if _, err := e.Add(f.raw, AddOptions{}); !errors.As(err, &dup) || dup.ID != 1 {
		t.Fatalf("second add: err = %v, want DuplicateError for #1", err)
	}
	if _, err := e.Add([]byte("not a torrent"), AddOptions{}); !errors.Is(err, metainfo.ErrInvalid) {
		t.Fatalf("garbage: err = %v", err)
	}

	// Files already on disk that we didn't create: refused without -existing.
	other := newFixture(t, "beta", "", 16384, 1000)
	other.writeData(t, dl)
	if _, err := e.Add(other.raw, AddOptions{}); !errors.Is(err, storage.ErrExists) {
		t.Fatalf("existing files: err = %v", err)
	}
	if len(e.List()) != 1 {
		t.Fatalf("list = %v", e.List())
	}

	// Lookups by ID, #ID and hash prefix.
	for _, ref := range []string{"1", "#1", f.mi.InfoHash.String()[:6], f.mi.InfoHash.String()} {
		if got, err := e.Get(ref); err != nil || got.ID != 1 {
			t.Errorf("Get(%q) = %v, %v", ref, got, err)
		}
	}
	for _, ref := range []string{"2", "abc", "ffffffff"} {
		if _, err := e.Get(ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Get(%q): err = %v, want ErrNotFound", ref, err)
		}
	}
}

func TestAmbiguousHashPrefix(t *testing.T) {
	tmp := t.TempDir()
	e := openEngine(t, testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl")))
	// Find two torrents whose info hashes share the first 4 hex characters
	// (birthday paradox: a few hundred tries).
	seen := map[string]*fixture{}
	var a, b *fixture
	for i := 0; a == nil; i++ {
		f := newFixture(t, fmt.Sprintf("t%d", i), "", 16384, 100)
		p := f.mi.InfoHash.String()[:4]
		if prev := seen[p]; prev != nil {
			a, b = prev, f
		}
		seen[p] = f
	}
	for _, f := range []*fixture{a, b} {
		if _, err := e.Add(f.raw, AddOptions{Paused: true}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.Get(a.mi.InfoHash.String()[:4]); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("err = %v, want ErrAmbiguous", err)
	}
	if v, err := e.Get(a.mi.InfoHash.String()); err != nil || v.Name != a.mi.Info.Name {
		t.Fatalf("full hash: %v, %v", v, err)
	}
}

func TestTransferBetweenEngines(t *testing.T) {
	tracker := newTracker(t)
	f := newFixture(t, "movie", tracker, 16384, 100000, 30000, 7)
	tmp := t.TempDir()

	// Engine A already has the data: added with Existing, it checks and seeds.
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	a := openEngine(t, testConfig(filepath.Join(tmp, "stateA"), seedDir))
	if _, err := a.Add(f.raw, AddOptions{Existing: true}); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, a, "1", StateSeeding); !v.Complete || v.DoneBytes != v.TotalBytes {
		t.Fatalf("seeder view = %+v", v)
	}

	// Engine B downloads it from A.
	dl := filepath.Join(tmp, "dl")
	b := openEngine(t, testConfig(filepath.Join(tmp, "stateB"), dl))
	if _, err := b.Add(f.raw, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	v := waitState(t, b, "1", StateSeeding)
	f.checkData(t, dl)
	if !v.Complete || v.PiecesHave != v.PiecesTotal || v.Downloaded != int64(len(f.data)) {
		t.Fatalf("leecher view = %+v", v)
	}
	files, err := b.Files("1")
	if err != nil || len(files) != 3 {
		t.Fatalf("files = %v, %v", files, err)
	}
	for _, file := range files {
		if file.Done != file.Size {
			t.Errorf("file %s: done %d of %d", file.Path, file.Done, file.Size)
		}
	}
	if o := b.Overview(); o.Torrents != 1 || o.Complete != 1 || o.ByState[StateSeeding] != 1 || o.Downloaded != int64(len(f.data)) {
		t.Fatalf("overview = %+v", o)
	}
	// A uploaded everything; it shows up in its lifetime total.
	deadline := time.Now().Add(5 * time.Second)
	for {
		av, _ := a.Get("1")
		if av.Uploaded >= int64(len(f.data)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("seeder uploaded %d of %d", av.Uploaded, len(f.data))
		}
		time.Sleep(10 * time.Millisecond)
	}

	// After a restart B trusts its clean resume data: complete, no peers needed.
	b.Close()
	b2 := openEngine(t, testConfig(filepath.Join(tmp, "stateB"), dl))
	v2 := waitState(t, b2, "1", StateSeeding)
	if v2.Downloaded != int64(len(f.data)) {
		t.Errorf("lifetime downloaded after restart = %d", v2.Downloaded)
	}
}

func TestPauseAndResumeSurviveRestart(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	e := openEngine(t, cfg)
	f := newFixture(t, "gamma", "", 16384, 40000)
	if _, err := e.Add(f.raw, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, e, "1", StateStalled) // running, but no peers anywhere

	if v, err := e.Pause("1"); err != nil || v.State != StatePaused {
		t.Fatalf("pause: %v, %v", v.State, err)
	}
	waitStopped(t, e, "1")
	if v, _ := e.Get("1"); v.State != StatePaused {
		t.Fatalf("after stopping: %s", v.State)
	}

	e.Close()
	e2 := openEngine(t, cfg)
	if v := waitState(t, e2, "1", StatePaused); v.ID != 1 {
		t.Fatalf("restored view = %+v", v)
	}
	if _, err := e2.Resume("1"); err != nil {
		t.Fatal(err)
	}
	waitState(t, e2, "1", StateStalled)

	e2.Close()
	e3 := openEngine(t, cfg)
	waitState(t, e3, "1", StateStalled) // started automatically: not paused
}

func TestPauseWhileQueuedAndResumeWhileStopping(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	cfg.MaxActive = 1
	e := openEngine(t, cfg)
	f1 := newFixture(t, "one", "", 16384, 1000)
	f2 := newFixture(t, "two", "", 16384, 1000)
	e.Add(f1.raw, AddOptions{})
	e.Add(f2.raw, AddOptions{})
	waitState(t, e, "2", StateQueued)

	// Pause a queued torrent: persisted without ever running.
	e.Pause("2")
	store, _ := resume.OpenStore(cfg.StateDir)
	if st, _, err := store.Load(f2.mi.InfoHash); err != nil || !st.Paused {
		t.Fatalf("queued pause not saved: %+v, %v", st, err)
	}

	// Pause and immediately resume a running one: it must end up running.
	e.Pause("1")
	e.Resume("1")
	waitStopped(t, e, "2")
	waitState(t, e, "1", StateStalled)
	waitState(t, e, "2", StatePaused)
}

func TestQueueRespectsMaxActive(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	cfg.MaxActive = 1
	e := openEngine(t, cfg)
	f1 := newFixture(t, "first", "", 16384, 1000)
	f2 := newFixture(t, "second", "", 16384, 1000)

	// A complete torrent seeds without taking a download slot.
	seeded := newFixture(t, "done", "", 16384, 1000)
	seeded.writeData(t, cfg.DownloadDir)
	e.Add(seeded.raw, AddOptions{Existing: true})
	waitState(t, e, "1", StateSeeding)

	e.Add(f1.raw, AddOptions{})
	e.Add(f2.raw, AddOptions{})
	waitState(t, e, "2", StateStalled)
	if v, _ := e.Get("3"); v.State != StateQueued {
		t.Fatalf("#3 = %s, want queued", v.State)
	}
	// Freeing the slot starts the next one.
	e.Pause("2")
	waitState(t, e, "3", StateStalled)
}

func TestRemoveDeletesOnlyTheTorrentsFiles(t *testing.T) {
	tmp := t.TempDir()
	dl := filepath.Join(tmp, "dl")
	cfg := testConfig(filepath.Join(tmp, "state"), dl)
	e := openEngine(t, cfg)

	keep := newFixture(t, "keep", "", 16384, 3000)
	keep.writeData(t, dl)
	gone := newFixture(t, "gone", "", 16384, 3000, 2000)
	gone.writeData(t, dl)
	mine := filepath.Join(dl, "gone", "sub", "my-notes.txt") // user's file inside the torrent folder
	os.WriteFile(mine, []byte("mine"), 0o644)

	e.Add(keep.raw, AddOptions{Existing: true})
	e.Add(gone.raw, AddOptions{Existing: true})
	waitState(t, e, "2", StateSeeding)

	if r, err := e.Remove("1", false); err != nil || r.FilesDeleted {
		t.Fatalf("remove: %+v, %v", r, err)
	}
	keep.checkData(t, dl) // removed without -delete: data stays
	if r, err := e.Remove("2", true); err != nil || !r.FilesDeleted {
		t.Fatalf("remove -delete: %+v, %v", r, err)
	}
	for i := range gone.mi.Info.Files {
		if _, err := os.Stat(gone.path(dl, i)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("file %d not deleted", i)
		}
	}
	if b, err := os.ReadFile(mine); err != nil || string(b) != "mine" {
		t.Fatal("user's own file was touched")
	}
	if len(e.List()) != 0 {
		t.Fatalf("list = %v", e.List())
	}
	store, _ := resume.OpenStore(cfg.StateDir)
	if entries, _ := store.List(); len(entries) != 0 {
		t.Fatalf("resume entries left: %d", len(entries))
	}
	if _, err := e.Remove("2", false); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second remove: %v", err)
	}
}

func TestFreshTorrentNeverAdoptsFilesThatAppearLater(t *testing.T) {
	tmp := t.TempDir()
	dl := filepath.Join(tmp, "dl")
	e := openEngine(t, testConfig(filepath.Join(tmp, "state"), dl))
	f := newFixture(t, "late", "", 16384, 3000)
	if _, err := e.Add(f.raw, AddOptions{Paused: true}); err != nil {
		t.Fatal(err)
	}
	// Someone else creates a file at the torrent's path before it starts.
	p := f.path(dl, 0)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("not torrent data"), 0o644)

	e.Resume("1")
	v := waitState(t, e, "1", StateError)
	if v.Error == "" {
		t.Fatal("no error message")
	}
	// Even removing with delete must leave that file alone: it isn't ours,
	// and the result must say nothing was deleted.
	if r, err := e.Remove("1", true); err != nil || r.FilesDeleted {
		t.Fatalf("remove -delete of a never-started torrent: %+v, %v", r, err)
	}
	if b, _ := os.ReadFile(p); string(b) != "not torrent data" {
		t.Fatal("unrelated file was modified or deleted")
	}
}

func TestErrorThenResumeRetries(t *testing.T) {
	tmp := t.TempDir()
	dl := filepath.Join(tmp, "dl")
	e := openEngine(t, testConfig(filepath.Join(tmp, "state"), dl))
	f := newFixture(t, "retry", "", 16384, 3000)
	e.Add(f.raw, AddOptions{Paused: true})
	p := f.path(dl, 0)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("x"), 0o644)
	e.Resume("1")
	waitState(t, e, "1", StateError)

	os.Remove(p) // the user fixes the problem…
	e.Resume("1")
	waitState(t, e, "1", StateStalled) // …and resume retries
}

func TestLegacyStateGetsAnID(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	f := newFixture(t, "legacy", "", 16384, 3000)
	f.writeData(t, cfg.DownloadDir)

	// What `denco download` leaves behind: a state with no ID.
	store, _ := resume.OpenStore(cfg.StateDir)
	layout, _ := storage.NewLayout(cfg.DownloadDir, &f.mi.Info)
	have := savedHave(&resume.State{}, &f.mi.Info)
	have.SetAll()
	st := &resume.State{
		InfoHash: f.mi.InfoHash.String(), Name: "legacy", DownloadDir: layout.Root,
		Pieces: f.mi.Info.NumPieces(), Have: have.Bytes(), Files: resume.Stamp(layout), Clean: true,
	}
	if err := store.Save(st, f.raw); err != nil {
		t.Fatal(err)
	}

	e := openEngine(t, cfg)
	if v := waitState(t, e, "1", StateSeeding); v.Name != "legacy" {
		t.Fatalf("view = %+v", v)
	}
	e.Close()
	saved, _, _ := store.Load(f.mi.InfoHash)
	if saved.ID != 1 {
		t.Fatalf("saved ID = %d", saved.ID)
	}
}

func TestPortFallback(t *testing.T) {
	tmp := t.TempDir()
	a := openEngine(t, testConfig(filepath.Join(tmp, "a"), tmp))
	cfg := testConfig(filepath.Join(tmp, "b"), tmp)
	cfg.ListenAddr = fmt.Sprintf("127.0.0.1:%d", a.Overview().ListenPort)
	b := openEngine(t, cfg)
	if p := b.Overview().ListenPort; p == 0 || p == a.Overview().ListenPort {
		t.Fatalf("port = %d", p)
	}
}

func TestFileDone(t *testing.T) {
	// 3 files across pieces of 10 bytes: [0,15) [15,15) [15,32)
	info := &metainfo.Info{PieceLength: 10, TotalLength: 32, Pieces: make([]metainfo.Hash, 4)}
	have := savedHave(&resume.State{}, info)
	have.Set(1) // bytes 10..20
	have.Set(3) // bytes 30..32
	cases := []struct {
		f    storage.File
		want int64
	}{
		{storage.File{Offset: 0, Length: 15}, 5},
		{storage.File{Offset: 15, Length: 0}, 0},
		{storage.File{Offset: 15, Length: 17}, 5 + 2},
	}
	for _, c := range cases {
		if got := fileDone(info, have, c.f); got != c.want {
			t.Errorf("fileDone(%+v) = %d, want %d", c.f, got, c.want)
		}
	}
	layout := &storage.Layout{Files: []storage.File{
		{Offset: 0, Length: 15}, {Offset: 15, Length: 0}, {Offset: 15, Length: 17},
	}}
	if w, d, s := selection(info, layout, have, nil); w != 32 || d != 12 || s != 0 {
		t.Errorf("selection(all) = %d, %d, %d; want 32, 12, 0", w, d, s)
	}
	// Skipping file 0: piece 1's bytes inside it (10..15) no longer count.
	if w, d, s := selection(info, layout, have, []bool{true}); w != 17 || d != 7 || s != 1 {
		t.Errorf("selection(skip 0) = %d, %d, %d; want 17, 7, 1", w, d, s)
	}
}

// transferTime measures how long a fresh engine takes to download a
// 768 KiB torrent from a seeding engine, with the given limits on each
// side. Each call gets its own tracker, so earlier engines (still running
// until the test ends) can't join the swarm and bypass the limits.
func transferTime(t *testing.T, seedUp, leechDown int64) time.Duration {
	t.Helper()
	f := newFixture(t, "limited", newTracker(t), 16384, 768<<10)
	tmp := t.TempDir()
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	ca := testConfig(filepath.Join(tmp, "sa"), seedDir)
	ca.UploadLimit = seedUp
	a := openEngine(t, ca)
	a.Add(f.raw, AddOptions{Existing: true})
	waitState(t, a, "1", StateSeeding)

	cb := testConfig(filepath.Join(tmp, "sb"), filepath.Join(tmp, "dl"))
	cb.DownloadLimit = leechDown
	b := openEngine(t, cb)
	start := time.Now()
	b.Add(f.raw, AddOptions{})
	waitState(t, b, "1", StateSeeding)
	f.checkData(t, filepath.Join(tmp, "dl"))
	return time.Since(start)
}

func TestSpeedLimits(t *testing.T) {
	// 768 KiB at 1 MiB/s: the 256 KiB burst is free, the rest takes ~0.5s.
	const limit = 1 << 20
	if d := transferTime(t, 0, 0); d > 400*time.Millisecond {
		t.Skipf("unlimited transfer already takes %v here; can't measure limits", d)
	}
	if d := transferTime(t, limit, 0); d < 400*time.Millisecond {
		t.Errorf("upload limit not applied: took %v", d)
	}
	if d := transferTime(t, 0, limit); d < 400*time.Millisecond {
		t.Errorf("download limit not applied: took %v", d)
	}
}

func TestSetLimits(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), tmp)
	cfg.DownloadLimit = 5000
	e := openEngine(t, cfg)
	if o := e.Overview(); o.DownLimit != 5000 || o.UpLimit != 0 || o.MaxPeers != 50 {
		t.Fatalf("overview = %+v", o)
	}
	e.SetLimits(0, 250_000)
	if o := e.Overview(); o.DownLimit != 0 || o.UpLimit != 250_000 {
		t.Fatalf("after SetLimits: %+v", o)
	}
}
