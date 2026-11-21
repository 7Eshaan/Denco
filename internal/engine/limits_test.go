package engine

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/resume"
)

// torrentTransferTime is transferTime with the limits set on the torrents
// (D59) instead of the engines: the seeding torrent's own upload limit and
// the downloading torrent's own download limit.
func torrentTransferTime(t *testing.T, seedUp, leechDown int64) time.Duration {
	t.Helper()
	f := newFixture(t, "own-limits", newTracker(t), 16384, 768<<10)
	tmp := t.TempDir()
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	a := openEngine(t, testConfig(filepath.Join(tmp, "sa"), seedDir))
	a.Add(f.raw, AddOptions{Existing: true})
	if _, err := a.SetTorrentLimits("1", 0, seedUp); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, "1", StateSeeding)

	b := openEngine(t, testConfig(filepath.Join(tmp, "sb"), filepath.Join(tmp, "dl")))
	b.Add(f.raw, AddOptions{Paused: true})
	if _, err := b.SetTorrentLimits("1", leechDown, 0); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	b.Resume("1")
	waitState(t, b, "1", StateSeeding)
	f.checkData(t, filepath.Join(tmp, "dl"))
	return time.Since(start)
}

func TestTorrentLimits(t *testing.T) {
	// 768 KiB at 1 MiB/s: the 256 KiB burst is free, the rest takes ~0.5s.
	const limit = 1 << 20
	if d := torrentTransferTime(t, 0, 0); d > 400*time.Millisecond {
		t.Skipf("unlimited transfer already takes %v here; can't measure limits", d)
	}
	if d := torrentTransferTime(t, limit, 0); d < 400*time.Millisecond {
		t.Errorf("the torrent's upload limit wasn't applied: took %v", d)
	}
	if d := torrentTransferTime(t, 0, limit); d < 400*time.Millisecond {
		t.Errorf("the torrent's download limit wasn't applied: took %v", d)
	}
}

func TestTorrentLimitsAreSavedAndShown(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	e := openEngine(t, cfg)
	f := newFixture(t, "lim", "", 16384, 3000)
	e.Add(f.raw, AddOptions{})
	waitState(t, e, "1", StateStalled)

	v, err := e.SetTorrentLimits("1", 500_000, 2_000_000) // while running
	if err != nil || v.DownLimit != 500_000 || v.UpLimit != 2_000_000 {
		t.Fatalf("set: %+v, %v", v, err)
	}
	if _, err := e.SetTorrentLimits("1", -1, 0); !errors.Is(err, ErrBadLimit) {
		t.Fatalf("negative limit: %v", err)
	}
	e.Pause("1") // limits outlive the run
	waitStopped(t, e, "1")
	if v, _ := e.Get("1"); v.DownLimit != 500_000 || v.UpLimit != 2_000_000 {
		t.Fatalf("after pause: %d, %d", v.DownLimit, v.UpLimit)
	}
	e.Close()
	store, _ := resume.OpenStore(cfg.StateDir)
	if st, _, err := store.Load(f.mi.InfoHash); err != nil || st.DownLimit != 500_000 || st.UpLimit != 2_000_000 {
		t.Fatalf("saved: %+v, %v", st, err)
	}
	e2 := openEngine(t, cfg)
	if v, _ := e2.Get("1"); v.DownLimit != 500_000 || v.UpLimit != 2_000_000 {
		t.Fatalf("after restart: %d, %d", v.DownLimit, v.UpLimit)
	}
	e2.SetTorrentLimits("1", 0, 0) // lifting them is saved too
	e2.Close()
	if st, _, _ := store.Load(f.mi.InfoHash); st.DownLimit != 0 || st.UpLimit != 0 {
		t.Fatalf("lifted limits saved as %d, %d", st.DownLimit, st.UpLimit)
	}
}
