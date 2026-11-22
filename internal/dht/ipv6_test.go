package dht

import (
	"context"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
)

// needIPv6 skips the test where the machine has no IPv6 loopback (some CI
// runners and containers).
func needIPv6(t *testing.T) {
	t.Helper()
	c, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Skipf("no IPv6 loopback here: %v", err)
	}
	c.Close()
}

func testServer6(t *testing.T, cfg Config) *Server {
	t.Helper()
	cfg.IPv6 = true
	if cfg.Addr == "" {
		cfg.Addr = "[::1]:0"
	}
	return testServer(t, cfg)
}

func network6(t *testing.T, n int) []*Server {
	t.Helper()
	boot := testServer6(t, Config{})
	nodes := []*Server{boot}
	for i := 1; i < n; i++ {
		nodes = append(nodes, testServer6(t, Config{Bootstrap: []string{boot.Addr().String()}}))
	}
	waitFor(t, func() bool {
		for _, s := range nodes[1:] {
			if _, good := s.table.counts(time.Now()); good < K {
				return false
			}
		}
		return true
	})
	return nodes
}

// An IPv6 DHT (BEP 32) works on its own: nodes find each other through
// "nodes6", and a peer announced over IPv6 is found as an IPv6 address.
func TestIPv6AnnounceAndFindPeers(t *testing.T) {
	needIPv6(t)
	nodes := network6(t, 12)
	for _, s := range nodes {
		if !s.IPv6() {
			t.Fatal("not an IPv6 node")
		}
		for _, n := range s.table.closest(NewID(), 100) {
			if !n.Addr.Addr().Is6() {
				t.Fatalf("IPv4 node %v in an IPv6 table", n.Addr)
			}
		}
	}
	var hash metainfo.Hash
	copy(hash[:], NewID().String())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	nodes[3].GetPeers(ctx, hash, 4242)
	peers, err := nodes[9].GetPeers(ctx, hash, 0)
	if err != nil || !slices.Contains(peers, netip.MustParseAddrPort("[::1]:4242")) {
		t.Fatalf("lookup found %v, %v; want [::1]:4242", peers, err)
	}
}

// An IPv6 node answers with "nodes6" only, and ignores hints of IPv4 nodes.
func TestIPv6NodeSpeaksNodes6(t *testing.T) {
	needIPv6(t)
	s := testServer6(t, Config{})
	other := testServer6(t, Config{})
	s.AddNode(other.Addr())
	s.AddNode(netip.MustParseAddrPort("127.0.0.1:6881")) // wrong family: dropped
	waitFor(t, func() bool { return s.table.has(NodeInfo{ID: other.ID(), Addr: other.Addr()}) })
	if all, _ := s.table.counts(time.Now()); all != 1 {
		t.Fatalf("table has %d nodes, want just the IPv6 one", all)
	}

	c, err := net.ListenUDP("udp6", &net.UDPAddr{IP: net.IPv6loopback})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	r := &rawClient{t: t, c: c, id: NewID(), to: s.Addr()}
	m := r.ask("find_node", map[string]any{"target": string(other.ID().String()[:20])})
	if _, ok := m.body["nodes"]; ok {
		t.Error("an IPv6 node sent an IPv4 node list")
	}
	nodes6, _ := m.body["nodes6"].(string)
	if got := decodeNodes(nodes6, true); len(got) != 1 || got[0].Addr != other.Addr() {
		t.Fatalf("nodes6 = %v, want %v", got, other.Addr())
	}
}

// Dual merges what the IPv4 and IPv6 networks know: a peer announced only
// on one of them is found through Dual, and node hints go to the right one.
func TestDualSearchesBothFamilies(t *testing.T) {
	needIPv6(t)
	v4, v6 := network(t, 10), network6(t, 10)
	var hash metainfo.Hash
	copy(hash[:], NewID().String())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	v4[2].GetPeers(ctx, hash, 1111) // announced over IPv4 only
	v6[2].GetPeers(ctx, hash, 2222) // and another peer over IPv6 only

	d := &Dual{V4: v4[7], V6: v6[7]}
	peers, err := d.GetPeers(ctx, hash, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"127.0.0.1:1111", "[::1]:2222"} {
		if !slices.Contains(peers, netip.MustParseAddrPort(want)) {
			t.Errorf("Dual found %v; missing %s", peers, want)
		}
	}
	if d.Port() != v4[7].Port() {
		t.Error("Port must be the IPv4 node's")
	}

	// Without an IPv6 node Dual is just the IPv4 one.
	only4 := &Dual{V4: v4[8]}
	if peers, _ := only4.GetPeers(ctx, hash, 0); slices.Contains(peers, netip.MustParseAddrPort("[::1]:2222")) {
		t.Error("an IPv4-only Dual found an IPv6 peer")
	}
	only4.AddNode(netip.MustParseAddrPort("[::1]:1")) // no IPv6 node: ignored, no panic
}
