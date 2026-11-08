package dht

import (
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// --- messages -------------------------------------------------------------------------------

func TestMessages(t *testing.T) {
	id := NewID()
	m, err := decode(encodeQuery("aa", "ping", map[string]any{"id": string(id[:])}))
	if err != nil || m.kind != "q" || m.method != "ping" || m.tid != "aa" || m.id != id {
		t.Fatalf("query: %+v %v", m, err)
	}
	m, err = decode(encodeResponse("bb", map[string]any{"id": string(id[:]), "token": "x"}))
	if err != nil || m.kind != "r" || m.id != id {
		t.Fatalf("response: %+v %v", m, err)
	}
	m, err = decode(encodeError("cc", errMethod, "nope"))
	if err != nil || m.kind != "e" || m.code != errMethod || m.text != "nope" {
		t.Fatalf("error: %+v %v", m, err)
	}
	for name, b := range map[string][]byte{
		"not bencode": []byte("hello"),
		"list":        []byte("le"),
		"no id":       encodeQuery("t", "ping", map[string]any{}),
		"short id":    encodeResponse("t", map[string]any{"id": "short"}),
		"bad y":       []byte("d1:t2:aa1:y1:xe"),
	} {
		if _, err := decode(b); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}

func TestCompactNodes(t *testing.T) {
	a := NodeInfo{NewID(), netip.MustParseAddrPort("1.2.3.4:5")}
	b := NodeInfo{NewID(), netip.MustParseAddrPort("[::ffff:6.7.8.9]:10")}
	v6 := NodeInfo{NewID(), netip.MustParseAddrPort("[2001:db8::1]:1")} // skipped: IPv4 only
	zero := NodeInfo{NewID(), netip.MustParseAddrPort("1.1.1.1:0")}     // unusable port
	got := decodeNodes(encodeNodes([]NodeInfo{a, b, v6, zero}) + "junk")
	want := []NodeInfo{a, {b.ID, netip.MustParseAddrPort("6.7.8.9:10")}}
	if !slices.Equal(got, want) {
		t.Fatalf("nodes = %v, want %v", got, want)
	}
	body := map[string]any{"values": []any{"\x01\x02\x03\x04\x00\x50", "short", 7, "\x01\x02\x03\x04\x00\x00"}}
	if v := decodeValues(body); len(v) != 1 || v[0] != netip.MustParseAddrPort("1.2.3.4:80") {
		t.Fatalf("values = %v", v)
	}
}

func TestTokens(t *testing.T) {
	now := time.Unix(1000, 0)
	tk := newTokens(func() time.Time { return now })
	ip, other := netip.MustParseAddr("1.2.3.4"), netip.MustParseAddr("1.2.3.5")
	tok := tk.issue(ip)
	if !tk.valid(tok, ip) || tk.valid(tok, other) || tk.valid("", ip) {
		t.Fatal("token must be valid for its IP only")
	}
	now = now.Add(rotateEvery + time.Second) // one rotation: still accepted
	if !tk.valid(tok, ip) {
		t.Fatal("token rejected after one rotation")
	}
	now = now.Add(rotateEvery + time.Second) // two: expired
	if tk.valid(tok, ip) {
		t.Fatal("token accepted after two rotations")
	}
	tok = tk.issue(ip)
	now = now.Add(3 * rotateEvery) // long idle
	if tk.valid(tok, ip) {
		t.Fatal("token accepted after a long idle period")
	}
}

// --- routing table ------------------------------------------------------------------------

// idWithPrefix returns an ID sharing exactly n leading bits with self.
func idWithPrefix(self ID, n int) ID {
	id := NewID()
	for i := 0; i < n; i++ { // copy n bits
		mask := byte(0x80 >> (i % 8))
		id[i/8] = id[i/8]&^mask | self[i/8]&mask
	}
	mask := byte(0x80 >> (n % 8)) // flip bit n
	id[n/8] = id[n/8]&^mask | ^self[n/8]&mask
	return id
}

func addr(i int) netip.AddrPort {
	return netip.AddrPortFrom(netip.MustParseAddr("10.0.0.1"), uint16(1000+i))
}

func TestTable(t *testing.T) {
	self := NewID()
	tb := newTable(self)
	now := time.Now()
	tb.seen(NodeInfo{self, addr(0)}, now) // ourselves: ignored
	if all, _ := tb.counts(now); all != 0 {
		t.Fatal("added our own ID")
	}
	if tb.bucket(idWithPrefix(self, 3)) != 3 || tb.bucket(self) != -1 {
		t.Fatal("bucket index")
	}

	// Bucket 0 fills up at K; a newcomer is refused while everyone is fine…
	var first []NodeInfo
	for i := 0; i < K; i++ {
		n := NodeInfo{idWithPrefix(self, 0), addr(i)}
		first = append(first, n)
		tb.seen(n, now)
	}
	late := NodeInfo{idWithPrefix(self, 0), addr(100)}
	tb.seen(late, now)
	if tb.has(late) || tb.room(late.ID) {
		t.Fatal("full bucket took a new node without a bad one to replace")
	}
	// …but replaces a node that has become bad.
	for i := 0; i < badAfter; i++ {
		tb.failed(first[2].Addr)
	}
	if !tb.room(late.ID) {
		t.Fatal("no room despite a bad node")
	}
	tb.seen(late, now)
	if !tb.has(late) || tb.has(first[2]) {
		t.Fatal("bad node not replaced")
	}
	// Answering again clears failures; enough failures remove a node.
	tb.failed(first[3].Addr)
	tb.seen(first[3], now)
	for i := 0; i < forgetAfter; i++ {
		tb.failed(first[4].Addr)
	}
	if tb.has(first[4]) || !tb.has(first[3]) {
		t.Fatal("forget/reset failures")
	}

	// closest is ordered by XOR distance and skips bad nodes.
	target := idWithPrefix(self, 0)
	got := tb.closest(target, 100)
	for i := 1; i < len(got); i++ {
		if closer(target, got[i].ID, got[i-1].ID) {
			t.Fatal("closest not sorted by distance")
		}
	}
	tb.failed(first[5].Addr)
	tb.failed(first[5].Addr)
	if slices.Contains(tb.closest(target, 100), first[5]) {
		t.Fatal("bad node returned by closest")
	}
	// An address that comes back with a different ID replaces its old entry.
	moved := NodeInfo{idWithPrefix(self, 7), first[6].Addr}
	tb.seen(moved, now)
	if !tb.has(moved) || slices.Contains(tb.closest(first[6].ID, 100), first[6]) {
		t.Fatal("address re-use")
	}
	// Questionable: not heard from for 15 minutes, oldest first.
	if q := tb.questionable(now.Add(questionableIn+time.Minute), 3); len(q) != 3 {
		t.Fatalf("questionable = %v", q)
	}
}

func TestPeerStore(t *testing.T) {
	s := newPeerStore()
	h := NewID()
	now := time.Now()
	for i := 0; i < maxPeersPerHash+10; i++ {
		s.add(h, addr(i), now)
	}
	if _, n := s.counts(); n != maxPeersPerHash {
		t.Fatalf("stored %d peers, cap is %d", n, maxPeersPerHash)
	}
	if got := s.get(h, now); len(got) != maxValues {
		t.Fatalf("get returned %d, want %d", len(got), maxValues)
	}
	s.expire(now.Add(peerTTL))
	if hs, _ := s.counts(); hs != 0 || len(s.get(h, now.Add(peerTTL))) != 0 {
		t.Fatal("expired peers kept")
	}
}

// --- a server, driven over raw UDP --------------------------------------------------------------

func testServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	if cfg.Addr == "" {
		cfg.Addr = "127.0.0.1:0"
	}
	if cfg.QueryTimeout == 0 {
		cfg.QueryTimeout = 500 * time.Millisecond
	}
	if cfg.MaintainEvery == 0 {
		cfg.MaintainEvery = 100 * time.Millisecond
	}
	s, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type rawClient struct {
	t    *testing.T
	c    *net.UDPConn
	id   ID
	to   netip.AddrPort
	seen int
}

func newRawClient(t *testing.T, to netip.AddrPort) *rawClient {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawClient{t: t, c: c, id: NewID(), to: to}
}

// ask sends a query and returns the decoded reply (or fails on timeout).
func (r *rawClient) ask(method string, args map[string]any) msg {
	r.t.Helper()
	args["id"] = string(r.id[:])
	r.c.WriteToUDPAddrPort(encodeQuery("zz", method, args), r.to)
	m, ok := r.read(2 * time.Second)
	if !ok {
		r.t.Fatalf("%s: no reply", method)
	}
	return m
}

func (r *rawClient) read(wait time.Duration) (msg, bool) {
	buf := make([]byte, 4096)
	r.c.SetReadDeadline(time.Now().Add(wait))
	for {
		n, _, err := r.c.ReadFromUDPAddrPort(buf)
		if err != nil {
			return msg{}, false
		}
		m, err := decode(buf[:n])
		if err == nil && m.kind != "q" { // the server may ping us back: skip that
			return m, true
		}
	}
}

func TestServerAnswersQueries(t *testing.T) {
	s := testServer(t, Config{})
	r := newRawClient(t, s.Addr())

	if m := r.ask("ping", map[string]any{}); m.kind != "r" || m.id != s.ID() {
		t.Fatalf("ping: %+v", m)
	}
	if m := r.ask("find_node", map[string]any{"target": string(r.id[:])}); m.kind != "r" {
		t.Fatalf("find_node: %+v", m)
	}
	hash := NewID()
	m := r.ask("get_peers", map[string]any{"info_hash": string(hash[:])})
	tok, _ := bencode.GetString(m.body, "token")
	if m.kind != "r" || tok == "" || len(decodeValues(m.body)) != 0 {
		t.Fatalf("get_peers on an unknown hash: %+v", m)
	}

	if m := r.ask("announce_peer", map[string]any{"info_hash": string(hash[:]), "port": 4242, "token": "forged"}); m.kind != "e" || m.code != errProtocol {
		t.Fatalf("announce with a bad token: %+v", m)
	}
	if m := r.ask("announce_peer", map[string]any{"info_hash": string(hash[:]), "port": 4242, "token": tok}); m.kind != "r" {
		t.Fatalf("announce: %+v", m)
	}
	if m := r.ask("announce_peer", map[string]any{"info_hash": string(hash[:]), "port": 1, "implied_port": 1, "token": tok}); m.kind != "r" {
		t.Fatalf("announce with implied_port: %+v", m)
	}
	m = r.ask("get_peers", map[string]any{"info_hash": string(hash[:])})
	myPort := r.c.LocalAddr().(*net.UDPAddr).Port
	want := []netip.AddrPort{
		netip.MustParseAddrPort("127.0.0.1:4242"),
		netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), uint16(myPort)),
	}
	got := decodeValues(m.body)
	slices.SortFunc(got, func(a, b netip.AddrPort) int { return a.Compare(b) })
	slices.SortFunc(want, func(a, b netip.AddrPort) int { return a.Compare(b) })
	if !slices.Equal(got, want) {
		t.Fatalf("stored peers = %v, want %v", got, want)
	}

	if m := r.ask("vote", map[string]any{}); m.kind != "e" || m.code != errMethod {
		t.Fatalf("unknown method: %+v", m)
	}
	if m := r.ask("find_node", map[string]any{"target": "short"}); m.kind != "e" || m.code != errProtocol {
		t.Fatalf("bad target: %+v", m)
	}
	// Garbage is ignored, and the server keeps working.
	r.c.WriteToUDPAddrPort([]byte("\x00garbage"), s.Addr())
	r.c.WriteToUDPAddrPort([]byte("d1:y1:qe"), s.Addr())
	if m := r.ask("ping", map[string]any{}); m.kind != "r" {
		t.Fatal("server stopped answering after garbage")
	}
}

