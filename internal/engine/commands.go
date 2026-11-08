package engine

import (
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/resume"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/torrent"
)

var (
	// ErrNotFound: no torrent matches the ID or hash prefix.
	ErrNotFound = errors.New("no such torrent")
	// ErrAmbiguous: a hash prefix matches more than one torrent.
	ErrAmbiguous = errors.New("ambiguous torrent reference")
	// ErrBusy: the torrent is being removed; try again in a moment.
	ErrBusy = errors.New("torrent is being removed")
	// ErrBadFile: a file index doesn't exist, or the selection would leave
	// nothing to download.
	ErrBadFile = errors.New("bad file selection")
)

// DuplicateError is returned by Add for a torrent that is already added
// (same info hash).
type DuplicateError struct {
	ID   int
	Name string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("already added as #%d (%s)", e.ID, e.Name)
}

// AddOptions are the per-torrent choices for Add.
type AddOptions struct {
	Dir      string // download directory; "" means Config.DownloadDir. Created if missing.
	Existing bool   // reuse data already at the target paths (hash-checked first)
	Paused   bool   // add without starting
	Skip     []int  // indexes of files not to download (torrent order, from 0); needs metadata
}

// Add validates a .torrent file's bytes and adds it. It refuses duplicates
// (DuplicateError) and, unless opts.Existing, torrents whose files already
// exist on disk (storage.ErrExists), so unrelated files are never touched.
func (e *Engine) Add(raw []byte, opts AddOptions) (Torrent, error) {
	mi, err := metainfo.Parse(raw)
	if err != nil {
		return Torrent{}, err
	}
	dir := opts.Dir
	if dir == "" {
		dir = e.cfg.DownloadDir
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Torrent{}, ErrClosed
	}
	if old := e.byHash[mi.InfoHash]; old != nil {
		if old.removing {
			return Torrent{}, ErrBusy
		}
		return Torrent{}, &DuplicateError{ID: old.id, Name: old.name()}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Torrent{}, fmt.Errorf("download directory: %w", err)
	}
	layout, err := storage.NewLayout(dir, &mi.Info)
	if err != nil {
		return Torrent{}, err
	}
	skip, err := normalizeSkip(&mi.Info, opts.Skip)
	if err != nil {
		return Torrent{}, err
	}
	// Every file is checked, skipped ones too: a piece on a border writes
	// into its neighbour, so a user's file there could be damaged (D44).
	if ex := layout.Existing(); len(ex) > 0 && !opts.Existing {
		return Torrent{}, fmt.Errorf("%w: %s (add with -existing to hash-check and reuse it)", storage.ErrExists, ex[0])
	}

	t := &entry{
		id: e.nextID, hash: mi.InfoHash, mi: mi, layout: layout, paused: opts.Paused,
		have: bitfield.New(mi.Info.NumPieces()),
		state: resume.State{
			ID:          e.nextID,
			InfoHash:    mi.InfoHash.String(),
			Name:        mi.Info.Name,
			DownloadDir: layout.Root,
			Pieces:      mi.Info.NumPieces(),
			Have:        bitfield.New(mi.Info.NumPieces()).Bytes(),
			AddedAt:     time.Now().UTC(),
			Paused:      opts.Paused,
			Fresh:       true,
			Existing:    opts.Existing,
			Skip:        skip,
		},
	}
	// Persist first: if this fails nothing has changed, and once it succeeds
	// the torrent survives a daemon restart even before it starts.
	if err := e.store.Save(&t.state, mi.Raw); err != nil {
		return Torrent{}, err
	}
	e.nextID++
	e.byID[t.id] = t
	e.byHash[mi.InfoHash] = t
	e.log.Info("torrent added", "id", t.id, "torrent", mi.Info.Name)
	e.scheduleLocked()
	return e.viewLocked(t), nil
}

