package pieces

import (
	"slices"
	"testing"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
)

// filesInfo builds an Info with the given file sizes and piece length.
func filesInfo(pieceLen int64, sizes ...int64) *metainfo.Info {
	info := &metainfo.Info{PieceLength: pieceLen}
	for _, n := range sizes {
		info.Files = append(info.Files, metainfo.File{Length: n, Offset: info.TotalLength})
		info.TotalLength += n
	}
	info.Pieces = make([]metainfo.Hash, (info.TotalLength+pieceLen-1)/pieceLen)
	return info
}

func pieceList(b *bitfield.Bitfield) []int {
	var out []int
	for i := 0; i < b.Len(); i++ {
		if b.Has(i) {
			out = append(out, i)
		}
	}
	return out
}

func TestWanted(t *testing.T) {
	// Files [0,25) [25,25) [25,40) [40,60), pieces of 10 bytes: 0..5.
	info := filesInfo(10, 25, 0, 15, 20)
	if Wanted(info, nil) != nil || Wanted(info, []bool{false, false}) != nil {
		t.Error("nothing skipped must mean nil (all)")
	}
	cases := []struct {
		skip []bool
		want []int
	}{
		{[]bool{true}, []int{2, 3, 4, 5}},                  // piece 2 is shared with file 2: still wanted
		{[]bool{false, false, true}, []int{0, 1, 2, 4, 5}}, // file 2 = pieces 2-3; 2 is shared with file 0
		{[]bool{true, true, true}, []int{4, 5}},
		{[]bool{false, false, false, true}, []int{0, 1, 2, 3}},
		{[]bool{true, false, true, true}, nil}, // only the empty file is left: nothing to download
	}
	for _, c := range cases {
		if got := pieceList(Wanted(info, c.skip)); !slices.Equal(got, c.want) {
			t.Errorf("Wanted(skip %v) = %v, want %v", c.skip, got, c.want)
		}
	}
}

func TestSkipSet(t *testing.T) {
	if SkipSet(nil, 3) != nil {
		t.Error("no indexes = nil")
	}
	if got := SkipSet([]int{2, 0, 7, -1}, 3); !slices.Equal(got, []bool{true, false, true}) {
		t.Errorf("SkipSet = %v", got)
	}
}

func TestManagerOnlyRequestsWantedPieces(t *testing.T) {
	info := filesInfo(peer.BlockSize, 3*peer.BlockSize, 3*peer.BlockSize) // pieces 0-2 | 3-5
	want := Wanted(info, []bool{false, true})
	m := NewManager(info, nil, Config{})
	m.SetWanted(want)
	all := bitfield.New(6)
	all.SetAll()

	if got, wantLeft := m.WantedBytes(), int64(3*peer.BlockSize); got != wantLeft || m.BytesLeft() != wantLeft {
		t.Errorf("WantedBytes = %d, BytesLeft = %d; want %d", got, m.BytesLeft(), wantLeft)
	}
	if m.MissingBytes() != info.TotalLength {
		t.Errorf("MissingBytes = %d, want the whole torrent", m.MissingBytes())
	}
	reqs := m.NextRequests(peerA, all, 100)
	if len(reqs) != 3 {
		t.Fatalf("requested %d blocks, want 3", len(reqs))
	}
	for _, b := range reqs {
		if b.Index > 2 {
			t.Errorf("requested unwanted piece %d", b.Index)
		}
	}
	only5 := bitfield.New(6)
	only5.Set(5)
	if m.Interesting(only5) {
		t.Error("a peer with only unwanted pieces is not interesting")
	}
	for _, b := range reqs {
		r := m.BlockReceived(peerA, b.Index, b.Begin, make([]byte, b.Length))
		m.PieceVerified(int(b.Index), r.Complete)
	}
	if !m.Complete() || m.HaveAll() || m.BytesLeft() != 0 {
		t.Errorf("after wanted pieces: Complete=%v HaveAll=%v BytesLeft=%d", m.Complete(), m.HaveAll(), m.BytesLeft())
	}
	if got := m.NextRequests(peerA, all, 100); len(got) != 0 {
		t.Errorf("finished selection still requests %v (endgame must not reach unwanted pieces)", got)
	}

	// Wanting everything again resumes the download.
	m.SetWanted(nil)
	if m.Complete() || len(m.NextRequests(peerA, all, 100)) != 3 {
		t.Error("SetWanted(nil) should make pieces 3-5 wanted again")
	}
}

func TestSetWantedAbandonsActivePieces(t *testing.T) {
	info := filesInfo(2*peer.BlockSize, 2*peer.BlockSize, 2*peer.BlockSize) // file 0 = piece 0, file 1 = piece 1
	m := NewManager(info, nil, Config{})
	all := bitfield.New(2)
	all.SetAll()
	m.NextRequests(peerA, all, 100) // both pieces active
	if m.ActivePieces() != 2 {
		t.Fatalf("active = %d", m.ActivePieces())
	}
	abandoned := m.SetWanted(Wanted(info, []bool{true}))
	if !slices.Equal(abandoned, []int{0}) || m.ActivePieces() != 1 {
		t.Fatalf("abandoned %v, active %d; want [0], 1", abandoned, m.ActivePieces())
	}
	if r := m.BlockReceived(peerA, 0, 0, make([]byte, peer.BlockSize)); r.Accepted {
		t.Error("a block of an abandoned piece must not be accepted")
	}
	if left := m.PeerRequestsDropped(peerA); len(left) != 2 {
		t.Errorf("piece 1 should still have 2 outstanding blocks, got %v", left)
	}
}
