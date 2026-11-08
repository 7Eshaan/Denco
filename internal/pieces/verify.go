// Package pieces decides which pieces and blocks to download and verifies
// piece data against the torrent's SHA-1 hashes.
//
//   - verify.go:  hash checks and full rechecks of data on disk
//   - manager.go: rarest-first piece picking and per-block request
//     bookkeeping (Manager)
package pieces

import (
	"context"
	"crypto/sha1"
	"errors"
	"io"
	"os"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// Check reports whether data hashes to want.
func Check(data []byte, want metainfo.Hash) bool {
	return sha1.Sum(data) == want
}

// VerifyPiece reads piece i from r into buf (which must be at least
// PieceLength long) and checks its hash. Data that isn't on disk yet (missing
// or short file) reports false with no error; other read errors are returned.
func VerifyPiece(r io.ReaderAt, info *metainfo.Info, i int, buf []byte) (bool, error) {
	size := info.PieceSize(i)
	data := buf[:size]
	if _, err := r.ReadAt(data, int64(i)*info.PieceLength); err != nil {
		if missing(err) {
			return false, nil
		}
		return false, err
	}
	return Check(data, info.Pieces[i]), nil
}

func missing(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

// Recheck hashes every piece on disk using several workers and returns the
// set of pieces that are present and correct. progress, if non-nil, is
// called (from worker goroutines) with the number of pieces checked so far.
func Recheck(ctx context.Context, r io.ReaderAt, info *metainfo.Info, progress func(checked int)) (*bitfield.Bitfield, error) {
	n := info.NumPieces()
	have := bitfield.New(n)
	workers := min(runtime.NumCPU(), 4, max(n, 1)) // disk-bound beyond ~4

	var (
		mu       sync.Mutex // guards have and firstErr
		firstErr error
		next     atomic.Int64 // next piece index to claim
		checked  atomic.Int64
		wg       sync.WaitGroup
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, info.PieceLength)
			for {
				i := int(next.Add(1) - 1)
				if i >= n || ctx.Err() != nil {
					return
				}
				ok, err := VerifyPiece(r, info, i, buf)
				mu.Lock()
				if err != nil && firstErr == nil {
					firstErr = err
				}
				if ok {
					have.Set(i)
				}
				mu.Unlock()
				if err != nil {
					return
				}
				c := checked.Add(1)
				if progress != nil {
					progress(int(c))
				}
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return have, nil
}
