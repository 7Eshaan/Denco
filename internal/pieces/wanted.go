package pieces

import (
	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// File selection (DECISIONS D44). The user picks files; the download works
// in pieces. A piece is wanted if it overlaps at least one wanted file, so
// a piece on the border between a wanted and a skipped file is downloaded
// whole — its hash covers both files — and its bytes in the skipped file
// are written too.

// Wanted returns the pieces needed for the files not skipped (skip[i] true
// means file i is skipped; a short or nil skip skips nothing past its end).
// It returns nil when nothing is skipped: nil means "every piece" to the
// Manager and to bitfield.Covers.
func Wanted(info *metainfo.Info, skip []bool) *bitfield.Bitfield {
	if !anySkipped(skip) {
		return nil
	}
	want := bitfield.New(info.NumPieces())
	for i, f := range info.Files {
		if f.Length == 0 || (i < len(skip) && skip[i]) {
			continue
		}
		first := int(f.Offset / info.PieceLength)
		last := int((f.Offset + f.Length - 1) / info.PieceLength)
		for p := first; p <= last; p++ {
			want.Set(p)
		}
	}
	return want
}

// SkipSet turns a list of skipped file indexes (as stored in resume data)
// into a per-file flag slice for n files. Out-of-range indexes are ignored.
func SkipSet(indexes []int, n int) []bool {
	if len(indexes) == 0 {
		return nil
	}
	skip := make([]bool, n)
	for _, i := range indexes {
		if i >= 0 && i < n {
			skip[i] = true
		}
	}
	return skip
}

func anySkipped(skip []bool) bool {
	for _, s := range skip {
		if s {
			return true
		}
	}
	return false
}
