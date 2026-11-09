package pieces

import (
	"fmt"
	"math/rand/v2"
	"net/netip"
	"os"
	"slices"
	"testing"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
)

// A swarm simulation through the real Manager: peers of mixed speeds, each
// with a request pipeline like the session's, answering in order. It
// measures what partly downloaded pieces cost in memory (DECISIONS D51).

type simPeer struct {
	key   PeerKey
	speed float64 // blocks per tick
	owed  float64
	queue []peer.Block
}

type simResult struct {
	ticks      int   // until every piece was received
	peakActive int64 // most bytes held by active pieces at once
}

// simulate downloads info from peers with the given speeds (blocks/tick),
// keeping pipeline requests outstanding per peer.
func simulate(info *metainfo.Info, cfg Config, speeds []float64, pipeline int) simResult {
	return simulateSeed(info, cfg, speeds, pipeline, 1)
}

// simulateSeed fixes the Manager's tie-breaking, so a run is repeatable.
func simulateSeed(info *metainfo.Info, cfg Config, speeds []float64, pipeline int, seed uint64) simResult {
	m := NewManager(info, nil, cfg)
	m.rng = rand.New(rand.NewPCG(seed, 2))
	all := full(info.NumPieces())
	peers := make([]*simPeer, len(speeds))
	for i, s := range speeds {
		peers[i] = &simPeer{key: netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 1, byte(i >> 8), byte(i)}), 6881), speed: s}
		m.PeerBitfield(all)
	}
	block := make([]byte, peer.BlockSize)
	var res simResult
	done := 0
	for res.ticks = 0; done < info.NumPieces() && res.ticks < 1_000_000; res.ticks++ {
		for _, p := range peers {
			if need := pipeline - len(p.queue); need > 0 {
				p.queue = append(p.queue, m.NextRequests(p.key, all, need)...)
			}
		}
		res.peakActive = max(res.peakActive, int64(len(m.active))*info.PieceLength)
		for _, p := range peers {
			p.owed += p.speed
			for p.owed >= 1 && len(p.queue) > 0 {
				p.owed--
				b := p.queue[0]
				p.queue = p.queue[1:]
				r := m.BlockReceived(p.key, b.Index, b.Begin, block[:b.Length])
				if r.Complete {
					m.PieceVerified(int(b.Index), true) // hashing is instant here
					done++
				}
			}
			if len(p.queue) == 0 {
				p.owed = 0 // an idle peer can't bank speed
			}
		}
	}
	return res
}

// mixedSpeeds: a few fast peers, many average, a long slow tail — what a
// public swarm looks like. Total ≈ the same for every seed.
func mixedSpeeds(n int, seed uint64) []float64 {
	r := rand.New(rand.NewPCG(seed, 1))
	s := make([]float64, n)
	for i := range s {
		switch x := r.Float64(); {
		case x < 0.1:
			s[i] = 8 + 8*r.Float64() // fast
		case x < 0.6:
			s[i] = 1 + 2*r.Float64()
		default:
			s[i] = 0.05 + 0.3*r.Float64() // slow
		}
	}
	return s
}

func simInfo(pieceLen int64, pieces int) *metainfo.Info {
	total := pieceLen * int64(pieces)
	return &metainfo.Info{Name: "sim", PieceLength: pieceLen, TotalLength: total,
		Pieces: make([]metainfo.Hash, pieces), Files: []metainfo.File{{Length: total}}}
}

// With mixed-speed peers, partly downloaded pieces must stay near what is
// actually in flight (40 peers × 32 blocks × 16 KiB = 20 MiB), and the
// download must not get slower. Before peers kept to their own pieces
// (D51) this peaked at 91 MiB (256 KiB pieces) and 128 MiB (1 MiB).
func TestSimulatedSwarmMemory(t *testing.T) {
	cases := []struct {
		pieceLen int64
		maxPeak  int64 // MiB
		maxTicks int
	}{
		{256 << 10, 40, 220},
		{1 << 20, 72, 220},
		{4 << 20, 128, 580}, // one piece per peer doesn't fit: the budget caps it
	}
	for _, c := range cases {
		res := simulate(simInfo(c.pieceLen, int(256<<20/c.pieceLen)), Config{}, mixedSpeeds(40, 1), 32)
		t.Logf("piece %s: %d ticks, peak %d MiB", sizeName(c.pieceLen), res.ticks, res.peakActive>>20)
		if res.peakActive>>20 > c.maxPeak || res.ticks > c.maxTicks {
			t.Errorf("piece %s: %d ticks (max %d), peak %d MiB (max %d)",
				sizeName(c.pieceLen), res.ticks, c.maxTicks, res.peakActive>>20, c.maxPeak)
		}
	}
}

// TestSimSpread prints results over 20 tie-breaking seeds (the numbers in
// D51): SIM_SPREAD=1 go test -v -run SimSpread ./internal/pieces
func TestSimSpread(t *testing.T) {
	if os.Getenv("SIM_SPREAD") == "" {
		t.Skip()
	}
	for _, pl := range []int64{256 << 10, 1 << 20, 4 << 20} {
		var ticks, peaks []int
		for seed := uint64(1); seed <= 20; seed++ {
			r := simulateSeed(simInfo(pl, int(256<<20/pl)), Config{}, mixedSpeeds(40, 1), 32, seed)
			ticks, peaks = append(ticks, r.ticks), append(peaks, int(r.peakActive>>20))
		}
		slices.Sort(ticks)
		slices.Sort(peaks)
		fmt.Printf("%s: ticks %d..%d (median %d), peak %d..%d MiB\n", sizeName(pl), ticks[0], ticks[19], ticks[10], peaks[0], peaks[19])
	}
}

func sizeName(n int64) string {
	if n >= 1<<20 {
		return fmt.Sprintf("%d MiB", n>>20)
	}
	return fmt.Sprintf("%d KiB", n>>10)
}
