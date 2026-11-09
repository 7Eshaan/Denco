package engine

import (
	"errors"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/storage"
)

// Automatic retry of failed torrents (DECISIONS D47). A torrent that stops
// with an error (disk full, a folder it can't create, a drive that went
// away…) is retried on its own after RetryBase, doubling each time up to
// RetryMax, until it works or the user steps in. Errors no retry can fix
// — its files belong to someone else, an unsafe or invalid torrent — wait
// for the user, as before. A run that lasted retryResetAfter before failing
// counts as having worked: the delay starts small again.

const retryResetAfter = 10 * time.Minute

// retryable reports whether trying again later might succeed.
func retryable(err error) bool {
	return !errors.Is(err, storage.ErrExists) &&
		!errors.Is(err, storage.ErrUnsafePath) &&
		!errors.Is(err, metainfo.ErrInvalid)
}

// retryDelay is the wait before attempt n+1 (n ≥ 1 failures so far).
func retryDelay(base, maxDelay time.Duration, n int) time.Duration {
	d := base
	for i := 1; i < n && d < maxDelay; i++ {
		d *= 2
	}
	return min(d, maxDelay)
}

// failedLocked records why t's run stopped and, if it may help, schedules
// a retry.
func (e *Engine) failedLocked(t *entry, err error, ranFor time.Duration) {
	t.err = err
	if ranFor >= retryResetAfter {
		t.retries = 0
	}
	if !retryable(err) {
		e.log.Error("torrent stopped", "torrent", t.name(), "err", err)
		return
	}
	t.retries++
	delay := retryDelay(e.cfg.RetryBase, e.cfg.RetryMax, t.retries)
	t.retryAt = time.Now().Add(delay)
	var timer *time.Timer
	timer = time.AfterFunc(delay, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.closed || t.removing || t.retryTimer != timer || t.err == nil {
			return // cancelled, or the user already acted
		}
		e.log.Info("retrying torrent", "id", t.id, "torrent", t.name(), "attempt", t.retries+1)
		t.err, t.retryTimer, t.retryAt = nil, nil, time.Time{}
		e.scheduleLocked()
	})
	t.retryTimer = timer
	e.log.Error("torrent stopped; will retry", "torrent", t.name(), "err", err, "in", delay.Round(time.Second))
}

// stopRetryLocked cancels a pending retry (the user resumed, paused or
// removed the torrent). userRetry also forgets earlier failures.
func (t *entry) stopRetryLocked(userRetry bool) {
	if t.retryTimer != nil {
		t.retryTimer.Stop()
	}
	t.retryTimer, t.retryAt = nil, time.Time{}
	if userRetry {
		t.retries = 0
	}
}
