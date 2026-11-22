package torrent

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func needIPv6(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	l.Close()
}

// A torrent downloads from a peer that only has an IPv6 address (the
// tracker lists it in peers6) — and, with IPv4Only, never dials it.
func TestDownloadFromIPv6Peer(t *testing.T) {
	needIPv6(t)
	f := makeTorrent(t, pl, 6*pl)
	sd6 := newSeeder(t, f, seederOpts{ipv6: true})
	tr := newTracker(t, sd6.addr)
	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
	r.waitComplete(t, 10*time.Second)
	checkOnDisk(t, f, r.dir)
	if !sd6.addr.Addr().Is6() || sd6.accepts.Load() == 0 {
		t.Fatalf("IPv6 seeder %v accepted %d connections", sd6.addr, sd6.accepts.Load())
	}

	f2 := makeTorrent(t, pl, 6*pl+1)
	v6 := newSeeder(t, f2, seederOpts{ipv6: true})
	v4 := newSeeder(t, f2, seederOpts{})
	tr2 := newTracker(t, v6.addr, v4.addr)
	cfg := testConfig()
	cfg.IPv4Only = true
	r2 := start(t, f2.withTracker(t, tr2.srv.URL), t.TempDir(), nil, cfg, false)
	r2.waitComplete(t, 10*time.Second)
	if n := v6.accepts.Load(); n != 0 {
		t.Errorf("IPv4Only dialed the IPv6 peer %d times", n)
	}
}

// Bans cover an IPv6 /64 (one machine can pick any address in it) and a
// single IPv4 address.
func TestBanKeys(t *testing.T) {
	w := newSwarm(time.Second)
	w.ban(netip.MustParseAddrPort("[2001:db8:1:2:aaaa::1]:6881"))
	w.ban(netip.MustParseAddrPort("192.0.2.7:1"))
	for addr, banned := range map[string]bool{
		"[2001:db8:1:2:ffff::9]:51413": true,  // same /64, other address and port
		"[2001:db8:1:3::1]:6881":       false, // next /64
		"192.0.2.7:9999":               true,
		"192.0.2.8:1":                  false,
		"[::ffff:192.0.2.7]:5":         true, // IPv4-mapped is the same IPv4 address
	} {
		if got := w.isBanned(netip.MustParseAddrPort(addr)); got != banned {
			t.Errorf("isBanned(%s) = %v, want %v", addr, got, banned)
		}
	}
	w4 := newSwarm(time.Second)
	w4.ipv4Only = true
	w4.add([]netip.AddrPort{netip.MustParseAddrPort("[2001:db8::1]:1"), netip.MustParseAddrPort("192.0.2.1:1")})
	if len(w4.addrs) != 1 {
		t.Errorf("IPv4-only swarm kept %v", w4.addrs)
	}
}
