package webseed

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// multiFile is a 3-file torrent ("my files/a b.txt", "my files/sub/c#d",
// an empty file) with its data, served by a file server rooted at dir.
func multiFile(t *testing.T) (*metainfo.Info, []byte, string) {
	t.Helper()
	sizes := []int{40000, 0, 30000}
	paths := [][]string{{"a b.txt"}, {"empty"}, {"sub", "c#d"}}
	var data []byte
	dir := t.TempDir()
	var files []any
	for i, n := range sizes {
		chunk := bytes.Repeat([]byte{byte('a' + i)}, n)
		for k := range chunk {
			chunk[k] += byte(k % 7)
		}
		data = append(data, chunk...)
		p := filepath.Join(append([]string{dir, "my files"}, paths[i]...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, chunk, 0o644)
		pl := []any{}
		for _, s := range paths[i] {
			pl = append(pl, s)
		}
		files = append(files, map[string]any{"length": n, "path": pl})
	}
	const pieceLen = 16384
	var hashes []byte
	for off := 0; off < len(data); off += pieceLen {
		h := sha1.Sum(data[off:min(off+pieceLen, len(data))])
		hashes = append(hashes, h[:]...)
	}
	raw, _ := bencode.Encode(map[string]any{"name": "my files", "piece length": pieceLen, "pieces": string(hashes), "files": files})
	info, err := metainfo.ParseInfo(raw)
	if err != nil {
		t.Fatal(err)
	}
	return info, data, dir
}

func TestURLFor(t *testing.T) {
	info, _, _ := multiFile(t)
	single := &metainfo.Info{Name: "debian 13.iso", Files: []metainfo.File{{Length: 1}}}
	for _, tc := range []struct {
		base string
		info *metainfo.Info
		file int
		want string
	}{
		{"http://h/x/debian.iso", single, 0, "http://h/x/debian.iso"},
		{"https://h/pub/", single, 0, "https://h/pub/debian%2013.iso"},
		{"http://h/root/", info, 0, "http://h/root/my%20files/a%20b.txt"},
		{"http://h/root", info, 2, "http://h/root/my%20files/sub/c%23d"},
	} {
		if got, err := URLFor(tc.base, tc.info, tc.file); err != nil || got != tc.want {
			t.Errorf("URLFor(%q, %d) = %q, %v; want %q", tc.base, tc.file, got, err, tc.want)
		}
	}
	for _, bad := range []string{"ftp://h/x", "file:///etc/passwd", "not a url", "http:///nohost"} {
		if _, err := URLFor(bad, single, 0); !errors.Is(err, ErrBadURL) {
			t.Errorf("URLFor(%q): %v, want ErrBadURL", bad, err)
		}
	}
}

// A span crossing files is fetched with one ranged request per file, and
// the bytes land in the right places; the empty file is skipped.
func TestFetchAcrossFiles(t *testing.T) {
	info, data, dir := multiFile(t)
	var ranges []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ranges = append(ranges, r.URL.Path+" "+r.Header.Get("Range"))
		http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
	}))
	defer srv.Close()
	c := NewClient(true)
	off, n := int64(30000), 25000 // 10000 bytes of file a, then 15000 of c
	dst := make([]byte, n)
	var waited int
	if err := c.Fetch(context.Background(), srv.URL+"/", info, off, dst, func(_ context.Context, k int) error {
		waited += k
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst, data[off:off+int64(n)]) {
		t.Fatal("wrong bytes")
	}
	if waited != n {
		t.Errorf("waiter charged %d bytes, want %d", waited, n)
	}
	want := []string{"/my files/a b.txt bytes=30000-39999", "/my files/sub/c#d bytes=0-14999"}
	if len(ranges) != 2 || ranges[0] != want[0] || ranges[1] != want[1] {
		t.Errorf("requests %q, want %q", ranges, want)
	}
}

func TestFetchRefusesLocalAddresses(t *testing.T) {
	info, _, dir := multiFile(t)
	srv := httptest.NewServer(http.FileServer(http.Dir(dir)))
	defer srv.Close()
	// 127.0.0.1, and a name that resolves to it.
	for _, base := range []string{srv.URL + "/", strings.Replace(srv.URL, "127.0.0.1", "localhost", 1) + "/"} {
		err := NewClient(false).Fetch(context.Background(), base, info, 0, make([]byte, 10), nil)
		if !errors.Is(err, ErrLocalAddress) {
			t.Errorf("%s: %v, want ErrLocalAddress", base, err)
		}
	}
	// The rule itself. It is applied when connecting, so it also covers
	// redirects and names resolving to local addresses (tests can't reach a
	// public server to show the redirect case end to end).
	for addr, public := range map[string]bool{
		"8.8.8.8": true, "2001:4860:4860::8888": true, "127.0.0.1": false, "::1": false,
		"10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false, "169.254.1.1": false,
		"fe80::1": false, "fd00::1": false, "100.64.0.1": false, "0.0.0.0": false,
		"224.0.0.1": false, "::ffff:192.168.1.1": false,
	} {
		if got := Public(netip.MustParseAddr(addr)); got != public {
			t.Errorf("Public(%s) = %v, want %v", addr, got, public)
		}
	}
}

func TestFetchErrors(t *testing.T) {
	info, _, dir := multiFile(t)
	var mode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "busy":
			w.Header().Set("Retry-After", "120")
			w.WriteHeader(http.StatusServiceUnavailable)
		case "noranges": // ignores Range: the whole file with 200
			b, _ := os.ReadFile(filepath.Join(dir, "my files", "a b.txt"))
			w.Write(b)
		case "wrongrange": // the right length, from the wrong place
			w.Header().Set("Content-Range", "bytes 0-99/40000")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(make([]byte, 100))
		case "short":
			w.Header().Set("Content-Range", "bytes 100-199/40000")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(http.StatusPartialContent)
			w.Write(make([]byte, 40))
		case "stall":
			w.Header().Set("Content-Range", "bytes 100-199/40000")
			w.WriteHeader(http.StatusPartialContent)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "redirect":
			http.Redirect(w, r, "ftp://example.org/x", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(true)
	fetch := func() error {
		return c.Fetch(context.Background(), srv.URL+"/", info, 100, make([]byte, 100), nil)
	}

	mode = "busy"
	var re *RetryError
	if err := fetch(); !errors.As(err, &re) || re.After != 2*time.Minute {
		t.Errorf("503: %v", err)
	}
	mode = "noranges"
	if err := fetch(); !errors.Is(err, ErrNoRanges) {
		t.Errorf("no ranges: %v", err)
	}
	// …but a whole-file request answered with 200 is fine.
	whole := make([]byte, 40000)
	if err := c.Fetch(context.Background(), srv.URL+"/", info, 0, whole, nil); err != nil {
		t.Errorf("whole file with 200: %v", err)
	}
	for _, m := range []string{"wrongrange", "short", "redirect", "404"} {
		mode = m
		if err := fetch(); err == nil {
			t.Errorf("%s: no error", m)
		}
	}
	old := idleTimeout
	idleTimeout = 200 * time.Millisecond
	defer func() { idleTimeout = old }()
	mode = "stall"
	start := time.Now()
	if err := fetch(); err == nil || time.Since(start) > 3*time.Second {
		t.Errorf("stalled server: %v after %v", err, time.Since(start))
	}
}
