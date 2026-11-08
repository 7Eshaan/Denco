package pieces

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"math/rand/v2"
	"os"
	"sync/atomic"
	"testing"

	"github.com/7Eshaan/Denco/internal/metainfo"
)

// testTorrent returns random content and an Info describing it.
func testTorrent(t *testing.T, pieceLen, total int) ([]byte, *metainfo.Info) {
	t.Helper()
	data := make([]byte, total)
	r := rand.New(rand.NewPCG(1, 2))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	info := &metainfo.Info{Name: "t", PieceLength: int64(pieceLen), TotalLength: int64(total)}
	for off := 0; off < total; off += pieceLen {
		end := min(off+pieceLen, total)
		info.Pieces = append(info.Pieces, sha1.Sum(data[off:end]))
	}
	info.Files = []metainfo.File{{Length: int64(total)}}
	return data, info
}

// memReader is an io.ReaderAt over a byte slice whose tail may be "missing".
type memReader struct {
	data    []byte
	present int // bytes [present:] behave like a missing file
	fail    error
}

func (m *memReader) ReadAt(p []byte, off int64) (int, error) {
	if m.fail != nil {
		return 0, m.fail
	}
	if int(off)+len(p) > m.present {
		return 0, os.ErrNotExist
	}
	return copy(p, m.data[off:]), nil
}

func TestCheck(t *testing.T) {
	if !Check([]byte("abc"), sha1.Sum([]byte("abc"))) || Check([]byte("abd"), sha1.Sum([]byte("abc"))) {
		t.Fatal("Check wrong")
	}
}

func TestVerifyPiece(t *testing.T) {
	data, info := testTorrent(t, 1024, 1024*3+100)
	r := &memReader{data: data, present: len(data)}
	buf := make([]byte, info.PieceLength)
	for i := 0; i < info.NumPieces(); i++ {
		if ok, err := VerifyPiece(r, info, i, buf); !ok || err != nil {
			t.Fatalf("piece %d: ok=%v err=%v", i, ok, err)
		}
	}
	data[1500] ^= 0xff // corrupt piece 1
	if ok, _ := VerifyPiece(r, info, 1, buf); ok {
		t.Fatal("corrupt piece passed")
	}
}

func TestRecheck(t *testing.T) {
	data, info := testTorrent(t, 1024, 1024*20+7)
	corrupted := bytes.Clone(data)
	corrupted[1024*5+3] ^= 1                             // piece 5 bad
	r := &memReader{data: corrupted, present: 1024 * 15} // pieces 15.. missing

	var calls atomic.Int64
	have, err := Recheck(context.Background(), r, info, func(int) { calls.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < info.NumPieces(); i++ {
		want := i < 15 && i != 5
		if have.Has(i) != want {
			t.Errorf("piece %d: have=%v want=%v", i, have.Has(i), want)
		}
	}
	if int(calls.Load()) != info.NumPieces() {
		t.Errorf("progress called %d times, want %d", calls.Load(), info.NumPieces())
	}
}

func TestRecheckErrors(t *testing.T) {
	_, info := testTorrent(t, 1024, 4096)
	ioErr := errors.New("disk on fire")
	if _, err := Recheck(context.Background(), &memReader{fail: ioErr}, info, nil); !errors.Is(err, ioErr) {
		t.Errorf("read error not returned: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	data, _ := testTorrent(t, 1024, 4096)
	if _, err := Recheck(ctx, &memReader{data: data, present: len(data)}, info, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel not honoured: %v", err)
	}
}