// Pause stops a torrent (downloading or seeding) and remembers that it is
// paused across restarts. Pausing a paused torrent is a no-op.
func (e *Engine) Pause(ref string) (Torrent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return Torrent{}, err
	}
	if !t.paused {
		t.paused = true
		if t.running {
			t.cancel() // the run goroutine saves the paused flag when it stops
		} else {
			e.persistLocked(t)
		}
		e.log.Info("torrent paused", "id", t.id, "torrent", t.name())
	}
	return e.viewLocked(t), nil
}

// Resume un-pauses a torrent, retries one that stopped with an error, or
// restarts a finished one — unless its seeding limits would stop it again
// at once (ErrSeedLimit; SetSeedMode(SeedOn) seeds it regardless).
// It starts now, or waits in the queue if MaxActive torrents are downloading.
func (e *Engine) Resume(ref string) (Torrent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return Torrent{}, err
	}
	// A finished torrent may still be shutting down; either way it only
	// restarts if its limits allow (finish() then reschedules it).
	if t.finished {
		if reason := e.seedStopLocked(t); reason != "" {
			return Torrent{}, fmt.Errorf("%w: %s (seed it anyway: denco seed on %d)", ErrSeedLimit, reason, t.id)
		}
	}
	if t.paused || t.err != nil || t.finished {
		t.paused, t.err, t.finished = false, nil, false
		if !t.running {
			e.persistLocked(t)
		}
		e.log.Info("torrent resumed", "id", t.id, "torrent", t.name())
	}
	e.scheduleLocked()
	return e.viewLocked(t), nil
}

// Removed is the outcome of Remove.
type Removed struct {
	Torrent
	// FilesDeleted is true only if the torrent's files were deleted. A
	// torrent that never started owns nothing on disk (D32), so with
	// deleteFiles it stays false and nothing is touched.
	FilesDeleted bool `json:"files_deleted"`
}

// Remove stops a torrent and forgets it. With deleteFiles it also deletes
// the torrent's own files (never anything else; see storage.DeleteFiles).
// It waits for the torrent to stop, which can take a few seconds while
// trackers are told we're leaving.
func (e *Engine) Remove(ref string, deleteFiles bool) (Removed, error) {
	e.mu.Lock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		e.mu.Unlock()
		return Removed{}, err
	}
	view := e.viewLocked(t)
	t.removing = true
	running, done := t.running, t.done
	if running {
		t.cancel()
	}
	e.mu.Unlock()

	if running {
		<-done
	}

	e.mu.Lock()
	delete(e.byID, t.id)
	delete(e.byHash, t.hash)
	// A torrent that never started may not own anything at its paths.
	ownsFiles := t.layout != nil && (!t.state.Fresh || t.state.Existing)
	e.scheduleLocked() // a download slot may have opened
	e.mu.Unlock()

	res := Removed{Torrent: view}
	var errs []error
	if err := e.store.Delete(t.hash); err != nil {
		errs = append(errs, err)
	}
	if deleteFiles && ownsFiles {
		if err := deleteData(t.layout); err != nil {
			errs = append(errs, err)
		} else {
			res.FilesDeleted = true
		}
	}
	e.log.Info("torrent removed", "id", t.id, "torrent", t.name(), "files_deleted", res.FilesDeleted)
	return res, errors.Join(errs...)
}

