// Package ratelimit caps throughput in bytes per second. One Limiter is
// shared by every connection of the daemon for downloads, and one for
// uploads, so the limits are totals (DECISIONS D36).
//
// It is a token bucket: tokens (bytes) refill at the rate, up to a burst of
// a quarter second's worth (at least 32 KiB, so a 16 KiB block always fits).
// WaitN takes n tokens, going into debt if needed, and waits until its
// share of the debt is repaid (first come, first served). Callers take tokens *after* moving data, which is simple
// and keeps the long-run rate exact.
//
// A nil *Limiter, or a rate of 0, means unlimited. SetRate may be called at
// any time; waiting goroutines re-plan immediately.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

const minBurst = 32 << 10

// Limiter is safe for concurrent use.
type Limiter struct {
	mu      sync.Mutex
	rate    float64 // bytes/s; 0 = unlimited
	burst   float64
	tokens  float64 // may be negative (debt)
	last    time.Time
	changed chan struct{} // closed and replaced by SetRate
}

// New returns a limiter for bytesPerSec (0 = unlimited).
func New(bytesPerSec int64) *Limiter {
	l := &Limiter{changed: make(chan struct{})}
	l.setLocked(bytesPerSec, time.Now())
	l.tokens = l.burst
	return l
}

// Rate returns the current limit (0 = unlimited).
func (l *Limiter) Rate() int64 {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.rate)
}

// SetRate changes the limit. Debt from the old rate is forgiven, so
// raising or removing a limit takes effect at once.
func (l *Limiter) SetRate(bytesPerSec int64) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.setLocked(bytesPerSec, time.Now())
	l.tokens = min(max(l.tokens, 0), l.burst)
	close(l.changed)
	l.changed = make(chan struct{})
}

func (l *Limiter) setLocked(bytesPerSec int64, now time.Time) {
	l.rate = float64(max(bytesPerSec, 0))
	l.burst = max(l.rate/4, minBurst)
	l.last = now
}

// WaitN accounts for n bytes and blocks until the rate allows them. It
// returns early with ctx's error if ctx ends first.
//
// It works like a reservation: the bytes are taken at once, and the wait
// is fixed now from the debt already queued ahead of this call. So callers
// are served in the order they asked — no goroutine can starve the others
// by asking again the moment it is let through (DECISIONS D46). A rate
// change forgives the debt and lets waiters go.
func (l *Limiter) WaitN(ctx context.Context, n int) error {
	if l == nil || n <= 0 {
		return nil
	}
	l.mu.Lock()
	if l.rate == 0 {
		l.mu.Unlock()
		return nil
	}
	l.refillLocked(time.Now())
	l.tokens -= float64(n)
	if l.tokens >= 0 {
		l.mu.Unlock()
		return nil
	}
	wait := time.Duration(-l.tokens / l.rate * float64(time.Second))
	changed := l.changed
	l.mu.Unlock()

	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed: // SetRate forgave the debt
		return nil
	case <-t.C:
		return nil
	}
}

func (l *Limiter) refillLocked(now time.Time) {
	if d := now.Sub(l.last); d > 0 {
		l.tokens = min(l.tokens+d.Seconds()*l.rate, l.burst)
		l.last = now
	}
}
