// Package dht is a BitTorrent DHT node (BEP 5, "Mainline DHT"): a
// distributed table, spread over millions of clients, that maps info hashes
// to peers. It lets Denco find peers without any tracker — the only way a
// magnet link with just an info hash can start.
//
// Files:
//
//	server.go – Server: the UDP socket, sending queries, answering others, upkeep
//	lookup.go – iterative lookups (find_node, get_peers) and announce_peer
//	table.go  – routing table (k-buckets) and the store of announced peers
//	krpc.go   – message encoding, node IDs, compact node info
//	token.go  – announce tokens
//	state.go  – saving the node ID and good nodes between runs
//
// Concurrency: one reader goroutine owns the socket's read side; it answers
// queries itself (they are cheap) and hands responses to the waiting
// query by transaction ID. Lookups and the maintenance goroutine send
// queries concurrently; the table, token and peer store have their own
// mutexes. Close stops everything and waits. IPv4 only (IPv6, BEP 32, is
// not supported yet). See DECISIONS D42.
package dht

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/compact"
)

// DefaultBootstrap are well-known routers used to join the DHT when no
// saved nodes are known.
var DefaultBootstrap = []string{
	"router.bittorrent.com:6881",
	"dht.transmissionbt.com:6881",
	"router.utorrent.com:6881",
	"dht.libtorrent.org:25401",
}

// Config configures a Server. Zero values use defaults.
type Config struct {
	Addr      string   // UDP address to listen on, e.g. ":6881"
	Bootstrap []string // host:port routers; nil = none (tests); see DefaultBootstrap
	StateFile string   // where the node ID and good nodes are kept ("" = not saved)
	Logger    *slog.Logger

	QueryTimeout  time.Duration // one query; default 3s
	MaintainEvery time.Duration // ping questionable nodes, re-bootstrap if needed; default 1m
	RefreshEvery  time.Duration // random lookups to keep the table fresh; default 15m
	MaxQPS        int           // incoming queries per second we answer; default 500
}

// Server is one DHT node.
type Server struct {
	cfg   Config
	log   *slog.Logger
	conn  *net.UDPConn
	id    ID
	table *table
	tok   *tokens
	store *peerStore

	mu      sync.Mutex
	pending map[string]*pendingQuery // by transaction ID
	nextTID uint16
	routers map[netip.AddrPort]bool // resolved bootstrap addresses: never in the table

	verifying sync.Map      // addresses being pinged before joining the table
	verifySem chan struct{} // bounds those pings

	qpsSecond atomic.Int64 // unix second of the current incoming-query window
	qpsCount  atomic.Int64

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type pendingQuery struct {
	addr netip.AddrPort
	ch   chan msg
}

// ErrClosed is returned by queries after Close.
var ErrClosed = errors.New("dht: closed")

var errTimeout = errors.New("dht: query timed out")

// Open starts a node: it binds the UDP port, loads saved state and starts
// joining the DHT in the background.
func Open(cfg Config) (*Server, error) {
	if cfg.QueryTimeout <= 0 {
		cfg.QueryTimeout = 3 * time.Second
	}
	if cfg.MaintainEvery <= 0 {
		cfg.MaintainEvery = time.Minute
	}
	if cfg.RefreshEvery <= 0 {
		cfg.RefreshEvery = 15 * time.Minute
	}
	if cfg.MaxQPS <= 0 {
		cfg.MaxQPS = 500
	}
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ua, err := net.ResolveUDPAddr("udp4", cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("dht: %w", err)
	}
	conn, err := net.ListenUDP("udp4", ua)
	if err != nil {
		return nil, fmt.Errorf("dht: %w", err)
	}
	st := loadState(cfg.StateFile, log)
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg: cfg, log: log.With("component", "dht"), conn: conn, id: st.id,
		table: newTable(st.id), tok: newTokens(time.Now), store: newPeerStore(),
		pending: map[string]*pendingQuery{}, routers: map[netip.AddrPort]bool{},
		verifySem: make(chan struct{}, 16),
		ctx:       ctx, cancel: cancel,
	}
	s.table.load(st.nodes)
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.readLoop() }()
	go func() { defer s.wg.Done(); s.maintain() }()
	return s, nil
}

// Close stops the node and saves its state.
func (s *Server) Close() error {
	s.mu.Lock() // verify adds to wg under mu, so none can start after this
	s.cancel()
	s.mu.Unlock()
	err := s.conn.Close()
	s.wg.Wait()
	s.saveState()
	return err
}

