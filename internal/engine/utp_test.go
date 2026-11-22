package engine

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/peer"
)

// With transport both, the engine runs uTP on the DHT's UDP port, and two
// engines exchange a torrent over uTP (D64).
func TestUTPBetweenEngines(t *testing.T) {
	tracker := newTracker(t)
	f := newFixture(t, "movie", tracker, 16384, 300_000)
	tmp := t.TempDir()
	utpConfig := func(state, dl string) Config {
		cfg := testConfig(filepath.Join(tmp, state), dl)
		cfg.EnableDHT = true // no bootstrap routers: just sharing the port
		cfg.Session.PeerOpts.Transport = peer.TransportBoth
		return cfg
	}
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	acfg := utpConfig("stateA", seedDir)
	acfg.UploadLimit = 150_000 // time to look at the connection
	a := openEngine(t, acfg)
	ov := a.Overview()
	if ov.UTPPort == 0 || ov.UTPPort != ov.DHTPort || ov.Transport != "both" {
		t.Fatalf("uTP port %d, DHT port %d, transport %q", ov.UTPPort, ov.DHTPort, ov.Transport)
	}
	if _, err := a.Add(f.raw, AddOptions{Existing: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, "1", StateSeeding)

	dl := filepath.Join(tmp, "dl")
	b := openEngine(t, utpConfig("stateB", dl))
	if _, err := b.Add(f.raw, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ps, _ := b.Peers("1")
		if len(ps) == 1 && ps[0].UTP {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("peers %+v, want one over uTP", ps)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if o := b.Overview(); o.UTPPeers != 1 {
		t.Errorf("overview counts %d uTP peers", o.UTPPeers)
	}
	waitState(t, b, "1", StateSeeding)
	f.checkData(t, dl)
}

// transport tcp opens no uTP socket; utp refuses incoming TCP.
func TestTransportSettings(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "s1"), tmp)
	e := openEngine(t, cfg) // zero value: tcp, DHT off
	if ov := e.Overview(); ov.UTPPort != 0 || ov.Transport != "tcp" || e.mux4 != nil {
		t.Fatalf("tcp: %+v", ov)
	}
	cfg = testConfig(filepath.Join(tmp, "s2"), tmp)
	cfg.Session.PeerOpts.Transport = peer.TransportUTP
	e = openEngine(t, cfg)
	if ov := e.Overview(); ov.UTPPort == 0 || ov.DHT {
		t.Fatalf("utp without DHT: %+v", ov)
	}
	if o := e.peerOpts(); o.UTP == nil || o.Transport != peer.TransportUTP {
		t.Fatalf("session options: %+v", o)
	}
}
