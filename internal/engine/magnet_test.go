package engine

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/resume"
	"github.com/7Eshaan/Denco/internal/storage"
)

// seedEngine runs an engine seeding f from complete data and returns it.
func seedEngine(t *testing.T, f *fixture) *Engine {
	t.Helper()
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "seed")
	f.writeData(t, dir)
	a := openEngine(t, testConfig(filepath.Join(tmp, "state"), dir))
	if _, err := a.Add(f.raw, AddOptions{Existing: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, "1", StateSeeding)
	return a
}

func magnetURI(f *fixture, name, tracker string, peers ...string) string {
	u := "magnet:?xt=urn:btih:" + f.mi.InfoHash.String()
	if name != "" {
		u += "&dn=" + url.QueryEscape(name)
	}
	if tracker != "" {
		u += "&tr=" + url.QueryEscape(tracker)
	}
	for _, p := range peers {
		u += "&x.pe=" + p
	}
	return u
}

func TestMagnetThroughTracker(t *testing.T) {
	tracker := newTracker(t)
	// Many pieces: the info dict spans several metadata pieces.
	f := newFixture(t, "movie", tracker, 16384, 1500*16384+5, 100)
	seedEngine(t, f)

	tmp := t.TempDir()
	dl := filepath.Join(tmp, "dl")
	cfg := testConfig(filepath.Join(tmp, "state"), dl)
	b := openEngine(t, cfg)
	v, err := b.AddMagnet(magnetURI(f, "a name from dn", tracker), AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if v.ID != 1 || v.HasMetadata || v.Name != "a name from dn" || v.TotalBytes != 0 {
		t.Fatalf("before metadata: %+v", v)
	}
	done := waitState(t, b, "1", StateSeeding)
	f.checkData(t, dl)
	if !done.HasMetadata || done.Name != "movie" || done.Files != 2 || done.TotalBytes != int64(len(f.data)) {
		t.Fatalf("after: %+v", done)
	}
	// The fetched metadata was saved as a .torrent with the magnet's tracker.
	store, _ := resume.OpenStore(cfg.StateDir)
	st, mi, err := store.Load(f.mi.InfoHash)
	if err != nil || mi == nil || st.Magnet != "" || mi.Announce != tracker {
		t.Fatalf("saved: %+v %v %v", st, mi, err)
	}
	if files, err := b.Files("1"); err != nil || len(files) != 2 {
		t.Fatalf("files = %v, %v", files, err)
	}

	// Duplicates by info hash, whichever way they're added.
	var dup *DuplicateError
	if _, err := b.AddMagnet(magnetURI(f, "", ""), AddOptions{}); !errors.As(err, &dup) || dup.ID != 1 {
		t.Fatalf("magnet duplicate: %v", err)
	}
	if _, err := b.Add(f.raw, AddOptions{}); !errors.As(err, &dup) {
		t.Fatalf("torrent duplicate of magnet: %v", err)
	}
	if _, err := b.AddMagnet("magnet:?dn=no-hash", AddOptions{}); !errors.Is(err, magnet.ErrInvalid) {
		t.Fatalf("bad magnet: %v", err)
	}
}

func TestMagnetFromPeerAddressSurvivesRestart(t *testing.T) {
	f := newFixture(t, "direct", "", 16384, 50000)
	a := seedEngine(t, f)
	seedAddr := fmt.Sprintf("127.0.0.1:%d", a.Overview().ListenPort)

	tmp := t.TempDir()
	dl := filepath.Join(tmp, "dl")
	cfg := testConfig(filepath.Join(tmp, "state"), dl)
	b := openEngine(t, cfg)
	if _, err := b.AddMagnet(magnetURI(f, "", "", seedAddr), AddOptions{Paused: true}); err != nil {
		t.Fatal(err)
	}
	if v, _ := b.Get("1"); v.State != StatePaused || !strings.HasPrefix(v.Name, "magnet ") {
		t.Fatalf("paused magnet: %+v", v)
	}
	if _, err := b.Files("1"); !errors.Is(err, ErrNoMetadata) {
		t.Fatalf("files before metadata: %v", err)
	}

	// Restart while still a bare magnet: it comes back as one.
	b.Close()
	b2 := openEngine(t, cfg)
	if v, err := b2.Get("1"); err != nil || v.HasMetadata || v.State != StatePaused {
		t.Fatalf("after restart: %+v, %v", v, err)
	}
	if _, err := b2.Resume("1"); err != nil {
		t.Fatal(err)
	}
	waitState(t, b2, "1", StateSeeding)
	f.checkData(t, dl)
}

func TestMagnetWithoutPeersWaitsAndCanBeRemoved(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	e := openEngine(t, cfg)
	f := newFixture(t, "lonely", "", 16384, 1000)
	e.AddMagnet(magnetURI(f, "lonely", ""), AddOptions{})
	waitState(t, e, "1", StateMetadata)
	// The fetcher (started just after the state changes) explains the wait.
	deadline := time.Now().Add(5 * time.Second)
	for v, _ := e.Get("1"); !strings.Contains(v.TrackerError, "DHT"); v, _ = e.Get("1") {
		if time.Now().After(deadline) {
			t.Fatalf("no hint why nothing happens: %q", v.TrackerError)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if o := e.Overview(); o.ByState[StateMetadata] != 1 {
		t.Errorf("overview = %+v", o)
	}
	// It takes a download slot, like any incomplete torrent.
	e.Pause("1")
	waitStopped(t, e, "1")
	r, err := e.Remove("1", true)
	if err != nil || r.FilesDeleted {
		t.Fatalf("remove: %+v, %v", r, err)
	}
	store, _ := resume.OpenStore(cfg.StateDir)
	if entries, _ := store.List(); len(entries) != 0 {
		t.Fatalf("left behind: %d entries", len(entries))
	}
}

func TestMagnetRespectsExistingFiles(t *testing.T) {
	f := newFixture(t, "already", "", 16384, 40000)
	a := seedEngine(t, f)
	seedAddr := fmt.Sprintf("127.0.0.1:%d", a.Overview().ListenPort)

	// Somebody else's file sits where the torrent would go.
	tmp := t.TempDir()
	dl := filepath.Join(tmp, "dl")
	p := f.path(dl, 0)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("not ours"), 0o644)
	e := openEngine(t, testConfig(filepath.Join(tmp, "state"), dl))
	e.AddMagnet(magnetURI(f, "", "", seedAddr), AddOptions{})
	v := waitState(t, e, "1", StateError)
	if !strings.Contains(v.Error, storage.ErrExists.Error()) {
		t.Fatalf("error = %q", v.Error)
	}
	if b, _ := os.ReadFile(p); string(b) != "not ours" {
		t.Fatal("existing file was touched")
	}

	// With -existing and the real data in place, it checks and seeds.
	tmp2 := t.TempDir()
	dl2 := filepath.Join(tmp2, "dl")
	f.writeData(t, dl2)
	e2 := openEngine(t, testConfig(filepath.Join(tmp2, "state"), dl2))
	e2.AddMagnet(magnetURI(f, "", "", seedAddr), AddOptions{Existing: true})
	if v := waitState(t, e2, "1", StateSeeding); v.Downloaded != 0 {
		t.Errorf("downloaded %d bytes of data it already had", v.Downloaded)
	}
}

// pexIntroducer is a peer without metadata whose only use is a PEX message
// naming addrs — so a magnet pointing at it can only work through PEX.
func pexIntroducer(t *testing.T, hash metainfo.Hash, addrs ...netip.AddrPort) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c, err := peer.Accept(nc, func(h metainfo.Hash) bool { return h == hash },
					peer.Options{PeerID: peerid.New(), Extensions: true})
				if err != nil {
					return
				}
				defer c.Close()
				c.Send(peer.Extended(0, peer.ExtHandshake{M: map[string]int{peer.ExtPEX: 3}}.Encode()))
				c.Send(peer.Extended(peer.ExtPEXID, peer.PEX{Added: addrs}.Encode()))
				for range c.Incoming() {
				}
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

// enable_pex reaches the magnet fetcher: a magnet whose only peer knows
// the seed just by PEX completes with PEX on and stays stuck with it off.
func TestMagnetFindsPeersThroughPEX(t *testing.T) {
	f := newFixture(t, "exchanged", "", 16384, 5*16384+3)
	seed := seedEngine(t, f)
	intro := pexIntroducer(t, f.mi.InfoHash, seed.ln.Addr())
	uri := magnetURI(f, "", "", intro.String())

	for _, pex := range []bool{false, true} {
		tmp := t.TempDir()
		dl := filepath.Join(tmp, "dl")
		cfg := testConfig(filepath.Join(tmp, "state"), dl)
		cfg.Session.PEX = pex
		e := openEngine(t, cfg)
		if _, err := e.AddMagnet(uri, AddOptions{}); err != nil {
			t.Fatal(err)
		}
		if !pex {
			time.Sleep(time.Second)
			if v, _ := e.Get("1"); v.State != StateMetadata {
				t.Fatalf("PEX off: state %s, want still fetching metadata", v.State)
			}
			continue
		}
		waitState(t, e, "1", StateSeeding)
		f.checkData(t, dl)
		if ov := e.Overview(); !ov.PEX {
			t.Error("Overview doesn't report PEX on")
		}
	}
}
