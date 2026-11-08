package resume

import (
	"context"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/storage"
)

// Snapshot is what a Saver needs from a running torrent.
type Snapshot struct {
	Have       *bitfield.Bitfield // verified pieces, all already written to storage
	Uploaded   int64              // payload bytes this session
	Downloaded int64
}

// Saver keeps one torrent's resume state current. Safe for concurrent use.
type Saver struct {
	store  *Store
	mi     *metainfo.MetaInfo
	layout *storage.Layout
	st     *storage.Storage

	mu       sync.Mutex
	state    State              // template: identity, AddedAt, CompletedAt, options
	want     *bitfield.Bitfield // pieces of the wanted files (from state.Skip); nil = all
	baseUp   int64              // lifetime totals from earlier sessions
	baseDown int64
	last     savedKey
	hasLast  bool
	saved    State // the last state written successfully
}

// savedKey lets Save skip writes when nothing has changed.
type savedKey struct {
	pieces   int
	up, down int64
	clean    bool
}

// NewSaver prepares a saver. prev is the state loaded at startup, or nil
// for a torrent being added for the first time.
func NewSaver(store *Store, mi *metainfo.MetaInfo, layout *storage.Layout, st *storage.Storage, prev *State) *Saver {
	sv := &Saver{store: store, mi: mi, layout: layout, st: st}
	if prev != nil {
		sv.state = *prev
		sv.baseUp, sv.baseDown = prev.Uploaded, prev.Downloaded
	} else {
		sv.state = State{AddedAt: time.Now().UTC()}
	}
	sv.state.InfoHash = mi.InfoHash.String()
	sv.state.Name = mi.Info.Name
	sv.state.DownloadDir = layout.Root
	sv.state.Pieces = mi.Info.NumPieces()
	sv.want = pieces.Wanted(&mi.Info, pieces.SkipSet(sv.state.Skip, len(mi.Info.Files)))
	return sv
}

// Save writes the state now. clean must be true only for the final save
// after the session has stopped (no further writes can happen).
//
// Order matters: the caller took snap.Have before calling Save; Save then
// fsyncs storage, so every piece recorded is durably on disk.
func (sv *Saver) Save(snap Snapshot, clean bool) error {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	key := savedKey{snap.Have.Count(), snap.Uploaded, snap.Downloaded, clean}
	if sv.hasLast && key == sv.last {
		return nil // nothing new since the last save
	}
	if err := sv.st.Sync(); err != nil {
		return err
	}
	s := sv.state
	s.Have = snap.Have.Bytes()
	s.Uploaded = sv.baseUp + snap.Uploaded
	s.Downloaded = sv.baseDown + snap.Downloaded
	s.Clean = clean
	// CompletedAt is when the current selection finished; wanting more
	// files (or losing data) makes it incomplete again, so it is cleared.
	switch done := snap.Have.Covers(sv.want); {
	case done && s.CompletedAt.IsZero():
		s.CompletedAt = time.Now().UTC()
	case !done:
		s.CompletedAt = time.Time{}
	}
	sv.state.CompletedAt = s.CompletedAt
	s.Files = Stamp(sv.layout)
	if err := sv.store.Save(&s, sv.mi.Raw); err != nil {
		return err
	}
	sv.last, sv.hasLast = key, true
	sv.saved = s
	return nil
}

// SetPaused changes the paused flag recorded by the following saves.
func (sv *Saver) SetPaused(paused bool) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	if sv.state.Paused != paused {
		sv.state.Paused = paused
		sv.hasLast = false // force the next Save to write
	}
}

// SetSkip changes the skipped files recorded by the following saves.
func (sv *Saver) SetSkip(skip []int) {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	sv.state.Skip = append([]int(nil), skip...)
	sv.want = pieces.Wanted(&sv.mi.Info, pieces.SkipSet(skip, len(sv.mi.Info.Files)))
	sv.hasLast = false // force the next Save to write
}

// State returns the last state written successfully (zero before the first).
func (sv *Saver) State() State {
	sv.mu.Lock()
	defer sv.mu.Unlock()
	return sv.saved
}

// Run saves every interval (as unclean saves) until ctx is done.
// errs, if non-nil, receives save errors (e.g. disk full) without blocking.
func (sv *Saver) Run(ctx context.Context, interval time.Duration, snapshot func() Snapshot, errs func(error)) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := sv.Save(snapshot(), false); err != nil && errs != nil {
				errs(err)
			}
		}
	}
}
