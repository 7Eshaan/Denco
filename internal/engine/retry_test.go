package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRetryDelay(t *testing.T) {
	m := time.Minute
	for n, want := range map[int]time.Duration{1: m, 2: 2 * m, 3: 4 * m, 7: 60 * m, 30: 60 * m} {
		if got := retryDelay(m, time.Hour, n); got != want {
			t.Errorf("retryDelay(%d) = %v, want %v", n, got, want)
		}
	}
}

func retryConfig(t *testing.T, tmp string) Config {
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	cfg.RetryBase, cfg.RetryMax = 50*time.Millisecond, 200*time.Millisecond
	return cfg
}

func TestFailedTorrentRetriesByItself(t *testing.T) {
	tmp := t.TempDir()
	e := openEngine(t, retryConfig(t, tmp))
	// The empty second file is created when the torrent starts, which
	// fails while a plain file sits where its folder must go.
	f := newFixture(t, "retry", "", 16384, 3000, 0)
	block := filepath.Dir(f.path(filepath.Join(tmp, "dl"), 1))
	os.MkdirAll(filepath.Dir(block), 0o755)
	os.WriteFile(block, []byte("in the way"), 0o644)

	if _, err := e.Add(f.raw, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	v := waitState(t, e, "1", StateError)
	if v.RetryAt.IsZero() || v.Retries < 1 {
		t.Fatalf("a transient error must schedule a retry: %+v", v)
	}
	// It keeps trying on its own.
	deadline := time.Now().Add(5 * time.Second)
	for v, _ = e.Get("1"); v.Retries < 3; v, _ = e.Get("1") {
		if time.Now().After(deadline) {
			t.Fatalf("retries = %d after 5 s", v.Retries)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Pausing cancels the retry…
	e.Pause("1")
	if v, _ := e.Get("1"); !v.RetryAt.IsZero() {
		t.Errorf("paused, but a retry is still due at %v", v.RetryAt)
	}
	time.Sleep(300 * time.Millisecond)
	if v, _ := e.Get("1"); v.State != StatePaused {
		t.Fatalf("a retry ran on a paused torrent: state %s", v.State)
	}
	// …and resuming it counts as the user's retry: failures start over.
	e.Resume("1")
	deadline = time.Now().Add(5 * time.Second)
	for v, _ = e.Get("1"); v.State != StateError || v.Retries != 1; v, _ = e.Get("1") {
		if time.Now().After(deadline) {
			t.Fatalf("after resume: %+v", v)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Once the problem goes away, the next retry succeeds — no user action.
	os.Remove(block)
	waitState(t, e, "1", StateStalled)
	if _, err := os.Stat(f.path(filepath.Join(tmp, "dl"), 1)); err != nil {
		t.Errorf("the empty file wasn't created: %v", err)
	}
}

func TestPermanentErrorIsNotRetried(t *testing.T) {
	tmp := t.TempDir()
	e := openEngine(t, retryConfig(t, tmp))
	f := newFixture(t, "mine", "", 16384, 3000)
	e.Add(f.raw, AddOptions{Paused: true})
	p := f.path(filepath.Join(tmp, "dl"), 0)
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("the user's own file"), 0o644) // appeared after add: not ours
	e.Resume("1")
	v := waitState(t, e, "1", StateError)
	if !v.RetryAt.IsZero() {
		t.Fatalf("someone else's file can't go away by retrying: %+v", v)
	}
	time.Sleep(300 * time.Millisecond) // several RetryMax
	if v, _ := e.Get("1"); v.State != StateError || v.Retries != 0 {
		t.Fatalf("permanent error was retried: %+v", v)
	}
	if got, _ := os.ReadFile(p); string(got) != "the user's own file" {
		t.Fatal("the user's file was touched")
	}
}
