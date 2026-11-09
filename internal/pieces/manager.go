package pieces

import (
	"math/rand/v2"
	"net/netip"
	"slices"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
)

// PeerKey identifies a connected peer to the Manager.
type PeerKey = netip.AddrPort

// Config tunes the Manager. Zero values use defaults.
type Config struct {
	// MaxActiveBytes caps memory held by partly downloaded pieces
	// (default 128 MiB). At least 4 pieces may always be active.
	MaxActiveBytes int64
	// RandomFirst: pick pieces at random until this many are complete
	// (default 4), so we quickly have something to offer other peers.
	RandomFirst int
}

// Manager tracks which pieces we have, which are being downloaded, which
// blocks are requested from whom, and how many peers have each piece.
//
// Lifecycle of a piece:
//
//	missing → active (blocks requested/received) → pending (all blocks in,
//	awaiting hash check) → have        (or back to missing if the hash fails)
//
// Not safe for concurrent use: the torrent session goroutine owns it.
type Manager struct {
	info *metainfo.Info
	have *bitfield.Bitfield
	want *bitfield.Bitfield // pieces of the files the user wants; nil = all (D44)

	avail   []int           // number of connected peers having each piece
	active  map[int]*active // pieces being downloaded
	pending map[int]bool    // complete in memory, being verified

	maxActive   int
	randomFirst int
	noEndgame   bool // SetEndgame(false): never request a block twice
	rng         *rand.Rand
}

type active struct {
	data         []byte
	blocks       []blockState
	received     int // number of blocks done
	contributors map[PeerKey]bool
}

type blockState struct {
	done   bool
	owners []PeerKey // peers we've asked for this block (more than one only in endgame)
}

// NewManager starts from have (pieces already verified, e.g. from resume
// data); have is cloned. A nil have means nothing yet.
func NewManager(info *metainfo.Info, have *bitfield.Bitfield, cfg Config) *Manager {
	if have == nil {
		have = bitfield.New(info.NumPieces())
	} else {
		have = have.Clone()
	}
	budget := cfg.MaxActiveBytes
	if budget <= 0 {
		budget = 128 << 20
	}
	rf := cfg.RandomFirst
	if rf <= 0 {
		rf = 4
	}
	return &Manager{
		info:        info,
		have:        have,
		avail:       make([]int, info.NumPieces()),
		active:      make(map[int]*active),
		pending:     make(map[int]bool),
		maxActive:   max(4, int(budget/info.PieceLength)),
		randomFirst: rf,
		rng:         rand.New(rand.NewPCG(rand.Uint64(), rand.Uint64())),
	}
}

// --- state queries ------------------------------------------------------------

// Have returns a copy of the verified-pieces bitfield.
func (m *Manager) Have() *bitfield.Bitfield { return m.have.Clone() }

// HasPiece reports whether piece i is verified.
func (m *Manager) HasPiece(i int) bool { return m.have.Has(i) }

// Complete reports whether every *wanted* piece is verified: the download
// is finished, though with skipped files we are only a partial seed.
func (m *Manager) Complete() bool { return m.have.Covers(m.want) }

// HaveAll reports whether every piece of the torrent is verified (a full seed).
func (m *Manager) HaveAll() bool { return m.have.Full() }

// BytesLeft is the size of the wanted pieces not yet verified.
func (m *Manager) BytesLeft() int64 {
	return m.sizeOf(func(i int) bool { return m.isWanted(i) && !m.have.Has(i) })
}

// MissingBytes is the size of every piece not yet verified, wanted or not
// (what trackers are told is "left", so they never count a partial seed
// as a seed).
func (m *Manager) MissingBytes() int64 { return m.sizeOf(func(i int) bool { return !m.have.Has(i) }) }

// WantedBytes is the size of all wanted pieces.
func (m *Manager) WantedBytes() int64 { return m.sizeOf(m.isWanted) }

func (m *Manager) sizeOf(include func(int) bool) int64 {
	var n int64
	for i := 0; i < m.info.NumPieces(); i++ {
		if include(i) {
			n += m.info.PieceSize(i)
		}
	}
	return n
}

// isWanted: piece i belongs to a file the user wants.
func (m *Manager) isWanted(i int) bool { return m.want == nil || m.want.Has(i) }

// SetWanted changes which pieces are wanted (nil = all). Active pieces that
// are no longer wanted are abandoned — their partial data is dropped — and
// returned, so the caller can cancel its outstanding requests for them.
// Pieces already verified stay verified.
func (m *Manager) SetWanted(want *bitfield.Bitfield) (abandoned []int) {
	if want != nil && want.Len() != m.info.NumPieces() {
		return nil // programming error; keep the old selection
	}
	if want != nil {
		want = want.Clone()
	}
	m.want = want
	for _, idx := range m.activeIndexes() {
		if !m.isWanted(idx) {
			delete(m.active, idx)
			abandoned = append(abandoned, idx)
		}
	}
	return abandoned
}