// SetFilesWanted marks files (indexes in torrent order, from 0) as wanted
// or skipped. It takes effect at once: a running download stops fetching
// pieces only skipped files need (and finishes if nothing else is
// missing); wanting more of a torrent that had already finished restarts
// it, so it queues and downloads like any other. Returns the new file list.
func (e *Engine) SetFilesWanted(ref string, files []int, wanted bool) ([]File, error) {
	e.mu.Lock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	if t.mi == nil {
		e.mu.Unlock()
		return nil, fmt.Errorf("%s: %w", t.name(), ErrNoMetadata)
	}
	info := &t.mi.Info
	skip := pieces.SkipSet(t.state.Skip, len(info.Files))
	if skip == nil {
		skip = make([]bool, len(info.Files))
	}
	for _, i := range files {
		if i < 0 || i >= len(info.Files) {
			e.mu.Unlock()
			return nil, fmt.Errorf("%w: no file with index %d (the torrent has %d)", ErrBadFile, i, len(info.Files))
		}
		skip[i] = !wanted
	}
	list, err := normalizeSkip(info, skipIndexes(skip))
	if err != nil {
		e.mu.Unlock()
		return nil, err
	}
	var saver *resume.Saver
	var sess *torrent.Session
	if !slices.Equal(list, t.state.Skip) {
		t.state.Skip = list
		want := t.wantLocked()
		switch {
		case t.sess != nil:
			t.saver.SetSkip(list)
			if isClosed(t.sess.Completed()) && !t.sess.Have().Covers(want) {
				// It had finished and now wants more: restart it. finish()
				// reschedules it; the final save records the selection.
				t.cancel()
			} else {
				t.sess.SetWanted(want)
				saver, sess = t.saver, t.sess
			}
		default:
			// Not running, or still checking its data: run() reads the
			// selection when it creates the session.
			e.persistLocked(t)
			e.scheduleLocked() // it may have become complete, or incomplete
		}
		e.log.Info("file selection changed", "id", t.id, "torrent", t.name(), "skipped", len(list))
	}
	out := e.filesLocked(t)
	e.mu.Unlock()

	if saver != nil { // persist now rather than at the next periodic save
		st := sess.Stats()
		if err := saver.Save(resume.Snapshot{Have: sess.Have(), Uploaded: st.Uploaded, Downloaded: st.Downloaded}, false); err != nil {
			e.log.Warn("saving resume data", "torrent", t.name(), "err", err)
		}
	}
	return out, nil
}

// normalizeSkip validates skipped file indexes and returns them sorted and
// without duplicates (nil when nothing is skipped). Skipping every file
// with data is refused: there would be nothing to download.
func normalizeSkip(info *metainfo.Info, skip []int) ([]int, error) {
	if len(skip) == 0 {
		return nil, nil
	}
	flags := make([]bool, len(info.Files))
	for _, i := range skip {
		if i < 0 || i >= len(info.Files) {
			return nil, fmt.Errorf("%w: no file with index %d (the torrent has %d)", ErrBadFile, i, len(info.Files))
		}
		flags[i] = true
	}
	if want := pieces.Wanted(info, flags); want != nil && want.Count() == 0 {
		return nil, fmt.Errorf("%w: that would skip every file (pause or remove the torrent instead)", ErrBadFile)
	}
	return skipIndexes(flags), nil
}

func skipIndexes(flags []bool) []int {
	var out []int
	for i, s := range flags {
		if s {
			out = append(out, i)
		}
	}
	return out
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func deleteData(layout *storage.Layout) error {
	st, err := storage.Open(layout, storage.Options{AllowExisting: true})
	if err != nil {
		return err
	}
	return st.DeleteFiles()
}

// resolveLocked finds a torrent by ID ("3") or by an info hash prefix of
// at least 4 hex characters ("c12fe1").
func (e *Engine) resolveLocked(ref string) (*entry, error) {
	ref = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ref), "#"))
	if id, err := strconv.Atoi(ref); err == nil {
		if t := e.byID[id]; t != nil && !t.removing {
			return t, nil
		}
		return nil, fmt.Errorf("%w: #%d", ErrNotFound, id)
	}
	if len(ref) < 4 {
		return nil, fmt.Errorf("%w: %q (use an ID or at least 4 characters of the info hash)", ErrNotFound, ref)
	}
	var found *entry
	for h, t := range e.byHash {
		if t.removing || !strings.HasPrefix(h.String(), ref) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("%w: %q", ErrAmbiguous, ref)
		}
		found = t
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, ref)
	}
	return found, nil
}
