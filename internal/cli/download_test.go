package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"fmt"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/config"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/resume"
)

// TestMain points the user config directory (and DENCO_HOME) at a temp dir
// for the whole package, so no test can ever write resume data or a daemon
// file into the real ~/Library/Application Support/denco (or %AppData%,
// ~/.config) — even one that forgets to pass -state or a home.
func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "denco-cli-test-home-*")
	if err != nil {
		panic(err)
	}
	for _, k := range []string{"HOME", "XDG_CONFIG_HOME", "AppData", "DENCO_HOME"} {
		os.Setenv(k, tmp)
	}
	// No test may contact the public DHT routers: daemons here start with
	// DHT on (the default) but no routers, so they stay silent.
	config.DefaultDHTBootstrap = ""
	// Nor talk to the real router (UPnP multicast, NAT-PMP).
	config.DefaultPortForwarding = false
	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

// syncBuffer is a bytes.Buffer safe to write from one goroutine and read
// from another.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// testSeeder serves (part of) a torrent on loopback and records requests.
type testSeeder struct {
	addr      netip.AddrPort
	served    atomic.Int64 // blocks sent
	mu        sync.Mutex
	requested map[uint32]bool
}

func (s *testSeeder) wasRequested(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requested[uint32(i)]
}

// serveSeeder runs a minimal seeder for data. has selects which pieces it
// offers (nil = all); delay slows each block down.
func serveSeeder(t *testing.T, mi *metainfo.MetaInfo, data []byte, has func(int) bool, delay time.Duration) *testSeeder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ts := &testSeeder{addr: netip.MustParseAddrPort(ln.Addr().String()), requested: map[uint32]bool{}}
	bf := bitfield.New(mi.Info.NumPieces())
	for i := 0; i < mi.Info.NumPieces(); i++ {
		if has == nil || has(i) {
			bf.Set(i)
		}
	}
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c, err := peer.Accept(nc, func(h metainfo.Hash) bool { return h == mi.InfoHash }, peer.Options{PeerID: peerid.New()})
				if err != nil {
					return
				}
				defer c.Close()
				c.Send(peer.BitfieldMsg(bf.Bytes()))
				for m := range c.Incoming() {
					switch m.ID {
					case peer.MsgInterested:
						c.Send(peer.Unchoke())
					case peer.MsgRequest:
						b, _ := peer.ParseBlock(m)
						ts.mu.Lock()
						ts.requested[b.Index] = true
						ts.mu.Unlock()
						if !bf.Has(int(b.Index)) {
							continue
						}
						time.Sleep(delay)
						off := int64(b.Index)*mi.Info.PieceLength + int64(b.Begin)
						c.Send(peer.Piece(b.Index, b.Begin, data[off:off+int64(b.Length)]))
						ts.served.Add(1)
					}
				}
			}()
		}
	}()
	return ts
}

// cliTorrent writes a single-file torrent announcing to a tracker that
// returns the given seeders, and returns the torrent path and content.
func cliTorrent(t *testing.T, pieces int, seeders func() []netip.AddrPort) (string, []byte, *metainfo.MetaInfo) {
	t.Helper()
	const pl = 16384
	data := make([]byte, pl*(pieces-1)+321)
	r := rand.New(rand.NewPCG(uint64(pieces), 4))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	var hashes []byte
	for off := 0; off < len(data); off += pl {
		h := sha1.Sum(data[off:min(off+pl, len(data))])
		hashes = append(hashes, h[:]...)
	}
	info := map[string]any{"name": "movie.bin", "piece length": pl, "length": len(data), "pieces": string(hashes)}
	tr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		var compact []byte
		for _, s := range seeders() {
			ip := s.Addr().As4()
			compact = append(compact, ip[0], ip[1], ip[2], ip[3], byte(s.Port()>>8), byte(s.Port()))
		}
		body, _ := bencode.Encode(map[string]any{"interval": 60, "complete": 1, "peers": string(compact)})
		w.Write(body)
	}))
	t.Cleanup(tr.Close)
	raw, _ := bencode.Encode(map[string]any{"announce": tr.URL, "info": info})
	mi, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "movie.torrent")
	os.WriteFile(path, raw, 0o644)
	return path, data, mi
}

