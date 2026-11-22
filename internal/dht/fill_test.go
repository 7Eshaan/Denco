package dht

import (
	"encoding/json"
	"net"
	"net/netip"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// spyNode is a DHT node on loopback that answers find_node with no nodes
// and records every target it is asked about. drop, if set, decides to
// ignore a query (target, and how often that target's bucket was asked).
type spyNode struct {
	conn *net.UDPConn
	id   ID
	self ID // the server under test, to bucket the targets

	mu      sync.Mutex
	asked   map[int]int // bucket (relative to self) → find_node queries
	dropped map[int]int
	drop    func(bucket, seen int) bool
}

func newSpy(t *testing.T, self ID, id ID) *spyNode {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	sp := &spyNode{conn: c, id: id, self: self, asked: map[int]int{}, dropped: map[int]int{}}
	t.Cleanup(func() { c.Close() })
	go sp.serve()
	return sp
}

func (sp *spyNode) addr() netip.AddrPort { return sp.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

func (sp *spyNode) serve() {
	buf := make([]byte, 4096)
	for {
		n, from, err := sp.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		m, err := decode(buf[:n])
		if err != nil || m.kind != "q" {
			continue
		}
		if m.method == "find_node" {
			target, _ := m.hash("target")
			b := commonPrefix(sp.self, target)
			if b == 160 {
				b = -1 // our own ID: the join lookup, not a bucket fill
			}
			sp.mu.Lock()
			seen := sp.asked[b]
			sp.asked[b]++
			drop := b >= 0 && sp.drop != nil && sp.drop(b, seen)
			if drop {
				sp.dropped[b]++
			}
			sp.mu.Unlock()
			if drop {
				continue
			}
		}
		sp.conn.WriteToUDPAddrPort(encodeResponse(m.tid, map[string]any{"id": string(sp.id[:]), "nodes": ""}), from)
	}
}

func (sp *spyNode) counts() map[int]int {
	sp.mu.Lock()
	defer sp.mu.Unlock()
	out := map[int]int{}
	for b, n := range sp.asked {
		out[b] = n
	}
	return out
}

// A new node fills its table with one lookup aimed *inside* each sparse
// bucket, shallowest first (D42): random targets would land in bucket 0
// half the time and almost never in the deeper ones. Each bucket is tried
// once per refresh period, except that a lookup that reached nobody
// doesn't count. Here the new node knows only the spy, so every lookup
// target shows up there.
func TestBucketFillTargetsEachSparseBucket(t *testing.T) {
	self := NewID()
	spyID := idInBucket(self, 7) // in bucket 7: buckets 0…7 are all sparse
	sp := newSpy(t, self, spyID)
	sp.drop = func(b, seen int) bool { return b == 3 && seen == 0 } // bucket 3's first lookup reaches nobody

	// The new node's saved state: its ID, and the spy as its only node.
	state := filepath.Join(t.TempDir(), "dht.json")
	data, _ := json.Marshal(savedState{ID: self.String(), Nodes: []savedNode{{ID: spyID.String(), Addr: sp.addr().String()}}})
	if err := writeFile(state, string(data)); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, Config{StateFile: state, MaintainEvery: time.Hour}) // no rounds but the first
	if s.ID() != self {
		t.Fatal("state file ID not used")
	}

	// The startup round: one lookup per bucket 0…7.
	waitFor(t, func() bool {
		c := sp.counts()
		for b := 0; b < 8; b++ {
			if c[b] == 0 {
				return false
			}
		}
		return true
	})
	// Bucket 3's lookup timed out, so bucket 3 may be tried again.
	waitFor(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.filled[3].IsZero()
	})
	for b, n := range sp.counts() {
		if b >= 0 && n != 1 {
			t.Errorf("bucket %d was asked %d times in the first round, want 1", b, n)
		}
	}

	// The next round retries only bucket 3: the others were tried already.
	s.fillBuckets(growLookups)
	c := sp.counts()
	for b := 0; b < 8; b++ {
		want := 1
		if b == 3 {
			want = 2
		}
		if c[b] != want {
			t.Errorf("after the second round bucket %d was asked %d times, want %d (all: %v)", b, c[b], want, c)
		}
	}
	// And then nothing more until the refresh period has passed.
	s.fillBuckets(growLookups)
	for b, n := range sp.counts() {
		if b >= 0 && n != c[b] {
			t.Errorf("a third round asked bucket %d again", b)
		}
	}
}