// ActivePieces is the number of pieces being downloaded (for stats/tests).
func (m *Manager) ActivePieces() int { return len(m.active) }

// Availability returns how many connected peers have piece i.
func (m *Manager) Availability(i int) int { return m.avail[i] }

// wanted: we still need piece i — the user wants it, it isn't verified,
// and nobody is already finishing it.
func (m *Manager) wanted(i int) bool {
	return m.isWanted(i) && !m.have.Has(i) && !m.pending[i]
}

// Interesting reports whether a peer with peerHas has anything we want.
// The session sends interested / not-interested based on this.
func (m *Manager) Interesting(peerHas *bitfield.Bitfield) bool {
	for i := 0; i < m.info.NumPieces(); i++ {
		if peerHas.Has(i) && m.wanted(i) {
			return true
		}
	}
	return false
}

// --- availability ---------------------------------------------------------------

// PeerBitfield records a peer's full bitfield (on connect).
func (m *Manager) PeerBitfield(b *bitfield.Bitfield) { m.addAvail(b, 1) }

// PeerGoneBitfield removes a peer's pieces from availability (on disconnect).
func (m *Manager) PeerGoneBitfield(b *bitfield.Bitfield) { m.addAvail(b, -1) }

// PeerHave records a have message.
func (m *Manager) PeerHave(i int) {
	if i >= 0 && i < len(m.avail) {
		m.avail[i]++
	}
}

func (m *Manager) addAvail(b *bitfield.Bitfield, d int) {
	for i := range m.avail {
		if b.Has(i) {
			m.avail[i] = max(0, m.avail[i]+d)
		}
	}
}

// --- requesting -----------------------------------------------------------------

// NextRequests returns up to n blocks to request from peer p, which has the
// pieces in peerHas. Order of preference:
//  1. unrequested blocks of pieces already active (finish what we started),
//  2. blocks of a newly started piece (rarest first),
//  3. endgame: blocks already requested from *other* peers, when nothing
//     else is left to ask for.
func (m *Manager) NextRequests(p PeerKey, peerHas *bitfield.Bitfield, n int) []peer.Block {
	var out []peer.Block
	take := func(idx int, a *active, endgame bool) {
		for bi := range a.blocks {
			if len(out) >= n {
				return
			}
			b := &a.blocks[bi]
			if b.done || slices.Contains(b.owners, p) {
				continue
			}
			if len(b.owners) > 0 && !endgame {
				continue
			}
			b.owners = append(b.owners, p)
			out = append(out, m.block(idx, bi))
		}
	}

	// 1. Continue active pieces, in index order for determinism.
	for _, idx := range m.activeIndexes() {
		if len(out) >= n {
			return out
		}
		if peerHas.Has(idx) {
			take(idx, m.active[idx], false)
		}
	}
	// 2. Start new pieces.
	for len(out) < n {
		idx, ok := m.pickNew(peerHas)
		if !ok {
			break
		}
		take(idx, m.start(idx), false)
	}
	// 3. Endgame.
	if len(out) < n && !m.noEndgame && m.inEndgame() {
		for _, idx := range m.activeIndexes() {
			if len(out) >= n {
				break
			}
			if peerHas.Has(idx) {
				take(idx, m.active[idx], true)
			}
		}
	}
	return out
}

// SetEndgame turns endgame duplicate requests on (the default) or off.
// Under a download limit they only compete with each other for our own
// bandwidth: the session turns them off (DECISIONS D46).
func (m *Manager) SetEndgame(on bool) { m.noEndgame = !on }

// inEndgame: every missing piece is active or pending, and every block of
// every active piece has been requested.
func (m *Manager) inEndgame() bool {
	for i := 0; i < m.info.NumPieces(); i++ {
		if m.wanted(i) && m.active[i] == nil {
			return false
		}
	}
	for _, a := range m.active {
		for _, b := range a.blocks {
			if !b.done && len(b.owners) == 0 {
				return false
			}
		}
	}
	return true
}

