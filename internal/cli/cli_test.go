package cli

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/7Eshaan/Denco/internal/bencode"
)

func TestMagnetCommand(t *testing.T) {
	var out, errOut bytes.Buffer
	uri := "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a&dn=Demo&tr=http%3A%2F%2Ft.example%2Fa"
	if code := run([]string{"magnet", uri}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"Demo", "c12fe1c06bba254a9dc9f519b335aa7c1367a88a", "http://t.example/a"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	if code := run([]string{"magnet", "magnet:?dn=x"}, &out, &errOut); code != 1 {
		t.Errorf("invalid magnet exit = %d", code)
	}
	if code := run([]string{"magnet"}, &out, &errOut); code != 2 {
		t.Errorf("no args exit = %d", code)
	}
}

func TestAnnounceCommand(t *testing.T) {
	var mu sync.Mutex
	var events []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		mu.Lock()
		events = append(events, q.Get("event"))
		mu.Unlock()
		body, _ := bencode.Encode(map[string]any{
			"interval": 1800, "complete": 4, "incomplete": 2,
			"peers": "\x0a\x00\x00\x01\x1a\xe1",
		})
		w.Write(body)
	}))
	defer srv.Close()

	data, _ := bencode.Encode(map[string]any{
		"announce": srv.URL + "/announce",
		"info": map[string]any{
			"name": "demo", "piece length": 16384, "length": 100,
			"pieces": strings.Repeat("x", 20),
		},
	})
	path := filepath.Join(t.TempDir(), "demo.torrent")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"announce", "-port", "7000", path}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"4 / 2", "10.0.0.1:6881", "Total unique peers: 1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(events, ",") != "started,stopped" {
		t.Errorf("events = %v, want started then stopped", events)
	}
}

func TestAnnounceCommandFailures(t *testing.T) {
	var out, errOut bytes.Buffer
	// Trackerless magnet.
	if code := run([]string{"announce", "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a"}, &out, &errOut); code != 1 {
		t.Errorf("trackerless exit = %d", code)
	}
	// Dead tracker.
	srv := httptest.NewServer(http.NotFoundHandler())
	dead := srv.URL
	srv.Close()
	uri := "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a&tr=" + url.QueryEscape(dead)
	out.Reset()
	if code := run([]string{"announce", uri}, &out, &errOut); code != 1 {
		t.Errorf("dead tracker exit = %d", code)
	}
	if !strings.Contains(out.String(), "✗ tier 0 failed") {
		t.Errorf("output:\n%s", out.String())
	}
	if code := run([]string{"announce"}, &out, &errOut); code != 2 {
		t.Errorf("no args exit = %d", code)
	}
}