// A node that queries us joins our table only after answering our ping.
func TestQueryingNodesAreVerified(t *testing.T) {
	a := testServer(t, Config{})
	r := newRawClient(t, a.Addr()) // never answers pings
	r.ask("ping", map[string]any{})
	b := testServer(t, Config{})
	if _, err := b.ping(context.Background(), a.Addr()); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return a.table.has(NodeInfo{ID: b.ID(), Addr: b.Addr()}) })
	if a.table.has(NodeInfo{ID: r.id, Addr: netip.MustParseAddrPort(r.c.LocalAddr().String())}) {
		t.Fatal("a node that never answered was added")
	}
	// A peer's PORT hint: pinged and added if it answers.
	c := testServer(t, Config{})
	a.AddNode(c.Addr())
	waitFor(t, func() bool { return a.table.has(NodeInfo{ID: c.ID(), Addr: c.Addr()}) })
}

func TestIncomingQueryCap(t *testing.T) {
	s := testServer(t, Config{MaxQPS: 5})
	r := newRawClient(t, s.Addr())
	for i := 0; i < 30; i++ {
		r.c.WriteToUDPAddrPort(encodeQuery("p", "ping", map[string]any{"id": string(r.id[:])}), s.Addr())
	}
	answers := 0
	for {
		if _, ok := r.read(300 * time.Millisecond); !ok {
			break
		}
		answers++
	}
	// 5 per second; the burst may straddle a second boundary.
	if answers == 0 || answers > 10 {
		t.Fatalf("%d answers to 30 queries with a cap of 5/s", answers)
	}
}

