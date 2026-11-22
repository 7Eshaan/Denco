package dht

import (
	"context"
	"net/netip"
	"sync"

	"github.com/7Eshaan/Denco/internal/metainfo"
)

// Dual is the DHT as Denco runs it (D60): an IPv4 node and, when the
// machine can use IPv6, an IPv6 node. BEP 32 keeps the two networks apart —
// separate routing tables, "nodes" and "nodes6" — so each lookup runs on
// both and the peers they find are merged. It satisfies the PeerFinder
// interfaces of torrent and metadata.
type Dual struct {
	V4 *Server // never nil
	V6 *Server // nil without IPv6
}

// GetPeers looks hash up on every node (announcing us with port > 0) and
// returns the peers found by any, without duplicates. It fails only if
// every lookup failed.
func (d *Dual) GetPeers(ctx context.Context, hash metainfo.Hash, port uint16) ([]netip.AddrPort, error) {
	nodes := d.nodes()
	type result struct {
		peers []netip.AddrPort
		err   error
	}
	results := make([]result, len(nodes))
	var wg sync.WaitGroup
	for i, s := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := s.GetPeers(ctx, hash, port)
			results[i] = result{p, err}
		}()
	}
	wg.Wait()
	seen := map[netip.AddrPort]bool{}
	var out []netip.AddrPort
	var firstErr error
	failed := 0
	for _, r := range results {
		if r.err != nil {
			failed++
			if firstErr == nil {
				firstErr = r.err
			}
		}
		for _, p := range r.peers {
			if !seen[p] {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	if failed == len(results) && len(out) == 0 {
		return nil, firstErr
	}
	return out, nil
}

// AddNode passes a node hint to the node of its address family.
func (d *Dual) AddNode(addr netip.AddrPort) {
	if isFamily(addr, true) {
		if d.V6 != nil {
			d.V6.AddNode(addr)
		}
		return
	}
	d.V4.AddNode(addr)
}

// Port is the IPv4 node's UDP port: the one the BEP 5 PORT message
// carries. The IPv6 node listens on the same number when it could.
func (d *Dual) Port() uint16 { return d.V4.Port() }

// Close stops both nodes (each saves its state).
func (d *Dual) Close() error {
	var err error
	for _, s := range d.nodes() {
		if e := s.Close(); e != nil && err == nil {
			err = e
		}
	}
	return err
}

func (d *Dual) nodes() []*Server {
	if d.V6 == nil {
		return []*Server{d.V4}
	}
	return []*Server{d.V4, d.V6}
}
