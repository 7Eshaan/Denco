package dht

import (
	"net/netip"
	"sort"
	"sync"
	"time"
)

// Routing table (BEP 5, simplified Kademlia; DECISIONS D42).
//
// Nodes are kept in 160 buckets by how many leading bits their ID shares
// with ours, at most K per bucket. A node is "good" if it answered us in
// the last 15 minutes, "questionable" if it hasn't, and "bad" after
// failing badAfter queries in a row. A full bucket takes a new node only by
// replacing a bad one, or a questionable one that has already failed a
// query (the server pings questionable nodes regularly), so long-lived
// nodes — the most reliable kind — are kept.

const (
	K              = 8                // bucket size, and how many closest nodes lookups use
	questionableIn = 15 * time.Minute // a node not heard from for this long is questionable
	badAfter       = 2                // failed queries in a row that make a node bad
	forgetAfter    = 5                // …and that remove it
)

type node struct {
	NodeInfo
	lastSeen time.Time // last response (or verified query) from it
	failures int       // queries in a row that went unanswered
}

func (n *node) bad() bool { return n.failures >= badAfter }

type table struct {
	self ID

	mu      sync.Mutex
	buckets [160][]*node
	byAddr  map[netip.AddrPort]*node
}

func newTable(self ID) *table {
	return &table{self: self, byAddr: map[netip.AddrPort]*node{}}
}

// bucket returns the bucket index for id, or -1 for our own ID.
func (t *table) bucket(id ID) int {
	cp := commonPrefix(t.self, id)
	if cp == 160 {
		return -1
	}
	return cp
}

// seen records that n answered us (or that we verified it): it is added,
// refreshed or, if its bucket is full, may replace a failing node.
func (t *table) seen(n NodeInfo, now time.Time) {
	b := t.bucket(n.ID)
	if b < 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if old := t.byAddr[n.Addr]; old != nil {
		if old.ID == n.ID {
			old.lastSeen, old.failures = now, 0
			return
		}
		t.removeLocked(old) // that address now claims another ID
	}
	for _, o := range t.buckets[b] {
		if o.ID == n.ID { // same ID, new address: keep the one we know works
			return
		}
	}
	nn := &node{NodeInfo: n, lastSeen: now}
	if len(t.buckets[b]) < K {
		t.buckets[b] = append(t.buckets[b], nn)
		t.byAddr[n.Addr] = nn
		return
	}
	// Full: replace the worst node, if any is bad or failing-and-stale.
	var worst *node
	for _, o := range t.buckets[b] {
		replaceable := o.bad() || (o.failures > 0 && now.Sub(o.lastSeen) > questionableIn)
		if replaceable && (worst == nil || o.failures > worst.failures) {
			worst = o
		}
	}
	if worst != nil {
		t.removeLocked(worst)
		t.buckets[b] = append(t.buckets[b], nn)
		t.byAddr[n.Addr] = nn
	}
}

// failed records an unanswered query to addr.
func (t *table) failed(addr netip.AddrPort) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n := t.byAddr[addr]; n != nil {
		n.failures++
		if n.failures >= forgetAfter {
			t.removeLocked(n)
		}
	}
}

func (t *table) removeLocked(n *node) {
	delete(t.byAddr, n.Addr)
	b := t.bucket(n.ID)
	for i, o := range t.buckets[b] {
		if o == n {
			t.buckets[b] = append(t.buckets[b][:i], t.buckets[b][i+1:]...)
			return
		}
	}
}

// has reports whether a node with this ID or address is in the table.
func (t *table) has(n NodeInfo) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byAddr[n.Addr] != nil {
		return true
	}
	if b := t.bucket(n.ID); b >= 0 {
		for _, o := range t.buckets[b] {
			if o.ID == n.ID {
				return true
			}
		}
	}
	return false
}

// room reports whether a node with this ID could be added without
// replacing anything (used before spending a ping on an unknown node).
func (t *table) room(id ID) bool {
	b := t.bucket(id)
	if b < 0 {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.buckets[b]) < K {
		return true
	}
	for _, o := range t.buckets[b] {
		if o.bad() {
			return true
		}
	}
	return false
}

