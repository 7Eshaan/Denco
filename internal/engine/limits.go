package engine

import (
	"errors"
	"fmt"
)

// Per-torrent speed limits (DECISIONS D59). Each torrent has its own
// download and upload limiter, applied on top of the daemon's total limits
// (Config.DownloadLimit/UploadLimit, SetLimits): data must clear both, so a
// torrent's limit can only make it slower than the total, never faster.
// The limiters live on the entry, not the run, so a change reaches a running
// session at once and survives pause and resume; the rates are saved
// (resume.State.DownLimit/UpLimit).

// ErrBadLimit: a negative speed limit.
var ErrBadLimit = errors.New("speed limits can't be negative")

// SetTorrentLimits sets one torrent's own download and upload limits in
// bytes/s (0 = no limit of its own).
func (e *Engine) SetTorrentLimits(ref string, down, up int64) (Torrent, error) {
	if down < 0 || up < 0 {
		return Torrent{}, fmt.Errorf("%w (got %d, %d)", ErrBadLimit, down, up)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return Torrent{}, err
	}
	if t.downLim.Rate() != down || t.upLim.Rate() != up {
		t.downLim.SetRate(down)
		t.upLim.SetRate(up)
		e.flagsChangedLocked(t)
		e.log.Info("torrent speed limits changed", "id", t.id, "torrent", t.name(), "download", down, "upload", up)
	}
	return e.viewLocked(t), nil
}
