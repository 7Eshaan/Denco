package storage

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// ErrExists is returned by Open when target files already exist and the
// caller didn't allow reusing them.
var ErrExists = errors.New("storage: files already exist")

// ErrReadOnly is returned by WriteAt on read-only storage.
var ErrReadOnly = errors.New("storage: read-only")

// ErrClosed is returned after Close.
var ErrClosed = errors.New("storage: closed")

// Options controls Open.
type Options struct {
	// AllowExisting permits files that already exist on disk. Set it when
	// resuming our own download, or when the user explicitly asks to reuse
	// existing data (it will be hash-checked, and bad pieces overwritten).
	AllowExisting bool
	// MaxOpenFiles caps cached file handles (default 32). Torrents can have
	// thousands of files; OS handle limits are much lower.
	MaxOpenFiles int
	// ReadOnly opens files read-only, creates nothing, and makes WriteAt
	// fail. Used to verify existing data without touching it.
	ReadOnly bool
	// Skipped marks files the user doesn't want (by index). Open doesn't
	// create them when empty; other files are only ever created by writes,
	// and pieces of skipped files aren't downloaded (except where a piece
	// also covers a wanted file, D44).
	Skipped []bool
}

// Storage reads and writes torrent byte ranges across the layout's files.
//
// It is safe for concurrent use. Operations are serialised by one mutex:
// simple and correct, and one torrent's disk I/O is rarely the bottleneck.
// See DECISIONS D21.
type Storage struct {
	layout   *Layout
	maxOpen  int
	readOnly bool

	mu      sync.Mutex
	handles map[int]*os.File // file index → open handle
	lru     []int            // file indexes, least recently used first
	closed  bool
}

// Open prepares storage for layout. Files and directories are created
// lazily on first write, except zero-length files, which are created now
// (no piece ever writes to them).
func Open(layout *Layout, opts Options) (*Storage, error) {
	if !opts.AllowExisting && !opts.ReadOnly {
		if ex := layout.Existing(); len(ex) > 0 {
			return nil, fmt.Errorf("%w: %s", ErrExists, summarize(ex))
		}
	}
	s := &Storage{
		layout:   layout,
		maxOpen:  opts.MaxOpenFiles,
		readOnly: opts.ReadOnly,
		handles:  make(map[int]*os.File),
	}
	if s.maxOpen <= 0 {
		s.maxOpen = 32
	}
	for i, f := range layout.Files {
		if f.Length == 0 && !opts.ReadOnly && !(i < len(opts.Skipped) && opts.Skipped[i]) {
			if err := createEmpty(f.Path); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

// Layout returns the layout this storage was opened with.
func (s *Storage) Layout() *Layout { return s.layout }

// ReadAt reads len(p) bytes at torrent offset off. Reading a region whose
// file doesn't exist yet returns an error wrapping os.ErrNotExist.
func (s *Storage) ReadAt(p []byte, off int64) (int, error) {
	return s.span(p, off, false)
}

// WriteAt writes p at torrent offset off, creating files as needed.
func (s *Storage) WriteAt(p []byte, off int64) (int, error) {
	if s.readOnly {
		return 0, ErrReadOnly
	}
	return s.span(p, off, true)
}

// span splits [off, off+len(p)) across files and performs the I/O.
func (s *Storage) span(p []byte, off int64, write bool) (int, error) {
	if off < 0 || off+int64(len(p)) > s.layout.TotalLength() {
		return 0, fmt.Errorf("storage: range [%d,+%d) outside torrent (%d bytes)", off, len(p), s.layout.TotalLength())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrClosed
	}

	files := s.layout.Files
	// First file whose end is past off.
	i := sort.Search(len(files), func(i int) bool { return files[i].Offset+files[i].Length > off })
	done := 0
	for done < len(p) && i < len(files) {
		f := files[i]
		if f.Length == 0 {
			i++
			continue
		}
		inFile := off + int64(done) - f.Offset
		n := min(int64(len(p)-done), f.Length-inFile)
		h, err := s.handle(i, write)
		if err != nil {
			return done, err
		}
		chunk := p[done : done+int(n)]
		if write {
			_, err = h.WriteAt(chunk, inFile)
		} else {
			_, err = h.ReadAt(chunk, inFile)
			if errors.Is(err, io.EOF) {
				// File shorter than expected (never fully written / truncated).
				err = fmt.Errorf("storage: %s is shorter than expected: %w", f.Rel, io.ErrUnexpectedEOF)
			}
		}
		if err != nil {
			return done, err
		}
		done += int(n)
		i++
	}
	return done, nil
}

// handle returns an open handle for file i, opening (and for writes,
// creating) it if needed. Caller holds s.mu.
func (s *Storage) handle(i int, create bool) (*os.File, error) {
	if h, ok := s.handles[i]; ok {
		s.touch(i)
		return h, nil
	}
	f := s.layout.Files[i]
	flags := os.O_RDWR
	if s.readOnly {
		flags = os.O_RDONLY
	}
	if create {
		if err := os.MkdirAll(filepath.Dir(f.Path), 0o755); err != nil {
			return nil, fmt.Errorf("storage: %w", err)
		}
		flags |= os.O_CREATE
	}
	h, err := os.OpenFile(f.Path, flags, 0o644)
	if err != nil {
		return nil, fmt.Errorf("storage: %w", err)
	}
	if len(s.handles) >= s.maxOpen {
		s.evictOldest()
	}
	s.handles[i] = h
	s.lru = append(s.lru, i)
	return h, nil
}

func (s *Storage) touch(i int) {
	for k, v := range s.lru {
		if v == i {
			copy(s.lru[k:], s.lru[k+1:])
			s.lru[len(s.lru)-1] = i
			return
		}
	}
}

func (s *Storage) evictOldest() {
	old := s.lru[0]
	s.lru = s.lru[1:]
	s.handles[old].Close()
	delete(s.handles, old)
}

// Sync flushes all open files to disk (called before saving resume data, so
// the "have" bitfield never claims pieces that only exist in OS buffers).
func (s *Storage) Sync() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, h := range s.handles {
		if err := h.Sync(); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Close closes all handles. Further I/O returns ErrClosed.
func (s *Storage) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.closeHandles()
}

func (s *Storage) closeHandles() error {
	var errs []error
	for i, h := range s.handles {
		if err := h.Close(); err != nil {
			errs = append(errs, err)
		}
		delete(s.handles, i)
	}
	s.lru = nil
	return errors.Join(errs...)
}

// DeleteFiles removes the torrent's files and then any directories that
// became empty, up to (not including) the download root. It never removes
// a directory that still contains anything — e.g. a user's own files —
// and never touches files outside the layout. Missing files are ignored.
func (s *Storage) DeleteFiles() error {
	if s.readOnly {
		return ErrReadOnly
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeHandles() // Windows can't delete open files
	s.closed = true

	var errs []error
	dirs := map[string]bool{}
	for _, f := range s.layout.Files {
		if err := os.Remove(f.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, err)
		}
		for d := filepath.Dir(f.Path); d != s.layout.Root && within(s.layout.Root, d); d = filepath.Dir(d) {
			dirs[d] = true
		}
	}
	// Deepest first, so children go before parents.
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, d := range ordered {
		os.Remove(d) // fails harmlessly if not empty or missing
	}
	return errors.Join(errs...)
}

func createEmpty(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("storage: %w", err)
	}
	return f.Close()
}

// summarize lists up to 3 paths for an error message.
func summarize(paths []string) string {
	if len(paths) <= 3 {
		return strings.Join(paths, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(paths[:3], ", "), len(paths)-3)
}