func TestDownloadCommand(t *testing.T) {
	var seed *testSeeder
	tpath, data, mi := cliTorrent(t, 6, func() []netip.AddrPort { return []netip.AddrPort{seed.addr} })
	seed = serveSeeder(t, mi, data, nil, 0)
	tmp := t.TempDir()
	dl := filepath.Join(tmp, "new", "dir") // must be created automatically
	state := filepath.Join(tmp, "state")

	var out, errOut bytes.Buffer
	if code := run([]string{"download", "-port", "0", "-state", state, "-dir", dl, tpath}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	got, err := os.ReadFile(filepath.Join(dl, "movie.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("downloaded file wrong (err=%v)", err)
	}
	if !strings.Contains(out.String(), "Done: movie.bin") {
		t.Errorf("stdout:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(state, mi.InfoHash.String()+".json")); err != nil {
		t.Errorf("no resume state saved: %v", err)
	}

	// Same command again (no -dir needed): resume data says it's complete.
	out.Reset()
	errOut.Reset()
	if code := run([]string{"download", "-port", "0", "-state", state, tpath}, &out, &errOut); code != 0 {
		t.Fatalf("second run exit = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Resuming: 6 / 6") || !strings.Contains(out.String(), "Already complete") {
		t.Errorf("second run stdout:\n%s", out.String())
	}

	// A fresh state dir knows nothing: refuses to overwrite the file...
	fresh := filepath.Join(tmp, "fresh-state")
	out.Reset()
	errOut.Reset()
	if code := run([]string{"download", "-port", "0", "-state", fresh, "-dir", dl, tpath}, &out, &errOut); code != 1 {
		t.Fatalf("fresh-state run exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "-existing") {
		t.Errorf("stderr should suggest -existing:\n%s", errOut.String())
	}
	// ...unless told to reuse it, in which case it verifies and is done.
	out.Reset()
	if code := run([]string{"download", "-port", "0", "-state", fresh, "-existing", "-dir", dl, tpath}, &out, &errOut); code != 0 {
		t.Fatalf("-existing exit = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "6 / 6 pieces already present") {
		t.Errorf("stdout:\n%s", out.String())
	}
}

func TestDownloadResumesAfterInterrupt(t *testing.T) {
	const n = 40
	var mu sync.Mutex
	var seeders []netip.AddrPort
	tpath, data, mi := cliTorrent(t, n, func() []netip.AddrPort {
		mu.Lock()
		defer mu.Unlock()
		return append([]netip.AddrPort(nil), seeders...)
	})
	tmp := t.TempDir()
	dl, state := filepath.Join(tmp, "dl"), filepath.Join(tmp, "state")
	// runDownload takes the arguments after the command name.
	args := []string{"-port", "0", "-state", state, "-save-every", "50ms", "-dir", dl, tpath}

	// Run 1: a slow seeder that only has the first half. Interrupt partway.
	half := serveSeeder(t, mi, data, func(i int) bool { return i < n/2 }, 5*time.Millisecond)
	mu.Lock()
	seeders = []netip.AddrPort{half.addr}
	mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	var out1, err1 syncBuffer // written by runDownload's goroutine, read here
	go func() { done <- runDownload(ctx, &env{home: tmp, stdout: &out1, stderr: &err1}, args) }()
	deadline := time.Now().Add(15 * time.Second)
	for half.served.Load() < 10 {
		if time.Now().After(deadline) {
			t.Fatalf("first run made no progress:\n%s\n%s", out1.String(), err1.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel() // like Ctrl-C
	if code := <-done; code != 0 {
		t.Fatalf("interrupted run exit = %d: %s", code, err1.String())
	}
	if !strings.Contains(out1.String(), "Run the same command again to resume") {
		t.Errorf("run 1 stdout:\n%s", out1.String())
	}

	// What did run 1 save? Read the state file directly.
	store, err := resume.OpenStore(state)
	if err != nil {
		t.Fatal(err)
	}
	saved, _, err := store.Load(mi.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Clean {
		t.Error("state after a normal stop should be marked clean")
	}
	savedHave, err := bitfield.FromBytes(saved.Have, n)
	if err != nil || savedHave.Count() == 0 {
		t.Fatalf("saved bitfield: %v (count %d)", err, savedHave.Count())
	}

	// Run 2: a full seeder. Nothing run 1 saved may be requested again.
	full := serveSeeder(t, mi, data, nil, 0)
	mu.Lock()
	seeders = []netip.AddrPort{full.addr}
	mu.Unlock()
	var out2, err2 syncBuffer
	if code := runDownload(context.Background(), &env{home: tmp, stdout: &out2, stderr: &err2}, args); code != 0 {
		t.Fatalf("resumed run exit = %d: %s", code, err2.String())
	}
	got, _ := os.ReadFile(filepath.Join(dl, "movie.bin"))
	if !bytes.Equal(got, data) {
		t.Fatal("resumed download produced wrong data")
	}
	want := fmt.Sprintf("Resuming: %d / %d pieces verified", savedHave.Count(), n)
	if !strings.Contains(out2.String(), want) {
		t.Errorf("stdout should contain %q:\n%s", want, out2.String())
	}
	for i := 0; i < n; i++ {
		if savedHave.Has(i) && full.wasRequested(i) {
			t.Errorf("piece %d was saved as done but downloaded again", i)
		}
	}
}

func TestDownloadCommandErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"download"}, &out, &errOut); code != 2 {
		t.Errorf("no args exit = %d", code)
	}
	if code := run([]string{"download", "-state", t.TempDir(), "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a"}, &out, &errOut); code != 1 {
		t.Errorf("magnet exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "denco add") {
		t.Errorf("stderr = %q", errOut.String())
	}
}

func TestListenFallsBackWhenPortBusy(t *testing.T) {
	busy, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := uint16(busy.Addr().(*net.TCPAddr).Port)

	var errOut bytes.Buffer
	ln, err := listen(port, peerid.New(), nil, &errOut)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if ln.Port() == port || ln.Port() == 0 {
		t.Fatalf("got port %d, want a different free port", ln.Port())
	}
	if !strings.Contains(errOut.String(), "unavailable") {
		t.Errorf("no warning printed: %q", errOut.String())
	}
}
