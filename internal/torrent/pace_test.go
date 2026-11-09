package torrent

import (
	"net/netip"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/ratelimit"
)

// A low download limit shared by many peers gives each one less than a
// block per RequestTimeout. They are slow because *we* throttle them, not
// snubbing us, so none may be dropped; and endgame duplicates would only
// compete for our own bandwidth (D36, D46). Before the fix: peers dropped
// and redialed over and over, ~10 copies of every block, 11 of 24 pieces
// after 30 s.
func TestLowDownloadLimitDoesNotSnubPeers(t *testing.T) {
	f := makeTorrent(t, pl, 24*pl)
	var seeders []*seeder
	var addrs []netip.AddrPort
	for i := 0; i < 8; i++ {
		sd := newSeeder(t, f, seederOpts{})
		seeders, addrs = append(seeders, sd), append(addrs, sd.addr)
	}
	tr := newTracker(t, addrs...)
	lim := ratelimit.New(64_000) // ≈ 8 KB/s per peer: a 16 KiB block every ~2 s
	cfg := testConfig()
	cfg.RequestTimeout = 500 * time.Millisecond
	cfg.PeerOpts.ReadLimiter = lim
	cfg.Pacer = NewPacer(lim)

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, cfg, false)
	r.waitComplete(t, 30*time.Second)
	checkOnDisk(t, f, r.dir)
	sent := int64(0)
	for i, sd := range seeders {
		sent += sd.blocksSent.Load()
		if n := sd.accepts.Load(); n != 1 {
			t.Errorf("seeder %d was connected %d times; throttled peers must not be dropped as snubbed", i, n)
		}
	}
	if sent > 24+8 { // a few blocks in flight when a peer is replaced are fine
		t.Errorf("seeders sent %d blocks for a 24-block torrent: duplicate requests under a limit", sent)
	}
	// Once stopped, its peers no longer share the limit with other torrents.
	if err := r.stopAndWait(t); err != nil {
		t.Fatal(err)
	}
	if n := cfg.Pacer.peers.Load(); n != 0 {
		t.Errorf("after the session stopped the pacer still counts %d peers", n)
	}
}

func TestPacer(t *testing.T) {
	var none *Pacer
	if none.PerPeer() != 0 || NewPacer(nil).PerPeer() != 0 || NewPacer(ratelimit.New(0)).PerPeer() != 0 {
		t.Fatal("no limit must mean no pacing")
	}
	p := NewPacer(ratelimit.New(100_000))
	if p.PerPeer() != 100_000 {
		t.Errorf("no peers yet: share = %d", p.PerPeer())
	}
	p.add(4)
	if p.PerPeer() != 25_000 {
		t.Errorf("4 peers: share = %d", p.PerPeer())
	}

	s := &Session{cfg: Config{RequestTimeout: 10 * time.Second, RequestQueue: 32, Pacer: p}}
	if s.requestTimeout() != 10*time.Second || s.pipeline() != 8 { // 25 KB/s: 2 s per block, 5 s ≈ 7.6 blocks
		t.Errorf("25 KB/s each: timeout %v, pipeline %d", s.requestTimeout(), s.pipeline())
	}
	p.add(96) // 1 KB/s each: a block takes 16 s
	if got := s.requestTimeout(); got != time.Duration(3*16384*float64(time.Second)/1000) {
		t.Errorf("1 KB/s each: timeout %v, want 3 × 16.4 s", got)
	}
	if s.pipeline() != 2 {
		t.Errorf("1 KB/s each: pipeline %d, want the minimum 2", s.pipeline())
	}
	p.add(10_000) // absurdly many: capped
	if s.requestTimeout() != maxPacedWait {
		t.Errorf("timeout %v, want the cap %v", s.requestTimeout(), maxPacedWait)
	}
}
