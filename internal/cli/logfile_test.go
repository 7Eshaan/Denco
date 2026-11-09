package cli

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
)

func TestLogFileRotates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "logs", "denco.log")
	l, err := openLogFile(p, 1000)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n" // 100 bytes
	var wg sync.WaitGroup
	for g := 0; g < 5; g++ { // slog writes from many goroutines
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				l.Write([]byte(line))
			}
		}()
	}
	wg.Wait()
	l.Close()
	cur, _ := os.ReadFile(p)
	old, err := os.ReadFile(p + ".1")
	if err != nil {
		t.Fatalf("no rotated file: %v", err)
	}
	if len(cur) > 1000 || len(old) > 1000 || len(cur)+len(old) < 1500 {
		t.Errorf("current %d bytes, .1 %d bytes; want each ≤ 1000 and the newest 2500 split between them", len(cur), len(old))
	}
	if strings.Count(string(cur), line)*100 != len(cur) {
		t.Error("a line was split or interleaved")
	}

	// Reopening appends rather than truncating.
	l, _ = openLogFile(p, 1000)
	l.Write([]byte("again\n"))
	l.Close()
	if got, _ := os.ReadFile(p); !strings.HasSuffix(string(got), "again\n") || len(got) != len(cur)+6 {
		t.Errorf("reopened log has %d bytes, want %d", len(got), len(cur)+6)
	}
}

func TestDaemonLogsToFile(t *testing.T) {
	home, tmp := t.TempDir(), t.TempDir()
	logPath := filepath.Join(tmp, "denco.log")
	h := startDaemonArgs(t, home, "-port", "0", "-dir", tmp, "-log", logPath)
	if !strings.Contains(h.out.String(), "Logging to "+logPath) {
		t.Errorf("startup output:\n%s", h.out.String())
	}
	path, _ := writeTorrent(t, tmp, "logged", 1)
	h.mustCmd("add", path)
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, _ := os.ReadFile(logPath)
		if strings.Contains(string(data), "torrent added") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("log file lacks the add:\n%s", data)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(h.out.String(), "torrent added") {
		t.Error("with a log file, the log must not also go to the terminal")
	}
}

func TestRetryText(t *testing.T) {
	waiting := engine.Torrent{ID: 4}
	if got := retryText(waiting); !strings.Contains(got, "needs you") || !strings.Contains(got, "denco resume 4") {
		t.Errorf("no retry: %q", got)
	}
	soon := engine.Torrent{ID: 4, RetryAt: time.Now().Add(2*time.Minute + 500*time.Millisecond), Retries: 2}
	if got := retryText(soon); !strings.Contains(got, "retrying by itself in 2m") || !strings.Contains(got, "attempt 3") {
		t.Errorf("retry due: %q", got)
	}
}
