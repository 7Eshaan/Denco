package torrent

import (
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
)

const pl = 16 * 1024 // piece length used by most tests (1 block per piece)

func TestDownloadFromOneSeeder(t *testing.T) {
	f := makeTorrent(t, pl*2, 100_000, 3, 250_000) // multi-file, odd sizes, 2 blocks/piece
	sd := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, sd.addr)

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
	r.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, r.dir)

	st := r.s.Stats()
	if st.BytesLeft != 0 || st.PiecesHave != st.PiecesTotal {
		t.Errorf("stats after completion: %+v", st)
	}
	if st.Downloaded < int64(len(f.data)) {
		t.Errorf("Downloaded = %d, want >= %d", st.Downloaded, len(f.data))
	}
	if err := r.stopAndWait(t); err != nil {
		t.Fatalf("Run error: %v", err)
	}
	// Tracker saw started → completed → stopped.
	ev := tr.eventLog()
	if len(ev) < 3 || ev[0] != "started" || !slices.Contains(ev, "completed") || ev[len(ev)-1] != "stopped" {
		t.Errorf("tracker events = %v", ev)
	}
}

func TestDownloadFromPartialSeeders(t *testing.T) {
	f := makeTorrent(t, pl, 20*pl+500)
	n := f.mi.Info.NumPieces()
	var even, odd []int
	for i := 0; i < n; i++ {
		if i%2 == 0 {
			even = append(even, i)
		} else {
			odd = append(odd, i)
		}
	}
	a := newSeeder(t, f, seederOpts{pieces: even})
	b := newSeeder(t, f, seederOpts{pieces: odd})
	tr := newTracker(t, a.addr, b.addr)

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
	r.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, r.dir)
	if a.blocksSent.Load() == 0 || b.blocksSent.Load() == 0 {
		t.Errorf("both seeders should have served: a=%d b=%d", a.blocksSent.Load(), b.blocksSent.Load())
	}
}

func TestCorruptPeerIsBannedAndDataStillCorrect(t *testing.T) {
	f := makeTorrent(t, pl, 30*pl)
	bad := newSeeder(t, f, seederOpts{corrupt: true})
	good := newSeeder(t, f, seederOpts{})
	// Only the corrupt peer is known at first, so it is certainly the sole
	// contributor of the pieces it sends — and must be banned for them.
	tr := newTracker(t, bad.addr)

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
	deadline := time.Now().Add(10 * time.Second)
	// The ban can happen within a single tick, so watch the seeder's side
	// (it sent data) rather than hoping to catch "1 peer" in a snapshot.
	for bad.blocksSent.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("never received data from the corrupt peer")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Give it a few ticks for the hash check and ban, then expect no peers.
	time.Sleep(100 * time.Millisecond)
	for r.s.Stats().Peers != 0 {
		if time.Now().After(deadline) {
			t.Fatal("corrupt peer never disconnected")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if r.s.Stats().PiecesHave != 0 {
		t.Fatal("a corrupt piece was accepted")
	}

	r.s.AddPeers([]netip.AddrPort{good.addr})
	r.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, r.dir) // no corrupt piece reached the disk
	if err := r.stopAndWait(t); err != nil {
		t.Fatal(err)
	}
	// Run has returned, so reading loop-owned state is safe now.
	if !r.s.swarm.isBanned(bad.addr) || r.s.swarm.isBanned(good.addr) {
		t.Errorf("bans: bad=%v good=%v", r.s.swarm.isBanned(bad.addr), r.s.swarm.isBanned(good.addr))
	}
	if n := bad.accepts.Load(); n != 1 {
		t.Errorf("banned peer was redialed (%d connections)", n)
	}
}

// TestHashFailureNearEndDoesNotStall reproduces a real bug: when the last
// pieces were all pending verification we sent "not interested"; if one then
// failed its hash we never said "interested" again and the download froze.
func TestHashFailureNearEndDoesNotStall(t *testing.T) {
	f := makeTorrent(t, pl, 6*pl)
	bad := newSeeder(t, f, seederOpts{corrupt: true})
	good := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, bad.addr, good.addr)
	for i := 0; i < 15; i++ { // many runs: the race only bites sometimes
		r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
		r.waitComplete(t, 15*time.Second)
		checkOnDisk(t, f, r.dir)
		r.stopAndWait(t)
	}
}

func TestResumeOnlyFetchesMissingPieces(t *testing.T) {
	f := makeTorrent(t, pl, 10*pl)
	dir := t.TempDir()

	// Pieces 0–5 are already on disk and verified.
	have := bitfield.New(10)
	path := filepath.Join(dir, "fixture", "dir", "fileA")
	os.MkdirAll(filepath.Dir(path), 0o755)
	os.WriteFile(path, f.data[:6*pl], 0o644)
	for i := 0; i < 6; i++ {
		have.Set(i)
	}

	sd := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, sd.addr)
	r := start(t, f.withTracker(t, tr.srv.URL), dir, have, testConfig(), true)
	r.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, dir)
	for i := 0; i < 6; i++ {
		if sd.wasRequested(i) {
			t.Errorf("piece %d was already verified but got requested", i)
		}
	}
	for i := 6; i < 10; i++ {
		if !sd.wasRequested(i) {
			t.Errorf("missing piece %d never requested", i)
		}
	}
}

