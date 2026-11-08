package resume

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/storage"
)

// ErrMismatch means the saved state doesn't fit this torrent or layout
// (different torrent, piece count, or file list). Callers should fall back
// to a full recheck.
var ErrMismatch = errors.New("resume: saved state doesn't match the torrent")

// Stamp records the current size and mtime of every file in the layout.
func Stamp(layout *storage.Layout) []FileStamp {
	out := make([]FileStamp, len(layout.Files))
	for i, f := range layout.Files {
		fi, err := os.Stat(f.Path)
		if err != nil {
			out[i] = FileStamp{Size: -1}
			continue
		}
		out[i] = FileStamp{Size: fi.Size(), ModTime: fi.ModTime().UnixNano()}
	}
	return out
}

// Report says what Restore did, for logging and the UI.
type Report struct {
	Saved     int // pieces the state claimed
	Kept      int // pieces still trusted afterwards
	Dropped   int // pieces dropped because their file is missing or too short
	Rechecked int // pieces re-hashed because a file changed while we were stopped
	Failed    int // rechecked pieces that no longer matched
	Notes     []string
}

// Restore turns saved state into the set of pieces we can trust now.
//
// Rules (see DECISIONS D27):
//   - Before every save the data was fsynced, so pieces in Have were on disk
//     at that point, and verified pieces are never rewritten.
//   - A file that is missing, or shorter than a piece needs, loses those pieces.
//   - If the last save was a clean shutdown (Clean) and a file's size or
//     mtime has changed since, something else modified it: its pieces are
//     re-hashed and kept only if they still match.
//   - After a crash (!Clean), files are expected to be newer than the stamps
//     (we kept writing after the last save), so mtimes are not compared.
//     Pieces finished after that save are simply downloaded again.
//
// r reads the torrent's data (normally the opened storage).
func Restore(ctx context.Context, st *State, mi *metainfo.MetaInfo, layout *storage.Layout, r io.ReaderAt) (*bitfield.Bitfield, Report, error) {
	info := &mi.Info
	var rep Report
	if st.InfoHash != mi.InfoHash.String() || st.Pieces != info.NumPieces() || len(st.Files) != len(layout.Files) {
		return nil, rep, ErrMismatch
	}
	have, err := bitfield.FromBytes(st.Have, info.NumPieces())
	if err != nil {
		return nil, rep, fmt.Errorf("%w: %v", ErrMismatch, err)
	}
	rep.Saved = have.Count()

	drop := map[int]bool{}
	recheck := map[int]bool{}
	for i, f := range layout.Files {
		if f.Length == 0 {
			continue
		}
		first, last := pieceSpan(info, f)
		fi, err := os.Stat(f.Path)
		if err != nil {
			if anyHave(have, first, last) {
				rep.Notes = append(rep.Notes, f.Rel+": missing")
			}
			for p := first; p <= last; p++ {
				if have.Has(p) {
					drop[p] = true
				}
			}
			continue
		}
		size := fi.Size()
		for p := first; p <= last; p++ {
			if have.Has(p) && size < neededSize(info, f, p) {
				drop[p] = true
			}
		}
		stamp := st.Files[i]
		changed := size > f.Length ||
			(st.Clean && (size != stamp.Size || fi.ModTime().UnixNano() != stamp.ModTime))
		if changed && anyHave(have, first, last) {
			rep.Notes = append(rep.Notes, f.Rel+": changed since last run, re-checking")
			for p := first; p <= last; p++ {
				if have.Has(p) && !drop[p] {
					recheck[p] = true
				}
			}
		}
	}

	for p := range drop {
		have.Clear(p)
		delete(recheck, p)
	}
	rep.Dropped = len(drop)

	order := make([]int, 0, len(recheck))
	for p := range recheck {
		order = append(order, p)
	}
	sort.Ints(order)
	buf := make([]byte, info.PieceLength)
	for _, p := range order {
		if err := ctx.Err(); err != nil {
			return nil, rep, err
		}
		ok, err := pieces.VerifyPiece(r, info, p, buf)
		if err != nil {
			return nil, rep, err
		}
		rep.Rechecked++
		if !ok {
			have.Clear(p)
			rep.Failed++
		}
	}
	rep.Kept = have.Count()
	return have, rep, nil
}

// pieceSpan returns the first and last piece overlapping file f (non-empty).
func pieceSpan(info *metainfo.Info, f storage.File) (int, int) {
	return int(f.Offset / info.PieceLength), int((f.Offset + f.Length - 1) / info.PieceLength)
}

// neededSize is how long f must be for piece p's bytes in f to exist.
func neededSize(info *metainfo.Info, f storage.File, p int) int64 {
	end := int64(p)*info.PieceLength + info.PieceSize(p) // piece end, torrent offset
	return min(f.Length, end-f.Offset)
}

func anyHave(b *bitfield.Bitfield, first, last int) bool {
	for p := first; p <= last; p++ {
		if b.Has(p) {
			return true
		}
	}
	return false
}
