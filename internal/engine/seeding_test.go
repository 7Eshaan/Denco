package engine

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRatio(t *testing.T) {
	cases := []struct {
		up, down, done int64
		want           float64
	}{
		{0, 0, 0, -1},
		{50, 100, 100, 0.5},
		{300, 0, 100, 3},     // data was already there: what we have counts
		{100, 200, 100, 0.5}, // wasted downloads count against us
	}
	for _, c := range cases {
		if got := ratio(c.up, c.down, c.done); got != c.want {
			t.Errorf("ratio(%d, %d, %d) = %v, want %v", c.up, c.down, c.done, got, c.want)
		}
	}
}

func TestSeedStopReason(t *testing.T) {
	h := time.Hour
	cases := []struct {
		mode     string
		r        float64
		seeded   time.Duration
		limRatio float64
		limTime  time.Duration
		want     string // substring; "" = keep seeding
	}{
		{"", 5, 100 * h, 0, 0, ""},
		{"", 2, 0, 2, 0, "ratio 2.00 reached the limit 2"},
		{"", 1.99, 0, 2, 0, ""},
		{"", -1, 0, 2, 0, ""},
		{"", 0, 25 * h, 0, 24 * h, "seeded for 1d 1h"},
		{SeedOn, 9, 99 * h, 1, h, ""},
		{SeedOff, 0, 0, 0, 0, "seeding is off"},
	}
	for _, c := range cases {
		got := seedStopReason(c.mode, c.r, c.seeded, c.limRatio, c.limTime)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("seedStopReason(%q, %v, %v, %v, %v) = %q, want %q", c.mode, c.r, c.seeded, c.limRatio, c.limTime, got, c.want)
		}
	}
}

// seedPair: engine A seeds f (data already on disk); B downloads it.
func seedPair(t *testing.T, f *fixture, tmp string, cfgA func(*Config)) (a, b *Engine, stateA, seedDir string) {
	t.Helper()
	seedDir, stateA = filepath.Join(tmp, "seed"), filepath.Join(tmp, "sa")
	f.writeData(t, seedDir)
	ca := testConfig(stateA, seedDir)
	if cfgA != nil {
		cfgA(&ca)
	}
	a = openEngine(t, ca)
	if _, err := a.Add(f.raw, AddOptions{Existing: true}); err != nil {
		t.Fatal(err)
	}
	waitState(t, a, "1", StateSeeding)
	b = openEngine(t, testConfig(filepath.Join(tmp, "sb"), filepath.Join(tmp, "dl")))
	return a, b, stateA, seedDir
}

