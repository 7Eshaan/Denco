package torrent

import (
	"context"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// theirPEXID is the ut_pex ID our raw test peers ask Denco to use.
const theirPEXID = 7

func pexConfig() Config {
	cfg := testConfig()
	cfg.PeerOpts.Extensions = true
	cfg.PEX = true
	cfg.PEXInterval = 100 * time.Millisecond
	return cfg
}

// rawListener is a hand-driven peer that accepts connections for one
// torrent. handle runs per connection; nil just holds it open.
type rawListener struct {
	ln      net.Listener
	addr    netip.AddrPort
	accepts atomic.Int64
	mu      sync.Mutex
	conns   []*peer.Conn
}

func newRawListener(t *testing.T, hash metainfo.Hash, handle func(*peer.Conn)) *rawListener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r := &rawListener{ln: ln, addr: netip.MustParseAddrPort(ln.Addr().String())}
	t.Cleanup(r.close)
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			r.accepts.Add(1)
			c, err := peer.Accept(nc, func(h metainfo.Hash) bool { return h == hash },
				peer.Options{PeerID: peerid.New(), Extensions: true})
			if err != nil {
				continue
			}
			r.mu.Lock()
			r.conns = append(r.conns, c)
			r.mu.Unlock()
			go func() {
				if handle != nil {
					handle(c)
				}
				for range c.Incoming() { // hold it open until closed
				}
			}()
		}
	}()
	return r
}

func (r *rawListener) close() {
	r.ln.Close()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.conns {
		c.Close()
	}
}

// pexHello sends an extension handshake offering ut_pex (and our port, if any).
func pexHello(c *peer.Conn, port int) {
	c.Send(peer.Extended(0, peer.ExtHandshake{M: map[string]int{peer.ExtPEX: theirPEXID}, Port: port}.Encode()))
}

