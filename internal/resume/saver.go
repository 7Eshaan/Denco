package resume

import (
	"context"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
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
	state    State // template: identity, AddedAt, CompletedAt, options
	baseUp   int64 // lifetime totals from earlier sessions
	baseDown int64
	last     savedKey
	hasLast  bool
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
	if snap.Have.Full() && s.CompletedAt.IsZero() {
		s.CompletedAt = time.Now().UTC()
		sv.state.CompletedAt = s.CompletedAt
	}
	s.Files = Stamp(sv.layout)
	if err := sv.store.Save(&s, sv.mi.Raw); err != nil {
		return err
	}
	sv.last, sv.hasLast = key, true
	return nil
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
