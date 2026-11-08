package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/resume"
)

// Seeding controls (DECISIONS D45). A complete torrent seeds until it
// reaches the global limits (seed_ratio, seed_time), unless its own mode
// says otherwise: SeedOn seeds with no limits, SeedOff doesn't seed. When
// it stops seeding it is "finished": not running, and not started again on
// its own. Uploads can also be paused per torrent; the torrent keeps
// downloading and stays connected, and its seeding time doesn't grow.

// Seeding modes (Torrent.SeedMode, SetSeedMode).
const (
	SeedDefault = "default" // follow the global limits
	SeedOn      = "on"      // seed until the user stops it
	SeedOff     = "off"     // stop as soon as the download is complete
)

var (
	// ErrSeedLimit: a finished torrent can't resume because it would stop
	// again at once (limit reached, or seeding off).
	ErrSeedLimit = errors.New("not seeding")
	// ErrBadSeedMode: unknown seeding mode.
	ErrBadSeedMode = errors.New("seeding mode must be on, off or default")
)

// flagsTo records the entry's per-torrent choices in a state to be saved.
func (t *entry) flagsTo(st *resume.State) {
	st.Paused, st.UploadsPaused, st.Seed, st.Finished = t.paused, t.uploadsPaused, t.seedMode, t.finished
}

// flagsDiffer reports whether the saved state lags the entry's choices.
func (t *entry) flagsDiffer() bool {
	st := t.state
	return st.Paused != t.paused || st.UploadsPaused != t.uploadsPaused || st.Seed != t.seedMode || st.Finished != t.finished
}

// seedingTimeLocked is the lifetime seeding time, this run included.
func (t *entry) seedingTimeLocked() time.Duration {
	return time.Duration(t.state.SeedSeconds)*time.Second + time.Duration(t.seeded.Load())
}

// ratio is uploaded ÷ downloaded. A torrent whose data was already on disk
// (added with -existing) downloaded little or nothing, so the bytes it has
// count as downloaded when they're more: otherwise its ratio would be
// infinite. -1 means there is nothing to divide by yet.
func ratio(up, down, done int64) float64 {
	d := max(down, done)
	if d <= 0 {
		return -1
	}
	return float64(up) / float64(d)
}

// seedStopReason says why a complete torrent should not (or no longer)
// seed, or "" if it may. Pure, for testability.
func seedStopReason(mode string, r float64, seeded time.Duration, limRatio float64, limTime time.Duration) string {
	switch mode {
	case SeedOn:
		return ""
	case SeedOff:
		return "seeding is off for this torrent"
	}
	if limRatio > 0 && r >= limRatio {
		return fmt.Sprintf("ratio %.2f reached the limit %s", r, strconv.FormatFloat(limRatio, 'f', -1, 64))
	}
	if limTime > 0 && seeded >= limTime {
		return fmt.Sprintf("seeded for %s (limit %s)", format.Duration(seeded), format.Duration(limTime))
	}
	return ""
}

// seedStopLocked applies seedStopReason to a torrent's current numbers.
func (e *Engine) seedStopLocked(t *entry) string {
	v := e.viewLocked(t)
	return seedStopReason(t.seedMode, v.Ratio, v.SeedingTime, e.seedRatio, e.seedTime)
}

// seedPolicyLocked describes in words how long t seeds.
func (e *Engine) seedPolicyLocked(t *entry) string {
	switch t.seedMode {
	case SeedOn:
		return "on, no limits (this torrent)"
	case SeedOff:
		return "off (this torrent)"
	}
	var lims []string
	if e.seedRatio > 0 {
		lims = append(lims, "ratio "+strconv.FormatFloat(e.seedRatio, 'f', -1, 64))
	}
	if e.seedTime > 0 {
		lims = append(lims, format.Duration(e.seedTime))
	}
	switch len(lims) {
	case 0:
		return "until stopped (no limits set)"
	case 1:
		return "until " + lims[0]
	default:
		return "until " + lims[0] + " or " + lims[1]
	}
}

// watchSeeding runs in a torrent's run goroutine once it is complete: it
// adds up seeding time and stops the torrent ("finished") when its limits
// are reached. Returns when ctx ends or it stopped the torrent.
func (e *Engine) watchSeeding(ctx context.Context, t *entry) {
	tick := time.NewTicker(e.cfg.SeedCheckEvery)
	defer tick.Stop()
	last := time.Now()
	for {
		now := time.Now()
		e.mu.Lock()
		if !t.uploadsPaused { // paused uploads aren't seeding
			t.seeded.Add(int64(now.Sub(last)))
		}
		last = now
		reason := ""
		if !t.paused && !t.removing && t.cancel != nil {
			reason = e.seedStopLocked(t)
		}
		if reason != "" {
			t.finished = true
			t.cancel() // run() records "finished" in its final save
			e.log.Info("stopped seeding", "id", t.id, "torrent", t.name(), "reason", reason)
		}
		e.mu.Unlock()
		if reason != "" {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// SetUploadsPaused pauses or resumes uploading for one torrent. Downloading
// goes on; peers simply stay choked.
func (e *Engine) SetUploadsPaused(ref string, paused bool) (Torrent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return Torrent{}, err
	}
	if t.uploadsPaused != paused {
		t.uploadsPaused = paused
		e.flagsChangedLocked(t)
		if t.sess != nil {
			t.sess.SetUploadsPaused(paused)
		}
		e.log.Info("uploads paused changed", "id", t.id, "torrent", t.name(), "paused", paused)
	}
	return e.viewLocked(t), nil
}

// SetSeedMode sets a torrent's seeding mode: SeedOn (seed with no limits),
// SeedOff (don't seed: a complete torrent stops now, an incomplete one
// when it completes) or SeedDefault (the global limits). A finished
// torrent that may seed under its new mode starts again.
func (e *Engine) SetSeedMode(ref, mode string) (Torrent, error) {
	switch mode {
	case SeedOn, SeedOff:
	case SeedDefault, "":
		mode = ""
	default:
		return Torrent{}, fmt.Errorf("%w (got %q)", ErrBadSeedMode, mode)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return Torrent{}, err
	}
	if t.seedMode != mode {
		t.seedMode = mode
		reason := ""
		if t.completeLocked() {
			reason = e.seedStopLocked(t)
		}
		switch {
		case t.finished && reason == "":
			t.finished = false // seeds again (scheduleLocked starts it)
		case !t.finished && reason != "" && t.sess != nil && !t.paused:
			t.finished = true
			t.cancel()
		}
		e.flagsChangedLocked(t)
		e.log.Info("seeding mode changed", "id", t.id, "torrent", t.name(), "mode", modeName(mode))
		e.scheduleLocked()
	}
	return e.viewLocked(t), nil
}

// SetSeedLimits changes the global seeding limits (0 = no limit). Running
// torrents past a new limit stop within SeedCheckEvery; finished ones stay
// finished until the user resumes them.
func (e *Engine) SetSeedLimits(ratio float64, seedTime time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seedRatio, e.seedTime = ratio, seedTime
	e.log.Info("seeding limits changed", "ratio", ratio, "time", seedTime)
}

// flagsChangedLocked records changed choices: through the running
// torrent's saver (its next save writes them), or saved now.
func (e *Engine) flagsChangedLocked(t *entry) {
	if t.saver != nil {
		t.saver.Update(t.flagsTo)
		return
	}
	e.persistLocked(t)
}

func modeName(mode string) string {
	if mode == "" {
		return SeedDefault
	}
	return mode
}