// pickNew chooses a piece to start: random among candidates while we have
// fewer than randomFirst pieces, otherwise rarest-first with random
// tie-breaking. Respects the active-piece memory budget.
func (m *Manager) pickNew(peerHas *bitfield.Bitfield) (int, bool) {
	if len(m.active) >= m.maxActive && !m.abandonIdle() {
		return 0, false
	}
	random := m.have.Count()+len(m.pending) < m.randomFirst
	best, bestAvail, ties := -1, 0, 0
	for i := 0; i < m.info.NumPieces(); i++ {
		if !peerHas.Has(i) || !m.wanted(i) || m.active[i] != nil {
			continue
		}
		a := m.avail[i]
		if random {
			a = 0 // all candidates equal
		}
		switch {
		case best < 0 || a < bestAvail:
			best, bestAvail, ties = i, a, 1
		case a == bestAvail:
			// Reservoir sampling: each tied candidate wins with prob 1/ties.
			ties++
			if m.rng.IntN(ties) == 0 {
				best = i
			}
		}
	}
	return best, best >= 0
}

// abandonIdle frees one active slot held by a piece with no data and no
// outstanding requests (e.g. its peer disconnected). Reports success.
func (m *Manager) abandonIdle() bool {
	for _, idx := range m.activeIndexes() {
		a := m.active[idx]
		if a.received > 0 {
			continue
		}
		idle := true
		for _, b := range a.blocks {
			if len(b.owners) > 0 {
				idle = false
				break
			}
		}
		if idle {
			delete(m.active, idx)
			return true
		}
	}
	return false
}

func (m *Manager) start(idx int) *active {
	size := m.info.PieceSize(idx)
	nb := int((size + peer.BlockSize - 1) / peer.BlockSize)
	a := &active{
		data:         make([]byte, size),
		blocks:       make([]blockState, nb),
		contributors: map[PeerKey]bool{},
	}
	m.active[idx] = a
	return a
}

func (m *Manager) block(idx, bi int) peer.Block {
	begin := int64(bi) * peer.BlockSize
	length := min(peer.BlockSize, m.info.PieceSize(idx)-begin)
	return peer.Block{Index: uint32(idx), Begin: uint32(begin), Length: uint32(length)}
}

func (m *Manager) activeIndexes() []int {
	idx := make([]int, 0, len(m.active))
	for i := range m.active {
		idx = append(idx, i)
	}
	slices.Sort(idx)
	return idx
}

// --- receiving ------------------------------------------------------------------

// Received is the outcome of BlockReceived.
type Received struct {
	// Accepted is false for blocks we didn't want: unknown piece, bad
	// offset/length, or a duplicate already received (normal in endgame).
	Accepted bool
	// Cancel lists other peers that were also asked for this block (endgame);
	// the session should send them a cancel message.
	Cancel []PeerKey
	// Complete is set when this block finished its piece. Data is then the
	// whole piece (ownership passes to the caller) and the piece is pending:
	// the caller must hash-check it and report back with PieceVerified.
	Complete     bool
	Data         []byte
	Contributors []PeerKey // peers that supplied blocks of this piece
}

// BlockReceived records block data from peer p.
func (m *Manager) BlockReceived(p PeerKey, index, begin uint32, data []byte) Received {
	a := m.active[int(index)]
	if a == nil || begin%peer.BlockSize != 0 {
		return Received{}
	}
	bi := int(begin / peer.BlockSize)
	if bi >= len(a.blocks) {
		return Received{}
	}
	want := m.block(int(index), bi)
	if uint32(len(data)) != want.Length || a.blocks[bi].done {
		return Received{}
	}

	b := &a.blocks[bi]
	copy(a.data[begin:], data)
	b.done = true
	a.received++
	a.contributors[p] = true

	res := Received{Accepted: true}
	for _, o := range b.owners {
		if o != p {
			res.Cancel = append(res.Cancel, o)
		}
	}
	b.owners = nil

	if a.received == len(a.blocks) {
		delete(m.active, int(index))
		m.pending[int(index)] = true
		res.Complete = true
		res.Data = a.data
		for c := range a.contributors {
			res.Contributors = append(res.Contributors, c)
		}
	}
	return res
}

// PieceVerified reports the hash check result for a pending piece. On
// success the piece becomes "have" (the caller should already have written
// it to disk); on failure it returns to missing and will be downloaded again.
func (m *Manager) PieceVerified(index int, ok bool) {
	if !m.pending[index] {
		return
	}
	delete(m.pending, index)
	if ok {
		m.have.Set(index)
	}
}

// PeerRequestsDropped forgets every outstanding request to p (the peer
// choked us, timed out, or disconnected) so those blocks can be requested
// from someone else. It returns the dropped blocks.
func (m *Manager) PeerRequestsDropped(p PeerKey) []peer.Block {
	var dropped []peer.Block
	for _, idx := range m.activeIndexes() {
		a := m.active[idx]
		for bi := range a.blocks {
			b := &a.blocks[bi]
			if k := slices.Index(b.owners, p); k >= 0 {
				b.owners = slices.Delete(b.owners, k, k+1)
				dropped = append(dropped, m.block(idx, bi))
			}
		}
	}
	return dropped
}
