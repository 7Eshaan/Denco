package torrent

import (
	"context"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// fakeDHT answers lookups from a script: answers[i] for the i-th lookup,
// the last one repeated.
type fakeDHT struct {
	mu      sync.Mutex
	answers [][]netip.AddrPort
	calls   []dhtCall
	nodes   []netip.AddrPort
}

type dhtCall struct {
	hash metainfo.Hash
	port uint16
}

func (f *fakeDHT) GetPeers(ctx context.Context, hash metainfo.Hash, port uint16) ([]netip.AddrPort, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, dhtCall{hash, port})
	if len(f.answers) == 0 {
		return nil, nil
	}
	return f.answers[min(len(f.calls), len(f.answers))-1], nil
}

func (f *fakeDHT) AddNode(a netip.AddrPort) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = append(f.nodes, a)
}

func (f *fakeDHT) Port() uint16 { return 7777 }

func (f *fakeDHT) snapshot() ([]dhtCall, []netip.AddrPort) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]dhtCall(nil), f.calls...), append([]netip.AddrPort(nil), f.nodes...)
}

func dhtConfig(d PeerFinder) Config {
	cfg := testConfig()
	cfg.DHT = d
	cfg.PeerOpts.DHT = true
	cfg.DHTRetry = 50 * time.Millisecond
	return cfg
}

// The DHT is the only source of peers; its first lookup finds nobody (as
// at daemon start), the retry finds the seed. Lookups announce our port.
func TestDHTFindsSeed(t *testing.T) {
	f := makeTorrent(t, pl, 5*pl+7)
	sd := newSeeder(t, f, seederOpts{})
	d := &fakeDHT{answers: [][]netip.AddrPort{nil, {sd.addr}}}
	leech := startNode(t, f.mi, t.TempDir(), nil, dhtConfig(d))
	leech.waitComplete(t, 10*time.Second)
	checkOnDisk(t, f, leech.dir)

	calls, _ := d.snapshot()
	if len(calls) < 2 || calls[0].hash != f.mi.InfoHash || calls[0].port != leech.ln.Port() {
		t.Fatalf("lookups = %+v; want ≥ 2 for our hash announcing port %d", calls, leech.ln.Port())
	}
	if st := leech.s.Stats(); st.DHTPeers != 1 {
		t.Errorf("Stats.DHTPeers = %d, want 1", st.DHTPeers)
	}
}

// Peers that run a DHT get our DHT port, and theirs is passed to our node.
func TestDHTPortMessages(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	d := &fakeDHT{}
	n := startNode(t, f.mi, t.TempDir(), nil, dhtConfig(d))
	c, err := peer.Dial(context.Background(), n.ln.Addr(), f.mi.InfoHash, peer.Options{PeerID: peerid.New(), DHT: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	got := uint16(0)
	timeout := time.After(5 * time.Second)
	for got == 0 {
		select {
		case m := <-c.Incoming():
			if m != nil && m.ID == peer.MsgPort {
				got, _ = peer.ParsePort(m)
			}
		case <-timeout:
			t.Fatal("no PORT message")
		}
	}
	if got != 7777 {
		t.Fatalf("PORT %d, want our DHT port 7777", got)
	}
	c.Send(peer.Port(5555))
	waitFor(t, 5*time.Second, func() bool {
		_, nodes := d.snapshot()
		return len(nodes) == 1 && nodes[0] == netip.AddrPortFrom(c.Remote.Addr(), 5555)
	})
}

// A private torrent never touches the DHT: no lookups, no PORT messages.
func TestDHTNotForPrivateTorrents(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	mi := privateCopy(t, f)
	d := &fakeDHT{}
	n := startNode(t, mi, t.TempDir(), nil, dhtConfig(d))
	c, err := peer.Dial(context.Background(), n.ln.Addr(), mi.InfoHash, peer.Options{PeerID: peerid.New(), DHT: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Send(peer.Port(5555))
	timeout := time.After(500 * time.Millisecond)
	for done := false; !done; {
		select {
		case m := <-c.Incoming():
			if m != nil && m.ID == peer.MsgPort {
				t.Fatal("sent PORT for a private torrent")
			}
		case <-timeout:
			done = true
		}
	}
	if calls, nodes := d.snapshot(); len(calls) != 0 || len(nodes) != 0 {
		t.Fatalf("private torrent used the DHT: lookups %v, nodes %v", calls, nodes)
	}
}