// ID is this node's ID.
func (s *Server) ID() ID { return s.id }

// Port is the UDP port the node listens on.
func (s *Server) Port() uint16 { return uint16(s.conn.LocalAddr().(*net.UDPAddr).Port) }

// Addr is the node's local address (useful in tests).
func (s *Server) Addr() netip.AddrPort {
	return s.conn.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Stats describes the node.
type Stats struct {
	Nodes, GoodNodes int // in the routing table
	Torrents, Peers  int // announced to us and stored
}

// Stats returns a snapshot.
func (s *Server) Stats() Stats {
	var st Stats
	st.Nodes, st.GoodNodes = s.table.counts(time.Now())
	st.Torrents, st.Peers = s.store.counts()
	return st
}

// AddNode is a hint that a DHT node may run at addr (a peer's PORT
// message). It is pinged, and joins the table only if it answers.
func (s *Server) AddNode(addr netip.AddrPort) {
	if compact.Usable(addr) && addr.Addr().Unmap().Is4() {
		s.verify(NodeInfo{Addr: addr}, true)
	}
}

// --- sending queries --------------------------------------------------------------

// query sends one query and waits for the answer. A response updates the
// routing table; a timeout counts against the node.
func (s *Server) query(ctx context.Context, addr netip.AddrPort, method string, args map[string]any) (msg, error) {
	args["id"] = string(s.id[:])
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		return msg{}, ErrClosed
	}
	var tid string
	for { // two bytes, as most clients use; skip any still in flight
		s.nextTID++
		tid = string(binary.BigEndian.AppendUint16(nil, s.nextTID))
		if s.pending[tid] == nil {
			break
		}
	}
	p := &pendingQuery{addr: addr, ch: make(chan msg, 1)}
	s.pending[tid] = p
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, tid)
		s.mu.Unlock()
	}()

	if _, err := s.conn.WriteToUDPAddrPort(encodeQuery(tid, method, args), addr); err != nil {
		return msg{}, fmt.Errorf("dht: sending to %s: %w", addr, err)
	}
	t := time.NewTimer(s.cfg.QueryTimeout)
	defer t.Stop()
	select {
	case m := <-p.ch:
		if m.kind == "e" {
			return m, fmt.Errorf("dht: %s answered error %d: %s", addr, m.code, m.text)
		}
		if !s.isRouter(addr) {
			s.table.seen(NodeInfo{ID: m.id, Addr: addr}, time.Now())
		}
		return m, nil
	case <-t.C:
		s.table.failed(addr)
		return msg{}, errTimeout
	case <-ctx.Done():
		return msg{}, ctx.Err()
	case <-s.ctx.Done():
		return msg{}, ErrClosed
	}
}

func (s *Server) ping(ctx context.Context, addr netip.AddrPort) (msg, error) {
	return s.query(ctx, addr, "ping", map[string]any{})
}

// verify pings a node we heard of (it queried us, or a peer named it) and
// so lets it into the table if it answers. Bounded and deduplicated; with
// force, also when its bucket looks full (a PORT hint has no ID yet).
func (s *Server) verify(n NodeInfo, force bool) {
	if s.isRouter(n.Addr) || (!force && (s.table.has(n) || !s.table.room(n.ID))) {
		return
	}
	if _, busy := s.verifying.LoadOrStore(n.Addr, true); busy {
		return
	}
	select {
	case s.verifySem <- struct{}{}:
	default:
		s.verifying.Delete(n.Addr)
		return // enough pings in flight; it'll come round again
	}
	s.mu.Lock()
	if s.ctx.Err() != nil {
		s.mu.Unlock()
		<-s.verifySem
		s.verifying.Delete(n.Addr)
		return
	}
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		defer func() { <-s.verifySem; s.verifying.Delete(n.Addr) }()
		s.ping(s.ctx, n.Addr) // query() records the answer in the table
	}()
}

func (s *Server) isRouter(a netip.AddrPort) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routers[a]
}

// --- receiving ----------------------------------------------------------------------

func (s *Server) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, from, err := s.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if s.ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return
			}
			continue // e.g. ICMP errors surfacing on some OSes
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		m, err := decode(buf[:n])
		if err != nil {
			continue // garbage: ignore silently, as BEP 5 clients do
		}
		switch m.kind {
		case "q":
			if s.allowQuery() {
				s.answer(m, from)
			}
		default:
			s.mu.Lock()
			p := s.pending[m.tid]
			s.mu.Unlock()
			if p != nil && p.addr == from { // the right tid from the right node
				select {
				case p.ch <- m:
				default:
				}
			}
		}
	}
}

