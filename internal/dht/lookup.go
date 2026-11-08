package dht

import (
	"context"
	"net/netip"
	"sort"
	"sync"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

const (
	alpha          = 4    // queries in flight per lookup
	maxLookupAsks  = 200  // queries per lookup, a hard stop for hostile networks
	maxLookupCands = 1000 // nodes a lookup keeps track of
)

// tokenNode is a node that answered get_peers, with the token it gave us.
type tokenNode struct {
	NodeInfo
	token string
}

type lookupResult struct {
	peers   []netip.AddrPort
	closest []tokenNode // the K closest nodes that answered, closest first
}

type candState int

const (
	fresh candState = iota
	asked
	answered
	failed
)

type cand struct {
	NodeInfo
	state  candState
	router bool // bootstrap router: asked for nodes, never ranked
	token  string
}

// lookup walks towards target (Kademlia's iterative lookup): ask the
// closest nodes we know, learn closer ones from their answers, and repeat
// until the K closest nodes we've heard of have all answered. With
// getPeers it asks get_peers (collecting peers and tokens), else find_node.
// If the table is nearly empty the routers are asked too.
func (s *Server) lookup(ctx context.Context, target ID, getPeers bool) lookupResult {
	cands := map[netip.AddrPort]*cand{}
	add := func(n NodeInfo, router bool) {
		if (n.ID == s.id && !router) || cands[n.Addr] != nil || len(cands) >= maxLookupCands {
			return
		}
		cands[n.Addr] = &cand{NodeInfo: n, router: router}
	}
	for _, n := range s.table.closest(target, K) {
		add(n, false)
	}
	if len(cands) < K {
		for _, r := range s.routerList() {
			add(NodeInfo{Addr: r}, true)
		}
	}

	method, key := "find_node", "target"
	if getPeers {
		method, key = "get_peers", "info_hash"
	}
	type result struct {
		c   *cand
		m   msg
		err error
	}
	results := make(chan result)
	inflight, asks := 0, 0
	peers := map[netip.AddrPort]bool{}

	ranked := func() []*cand {
		var out []*cand
		for _, c := range cands {
			if !c.router && c.state != failed {
				out = append(out, c)
			}
		}
		sort.Slice(out, func(i, j int) bool { return closer(target, out[i].ID, out[j].ID) })
		return out
	}

	for ctx.Err() == nil {
		// Next to ask: unasked routers, then unasked nodes among the K closest.
		var next []*cand
		for _, c := range cands {
			if c.router && c.state == fresh {
				next = append(next, c)
			}
		}
		top := ranked()
		top = top[:min(K, len(top))]
		for _, c := range top {
			if c.state == fresh {
				next = append(next, c)
			}
		}
		for _, c := range next {
			if inflight >= alpha || asks >= maxLookupAsks {
				break
			}
			c.state = asked
			inflight++
			asks++
			go func() {
				m, err := s.query(ctx, c.Addr, method, map[string]any{key: string(target[:])})
				results <- result{c, m, err}
			}()
		}
		if inflight == 0 {
			break // the K closest have all answered (or nothing is left to ask)
		}
		r := <-results
		inflight--
		if r.err != nil {
			r.c.state = failed
			continue
		}
		r.c.state = answered
		r.c.ID = r.m.id
		r.c.token, _ = bencode.GetString(r.m.body, "token")
		nodes, _ := bencode.GetString(r.m.body, "nodes")
		for _, n := range decodeNodes(nodes) {
			add(n, false)
		}
		if getPeers {
			for _, p := range decodeValues(r.m.body) {
				peers[p] = true
			}
		}
	}
	for ; inflight > 0; inflight-- { // cancelled: let the senders finish
		<-results
	}

	var res lookupResult
	for p := range peers {
		res.peers = append(res.peers, p)
	}
	for _, c := range ranked() {
		if len(res.closest) == K {
			break
		}
		if c.state == answered {
			res.closest = append(res.closest, tokenNode{c.NodeInfo, c.token})
		}
	}
	return res
}

// GetPeers finds peers for a torrent. With port > 0 it also announces us
// (listening on that TCP port) to the closest nodes that gave us a token,
// so others find us. It returns what it found even if ctx ends early.
func (s *Server) GetPeers(ctx context.Context, hash metainfo.Hash, port uint16) ([]netip.AddrPort, error) {
	if s.ctx.Err() != nil {
		return nil, ErrClosed
	}
	res := s.lookup(ctx, ID(hash), true)
	if port > 0 {
		var wg sync.WaitGroup
		for _, n := range res.closest {
			if n.token == "" {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.query(ctx, n.Addr, "announce_peer", map[string]any{
					"info_hash": string(hash[:]), "port": int(port), "token": n.token, "implied_port": 0,
				})
			}()
		}
		wg.Wait()
	}
	if len(res.peers) == 0 && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return res.peers, nil
}