// dialPEX connects a hand-driven, extension-speaking peer to a node.
func dialPEX(t *testing.T, n *node, hash metainfo.Hash) *peer.Conn {
	t.Helper()
	c, err := peer.Dial(context.Background(), n.ln.Addr(), hash, peer.Options{PeerID: peerid.New(), Extensions: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// A leecher that only knows one useless peer learns the seed's address
// from that peer's PEX message and downloads everything from it.
func TestPEXFindsSeed(t *testing.T) {
	f := makeTorrent(t, pl, 5*pl+100)
	sd := newSeeder(t, f, seederOpts{})
	introducer := newRawListener(t, f.mi.InfoHash, func(c *peer.Conn) {
		pexHello(c, 0)
		c.Send(peer.Extended(peer.ExtPEXID, peer.PEX{Added: []netip.AddrPort{sd.addr}}.Encode()))
	})

	leech := startNode(t, f.mi, t.TempDir(), nil, pexConfig())
	leech.s.AddPeers([]netip.AddrPort{introducer.addr})
	leech.waitComplete(t, 10*time.Second)
	checkOnDisk(t, f, leech.dir)
}

// A node tells PEX peers about the peers it dialed — not about the
// recipient itself, nor incoming peers whose listening port is unknown —
// and later reports the ones that went away as dropped.
func TestPEXAdvertisesConnectedPeers(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	y1 := newRawListener(t, f.mi.InfoHash, nil)
	y2 := newRawListener(t, f.mi.InfoHash, nil)
	n := startNode(t, f.mi, t.TempDir(), nil, pexConfig())
	n.s.AddPeers([]netip.AddrPort{y1.addr, y2.addr})

	x := dialPEX(t, n, f.mi.InfoHash)
	pexHello(x, 5555)                 // x listens on 127.0.0.1:5555: never told about itself
	z := dialPEX(t, n, f.mi.InfoHash) // incoming without a port: not advertised
	pexHello(z, 0)

	next := func() peer.PEX {
		t.Helper()
		var msg peer.PEX
		if !waitExtended(t, x, func(id uint8, body []byte) bool {
			if id != theirPEXID {
				return false
			}
			var err error
			if msg, err = peer.ParsePEX(body); err != nil {
				t.Fatalf("bad PEX from Denco: %v", err)
			}
			return true
		}) {
			t.Fatal("no PEX message")
		}
		return msg
	}

	seen := map[netip.AddrPort]byte{}
	deadline := time.Now().Add(5 * time.Second)
	for len(seen) < 2 && time.Now().Before(deadline) {
		msg := next()
		for i, a := range msg.Added {
			if a != y1.addr && a != y2.addr {
				t.Fatalf("advertised %v; only %v and %v are dialed peers", a, y1.addr, y2.addr)
			}
			seen[a] = msg.Flags(i)
		}
	}
	if len(seen) != 2 {
		t.Fatalf("advertised %v, want both dialed peers", seen)
	}
	if seen[y1.addr]&peer.PEXReachable == 0 {
		t.Errorf("flags for a dialed peer = %#x, want reachable", seen[y1.addr])
	}

	y2.close()
	for time.Now().Before(deadline.Add(5 * time.Second)) {
		msg := next()
		// Both dialed peers were already sent; nobody else may ever be added
		// (not x, not z, not y2 again).
		if len(msg.Added) > 0 {
			t.Fatalf("later message added %v", msg.Added)
		}
		for _, a := range msg.Dropped {
			if a == y2.addr {
				return
			}
		}
	}
	t.Fatal("the closed peer was never reported as dropped")
}

// The first PEX message goes out right after the extension handshake —
// a magnet user's metadata fetch may stay only seconds — and not again
// within the interval.
func TestPEXFirstMessageIsPrompt(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	cfg := pexConfig()
	cfg.PEXInterval = time.Hour
	y := newRawListener(t, f.mi.InfoHash, nil)
	n := startNode(t, f.mi, t.TempDir(), nil, cfg)
	x := dialPEX(t, n, f.mi.InfoHash)
	pexHello(x, 0)
	time.Sleep(10 * cfg.Tick) // x is due a message, but there is nothing to say yet
	n.s.AddPeers([]netip.AddrPort{y.addr})

	var msg peer.PEX
	if !waitExtended(t, x, func(id uint8, body []byte) bool {
		msg, _ = peer.ParsePEX(body)
		return id == theirPEXID
	}) || len(msg.Added) != 1 || msg.Added[0] != y.addr {
		t.Fatalf("first PEX message = %+v, want %v added promptly", msg, y.addr)
	}

	z := newRawListener(t, f.mi.InfoHash, nil) // news, but the interval isn't over
	n.s.AddPeers([]netip.AddrPort{z.addr})
	waitFor(t, 5*time.Second, func() bool { return z.accepts.Load() > 0 })
	timeout := time.After(300 * time.Millisecond)
	for {
		select {
		case m := <-x.Incoming():
			if id, _, _ := peer.ParseExtended(m); m.ID == peer.MsgExtended && id == theirPEXID {
				t.Fatal("second PEX message within the interval")
			}
		case <-timeout:
			return
		}
	}
}

// privateCopy returns the fixture's torrent with private=1.
func privateCopy(t *testing.T, f *fixture) *metainfo.MetaInfo {
	t.Helper()
	v, _ := bencode.Decode(f.mi.InfoBytes)
	info := v.(map[string]any)
	info["private"] = int64(1)
	raw, _ := bencode.Encode(map[string]any{"info": info})
	mi, err := metainfo.Parse(raw)
	if err != nil || !mi.Info.Private {
		t.Fatalf("private torrent: %v", err)
	}
	return mi
}

// With PEX off, or for a private torrent, ut_pex isn't offered and PEX
// messages are ignored.
func TestPEXOffAndPrivate(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	off := pexConfig()
	off.PEX = false
	for name, c := range map[string]struct {
		mi  *metainfo.MetaInfo
		cfg Config
	}{
		"disabled": {f.mi, off},
		"private":  {privateCopy(t, f), pexConfig()},
	} {
		t.Run(name, func(t *testing.T) {
			sd := newRawListener(t, c.mi.InfoHash, nil)
			n := startNode(t, c.mi, t.TempDir(), nil, c.cfg)
			x := dialPEX(t, n, c.mi.InfoHash)
			pexHello(x, 0)
			var hs peer.ExtHandshake
			waitExtended(t, x, func(id uint8, body []byte) bool {
				hs, _ = peer.ParseExtHandshake(body)
				return id == 0
			})
			if hs.M[peer.ExtMetadata] == 0 || hs.M[peer.ExtPEX] != 0 {
				t.Fatalf("extension handshake m = %v; want ut_metadata only", hs.M)
			}
			x.Send(peer.Extended(peer.ExtPEXID, peer.PEX{Added: []netip.AddrPort{sd.addr}}.Encode()))
			time.Sleep(300 * time.Millisecond)
			if sd.accepts.Load() != 0 || n.s.Stats().Known != 0 {
				t.Fatalf("used a PEX peer: accepts=%d known=%d", sd.accepts.Load(), n.s.Stats().Known)
			}
		})
	}
}

// From one peer at most 50 addresses per message are taken, and a second
// message sooner than half an interval is ignored.
func TestPEXLimitsWhatOnePeerCanAdd(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	cfg := pexConfig()
	cfg.PEXInterval = time.Hour // so the second message is "too soon"
	n := startNode(t, f.mi, t.TempDir(), nil, cfg)
	x := dialPEX(t, n, f.mi.InfoHash)
	pexHello(x, 0)

	var many peer.PEX
	for i := 1; i <= 60; i++ { // closed loopback ports: dials fail fast
		many.Added = append(many.Added, netip.AddrPortFrom(netip.MustParseAddr("127.0.0.3"), uint16(i)))
	}
	x.Send(peer.Extended(peer.ExtPEXID, many.Encode()))
	waitFor(t, 5*time.Second, func() bool { return n.s.Stats().Known == peer.MaxPEXPeers })

	late := newRawListener(t, f.mi.InfoHash, nil)
	x.Send(peer.Extended(peer.ExtPEXID, peer.PEX{Added: []netip.AddrPort{late.addr}}.Encode()))
	time.Sleep(300 * time.Millisecond)
	if k := n.s.Stats().Known; k != peer.MaxPEXPeers || late.accepts.Load() != 0 {
		t.Fatalf("after a too-early message: known=%d accepts=%d", k, late.accepts.Load())
	}
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met in time")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A seed ignores PEX peers flagged as seeds; a malformed PEX message gets
// the sender disconnected.
func TestPEXSeedSkipsSeedsAndBadMessagesDrop(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	dir := t.TempDir()
	n := startNode(t, f.mi, dir, writeFixture(t, f, dir), pexConfig())
	x := dialPEX(t, n, f.mi.InfoHash)
	pexHello(x, 0)

	seed := netip.MustParseAddrPort("127.0.0.3:1")
	leech := netip.MustParseAddrPort("127.0.0.3:2")
	x.Send(peer.Extended(peer.ExtPEXID, peer.PEX{
		Added: []netip.AddrPort{seed, leech}, AddedFlags: []byte{peer.PEXSeed, 0},
	}.Encode()))
	waitFor(t, 5*time.Second, func() bool { return n.s.Stats().Known == 1 })

	time.Sleep(pexConfig().PEXInterval) // so the next message isn't "too soon"
	x.Send(peer.Extended(peer.ExtPEXID, []byte("le")))
	select {
	case <-x.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("not disconnected after a malformed PEX message")
	}
}
