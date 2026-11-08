package metainfo

import (
	"path"
	"strings"

	"github.com/7Eshaan/Denco/internal/bencode"
)

// Limits that reject absurd or hostile torrents before we allocate for them.
const (
	minPieceLength = 1 << 10   // 1 KiB  (real torrents use >= 16 KiB)
	maxPieceLength = 256 << 20 // 256 MiB
	maxFiles       = 1 << 20
)

// Info is the validated contents of a torrent's info dictionary.
type Info struct {
	Name        string
	PieceLength int64
	Pieces      []Hash
	Private     bool
	MultiFile   bool // true if the torrent had a "files" list

	// Files always has at least one entry. A single-file torrent becomes one
	// File with an empty Path, so later code never branches on the layout.
	Files       []File
	TotalLength int64
}

// File is one file in the torrent.
type File struct {
	// Path components *below* the torrent name; empty for single-file
	// torrents. Each component is validated as safe (see validComponent).
	Path   []string
	Length int64
	Offset int64 // byte offset of this file in the concatenated torrent data
}

// DisplayPath returns the file's path relative to the download directory,
// using forward slashes (for display; storage builds OS paths itself).
func (f File) DisplayPath(info *Info) string {
	return path.Join(append([]string{info.Name}, f.Path...)...)
}

// NumPieces returns the number of pieces.
func (i *Info) NumPieces() int { return len(i.Pieces) }

// PieceSize returns the length of piece idx (the last piece may be shorter).
func (i *Info) PieceSize(idx int) int64 {
	if idx == len(i.Pieces)-1 {
		if rem := i.TotalLength % i.PieceLength; rem != 0 {
			return rem
		}
	}
	return i.PieceLength
}

// ParseInfo validates a raw bencoded info dictionary. It is used both for
// .torrent files and for metadata received from peers via magnet links.
func ParseInfo(raw []byte) (*Info, error) {
	v, err := bencode.Decode(raw)
	if err != nil {
		return nil, invalid("info: %v", err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, invalid("info is not a dictionary")
	}

	info := &Info{}

	info.Name = preferUTF8(d, "name")
	if info.Name == "" {
		return nil, invalid("missing name")
	}
	if !validComponent(info.Name) {
		return nil, invalid("unsafe torrent name %q", info.Name)
	}

	plen, ok := bencode.GetInt(d, "piece length")
	if !ok {
		return nil, invalid("missing piece length")
	}
	if plen < minPieceLength || plen > maxPieceLength {
		return nil, invalid("piece length %d out of range", plen)
	}
	info.PieceLength = plen

	if p, ok := bencode.GetInt(d, "private"); ok && p == 1 {
		info.Private = true
	}

	if err := parseFiles(d, info); err != nil {
		return nil, err
	}

	pieces, ok := bencode.GetString(d, "pieces")
	if !ok {
		if _, v2 := d["meta version"]; v2 {
			return nil, invalid("BitTorrent v2-only torrents are not supported")
		}
		return nil, invalid("missing pieces")
	}
	if len(pieces)%HashSize != 0 {
		return nil, invalid("pieces length %d is not a multiple of %d", len(pieces), HashSize)
	}
	n := len(pieces) / HashSize
	want := (info.TotalLength + info.PieceLength - 1) / info.PieceLength
	if int64(n) != want {
		return nil, invalid("have %d piece hashes, expected %d for %d bytes", n, want, info.TotalLength)
	}
	info.Pieces = make([]Hash, n)
	for i := range info.Pieces {
		copy(info.Pieces[i][:], pieces[i*HashSize:])
	}
	return info, nil
}

// parseFiles fills Files, TotalLength and MultiFile.
func parseFiles(d map[string]any, info *Info) error {
	length, hasLength := bencode.GetInt(d, "length")
	files, hasFiles := bencode.GetList(d, "files")

	switch {
	case hasLength && hasFiles:
		return invalid("both length and files present")
	case hasLength:
		if length < 0 {
			return invalid("negative length")
		}
		info.Files = []File{{Length: length}}
		info.TotalLength = length
	case hasFiles:
		if len(files) == 0 {
			return invalid("empty files list")
		}
		if len(files) > maxFiles {
			return invalid("too many files (%d)", len(files))
		}
		info.MultiFile = true
		seen := make(map[string]bool, len(files))
		for i, f := range files {
			fd, ok := f.(map[string]any)
			if !ok {
				return invalid("file %d is not a dictionary", i)
			}
			file, err := parseFile(fd, i)
			if err != nil {
				return err
			}
			// Exact duplicates would make two files share one path. Case-only
			// differences (which collide on macOS/Windows) are left to storage.
			key := strings.Join(file.Path, "/")
			if seen[key] {
				return invalid("duplicate file path %q", strings.Join(file.Path, "/"))
			}
			seen[key] = true
			file.Offset = info.TotalLength
			info.TotalLength += file.Length
			if info.TotalLength < 0 {
				return invalid("total length overflows")
			}
			info.Files = append(info.Files, file)
		}
	default:
		return invalid("neither length nor files present")
	}
	if info.TotalLength == 0 {
		return invalid("torrent has no data")
	}
	return nil
}

func parseFile(fd map[string]any, i int) (File, error) {
	length, ok := bencode.GetInt(fd, "length")
	if !ok || length < 0 {
		return File{}, invalid("file %d: missing or negative length", i)
	}
	list, ok := bencode.GetList(fd, "path.utf-8")
	if !ok {
		list, ok = bencode.GetList(fd, "path")
	}
	if !ok || len(list) == 0 {
		return File{}, invalid("file %d: missing path", i)
	}
	parts := make([]string, 0, len(list))
	for _, p := range list {
		s, ok := p.(string)
		s = toValidUTF8(s)
		if !ok || !validComponent(s) {
			return File{}, invalid("file %d: unsafe path component %q", i, p)
		}
		parts = append(parts, s)
	}
	return File{Path: parts, Length: length}, nil
}

// preferUTF8 returns d[key+".utf-8"] if present, else d[key], with invalid
// UTF-8 replaced (old torrents sometimes use legacy encodings like GBK).
func preferUTF8(d map[string]any, key string) string {
	s, ok := bencode.GetString(d, key+".utf-8")
	if !ok || s == "" {
		s, _ = bencode.GetString(d, key)
	}
	return toValidUTF8(s)
}

func toValidUTF8(s string) string { return strings.ToValidUTF8(s, "_") }

// validComponent reports whether a single path element is safe to join under
// the download directory on every supported OS. It rejects traversal and
// separators; Windows-only naming limits are sanitised later by storage.
func validComponent(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	// ':' (drive letters, NTFS streams) and other Windows-illegal characters
	// are sanitised by storage, not rejected here: "1: Intro.mp4" is a
	// legitimate name on macOS/Linux.
	return !strings.ContainsAny(s, "/\\\x00")
}
