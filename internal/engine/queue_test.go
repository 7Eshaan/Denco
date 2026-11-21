package engine

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"github.com/7Eshaan/Denco/internal/resume"
)

// listOrder is the IDs as List returns them (queue order).
func listOrder(e *Engine) []int {
	var ids []int
	for _, v := range e.List() {
		ids = append(ids, v.ID)
	}
	return ids
}

// queueOf is the "position of length" each listed torrent shows.
func queueOf(t *testing.T, e *Engine, ref string) (int, int) {
	t.Helper()
	v, err := e.Get(ref)
	if err != nil {
		t.Fatal(err)
	}
	return v.Queue, v.QueueLength
}

func TestQueueMovesAndOrder(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	cfg.MaxActive = 1
	e := openEngine(t, cfg)

	// #1 is complete (it seeds and isn't queued); #2, #3, #4 download.
	done := newFixture(t, "done", "", 16384, 1000)
	done.writeData(t, cfg.DownloadDir)
	e.Add(done.raw, AddOptions{Existing: true})
	waitState(t, e, "1", StateSeeding)
	for _, name := range []string{"a", "b", "c"} {
		e.Add(newFixture(t, name, "", 16384, 1000).raw, AddOptions{})
	}
	waitState(t, e, "2", StateStalled)
	waitState(t, e, "4", StateQueued)

	order := func(want ...int) {
		t.Helper()
		if got := listOrder(e); !slices.Equal(got, want) {
			t.Fatalf("queue order %v, want %v", got, want)
		}
	}
	order(1, 2, 3, 4)
	if p, n := queueOf(t, e, "3"); p != 2 || n != 3 {
		t.Fatalf("#3 at %d of %d, want 2 of 3", p, n)
	}
	if p, _ := queueOf(t, e, "1"); p != 0 {
		t.Fatalf("a complete torrent shows queue position %d", p)
	}

	e.MoveInQueue("4", QueueTop)
	order(4, 1, 2, 3)
	// Moving doesn't stop what is already downloading.
	if v, _ := e.Get("4"); v.State != StateQueued {
		t.Fatalf("#4 = %s; a move must not stop the running download", v.State)
	}
	e.MoveInQueue("3", QueueUp)
	order(4, 1, 3, 2)
	// Up steps past the previous *download*: the complete #1 doesn't count.
	e.MoveInQueue("3", QueueUp)
	order(3, 4, 1, 2)
	if p, _ := queueOf(t, e, "2"); p != 3 {
		t.Fatalf("#2 at %d, want 3", p)
	}
	e.MoveInQueue("3", QueueUp) // already first: no change
	order(3, 4, 1, 2)
	e.MoveInQueue("3", QueueDown)
	order(4, 3, 1, 2)
	e.MoveInQueue("4", QueueBottom)
	order(3, 1, 2, 4)

	// The next free slot goes to the queue's head, not the lowest ID.
	e.MoveInQueue("4", QueueTop)
	order(4, 3, 1, 2)
	e.Pause("2")
	waitState(t, e, "4", StateStalled)
	if v, _ := e.Get("3"); v.State != StateQueued {
		t.Fatalf("#3 = %s, want queued behind #4", v.State)
	}

	// Errors: unknown move, complete torrent, unknown torrent.
	if _, err := e.MoveInQueue("3", "sideways"); !errors.Is(err, ErrBadQueueMove) {
		t.Fatalf("bad move: %v", err)
	}
	if _, err := e.MoveInQueue("1", QueueTop); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("complete torrent: %v", err)
	}
	if _, err := e.MoveInQueue("9", QueueTop); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown torrent: %v", err)
	}

	// The order survives a restart.
	e.Close()
	e = openEngine(t, cfg)
	order(4, 3, 1, 2)
	waitState(t, e, "4", StateStalled)
	waitState(t, e, "3", StateQueued)
	waitState(t, e, "2", StatePaused)
}

func TestForceStartIgnoresMaxActive(t *testing.T) {
	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	cfg.MaxActive = 1
	e := openEngine(t, cfg)
	e.Add(newFixture(t, "one", "", 16384, 1000).raw, AddOptions{})
	e.Add(newFixture(t, "two", "", 16384, 1000).raw, AddOptions{})
	e.Add(newFixture(t, "three", "", 16384, 1000).raw, AddOptions{})
	waitState(t, e, "1", StateStalled)
	waitState(t, e, "3", StateQueued)

	if v, err := e.ForceStart("3"); err != nil || !v.Forced {
		t.Fatalf("force: %+v, %v", v.Forced, err)
	}
	waitState(t, e, "3", StateStalled)
	// A forced torrent doesn't take the slot: #2 still waits for #1.
	if v, _ := e.Get("2"); v.State != StateQueued {
		t.Fatalf("#2 = %s, want queued", v.State)
	}

	// Forced is saved and honoured after a restart.
	e.Close()
	store, _ := resume.OpenStore(cfg.StateDir)
	e2 := openEngine(t, cfg)
	waitState(t, e2, "1", StateStalled)
	if v := waitState(t, e2, "3", StateStalled); !v.Forced {
		t.Fatal("forced lost on restart")
	}
	if v, _ := e2.Get("2"); v.State != StateQueued {
		t.Fatalf("#2 = %s after restart, want queued", v.State)
	}
	// The forced #3 holds no slot: when #1 stops, #2 gets it.
	e2.Pause("1")
	waitState(t, e2, "2", StateStalled)

	// Resuming normally ends the force (a running torrent keeps running).
	if v, _ := e2.Resume("3"); v.Forced {
		t.Fatal("resume kept the force")
	}
	// Pausing ends it too, and saves that.
	e2.ForceStart("2")
	e2.Pause("2")
	waitStopped(t, e2, "2")
	if v, _ := e2.Get("2"); v.Forced {
		t.Fatal("pause kept the force")
	}
	e2.Close()
	list, _ := store.List()
	for _, en := range list {
		if en.State.Forced {
			t.Fatalf("%s saved as forced", en.State.Name)
		}
		if en.State.Queue == 0 {
			t.Fatalf("%s saved without a queue position", en.State.Name)
		}
	}
}
