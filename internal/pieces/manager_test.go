package pieces

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
)

var (
	peerA = netip.MustParseAddrPort("10.0.0.1:1")
	peerB = netip.MustParseAddrPort("10.0.0.2:2")
	peerC = netip.MustParseAddrPort("10.0.0.3:3")
)

func full(n int) *bitfield.Bitfield {
	b := bitfield.New(n)
	b.SetAll()
	return b
}

func only(n int, idx ...int) *bitfield.Bitfield {
	b := bitfield.New(n)
	for _, i := range idx {
		b.Set(i)
	}
	return b
}

// deliver answers a block request from the true data.
func deliver(m *Manager, from PeerKey, data []byte, info *metainfo.Info, b peer.Block) Received {
	off := int64(b.Index)*info.PieceLength + int64(b.Begin)
	return m.BlockReceived(from, b.Index, b.Begin, data[off:off+int64(b.Length)])
}

func TestDownloadEverythingFromOneSeeder(t *testing.T) {
	// 5 pieces of 40000 bytes (3 blocks each, last block short) + a short last piece.
	data, info := testTorrent(t, 40000, 40000*5+1234)
	m := NewManager(info, nil, Config{})
	m.PeerBitfield(full(info.NumPieces()))
	assembled := make([]byte, len(data))

	for rounds := 0; !m.Complete(); rounds++ {
		if rounds > 100 {
			t.Fatal("download did not finish")
		}
		reqs := m.NextRequests(peerA, full(info.NumPieces()), 5)
		if len(reqs) == 0 {
			t.Fatal("no requests while incomplete")
		}
		for _, b := range reqs {
			if b.Length > peer.BlockSize || b.Length == 0 {
				t.Fatalf("bad block length %d", b.Length)
			}
			r := deliver(m, peerA, data, info, b)
			if !r.Accepted {
				t.Fatalf("block %+v rejected", b)
			}
			if r.Complete {
				ok := Check(r.Data, info.Pieces[b.Index])
				if !ok {
					t.Fatalf("piece %d assembled wrong", b.Index)
				}
				copy(assembled[int64(b.Index)*info.PieceLength:], r.Data)
				if len(r.Contributors) != 1 || r.Contributors[0] != peerA {
					t.Fatalf("contributors = %v", r.Contributors)
				}
				m.PieceVerified(int(b.Index), true)
			}
		}
	}
	if !bytes.Equal(assembled, data) {
		t.Fatal("assembled data differs")
	}
	if m.BytesLeft() != 0 || m.ActivePieces() != 0 {
		t.Fatalf("left=%d active=%d", m.BytesLeft(), m.ActivePieces())
	}
	if len(m.NextRequests(peerA, full(info.NumPieces()), 5)) != 0 {
		t.Fatal("requests after completion")
	}
}

func TestLastBlockOfLastPieceIsShort(t *testing.T) {
	_, info := testTorrent(t, 32768, 32768+100) // piece 1 is 100 bytes
	m := NewManager(info, only(2, 0), Config{})
	reqs := m.NextRequests(peerA, full(2), 10)
	if len(reqs) != 1 || reqs[0] != (peer.Block{Index: 1, Begin: 0, Length: 100}) {
		t.Fatalf("reqs = %+v", reqs)
	}
}

func TestRarestFirst(t *testing.T) {
	_, info := testTorrent(t, 16384, 16384*10)
	// Already have 4 pieces so random-first is over.
	m := NewManager(info, only(10, 6, 7, 8, 9), Config{})
	// Availability: piece 0 → 3 peers, piece 1 → 2 peers, piece 2 → 1 peer, 3–5 → 3.
	m.PeerBitfield(only(10, 0, 1, 2, 3, 4, 5))
	m.PeerBitfield(only(10, 0, 1, 3, 4, 5))
	m.PeerBitfield(only(10, 0, 3, 4, 5))

	order := []int{}
	for i := 0; i < 3; i++ {
		reqs := m.NextRequests(peerA, full(10), 1) // one block = one whole piece here
		order = append(order, int(reqs[0].Index))
	}
	if order[0] != 2 || order[1] != 1 {
		t.Fatalf("pick order = %v, want 2 then 1 first", order)
	}
	if m.Availability(0) != 3 || m.Availability(2) != 1 {
		t.Fatal("availability counts wrong")
	}
	m.PeerGoneBitfield(only(10, 0, 1, 2))
	m.PeerHave(9)
	if m.Availability(0) != 2 || m.Availability(2) != 0 || m.Availability(9) != 1 {
		t.Fatal("availability after leave/have wrong")
	}
}

