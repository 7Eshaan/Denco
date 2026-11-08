// Package resume saves and restores per-torrent progress so downloads
// survive restarts and crashes without re-downloading or re-hashing
// everything.
//
// On disk (in the state directory) each torrent has two files:
//
//	<infohash>.torrent  the original .torrent, byte for byte
//	<infohash>.json     State: download dir, verified pieces, totals, file stamps
//
// Every write goes to a temp file that is fsynced and then renamed over the
// old one, so a crash mid-save leaves the previous version intact.
// See DECISIONS D27.
package resume

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
)

// FormatVersion is bumped when State changes incompatibly.
const FormatVersion = 1

// ErrNotFound means there is no resume data for that torrent.
var ErrNotFound = errors.New("resume: no saved state")

// State is everything saved about one torrent.
type State struct {
	Version  int    `json:"version"`
	InfoHash string `json:"info_hash"` // 40 hex chars
	Name     string `json:"name"`

	DownloadDir string `json:"download_dir"` // absolute
	Pieces      int    `json:"pieces"`       // number of pieces (sanity check)
	Have        []byte `json:"have"`         // verified-pieces bitfield, wire format (base64 in JSON)

	// Lifetime payload totals across all sessions.
	Uploaded   int64 `json:"uploaded"`
	Downloaded int64 `json:"downloaded"`

	AddedAt     time.Time `json:"added_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
	SavedAt     time.Time `json:"saved_at"`

	// Clean is true only for the save made after a normal shutdown, when no
	// more writes can happen. See Restore for how it's used.
	Clean bool `json:"clean"`

	// Files records each file's size and modification time at save time,
	// in torrent order, to detect changes made while we weren't running.
	Files []FileStamp `json:"files"`

	// Daemon bookkeeping (Phase 8). All optional, so states written by
	// `denco download` load unchanged.
	//
	// ID is the short number users type (`denco pause 3`); 0 = not yet assigned.
	ID int `json:"id,omitempty"`
	// Paused: the user paused it; don't start it on daemon startup.
	Paused bool `json:"paused,omitempty"`
	// Fresh: added but never started. Any file already at its paths is not
	// ours, so it must not be opened for writing unless Existing is set.
	Fresh bool `json:"fresh,omitempty"`
	// Existing: the user asked to reuse data already at the paths; it is
	// hash-checked on the first start.
	Existing bool `json:"existing,omitempty"`
	// Magnet is set while a torrent added by magnet link has no metadata
	// yet. Such a state has no .torrent file (and Pieces/Have are empty);
	// once the metadata arrives the .torrent is written and Magnet cleared.
	Magnet string `json:"magnet,omitempty"`
}

// FileStamp is a file's size and mtime when the state was saved.
// Size -1 means the file did not exist.
type FileStamp struct {
	Size    int64 `json:"size"`
	ModTime int64 `json:"mtime_ns"`
}

// Store reads and writes resume files in one directory.
type Store struct {
	dir string
}

// OpenStore uses dir (created with owner-only permissions if missing).
func OpenStore(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("resume: %w", err)
	}
	return &Store{dir: dir}, nil
}

// DefaultDir is <user config dir>/denco/resume, e.g.
// ~/Library/Application Support/denco/resume on macOS,
// %AppData%\denco\resume on Windows, ~/.config/denco/resume on Linux.
func DefaultDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "denco", "resume"), nil
}

// Dir returns the store's directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(hash, ext string) string { return filepath.Join(s.dir, hash+ext) }

// Save writes st (and, if not already stored, the .torrent bytes).
func (s *Store) Save(st *State, torrent []byte) error {
	if len(st.InfoHash) != 40 {
		return fmt.Errorf("resume: bad info hash %q", st.InfoHash)
	}
	st.Version = FormatVersion
	st.SavedAt = time.Now().UTC()
	if _, err := os.Stat(s.path(st.InfoHash, ".torrent")); errors.Is(err, os.ErrNotExist) {
		switch {
		case len(torrent) > 0:
			if err := writeAtomic(s.path(st.InfoHash, ".torrent"), torrent); err != nil {
				return err
			}
		case st.Magnet == "":
			return errors.New("resume: first save needs the .torrent bytes")
		}
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return writeAtomic(s.path(st.InfoHash, ".json"), data)
}

// Load returns the saved state and parsed torrent for an info hash. For a
// magnet link still waiting for metadata the MetaInfo is nil.
func (s *Store) Load(hash metainfo.Hash) (*State, *metainfo.MetaInfo, error) {
	return s.load(hash.String())
}

func (s *Store) load(hash string) (*State, *metainfo.MetaInfo, error) {
	data, err := os.ReadFile(s.path(hash, ".json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, fmt.Errorf("resume: %w", err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return nil, nil, fmt.Errorf("resume: %s is corrupt: %w", s.path(hash, ".json"), err)
	}
	if st.Version != FormatVersion {
		return nil, nil, fmt.Errorf("resume: %s has format version %d, want %d", s.path(hash, ".json"), st.Version, FormatVersion)
	}
	if st.InfoHash != hash {
		return nil, nil, fmt.Errorf("resume: %s: info hash mismatch", hash)
	}
	mi, err := metainfo.Load(s.path(hash, ".torrent"))
	if errors.Is(err, os.ErrNotExist) && st.Magnet != "" {
		return &st, nil, nil // magnet without metadata yet
	}
	if err != nil {
		return nil, nil, fmt.Errorf("resume: %w", err)
	}
	if mi.InfoHash.String() != st.InfoHash || st.InfoHash != hash {
		return nil, nil, fmt.Errorf("resume: %s: info hash mismatch", hash)
	}
	return &st, mi, nil
}

// Entry is one stored torrent, as returned by List.
type Entry struct {
	State *State
	Meta  *metainfo.MetaInfo // nil for a magnet link without metadata yet
	Err   error              // set if this entry couldn't be loaded; the others still are
}

// List loads every stored torrent, oldest first. A broken entry is
// reported in Entry.Err instead of hiding the rest.
func (s *Store) List() ([]Entry, error) {
	names, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("resume: %w", err)
	}
	var out []Entry
	for _, d := range names {
		name := d.Name()
		if !strings.HasSuffix(name, ".json") || len(name) != 40+len(".json") {
			continue
		}
		st, mi, err := s.load(strings.TrimSuffix(name, ".json"))
		out = append(out, Entry{State: st, Meta: mi, Err: err})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].State == nil || out[j].State == nil {
			return out[i].State != nil
		}
		return out[i].State.AddedAt.Before(out[j].State.AddedAt)
	})
	return out, nil
}

// Delete removes a torrent's resume files. Missing files are not an error.
func (s *Store) Delete(hash metainfo.Hash) error {
	var errs []error
	for _, ext := range []string{".json", ".torrent"} {
		if err := os.Remove(s.path(hash.String(), ext)); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// writeAtomic replaces path with data so that readers see either the old or
// the new content, never a mix — even if we crash halfway.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	if err := tmp.Sync(); err != nil { // data on disk before the rename
		return fmt.Errorf("resume: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil { // atomic on POSIX and NTFS
		return fmt.Errorf("resume: %w", err)
	}
	ok = true
	syncDir(dir)
	return nil
}

// syncDir makes the rename itself durable on POSIX systems. On Windows
// directories can't be opened this way; the error is ignored.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}