func TestReconnectsAfterPeerDrops(t *testing.T) {
	f := makeTorrent(t, pl, 12*pl)
	sd := newSeeder(t, f, seederOpts{dropAfter: 3}) // hangs up every 3 blocks
	tr := newTracker(t, sd.addr)

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
	r.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, r.dir)
	if sd.accepts.Load() < 4 {
		t.Errorf("expected repeated reconnects, seeder accepted %d", sd.accepts.Load())
	}
}

func TestSnubbedPeerIsDropped(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	sd := newSeeder(t, f, seederOpts{neverRespond: true})
	tr := newTracker(t, sd.addr)
	cfg := testConfig()
	cfg.RequestTimeout = 150 * time.Millisecond
	cfg.RetryBase = time.Hour // don't reconnect during the test

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, cfg, false)
	deadline := time.Now().Add(5 * time.Second)
	sawConnected := false
	for time.Now().Before(deadline) {
		st := r.s.Stats()
		if st.Peers == 1 {
			sawConnected = true
		}
		if sawConnected && st.Peers == 0 {
			if st.State != StateStalled {
				t.Errorf("state = %s, want stalled", st.State)
			}
			return // dropped as expected
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("snubbing peer not dropped (sawConnected=%v, stats=%+v)", sawConnected, r.s.Stats())
}

func TestTrackerStatsAndPeerList(t *testing.T) {
	f := makeTorrent(t, pl, 100*pl)
	// Slow seeder: the download must last long enough to observe it, since
	// a finished session drops its connection to other seeds.
	sd := newSeeder(t, f, seederOpts{slow: 5 * time.Millisecond})
	tr := newTracker(t, sd.addr)
	tr.seeders, tr.leechers = 42, 7

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		st := r.s.Stats()
		if st.TrackerSeeders == 42 && st.TrackerLeechers == 7 && st.Peers == 1 {
			peers := r.s.Peers()
			if len(peers) != 1 || peers[0].Addr != sd.addr || peers[0].Progress != 1 || !peers[0].Outgoing {
				t.Fatalf("peers = %+v", peers)
			}
			if st.Seeds != 1 || st.Leechers != 0 {
				t.Fatalf("seeds/leechers = %d/%d", st.Seeds, st.Leechers)
			}
			r.waitComplete(t, 20*time.Second)
			// Still running after completion → seeding (Phase 6).
			if st := r.s.Stats(); st.State != StateSeeding || st.ETA() != 0 {
				t.Fatalf("final stats %+v", st)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("stats never reflected tracker/peer: %+v", r.s.Stats())
}

func TestAddPeersWithoutTracker(t *testing.T) {
	f := makeTorrent(t, pl, 5*pl)
	sd := newSeeder(t, f, seederOpts{})
	r := start(t, f.mi, t.TempDir(), nil, testConfig(), false) // trackerless
	r.s.AddPeers([]netip.AddrPort{sd.addr})
	r.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, r.dir)
}

func TestDeadTrackerAndDeadPeersDontCrash(t *testing.T) {
	f := makeTorrent(t, pl, 3*pl)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	deadPeer := netip.MustParseAddrPort(ln.Addr().String())
	ln.Close()
	mi := f.withTracker(t, "http://127.0.0.1:1/announce") // nothing listens on port 1

	r := start(t, mi, t.TempDir(), nil, testConfig(), false)
	r.s.AddPeers([]netip.AddrPort{deadPeer})
	time.Sleep(300 * time.Millisecond)
	st := r.s.Stats()
	if st.State != StateStalled || st.TrackerError == "" {
		t.Errorf("stats = %+v", st)
	}
	if err := r.stopAndWait(t); err != nil {
		t.Fatal(err)
	}
}

func TestStorageErrorStopsSession(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	sd := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, sd.addr)
	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)
	r.st.Close() // every write will now fail
	select {
	case err := <-r.done:
		if !errors.Is(err, ErrStorage) {
			t.Fatalf("err = %v, want ErrStorage", err)
		}
		r.done <- err
	case <-time.After(10 * time.Second):
		t.Fatal("session kept running after storage failure")
	}
}

