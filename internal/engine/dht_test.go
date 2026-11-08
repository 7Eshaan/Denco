package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/dht"
)

// dhtNetwork starts n DHT nodes on loopback that know each other and
// returns the address of the first, to bootstrap from.
func dhtNetwork(t *testing.T, n int) string {
	t.Helper()
	open := func(boot []string) *dht.Server {
		s, err := dht.Open(dht.Config{Addr: "127.0.0.1:0", Bootstrap: boot, QueryTimeout: 500 * time.Millisecond, MaintainEvery: 100 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	boot := open(nil)
	nodes := []*dht.Server{}
	for i := 0; i < n; i++ {
		nodes = append(nodes, open([]string{boot.Addr().String()}))
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, s := range nodes {
		for s.Stats().GoodNodes < n/2 {
			if time.Now().After(deadline) {
				t.Fatal("DHT test network didn't form")
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	return boot.Addr().String()
}

// A magnet link with nothing but the info hash: the seed announced itself
// to the DHT, and the other engine finds it there, fetches the metadata
// and downloads everything.
func TestMagnetThroughDHT(t *testing.T) {
	boot := dhtNetwork(t, 10)
	f := newFixture(t, "dht only", "", 16384, 7*16384+11, 500)

	tmp := t.TempDir()
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	scfg := testConfig(filepath.Join(tmp, "seedstate"), seedDir)
	scfg.EnableDHT, scfg.DHTBootstrap = true, []string{boot}
	seed := openEngine(t, scfg)
	if _, err := seed.Add(f.raw, AddOptions{Existing: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, seed, "1", StateSeeding)

	dl := filepath.Join(tmp, "dl")
	lcfg := testConfig(filepath.Join(tmp, "leechstate"), dl)
	lcfg.EnableDHT, lcfg.DHTBootstrap = true, []string{boot}
	leech := openEngine(t, lcfg)
	if _, err := leech.AddMagnet(magnetURI(f, "", ""), AddOptions{}); err != nil {
		t.Fatal(err)
	}
	waitState(t, leech, "1", StateSeeding)
	f.checkData(t, dl)
	// The download may finish on the peers the metadata fetcher handed over
	// before the session's own first lookup returns; that lookup must still
	// show up in the view.
	deadline := time.Now().Add(10 * time.Second)
	for v, _ := leech.Get("1"); v.DHTPeers == 0; v, _ = leech.Get("1") {
		if time.Now().After(deadline) {
			t.Fatal("torrent view reports no DHT peers")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ov := leech.Overview(); !ov.DHT || ov.DHTNodes == 0 || ov.DHTPort == 0 {
		t.Errorf("overview: DHT=%v nodes=%d port=%d", ov.DHT, ov.DHTNodes, ov.DHTPort)
	}

	// The routing table is saved for the next start.
	leech.Close()
	if _, err := os.Stat(filepath.Join(lcfg.StateDir, "dht.json")); err != nil {
		t.Errorf("DHT state not saved: %v", err)
	}
}
