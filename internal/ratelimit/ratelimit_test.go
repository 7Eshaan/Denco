package ratelimit

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestUnlimited(t *testing.T) {
	var nilL *Limiter
	for _, l := range []*Limiter{nilL, New(0)} {
		start := time.Now()
		for i := 0; i < 1000; i++ {
			if err := l.WaitN(context.Background(), 1<<20); err != nil {
				t.Fatal(err)
			}
		}
		if time.Since(start) > 100*time.Millisecond {
			t.Fatal("unlimited limiter waited")
		}
		if l.Rate() != 0 {
			t.Fatal("rate should be 0")
		}
	}
	nilL.SetRate(5) // must not panic
}

// take moves total bytes through l in 16 KiB steps from several goroutines
// and returns how long it took.
func take(l *Limiter, total, workers int) time.Duration {
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < total/workers; n += 16 << 10 {
				l.WaitN(context.Background(), 16<<10)
			}
		}()
	}
	wg.Wait()
	return time.Since(start)
}

func TestRateIsEnforcedAcrossGoroutines(t *testing.T) {
	const rate = 1 << 20 // 1 MiB/s; burst = 256 KiB
	l := New(rate)
	// 768 KiB: the first 256 KiB is burst, the rest must take ~0.5s.
	got := take(l, 768<<10, 4)
	if got < 400*time.Millisecond || got > 1500*time.Millisecond {
		t.Fatalf("768 KiB at 1 MiB/s took %v, want about 0.5s", got)
	}
}

func TestSetRateWakesWaiters(t *testing.T) {
	l := New(10_000) // 10 KB/s: a 1 MB debt would take 100s
	done := make(chan time.Duration)
	go func() {
		start := time.Now()
		l.WaitN(context.Background(), 1_000_000)
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond)
	l.SetRate(0) // lift the limit
	select {
	case d := <-done:
		if d > time.Second {
			t.Fatalf("waiter released after %v", d)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("waiter not released when the limit was lifted")
	}
	if l.Rate() != 0 {
		t.Fatal("rate not updated")
	}

	// Raising a limit from tiny to large: also released quickly.
	l.SetRate(1000)
	go func() {
		start := time.Now()
		l.WaitN(context.Background(), 500_000)
		done <- time.Since(start)
	}()
	time.Sleep(50 * time.Millisecond)
	l.SetRate(100 << 20)
	if d := <-done; d > time.Second {
		t.Fatalf("after raising the limit the waiter took %v", d)
	}
}

func TestContextCancelsWait(t *testing.T) {
	l := New(1000)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := l.WaitN(ctx, 1_000_000)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
}
