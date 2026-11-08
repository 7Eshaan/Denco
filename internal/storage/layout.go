// Package storage maps a torrent's byte stream onto files on disk.
//
// A torrent is one long run of bytes (all files concatenated in order) split
// into pieces. Storage translates "bytes X..Y of the torrent" into reads and
// writes on the right file(s), and decides where each file lives on disk.
//
// Safety rules enforced here (see DECISIONS D8, D20):
//   - every path is sanitised for the current OS and must stay inside the
//     download directory;
//   - existing files are never overwritten unless the caller opts in;
//   - deleting data removes only the torrent's own files, never whole folders.
package storage

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/7Eshaan/Denco/internal/metainfo"
)

// maxComponentBytes is the file-name length limit on common filesystems
// (ext4, APFS, NTFS all allow 255).
const maxComponentBytes = 255

// ErrUnsafePath means a file would land outside the download directory.
var ErrUnsafePath = errors.New("storage: unsafe path")

// Layout is where each file of a torrent lives on disk.
type Layout struct {
	Root  string // absolute download directory
	Files []File
}

// File is one torrent file placed on disk.
type File struct {
	Path    string // absolute OS path
	Rel     string // path relative to Root, slash-separated (for display)
	Length  int64
	Offset  int64 // offset in the torrent byte stream
	Renamed bool  // true if sanitising or collision handling changed the name
}

// TotalLength is the size of all files together.
func (l *Layout) TotalLength() int64 {
	if len(l.Files) == 0 {
		return 0
	}
	last := l.Files[len(l.Files)-1]
	return last.Offset + last.Length
}

// NewLayout decides on-disk paths for info's files under root. It does not
// touch the filesystem.
func NewLayout(root string, info *metainfo.Info) (*Layout, error) {
	return newLayout(root, info, runtime.GOOS == "windows")
}

// newLayout takes the Windows flag explicitly so tests can check both rules
// on any OS.
func newLayout(root string, info *metainfo.Info, windows bool) (*Layout, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("storage: download dir: %w", err)
	}
	l := &Layout{Root: absRoot}
	names := newNameSet()

	for _, f := range info.Files {
		// Every torrent's files sit under a folder (multi-file) or file
		// (single-file) named after the torrent.
		orig := append([]string{info.Name}, f.Path...)
		parts := make([]string, len(orig))
		renamed := false
		for i, c := range orig {
			parts[i] = sanitizeComponent(c, windows)
			if parts[i] != c {
				renamed = true
			}
		}
		final, changed, err := names.claim(parts)
		if err != nil {
			return nil, err
		}
		rel := strings.Join(final, "/")
		if !filepath.IsLocal(filepath.FromSlash(rel)) {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, rel)
		}
		abs := filepath.Join(absRoot, filepath.FromSlash(rel))
		if !within(absRoot, abs) {
			return nil, fmt.Errorf("%w: %q", ErrUnsafePath, rel)
		}
		l.Files = append(l.Files, File{
			Path:    abs,
			Rel:     rel,
			Length:  f.Length,
			Offset:  f.Offset,
			Renamed: renamed || changed,
		})
	}
	return l, nil
}

// Existing returns the files of this layout that already exist on disk.
// Callers use it to refuse overwriting unrelated files.
func (l *Layout) Existing() []string {
	var out []string
	for _, f := range l.Files {
		if _, err := os.Lstat(f.Path); err == nil {
			out = append(out, f.Path)
		}
	}
	return out
}

// within reports whether path is root or inside it (belt and braces on top
// of filepath.IsLocal).
func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// --- sanitising -------------------------------------------------------------

// windowsReserved are device names Windows refuses as file names, with or
// without an extension ("CON", "con.txt").
var windowsReserved = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true,
	"COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true,
	"LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// sanitizeComponent makes one path element safe on the target OS.
// metainfo already rejected traversal ("..", separators, NUL); this handles
// names that are merely illegal on some filesystems.
func sanitizeComponent(s string, windows bool) string {
	if windows {
		var b strings.Builder
		for _, r := range s {
			if r < 0x20 || strings.ContainsRune(`<>:"/\|?*`, r) {
				b.WriteRune('_')
			} else {
				b.WriteRune(r)
			}
		}
		s = strings.TrimRight(b.String(), ". ") // Windows silently strips these
		base := s
		if i := strings.IndexByte(base, '.'); i >= 0 {
			base = base[:i]
		}
		if windowsReserved[strings.ToUpper(strings.TrimRight(base, " "))] {
			s = "_" + s
		}
	}
	if s == "" || s == "." || s == ".." {
		s = "_"
	}
	return truncateName(s, maxComponentBytes)
}

// truncateName shortens s to at most max bytes, keeping a short extension
// and never splitting a UTF-8 character.
func truncateName(s string, max int) string {
	if len(s) <= max {
		return s
	}
	ext := filepath.Ext(s)
	if len(ext) > 16 {
		ext = ""
	}
	stem := s[:len(s)-len(ext)]
	cut := max - len(ext)
	for cut > 0 && !utf8.RuneStart(stem[cut]) {
		cut--
	}
	return stem[:cut] + ext
}

// --- collisions ---------------------------------------------------------------

// nameSet tracks claimed paths, case-insensitively: macOS and Windows
// filesystems are case-insensitive by default, so "A.txt" and "a.txt" would
// be the same file there. Using the same rule everywhere keeps a torrent's
// layout identical across operating systems.
type nameSet struct {
	files map[string]bool // claimed file paths (folded)
	dirs  map[string]bool // directories implied by claimed files (folded)
}

func newNameSet() *nameSet {
	return &nameSet{files: map[string]bool{}, dirs: map[string]bool{}}
}

func fold(parts []string) string { return strings.ToLower(strings.Join(parts, "/")) }

// claim reserves a path. If the file name collides with an existing file or
// directory it gets a " (1)", " (2)"… suffix. A directory colliding with an
// already-claimed *file* can't be fixed without moving other files, so that
// torrent is rejected.
func (n *nameSet) claim(parts []string) ([]string, bool, error) {
	for i := 1; i < len(parts); i++ {
		if n.files[fold(parts[:i])] {
			return nil, false, fmt.Errorf("%w: %q is both a file and a directory", ErrUnsafePath, strings.Join(parts[:i], "/"))
		}
	}
	out := append([]string(nil), parts...)
	last := len(out) - 1
	changed := false
	for k := 1; n.files[fold(out)] || n.dirs[fold(out)]; k++ {
		out[last] = withSuffix(parts[last], k)
		changed = true
	}
	n.files[fold(out)] = true
	for i := 1; i < len(out); i++ {
		n.dirs[fold(out[:i])] = true
	}
	return out, changed, nil
}

// withSuffix turns "song.mp3" into "song (2).mp3".
func withSuffix(name string, k int) string {
	ext := filepath.Ext(name)
	if ext == name { // ".hidden" has no stem
		ext = ""
	}
	suffix := " (" + strconv.Itoa(k) + ")"
	// Shorten the stem *before* adding the suffix, so a 255-byte name can't
	// lose its suffix to truncation (which would loop forever in claim).
	stem := truncateName(name[:len(name)-len(ext)], maxComponentBytes-len(ext)-len(suffix))
	return stem + suffix + ext
}