func TestRandomFirstSpreadsPicks(t *testing.T) {
	_, info := testTorrent(t, 16384, 16384*50)
	seen := map[uint32]bool{}
	for trial := 0; trial < 40; trial++ {
		m := NewManager(info, nil, Config{})
		// Piece 0 is the rarest, but with no pieces yet we pick randomly.
		m.PeerBitfield(full(50))
		m.PeerBitfield(only(50, 1, 2, 3)) // make 1–3 *more* common
		reqs := m.NextRequests(peerA, full(50), 1)
		seen[reqs[0].Index] = true
	}
	if len(seen) < 5 {
		t.Fatalf("random-first picked only %d distinct pieces in 40 trials", len(seen))
	}
}

func TestFinishActivePiecesFirst(t *testing.T) {
	_, info := testTorrent(t, 16384*4, 16384*4*3) // 3 pieces × 4 blocks
	m := NewManager(info, nil, Config{})
	a := m.NextRequests(peerA, full(3), 1)
	b := m.NextRequests(peerB, full(3), 3)
	for _, blk := range b {
		if blk.Index != a[0].Index {
			t.Fatalf("peer B started piece %d instead of finishing %d", blk.Index, a[0].Index)
		}
	}
	if m.ActivePieces() != 1 {
		t.Fatalf("active = %d", m.ActivePieces())
	}
}

func TestOnlyRequestsPiecesPeerHas(t *testing.T) {
	_, info := testTorrent(t, 16384, 16384*4)
	m := NewManager(info, nil, Config{})
	for _, b := range m.NextRequests(peerA, only(4, 2), 10) {
		if b.Index != 2 {
			t.Fatalf("requested piece %d the peer doesn't have", b.Index)
		}
	}
	if !m.Interesting(only(4, 3)) || m.Interesting(bitfield.New(4)) {
		t.Fatal("Interesting wrong")
	}
	m2 := NewManager(info, full(4), Config{})
	if m2.Interesting(full(4)) {
		t.Fatal("complete manager should find nothing interesting")
	}
}

func TestMemoryBudget(t *testing.T) {
	_, info := testTorrent(t, 16384*2, 16384*2*20)        // 20 pieces × 2 blocks
	m := NewManager(info, nil, Config{MaxActiveBytes: 1}) // floor of 4 pieces
	reqs := m.NextRequests(peerA, full(20), 100)
	if m.ActivePieces() != 4 || len(reqs) != 8 {
		t.Fatalf("active=%d reqs=%d, want 4 and 8", m.ActivePieces(), len(reqs))
	}
	// Peer A disconnects: its pieces become idle and may be abandoned so a
	// peer with different pieces isn't starved.
	if dropped := m.PeerRequestsDropped(peerA); len(dropped) != 8 {
		t.Fatalf("dropped %d", len(dropped))
	}
	var rest []int
	for i := 0; i < 20; i++ {
		if m.active[i] == nil {
			rest = append(rest, i)
		}
	}
	reqs = m.NextRequests(peerB, only(20, rest...), 2)
	if len(reqs) != 2 {
		t.Fatalf("peer B starved: %d reqs", len(reqs))
	}
}

