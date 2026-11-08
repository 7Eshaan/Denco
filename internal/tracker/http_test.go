package tracker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// testHash contains bytes that need escaping, including 0x20 (space) and '+'.
var testHash = metainfo.Hash{0x00, 0x20, '+', '&', '=', 0xff, 'a', 'Z', '~', '%', 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}

func serve(t *testing.T, status int, body func(q url.Values) any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, err := url.ParseQuery(r.URL.RawQuery)
		if err != nil {
			t.Errorf("server: bad query: %v", err)
		}
		var out []byte
		switch b := body(q).(type) {
		case string:
			out = []byte(b)
		default:
			out, err = bencode.Encode(b)
			if err != nil {
				t.Errorf("server: encode: %v", err)
			}
		}
		w.WriteHeader(status)
		w.Write(out)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func baseRequest() Request {
	return Request{
		InfoHash: testHash, PeerID: peerid.New(), Port: 6881,
		Uploaded: 10, Downloaded: 20, Left: 30, Event: EventStarted, Key: 0xbeef,
	}
}

func TestAnnounceCompactPeersAndParams(t *testing.T) {
	req := baseRequest()
	var got url.Values
	srv := serve(t, 200, func(q url.Values) any {
		got = q
		return map[string]any{
			"interval":     1800,
			"min interval": 900,
			"complete":     7,
			"incomplete":   3,
			"tracker id":   "tid",
			// 10.0.0.1:6881, 192.168.1.2:51413, plus 3 junk trailing bytes
			"peers":  "\x0a\x00\x00\x01\x1a\xe1" + "\xc0\xa8\x01\x02\xc8\xd5" + "xyz",
			"peers6": "\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x01\x1a\xe1",
		}
	})

	var c Client
	resp, err := c.Announce(context.Background(), srv.URL+"/announce", req)
	if err != nil {
		t.Fatal(err)
	}

	// Raw bytes must survive the round trip exactly (catches '+' vs %20 bugs).
	if got.Get("info_hash") != string(testHash[:]) {
		t.Errorf("info_hash mangled: %q", got.Get("info_hash"))
	}
	if got.Get("peer_id") != req.PeerID.String() {
		t.Errorf("peer_id = %q", got.Get("peer_id"))
	}
	for k, want := range map[string]string{
		"port": "6881", "uploaded": "10", "downloaded": "20", "left": "30",
		"compact": "1", "event": "started", "numwant": "50", "key": "beef",
	} {
		if got.Get(k) != want {
			t.Errorf("param %s = %q, want %q", k, got.Get(k), want)
		}
	}

	if resp.Interval != 30*time.Minute || resp.MinInterval != 15*time.Minute {
		t.Errorf("intervals = %v / %v", resp.Interval, resp.MinInterval)
	}
	if resp.Seeders != 7 || resp.Leechers != 3 || resp.TrackerID != "tid" {
		t.Errorf("stats = %+v", resp)
	}
	var peers []string
	for _, p := range resp.Peers {
		peers = append(peers, p.String())
	}
	if strings.Join(peers, ",") != "10.0.0.1:6881,192.168.1.2:51413,[::1]:6881" {
		t.Errorf("peers = %v", peers)
	}
}

func TestAnnounceDictPeers(t *testing.T) {
	srv := serve(t, 200, func(url.Values) any {
		return map[string]any{
			"interval": 60,
			"peers": []any{
				map[string]any{"ip": "1.2.3.4", "port": 1000, "peer id": "x"},
				map[string]any{"ip": "::ffff:5.6.7.8", "port": 2000}, // v4-mapped → v4
				map[string]any{"ip": "host.example", "port": 3000},   // skipped
				map[string]any{"ip": "9.9.9.9", "port": 0},           // skipped
				"junk",
			},
		}
	})
	var c Client
	resp, err := c.Announce(context.Background(), srv.URL, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Peers) != 2 || resp.Peers[0].String() != "1.2.3.4:1000" || resp.Peers[1].String() != "5.6.7.8:2000" {
		t.Errorf("peers = %v", resp.Peers)
	}
	if resp.Seeders != -1 || resp.Leechers != -1 {
		t.Errorf("unknown counts should be -1, got %d/%d", resp.Seeders, resp.Leechers)
	}
}

func TestAnnounceKeepsExistingQuery(t *testing.T) {
	var got url.Values
	srv := serve(t, 200, func(q url.Values) any { got = q; return map[string]any{"peers": ""} })
	var c Client
	if _, err := c.Announce(context.Background(), srv.URL+"/a?passkey=secret", baseRequest()); err != nil {
		t.Fatal(err)
	}
	if got.Get("passkey") != "secret" || got.Get("port") != "6881" {
		t.Errorf("query = %v", got)
	}
}

func TestAnnounceIntervalClampAndDefaults(t *testing.T) {
	srv := serve(t, 200, func(url.Values) any { return map[string]any{"interval": 5, "warning message": "slow down"} })
	var c Client
	resp, err := c.Announce(context.Background(), srv.URL, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != minInterval {
		t.Errorf("interval = %v, want clamp to %v", resp.Interval, minInterval)
	}
	if resp.Warning != "slow down" {
		t.Errorf("warning = %q", resp.Warning)
	}

	srv2 := serve(t, 200, func(url.Values) any { return map[string]any{} })
	resp, err = c.Announce(context.Background(), srv2.URL, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != defaultInterval {
		t.Errorf("missing interval: got %v, want %v", resp.Interval, defaultInterval)
	}
}

func TestAnnounceNoEventOmitted(t *testing.T) {
	var got url.Values
	srv := serve(t, 200, func(q url.Values) any { got = q; return map[string]any{} })
	req := baseRequest()
	req.Event = EventNone
	req.TrackerID = "a b"
	var c Client
	if _, err := c.Announce(context.Background(), srv.URL, req); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["event"]; ok {
		t.Error("event should be omitted for regular announces")
	}
	if got.Get("trackerid") != "a b" {
		t.Errorf("trackerid = %q", got.Get("trackerid"))
	}
}

func TestAnnounceErrors(t *testing.T) {
	var c Client
	ctx := context.Background()

	t.Run("failure reason 200", func(t *testing.T) {
		srv := serve(t, 200, func(url.Values) any { return map[string]any{"failure reason": "unregistered torrent"} })
		_, err := c.Announce(ctx, srv.URL, baseRequest())
		var fe *FailureError
		if !errors.As(err, &fe) || fe.Reason != "unregistered torrent" {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("failure reason with 400", func(t *testing.T) {
		srv := serve(t, 400, func(url.Values) any { return map[string]any{"failure reason": "bad passkey"} })
		var fe *FailureError
		if _, err := c.Announce(ctx, srv.URL, baseRequest()); !errors.As(err, &fe) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("http 500 html", func(t *testing.T) {
		srv := serve(t, 500, func(url.Values) any { return "<html>oops</html>" })
		_, err := c.Announce(ctx, srv.URL, baseRequest())
		if err == nil || !strings.Contains(err.Error(), "500") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("http 404 with valid dict", func(t *testing.T) {
		srv := serve(t, 404, func(url.Values) any { return map[string]any{} })
		_, err := c.Announce(ctx, srv.URL, baseRequest())
		if err == nil || !strings.Contains(err.Error(), "404") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("garbage 200", func(t *testing.T) {
		srv := serve(t, 200, func(url.Values) any { return "not bencode" })
		if _, err := c.Announce(ctx, srv.URL, baseRequest()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("not a dict", func(t *testing.T) {
		srv := serve(t, 200, func(url.Values) any { return "li1ee" })
		if _, err := c.Announce(ctx, srv.URL, baseRequest()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("too large", func(t *testing.T) {
		srv := serve(t, 200, func(url.Values) any { return strings.Repeat("x", maxResponseSize+10) })
		_, err := c.Announce(ctx, srv.URL, baseRequest())
		if err == nil || !strings.Contains(err.Error(), "too large") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("udp unsupported", func(t *testing.T) {
		if _, err := c.Announce(ctx, "udp://tracker.example:80", baseRequest()); !errors.Is(err, ErrUnsupportedScheme) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("bad url", func(t *testing.T) {
		if _, err := c.Announce(ctx, "http://[::1", baseRequest()); err == nil {
			t.Fatal("expected error")
		}
	})
	t.Run("context cancel", func(t *testing.T) {
		block := make(chan struct{})
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-block:
			case <-r.Context().Done():
			}
		}))
		defer srv.Close()
		defer close(block)
		cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
		defer cancel()
		if _, err := c.Announce(cctx, srv.URL, baseRequest()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		addr := srv.URL
		srv.Close()
		if _, err := c.Announce(ctx, addr, baseRequest()); err == nil {
			t.Fatal("expected error")
		}
	})
}

func TestEscapeBytes(t *testing.T) {
	if got := escapeBytes([]byte("aZ09-_.~ +/\x00\xff")); got != "aZ09-_.~%20%2B%2F%00%FF" {
		t.Errorf("escapeBytes = %q", got)
	}
}