// allowQuery is a per-second cap on queries we answer.
func (s *Server) allowQuery() bool {
	now := time.Now().Unix()
	if s.qpsSecond.Swap(now) != now {
		s.qpsCount.Store(0)
	}
	return s.qpsCount.Add(1) <= int64(s.cfg.MaxQPS)
}

// answer handles a query from another node.
func (s *Server) answer(m msg, from netip.AddrPort) {
	reply := func(r map[string]any) {
		r["id"] = string(s.id[:])
		s.conn.WriteToUDPAddrPort(encodeResponse(m.tid, r), from)
	}
	fail := func(code int, text string) {
		s.conn.WriteToUDPAddrPort(encodeError(m.tid, code, text), from)
	}
	switch m.method {
	case "ping":
		reply(map[string]any{})
	case "find_node":
		target, ok := m.hash("target")
		if !ok {
			fail(errProtocol, "bad target")
			return
		}
		reply(map[string]any{"nodes": encodeNodes(s.table.closest(target, K))})
	case "get_peers":
		hash, ok := m.hash("info_hash")
		if !ok {
			fail(errProtocol, "bad info_hash")
			return
		}
		r := map[string]any{"token": s.tok.issue(from.Addr())}
		if peers := s.store.get(hash, time.Now()); len(peers) > 0 {
			values := make([]any, len(peers))
			for i, p := range peers {
				values[i] = string(compact.Append(nil, p))
			}
			r["values"] = values
		}
		r["nodes"] = encodeNodes(s.table.closest(hash, K))
		reply(r)
	case "announce_peer":
		hash, ok := m.hash("info_hash")
		tok, _ := bencode.GetString(m.body, "token")
		port, _ := bencode.GetInt(m.body, "port")
		implied, _ := bencode.GetInt(m.body, "implied_port")
		if implied == 1 {
			port = int64(from.Port())
		}
		switch {
		case !ok || port <= 0 || port > 65535:
			fail(errProtocol, "bad announce")
			return
		case !s.tok.valid(tok, from.Addr()):
			fail(errProtocol, "bad token")
			return
		}
		s.store.add(hash, netip.AddrPortFrom(from.Addr(), uint16(port)), time.Now())
		reply(map[string]any{})
	default:
		fail(errMethod, "method unknown")
		return
	}
	s.verify(NodeInfo{ID: m.id, Addr: from}, false)
}

// --- upkeep -------------------------------------------------------------------------

// maintain joins the DHT, then keeps the table healthy: questionable nodes
// are pinged (so failing ones can be replaced), the table is re-filled from
// the routers when it runs low, and random lookups refresh it.
func (s *Server) maintain() {
	s.bootstrap()
	tick := time.NewTicker(s.cfg.MaintainEvery)
	defer tick.Stop()
	lastRefresh := time.Now()
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-tick.C:
			var wg sync.WaitGroup
			for _, n := range s.table.questionable(now, 16) {
				wg.Add(1)
				go func() { defer wg.Done(); s.ping(s.ctx, n.Addr) }()
			}
			wg.Wait()
			s.store.expire(now)
			if _, good := s.table.counts(now); good < K {
				s.bootstrap()
			} else if now.Sub(lastRefresh) >= s.cfg.RefreshEvery {
				lastRefresh = now
				s.lookup(s.ctx, NewID(), false)
				s.saveState()
			}
		}
	}
}

// bootstrap resolves the routers and looks up our own ID, which fills the
// buckets near us (and tells nodes near us that we exist).
func (s *Server) bootstrap() {
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	defer cancel()
	for _, hp := range s.cfg.Bootstrap {
		host, port, err := net.SplitHostPort(hp)
		if err != nil {
			continue
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip4", host)
		if err != nil {
			s.log.Debug("bootstrap router unresolved", "router", hp, "err", err)
			continue
		}
		var p uint16
		fmt.Sscan(port, &p)
		s.mu.Lock()
		for _, ip := range ips {
			s.routers[netip.AddrPortFrom(ip.Unmap(), p)] = true
		}
		s.mu.Unlock()
	}
	s.lookup(s.ctx, s.id, false)
	all, good := s.table.counts(time.Now())
	s.log.Debug("bootstrap done", "nodes", all, "good", good)
}

func (s *Server) routerList() []netip.AddrPort {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]netip.AddrPort, 0, len(s.routers))
	for a := range s.routers {
		out = append(out, a)
	}
	return out
}