// closest returns up to n nodes closest to target, skipping bad ones.
func (t *table) closest(target ID, n int) []NodeInfo {
	t.mu.Lock()
	all := make([]NodeInfo, 0, len(t.byAddr))
	for _, nd := range t.byAddr {
		if !nd.bad() {
			all = append(all, nd.NodeInfo)
		}
	}
	t.mu.Unlock()
	sort.Slice(all, func(i, j int) bool { return closer(target, all[i].ID, all[j].ID) })
	return all[:min(n, len(all))]
}

// questionable returns up to n nodes not heard from for questionableIn,
// least recently seen first (the server pings them).
func (t *table) questionable(now time.Time, n int) []NodeInfo {
	t.mu.Lock()
	var qs []*node
	for _, nd := range t.byAddr {
		if now.Sub(nd.lastSeen) > questionableIn {
			qs = append(qs, nd)
		}
	}
	sort.Slice(qs, func(i, j int) bool { return qs[i].lastSeen.Before(qs[j].lastSeen) })
	out := make([]NodeInfo, 0, min(n, len(qs)))
	for _, nd := range qs[:min(n, len(qs))] {
		out = append(out, nd.NodeInfo)
	}
	t.mu.Unlock()
	return out
}

// sparse returns the buckets, from 0 to the deepest one holding any node,
// that have room — the ones targeted lookups should fill. Buckets past the
// deepest are left alone: the network is too small to have nodes there.
func (t *table) sparse() []int {
	t.mu.Lock()
	defer t.mu.Unlock()
	deepest := 0
	for b := range t.buckets {
		if len(t.buckets[b]) > 0 {
			deepest = b
		}
	}
	var out []int
	for b := 0; b <= deepest; b++ {
		if len(t.buckets[b]) < K {
			out = append(out, b)
		}
	}
	return out
}

// idInBucket returns a random ID that falls in bucket b: it shares exactly
// b leading bits with self.
func idInBucket(self ID, b int) ID {
	id := NewID()
	for i := 0; i < b; i++ {
		mask := byte(0x80 >> (i % 8))
		id[i/8] = id[i/8]&^mask | self[i/8]&mask
	}
	mask := byte(0x80 >> (b % 8))
	id[b/8] = id[b/8]&^mask | ^self[b/8]&mask
	return id
}

// counts returns the number of nodes and how many are good.
func (t *table) counts(now time.Time) (all, good int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, nd := range t.byAddr {
		all++
		if !nd.bad() && now.Sub(nd.lastSeen) <= questionableIn {
			good++
		}
	}
	return all, good
}

// load adds nodes from a saved state as questionable (not yet verified),
// so the server pings them before trusting them.
func (t *table) load(nodes []NodeInfo) {
	for _, n := range nodes {
		t.seen(n, time.Time{})
	}
}

// --- peer store -------------------------------------------------------------------------

// Peers that announced to us (announce_peer), per info hash. Entries expire
// after peerTTL; sizes are capped so a flood of announces can't eat memory.
const (
	peerTTL         = 30 * time.Minute
	maxStoredHashes = 5000
	maxPeersPerHash = 500
	maxValues       = 50 // peers returned per get_peers answer (fits one datagram)
)

type peerStore struct {
	mu sync.Mutex
	m  map[ID]map[netip.AddrPort]time.Time
}

func newPeerStore() *peerStore { return &peerStore{m: map[ID]map[netip.AddrPort]time.Time{}} }

func (s *peerStore) add(hash ID, a netip.AddrPort, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	peers := s.m[hash]
	if peers == nil {
		if len(s.m) >= maxStoredHashes {
			return
		}
		peers = map[netip.AddrPort]time.Time{}
		s.m[hash] = peers
	}
	if _, ok := peers[a]; !ok && len(peers) >= maxPeersPerHash {
		return
	}
	peers[a] = now
}

// get returns up to maxValues live peers for hash (map order: random-ish).
func (s *peerStore) get(hash ID, now time.Time) []netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []netip.AddrPort
	for a, at := range s.m[hash] {
		if now.Sub(at) < peerTTL {
			out = append(out, a)
			if len(out) == maxValues {
				break
			}
		}
	}
	return out
}

func (s *peerStore) expire(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for h, peers := range s.m {
		for a, at := range peers {
			if now.Sub(at) >= peerTTL {
				delete(peers, a)
			}
		}
		if len(peers) == 0 {
			delete(s.m, h)
		}
	}
}

func (s *peerStore) counts() (hashes, peers int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.m {
		peers += len(p)
	}
	return len(s.m), peers
}