func TestEndgameDuplicatesAndCancels(t *testing.T) {
	data, info := testTorrent(t, 16384*2, 16384*2) // 1 piece × 2 blocks
	m := NewManager(info, nil, Config{})
	a := m.NextRequests(peerA, full(1), 10)
	if len(a) != 2 {
		t.Fatalf("A got %d", len(a))
	}
	// Everything is requested → endgame: B may ask for the same blocks.
	b := m.NextRequests(peerB, full(1), 10)
	if len(b) != 2 {
		t.Fatalf("B got %d endgame requests", len(b))
	}
	// A asking again gets nothing new (it already has those requests).
	if again := m.NextRequests(peerA, full(1), 10); len(again) != 0 {
		t.Fatalf("A re-requested %v", again)
	}

	r := deliver(m, peerA, data, info, a[0])
	if !r.Accepted || len(r.Cancel) != 1 || r.Cancel[0] != peerB {
		t.Fatalf("first copy: %+v", r)
	}
	// B's late duplicate is ignored.
	if r := deliver(m, peerB, data, info, a[0]); r.Accepted {
		t.Fatal("duplicate block accepted")
	}
	r = deliver(m, peerB, data, info, a[1])
	if !r.Complete || len(r.Cancel) != 1 || r.Cancel[0] != peerA || len(r.Contributors) != 2 {
		t.Fatalf("completion: %+v", r)
	}
}

func TestNoEndgameWhileBlocksUnrequested(t *testing.T) {
	_, info := testTorrent(t, 16384, 16384*3)
	m := NewManager(info, nil, Config{})
	m.NextRequests(peerA, only(3, 0), 10)        // piece 0 requested by A
	got := m.NextRequests(peerB, only(3, 0), 10) // B only has piece 0
	if len(got) != 0 {
		t.Fatalf("endgame started early: %v", got)
	}
}

func TestBlockReceivedRejectsBadBlocks(t *testing.T) {
	data, info := testTorrent(t, 16384*2, 16384*4)
	m := NewManager(info, nil, Config{})
	m.NextRequests(peerA, only(2, 0), 2) // piece 0 active
	cases := map[string]Received{
		"inactive piece": m.BlockReceived(peerA, 1, 0, data[:16384]),
		"misaligned":     m.BlockReceived(peerA, 0, 100, data[:16384]),
		"past end":       m.BlockReceived(peerA, 0, 16384*2, data[:16384]),
		"wrong length":   m.BlockReceived(peerA, 0, 0, data[:100]),
	}
	for name, r := range cases {
		if r.Accepted {
			t.Errorf("%s accepted", name)
		}
	}
	// Unsolicited-but-valid block for an active piece is fine.
	if r := m.BlockReceived(peerC, 0, 0, data[:16384]); !r.Accepted {
		t.Error("valid block from another peer rejected")
	}
}

func TestHashFailureRedownloads(t *testing.T) {
	data, info := testTorrent(t, 16384, 16384*2)
	m := NewManager(info, only(2, 1), Config{})
	reqs := m.NextRequests(peerA, full(2), 10)
	r := deliver(m, peerA, data, info, reqs[0])
	if !r.Complete {
		t.Fatal("expected completion")
	}
	// While pending verification it must not be re-requested.
	if again := m.NextRequests(peerB, full(2), 10); len(again) != 0 {
		t.Fatalf("pending piece re-requested: %v", again)
	}
	m.PieceVerified(0, false)
	if m.HasPiece(0) {
		t.Fatal("failed piece marked have")
	}
	if again := m.NextRequests(peerB, full(2), 10); len(again) != 1 || again[0].Index != 0 {
		t.Fatalf("failed piece not re-requested: %v", again)
	}
	m.PieceVerified(1, true) // not pending: ignored, no panic
}

func TestRequestsDroppedGoToOtherPeer(t *testing.T) {
	_, info := testTorrent(t, 16384*3, 16384*3)
	m := NewManager(info, nil, Config{})
	m.NextRequests(peerA, full(1), 2)
	if d := m.PeerRequestsDropped(peerA); len(d) != 2 {
		t.Fatalf("dropped = %v", d)
	}
	if got := m.NextRequests(peerB, full(1), 10); len(got) != 3 {
		t.Fatalf("B got %d, want all 3 blocks", len(got))
	}
}

func TestBytesLeftWithResume(t *testing.T) {
	_, info := testTorrent(t, 1000, 2500) // pieces 1000, 1000, 500
	have := only(3, 0, 2)
	m := NewManager(info, have, Config{})
	if m.BytesLeft() != 1000 {
		t.Fatalf("BytesLeft = %d", m.BytesLeft())
	}
	have.Set(1) // Manager must have cloned it
	if m.HasPiece(1) {
		t.Fatal("NewManager did not clone have")
	}
	if !m.Have().Has(0) {
		t.Fatal("Have() wrong")
	}
}