func TestSeedRatioLimitFinishesAndPersists(t *testing.T) {
	f := newFixture(t, "ratio", newTracker(t), 16384, 100000)
	tmp := t.TempDir()
	a, b, stateA, seedDir := seedPair(t, f, tmp, nil)
	b.Add(f.raw, AddOptions{})
	waitState(t, b, "1", StateSeeding)

	// A uploaded the whole torrent (ratio ~1). A limit set live stops it.
	deadline := time.Now().Add(5 * time.Second)
	for v, _ := a.Get("1"); v.State != StateSeeding || v.Ratio < 1; v, _ = a.Get("1") {
		if time.Now().After(deadline) { // its upload count appears on its next tick
			t.Fatalf("seeder before the limit: state %s, ratio %v", v.State, v.Ratio)
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.SetSeedLimits(0.5, 0)
	v := waitState(t, a, "1", StateFinished)
	if !strings.Contains(v.SeedNote, "ratio") {
		t.Errorf("SeedNote = %q", v.SeedNote)
	}
	waitStopped(t, a, "1")
	if o := a.Overview(); o.SeedRatio != 0.5 || o.ByState[StateFinished] != 1 {
		t.Errorf("overview = %+v", o)
	}

	// Finished is saved: a restart doesn't start it — even with no limits
	// configured any more — and Resume refuses while a limit applies.
	a.Close()
	a = openEngine(t, testConfig(stateA, seedDir))
	waitState(t, a, "1", StateFinished)
	time.Sleep(100 * time.Millisecond)
	a.mu.Lock()
	started := a.byID[1].running
	a.mu.Unlock()
	if started {
		t.Fatal("a finished torrent was started after a restart")
	}
	a.SetSeedLimits(0.5, 0)
	if _, err := a.Resume("1"); !errors.Is(err, ErrSeedLimit) || !strings.Contains(err.Error(), "denco seed on 1") {
		t.Fatalf("Resume past the limit: %v", err)
	}
	// "seed on" ignores the limits.
	if v, err := a.SetSeedMode("1", SeedOn); err != nil || v.SeedMode != SeedOn {
		t.Fatalf("SetSeedMode(on) = %+v, %v", v, err)
	}
	waitState(t, a, "1", StateSeeding)
	time.Sleep(100 * time.Millisecond) // several limit checks
	if v, _ := a.Get("1"); v.State != StateSeeding {
		t.Fatalf("mode on must ignore the ratio limit: %+v", v)
	}
	if _, err := a.SetSeedMode("1", "sometimes"); !errors.Is(err, ErrBadSeedMode) {
		t.Errorf("bad mode: %v", err)
	}
}

func TestSeedOffStopsOnCompletion(t *testing.T) {
	f := newFixture(t, "off", newTracker(t), 16384, 60000, 40000)
	tmp := t.TempDir()
	_, b, _, _ := seedPair(t, f, tmp, nil)
	b.Add(f.raw, AddOptions{Paused: true})
	if _, err := b.SetSeedMode("1", SeedOff); err != nil {
		t.Fatal(err)
	}
	b.Resume("1")
	v := waitState(t, b, "1", StateFinished)
	if !v.Complete || !strings.Contains(v.SeedNote, "off") {
		t.Fatalf("after completing with seeding off: %+v", v)
	}
	f.checkData(t, filepath.Join(tmp, "dl"))

	// Back to the (empty) global limits: it seeds again by itself.
	waitStopped(t, b, "1")
	b.SetSeedMode("1", SeedDefault)
	waitState(t, b, "1", StateSeeding)

	// And "off" on a seeding torrent stops it at once.
	if v, _ := b.SetSeedMode("1", SeedOff); v.State != StateFinished {
		t.Fatalf("seed off while seeding: state %s", v.State)
	}
	// Resume refuses: seeding is off for it — also while it is still
	// shutting down, and afterwards.
	if _, err := b.Resume("1"); !errors.Is(err, ErrSeedLimit) {
		t.Errorf("Resume with seeding off (stopping): %v", err)
	}
	waitStopped(t, b, "1")
	if _, err := b.Resume("1"); !errors.Is(err, ErrSeedLimit) {
		t.Errorf("Resume with seeding off (stopped): %v", err)
	}
}

func TestUploadsPausedPerTorrent(t *testing.T) {
	f := newFixture(t, "uploads", newTracker(t), 16384, 80000)
	tmp := t.TempDir()
	a, b, stateA, seedDir := seedPair(t, f, tmp, nil)
	if v, err := a.SetUploadsPaused("1", true); err != nil || !v.UploadsPaused {
		t.Fatalf("SetUploadsPaused = %+v, %v", v, err)
	}
	b.Add(f.raw, AddOptions{})
	time.Sleep(500 * time.Millisecond)
	if v, _ := b.Get("1"); v.PiecesHave != 0 {
		t.Fatalf("got %d pieces from a seeder with uploads paused", v.PiecesHave)
	}
	// The choice survives a restart of the seeder.
	a.Close()
	a = openEngine(t, testConfig(stateA, seedDir))
	if v := waitState(t, a, "1", StateSeeding); !v.UploadsPaused {
		t.Fatal("uploads paused was not saved")
	}
	time.Sleep(300 * time.Millisecond)
	if v, _ := b.Get("1"); v.PiecesHave != 0 {
		t.Fatalf("got %d pieces after the restart, uploads still paused", v.PiecesHave)
	}
	a.SetUploadsPaused("1", false)
	waitState(t, b, "1", StateSeeding)
	f.checkData(t, filepath.Join(tmp, "dl"))
}

func TestSeedTimeCountsOnlyUnpausedSeeding(t *testing.T) {
	f := newFixture(t, "time", "", 16384, 30000)
	tmp := t.TempDir()
	dir := filepath.Join(tmp, "data")
	f.writeData(t, dir)
	cfg := testConfig(filepath.Join(tmp, "s"), dir)
	cfg.SeedTime = 300 * time.Millisecond
	e := openEngine(t, cfg)
	e.Add(f.raw, AddOptions{Existing: true, Paused: true})
	e.SetUploadsPaused("1", true)
	e.Resume("1")
	waitState(t, e, "1", StateSeeding)
	time.Sleep(600 * time.Millisecond) // twice the limit, uploads paused
	if v, _ := e.Get("1"); v.State != StateSeeding || v.SeedingTime > 100*time.Millisecond {
		t.Fatalf("seeding time grew while uploads were paused: %+v", v)
	}
	e.SetUploadsPaused("1", false)
	start := time.Now()
	v := waitState(t, e, "1", StateFinished)
	// The limit fired only after ~300ms of unpaused seeding (the paused
	// 600ms didn't count). Once the run has stopped, SeedingTime is the
	// saved value: whole seconds, rounded up (D45).
	if el := time.Since(start); el < 250*time.Millisecond || v.SeedingTime < 300*time.Millisecond {
		t.Fatalf("finished %v after uploads resumed, seeding time %v; want ≥ 300ms of real seeding", el, v.SeedingTime)
	}
	if !strings.Contains(v.SeedNote, "seeded for") {
		t.Errorf("SeedNote = %q", v.SeedNote)
	}
}
