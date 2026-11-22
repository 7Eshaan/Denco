package engine

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/tracker"
)

func needIPv6(t *testing.T) {
	t.Helper()
	l, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	l.Close()
}

// With enable_ipv6 the engine runs an IPv6 DHT node next to the IPv4 one,
// saves its own state, and announces to trackers over both families.
func TestIPv6DHTNodeAndDualStackTrackers(t *testing.T) {
	needIPv6(t)
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), tmp)
	cfg.EnableDHT, cfg.EnableIPv6, cfg.DHT6Addr = true, true, "[::1]:0"
	e := openEngine(t, cfg)
	ov := e.Overview()
	if !ov.DHT || !ov.DHT6 || ov.DHT6Port == 0 || !ov.IPv6 {
		t.Fatalf("overview: %+v", ov)
	}
	if c, ok := e.cfg.Session.Announcer.(*tracker.Client); !ok || !c.DualStack {
		t.Errorf("announcer %#v is not dual-stack", e.cfg.Session.Announcer)
	}
	e.Close()
	for _, f := range []string{"dht.json", "dht6.json"} {
		if _, err := os.Stat(filepath.Join(cfg.StateDir, f)); err != nil {
			t.Errorf("%s not saved: %v", f, err)
		}
	}

	// Off: no IPv6 node, single-stack announces. And a peer port bound to
	// an IPv4 address gets no IPv6 node either.
	for _, c := range []func(*Config){
		func(c *Config) { c.EnableIPv6 = false },
		func(c *Config) { c.DHT6Addr = "" }, // ListenAddr is 127.0.0.1
	} {
		cfg2 := testConfig(filepath.Join(t.TempDir(), "state"), tmp)
		cfg2.EnableDHT, cfg2.EnableIPv6, cfg2.DHT6Addr = true, true, "[::1]:0"
		c(&cfg2)
		e2 := openEngine(t, cfg2)
		if ov := e2.Overview(); ov.DHT6 || !ov.DHT {
			t.Errorf("DHT6 = %v, DHT = %v; want IPv4 only", ov.DHT6, ov.DHT)
		}
	}
}

// A magnet whose only peer is reachable over IPv6 (x.pe=[::1]:port): the
// metadata and the data both come over IPv6. With enable_ipv6 off the
// same magnet finds nobody.
func TestMagnetFromIPv6Peer(t *testing.T) {
	needIPv6(t)
	f := newFixture(t, "six", "", 16384, 5*16384+7)
	tmp := t.TempDir()
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	scfg := testConfig(filepath.Join(tmp, "seedstate"), seedDir)
	scfg.ListenAddr = "[::1]:0"
	seed := openEngine(t, scfg)
	seed.Add(f.raw, AddOptions{Existing: true})
	waitState(t, seed, "1", StateSeeding)
	peer := "[::1]:" + strconv.Itoa(int(seed.Overview().ListenPort))

	lcfg := testConfig(filepath.Join(tmp, "leechstate"), filepath.Join(tmp, "dl"))
	lcfg.EnableIPv6 = true
	leech := openEngine(t, lcfg)
	if _, err := leech.AddMagnet(magnetURI(f, "", "", peer), AddOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, leech, "1", StateSeeding)
	f.checkData(t, filepath.Join(tmp, "dl"))

	ocfg := testConfig(filepath.Join(tmp, "offstate"), filepath.Join(tmp, "dl2"))
	ocfg.EnableIPv6 = false
	off := openEngine(t, ocfg)
	off.AddMagnet(magnetURI(f, "", "", peer), AddOptions{})
	time.Sleep(time.Second)
	if v, _ := off.Get("1"); v.State != StateMetadata || v.Peers != 0 {
		t.Errorf("with IPv6 off: state %s, %d peers; the IPv6 peer must be ignored", v.State, v.Peers)
	}
}
