package torrent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/ratelimit"
	"github.com/7Eshaan/Denco/internal/webseed"
)

// withWebSeeds returns a copy of the metainfo with a url-list (BEP 19) and,
// if trackerURL isn't empty, a tracker.
func (f *fixture) withWebSeeds(t *testing.T, trackerURL string, urls ...string) *metainfo.MetaInfo {
	t.Helper()
	list := make([]any, len(urls))
	for i, u := range urls {
		list[i] = u
	}
	root := map[string]any{"url-list": list, "info": bencode.Raw(f.mi.InfoBytes)}
	if trackerURL != "" {
		root["announce"] = trackerURL
	}
	raw, _ := bencode.Encode(root)
	mi, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return mi
}

// serveWebSeed serves the fixture's files over HTTP (ranges supported) and
// returns the web seed URL. corrupt flips a byte in every file; gate, if
// set, runs before each request and may answer instead (true = handled).
func serveWebSeed(t *testing.T, f *fixture, corrupt bool, gate func(w http.ResponseWriter) bool) (string, *atomic.Int64) {
	t.Helper()
	dir := t.TempDir()
	for _, file := range f.mi.Info.Files {
		p := filepath.Join(append([]string{dir, f.mi.Info.Name}, file.Path...)...)
		data := append([]byte(nil), f.data[file.Offset:file.Offset+file.Length]...)
		if corrupt && len(data) > 0 {
			data[len(data)/2] ^= 0xff
		}
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, data, 0o644)
	}
	var requests atomic.Int64
	fs := http.FileServer(http.Dir(dir))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if gate != nil && gate(w) {
			return
		}
		fs.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/", &requests
}

func webSeedConfig() Config {
	cfg := testConfig()
	cfg.WebSeeds = webseed.NewClient(true) // the test server is on loopback
	return cfg
}

// With no peers at all, a torrent downloads from its web seed, which shows
// up in the peer list.
func TestDownloadFromWebSeed(t *testing.T) {
	f := makeTorrent(t, pl, 3*pl+100, 0, 2*pl+7) // pieces span files; an empty file
	base, requests := serveWebSeed(t, f, false, nil)
	r := start(t, f.withWebSeeds(t, "", base), t.TempDir(), nil, webSeedConfig(), false)
	r.waitComplete(t, 10*time.Second)
	checkOnDisk(t, f, r.dir)
	if requests.Load() == 0 {
		t.Fatal("no request reached the web seed")
	}
	st := r.s.Stats()
	if st.WebSeeds != 1 || st.Downloaded != f.mi.Info.TotalLength {
		t.Errorf("stats: %d web seeds, downloaded %d of %d", st.WebSeeds, st.Downloaded, f.mi.Info.TotalLength)
	}
	var found bool
	for _, p := range r.s.Peers() {
		if p.WebSeed == base {
			found = p.Downloaded == f.mi.Info.TotalLength && p.Client == "web seed"
		}
	}
	if !found {
		t.Errorf("web seed missing from the peer list: %+v", r.s.Peers())
	}
}

// A web seed serving wrong data is given up after its piece fails the
// hash check; a real peer finishes the download.
func TestBadWebSeedIsGivenUp(t *testing.T) {
	f := makeTorrent(t, pl, 8*pl)
	base, _ := serveWebSeed(t, f, true, nil)
	sd := newSeeder(t, f, seederOpts{slow: 30 * time.Millisecond}) // slow, so the web seed gets work
	tr := newTracker(t, sd.addr)
	r := start(t, f.withWebSeeds(t, tr.srv.URL, base), t.TempDir(), nil, webSeedConfig(), false)
	r.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, r.dir)
	for _, p := range r.s.Peers() {
		if p.WebSeed != "" && p.WebSeedNote != "off: sent data that failed the hash check" {
			t.Errorf("bad web seed note = %q", p.WebSeedNote)
		}
	}
}

// A busy server (503, Retry-After) is retried, and then serves the data.
func TestWebSeedRetriesWhenBusy(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	var busy atomic.Int64
	busy.Store(3)
	base, requests := serveWebSeed(t, f, false, func(w http.ResponseWriter) bool {
		if busy.Add(-1) >= 0 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusServiceUnavailable)
			return true
		}
		return false
	})
	r := start(t, f.withWebSeeds(t, "", base), t.TempDir(), nil, webSeedConfig(), false)
	r.waitComplete(t, 10*time.Second)
	checkOnDisk(t, f, r.dir)
	if requests.Load() < 4 {
		t.Errorf("only %d requests: the 503s weren't retried", requests.Load())
	}
}

// The torrent's download limit (and the global one) pace web seeds too.
func TestWebSeedHonoursDownloadLimit(t *testing.T) {
	f := makeTorrent(t, pl, 48*pl) // 768 KiB
	base, _ := serveWebSeed(t, f, false, nil)
	cfg := webSeedConfig()
	cfg.TorrentDownLimiter = ratelimit.New(1 << 20) // 1 MiB/s, 256 KiB burst: ≈ 0.5 s
	start0 := time.Now()
	r := start(t, f.withWebSeeds(t, "", base), t.TempDir(), nil, cfg, false)
	r.waitComplete(t, 10*time.Second)
	if d := time.Since(start0); d < 400*time.Millisecond {
		t.Errorf("768 KiB from a web seed under a 1 MiB/s limit took only %v", d)
	}
}

// By default (the daemon's client) a web seed on this machine is refused:
// a torrent mustn't make us contact local services.
func TestLocalWebSeedRefusedByDefault(t *testing.T) {
	f := makeTorrent(t, pl, 2*pl)
	base, requests := serveWebSeed(t, f, false, nil)
	r := start(t, f.withWebSeeds(t, "", base), t.TempDir(), nil, testConfig(), false)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var note string
		for _, p := range r.s.Peers() {
			note = p.WebSeedNote
		}
		if len(note) > 4 && note[:4] == "off:" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("local web seed not given up; note %q", note)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if requests.Load() != 0 {
		t.Errorf("%d requests reached the local web seed", requests.Load())
	}
}