// --- a small DHT on loopback -------------------------------------------------------------------

func network(t *testing.T, n int) []*Server {
	t.Helper()
	boot := testServer(t, Config{})
	nodes := []*Server{boot}
	for i := 1; i < n; i++ {
		nodes = append(nodes, testServer(t, Config{Bootstrap: []string{boot.Addr().String()}}))
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

func TestAnnounceAndFindPeers(t *testing.T) {
	nodes := network(t, 20)
	var hash metainfo.Hash
	copy(hash[:], NewID().String())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if peers, err := nodes[5].GetPeers(ctx, hash, 4242); err != nil || len(peers) != 0 {
		t.Fatalf("first lookup: %v %v", peers, err)
	}
	stored := 0
	for _, s := range nodes {
		if _, p := s.store.counts(); p > 0 {
			stored++
		}
	}
	if stored == 0 {
		t.Fatal("the announce reached no node")
	}
	peers, err := nodes[15].GetPeers(ctx, hash, 0)
	if err != nil || !slices.Contains(peers, netip.MustParseAddrPort("127.0.0.1:4242")) {
		t.Fatalf("lookup found %v, %v; want the announced peer", peers, err)
	}
}

func TestStateSurvivesRestart(t *testing.T) {
	nodes := network(t, 12)
	file := filepath.Join(t.TempDir(), "dht.json")
	s := testServer(t, Config{StateFile: file, Bootstrap: []string{nodes[0].Addr().String()}})
	waitFor(t, func() bool { _, good := s.table.counts(time.Now()); return good >= K })
	id := s.ID()
	s.Close()

	// No bootstrap this time: the saved nodes are all it has.
	s2 := testServer(t, Config{StateFile: file})
	if s2.ID() != id {
		t.Fatal("node ID changed across a restart")
	}
	waitFor(t, func() bool { _, good := s2.table.counts(time.Now()); return good >= K })
}

func TestCorruptStateStartsFresh(t *testing.T) {
	file := filepath.Join(t.TempDir(), "dht.json")
	if err := writeFile(file, "{not json"); err != nil {
		t.Fatal(err)
	}
	s := testServer(t, Config{StateFile: file})
	if all, _ := s.table.counts(time.Now()); all != 0 {
		t.Fatal("nodes from a corrupt file")
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func writeFile(path, s string) error { return os.WriteFile(path, []byte(s), 0o600) }

// A response is accepted only from the node that was asked: a third party
// that guesses the transaction ID can't answer for it.
func TestResponsesMustComeFromTheQueriedNode(t *testing.T) {
	s := testServer(t, Config{QueryTimeout: 2 * time.Second})
	asked := newRawClient(t, s.Addr())
	forger := newRawClient(t, s.Addr())
	askedAddr := netip.MustParseAddrPort(asked.c.LocalAddr().String())

	type res struct {
		m   msg
		err error
	}
	done := make(chan res, 1)
	go func() {
		m, err := s.ping(context.Background(), askedAddr)
		done <- res{m, err}
	}()
	buf := make([]byte, 4096)
	asked.c.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _, err := asked.c.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	q, _ := decode(buf[:n])
	forger.c.WriteToUDPAddrPort(encodeResponse(q.tid, map[string]any{"id": string(forger.id[:])}), s.Addr())
	time.Sleep(100 * time.Millisecond)
	asked.c.WriteToUDPAddrPort(encodeResponse(q.tid, map[string]any{"id": string(asked.id[:])}), s.Addr())
	r := <-done
	if r.err != nil || r.m.id != asked.id {
		t.Fatalf("got %v / %v; want the asked node's answer, not the forger's", r.m.id, r.err)
	}
}