func TestAlreadyCompleteWithNoSeedReturnsImmediately(t *testing.T) {
	f := makeTorrent(t, pl, 2*pl)
	have := bitfield.New(2)
	have.SetAll()
	sd := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, sd.addr)
	cfg := testConfig()
	cfg.NoSeed = true
	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), have, cfg, false)
	select {
	case err := <-r.done:
		r.done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("NoSeed session with nothing to download kept running")
	}
	if sd.accepts.Load() != 0 {
		t.Error("NoSeed complete torrent dialed peers")
	}
	if st := r.s.Stats(); st.State != StateComplete {
		t.Errorf("state = %s", st.State)
	}
	if ev := tr.eventLog(); slices.Contains(ev, "completed") {
		t.Errorf("sent completed for a torrent that started complete: %v", ev)
	}
}

func TestSeedDropsOtherSeeds(t *testing.T) {
	f := makeTorrent(t, pl, 2*pl)
	have := bitfield.New(2)
	have.SetAll()
	sd := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, sd.addr)
	cfg := testConfig()
	cfg.RetryBase = time.Hour
	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), have, cfg, false)
	deadline := time.Now().Add(5 * time.Second)
	for sd.accepts.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("seeding session never dialed (peers behind NAT need that)")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if st := r.s.Stats(); st.Peers != 0 || st.State != StateSeeding {
		t.Errorf("seed-to-seed connection kept: %+v", st)
	}
}

func TestRate(t *testing.T) {
	var r rate
	if r.perSecond(time.Second) != 0 {
		t.Fatal("empty rate")
	}
	r.add(1000)
	r.tick()
	r.add(3000)
	r.tick()
	if got := r.perSecond(time.Second); got != 2000 {
		t.Fatalf("rate = %d, want 2000", got)
	}
	if got := r.perSecond(500 * time.Millisecond); got != 4000 {
		t.Fatalf("half-second ticks: rate = %d, want 4000", got)
	}
	for i := 0; i < rateWindow; i++ {
		r.tick() // window slides past the old data
	}
	if r.perSecond(time.Second) != 0 {
		t.Fatal("old data not expired")
	}
}

func TestSwarmBackoff(t *testing.T) {
	w := newSwarm(time.Second)
	a := netip.MustParseAddrPort("1.2.3.4:5")
	w.add([]netip.AddrPort{a, a, netip.MustParseAddrPort("0.0.0.0:1"), netip.MustParseAddrPort("1.1.1.1:0")})
	if len(w.addrs) != 1 {
		t.Fatalf("addrs = %v", w.addrs)
	}
	now := time.Now()
	if got := w.next(5, now); len(got) != 1 {
		t.Fatal("first dial")
	}
	if got := w.next(5, now); len(got) != 0 {
		t.Fatal("dialing peer handed out twice")
	}
	w.dialFailed(a, now)
	w.dialFailed(a, now) // pretend: 2 failures → 2s backoff
	if got := w.next(5, now.Add(1500*time.Millisecond)); len(got) != 0 {
		t.Fatal("retried before backoff")
	}
	if got := w.next(5, now.Add(2100*time.Millisecond)); len(got) != 1 {
		t.Fatal("not retried after backoff")
	}
	if w.backoff(100) != maxBackoff {
		t.Fatal("backoff not capped")
	}
	w.ban(a)
	w.dialFailed(a, now)
	if !w.isBanned(a) || len(w.next(5, now.Add(time.Hour))) != 0 {
		t.Fatal("banned peer dialable")
	}
}

func TestNoGoroutineLeakAfterStop(t *testing.T) {
	f := makeTorrent(t, pl, 40*pl)
	sd := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, sd.addr)
	mi := f.withTracker(t, tr.srv.URL)
	before := runtime.NumGoroutine()
	for i := 0; i < 10; i++ {
		r := start(t, mi, t.TempDir(), nil, testConfig(), false)
		time.Sleep(time.Duration(i*5) * time.Millisecond) // stop at different stages
		if err := r.stopAndWait(t); err != nil {
			t.Fatal(err)
		}
	}
	// The seeder and http server keep a few goroutines per open connection;
	// allow for those to wind down.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= before+4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("goroutines: before=%d after=%d", before, runtime.NumGoroutine())
}

func TestHaveSnapshot(t *testing.T) {
	f := makeTorrent(t, pl, 6*pl)
	have := bitfield.New(6)
	have.Set(2)
	sd := newSeeder(t, f, seederOpts{})
	r := start(t, f.mi, t.TempDir(), have, testConfig(), false)
	if got := r.s.Have(); got.Count() != 1 || !got.Has(2) {
		t.Fatalf("initial Have = %08b", got.Bytes())
	}
	got := r.s.Have()
	got.Set(0) // a copy: must not affect the session
	if r.s.Have().Has(0) {
		t.Fatal("Have() returned shared state")
	}
	r.s.AddPeers([]netip.AddrPort{sd.addr})
	r.waitComplete(t, 20*time.Second)
	if !r.s.Have().Full() {
		t.Fatal("Have() not full after completion")
	}
}
