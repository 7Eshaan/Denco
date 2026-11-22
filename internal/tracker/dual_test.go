package tracker

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/compact"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// needDualLocalhost skips unless "localhost" resolves to both 127.0.0.1
// and ::1 and IPv6 loopback works: the setup the dual-stack tests need.
func needDualLocalhost(t *testing.T) {
	t.Helper()
	ips, err := net.DefaultResolver.LookupNetIP(context.Background(), "ip", "localhost")
	has := map[string]bool{}
	for _, ip := range ips {
		has[ip.Unmap().String()] = true
	}
	if err != nil || !has["127.0.0.1"] || !has["::1"] {
		t.Skipf("localhost doesn't resolve to both 127.0.0.1 and ::1 here (%v, %v)", ips, err)
	}
	c, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback: %v", err)
	}
	c.Close()
}

// listenBoth listens on 127.0.0.1 and [::1] with the same port number.
func listenBoth(t *testing.T) (net.Listener, net.Listener, int) {
	t.Helper()
	for try := 0; try < 20; try++ {
		l4, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		port := l4.Addr().(*net.TCPAddr).Port
		l6, err := net.Listen("tcp6", "[::1]:"+strconv.Itoa(port))
		if err == nil {
			t.Cleanup(func() { l4.Close(); l6.Close() })
			return l4, l6, port
		}
		l4.Close()
	}
	t.Fatal("no port free on both 127.0.0.1 and ::1")
	return nil, nil, 0
}

// A dual-stack HTTP announce reaches the tracker over both families (so it
// lists us under both addresses) and returns the peers of both answers.
func TestDualStackHTTPAnnounce(t *testing.T) {
	needDualLocalhost(t)
	var mu sync.Mutex
	from := map[string]bool{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.RemoteAddr)
		ip := netip.MustParseAddr(host).Unmap()
		mu.Lock()
		from[ip.String()] = true
		mu.Unlock()
		d := map[string]any{"interval": 1800}
		if ip.Is4() {
			d["peers"] = string(compact.Append(nil, netip.MustParseAddrPort("10.0.0.1:6881")))
			d["complete"] = 5
		} else {
			d["peers6"] = string(compact.Append(nil, netip.MustParseAddrPort("[2001:db8::1]:6881")))
			d["interval"] = 900
			d["complete"] = 7
		}
		b, _ := bencode.Encode(d)
		w.Write(b)
	})
	l4, l6, port := listenBoth(t)
	for _, l := range []net.Listener{l4, l6} {
		srv := &http.Server{Handler: handler}
		go srv.Serve(l)
		t.Cleanup(func() { srv.Close() })
	}

	c := &Client{DualStack: true}
	resp, err := c.Announce(context.Background(), "http://localhost:"+strconv.Itoa(port)+"/announce", baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got := len(from)
	mu.Unlock()
	if got != 2 {
		t.Errorf("tracker heard from %v; want both 127.0.0.1 and ::1", from)
	}
	for _, want := range []string{"10.0.0.1:6881", "[2001:db8::1]:6881"} {
		found := false
		for _, p := range resp.Peers {
			found = found || p == netip.MustParseAddrPort(want)
		}
		if !found {
			t.Errorf("merged peers %v lack %s", resp.Peers, want)
		}
	}
	if resp.Interval != 900*time.Second || resp.Seeders != 7 {
		t.Errorf("merged interval %v, seeders %d; want the shorter (15m) and the larger (7)", resp.Interval, resp.Seeders)
	}

	// Without DualStack: one announce only.
	clear(from)
	if _, err := (&Client{}).Announce(context.Background(), "http://localhost:"+strconv.Itoa(port)+"/announce", baseRequest()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(from) != 1 {
		t.Errorf("single-stack announce reached %v", from)
	}
}

// A family that only exists on one side (an IPv4-literal tracker) or never
// answers doesn't spoil the announce: the other answer is used, and a hung
// IPv6 attempt is given up dualGrace after IPv4 succeeded.
func TestDualStackToleratesOneFamily(t *testing.T) {
	needDualLocalhost(t)
	srv := serve(t, 200, func(url.Values) any { return map[string]any{"interval": 60} })
	c := &Client{DualStack: true}
	if _, err := c.Announce(context.Background(), srv.URL+"/announce", baseRequest()); err != nil {
		t.Fatalf("IPv4-only tracker: %v", err)
	}

	l4, l6, port := listenBoth(t)
	go (&http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := bencode.Encode(map[string]any{"interval": 60})
		w.Write(b)
	})}).Serve(l4)
	go func() { // IPv6: accepts, never answers
		for {
			c, err := l6.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	old := dualGrace
	dualGrace = 200 * time.Millisecond
	defer func() { dualGrace = old }()
	start := time.Now()
	if _, err := (&Client{DualStack: true}).Announce(context.Background(), "http://localhost:"+strconv.Itoa(port)+"/a", baseRequest()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Errorf("a silent IPv6 side held the announce for %v", d)
	}
}

// UDP trackers too: one announce per family, connection IDs kept apart.
func TestDualStackUDPAnnounce(t *testing.T) {
	needDualLocalhost(t)
	var ft4, ft6 *fakeUDPTracker
	for try := 0; try < 20 && ft6 == nil; try++ {
		ft4 = newUDPTracker(t, "udp4", "127.0.0.1:0")
		port := ft4.conn.LocalAddr().(*net.UDPAddr).Port
		if pc, err := net.ListenPacket("udp6", "[::1]:"+strconv.Itoa(port)); err == nil {
			pc.Close()
			ft6 = newUDPTracker(t, "udp6", "[::1]:"+strconv.Itoa(port))
		}
	}
	if ft6 == nil {
		t.Skip("no UDP port free on both families")
	}
	ft4.set(func(f *fakeUDPTracker) { f.peers = []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:1")} })
	ft6.set(func(f *fakeUDPTracker) { f.peers = []netip.AddrPort{netip.MustParseAddrPort("[2001:db8::2]:2")} })
	port := ft4.conn.LocalAddr().(*net.UDPAddr).Port
	c := &Client{UDP: fastUDP(), DualStack: true}
	resp, err := c.Announce(context.Background(), "udp://localhost:"+strconv.Itoa(port)+"/announce",
		Request{PeerID: peerid.New(), Port: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, a4 := ft4.counts(); a4 != 1 {
		t.Errorf("IPv4 tracker got %d announces", a4)
	}
	if _, a6 := ft6.counts(); a6 != 1 {
		t.Errorf("IPv6 tracker got %d announces", a6)
	}
	if len(resp.Peers) != 2 {
		t.Errorf("merged peers = %v", resp.Peers)
	}
	c.UDP.mu.Lock()
	keys := len(c.UDP.conns)
	c.UDP.mu.Unlock()
	if keys != 2 {
		t.Errorf("%d cached connection IDs; want one per family", keys)
	}
}
