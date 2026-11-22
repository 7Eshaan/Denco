package torrent

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/ratelimit"
	"github.com/7Eshaan/Denco/internal/utp"
)

// startUTPNode is startNode with uTP on the peer port's number (D64), the
// way the engine sets it up.
func startUTPNode(t *testing.T, mi *metainfo.MetaInfo, dir string, have *bitfield.Bitfield, cfg Config) *node {
	t.Helper()
	opts := cfg.PeerOpts
	opts.PeerID = cfg.PeerID
	ln, err := Listen("127.0.0.1:0", opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(ln.Port())})
	if err != nil {
		t.Skipf("UDP port %d taken: %v", ln.Port(), err)
	}
	sock := utp.NewSocket(pc, utp.Config{ConnectTimeout: 300 * time.Millisecond})
	ln.Serve(sock) // closed with ln
	cfg.Port = ln.Port()
	cfg.PeerOpts.UTP, cfg.PeerOpts.Transport = &utp.Dialer{V4: sock}, peer.TransportBoth
	r := start(t, mi, dir, have, cfg, have != nil)
	ln.Add(r.s)
	return &node{running: r, ln: ln}
}

// Two nodes connect over uTP (tried first) and transfer the torrent; both
// see the peer as uTP, encrypted too.
func TestUTPTransfer(t *testing.T) {
	f := makeTorrent(t, pl*2, 300_000, 77)
	seedDir := t.TempDir()
	seedCfg := encConfig(peer.EncryptionPrefer)
	seedCfg.UploadLimiter = ratelimit150k() // time to look before it completes
	seed := startUTPNode(t, f.mi, seedDir, writeFixture(t, f, seedDir), seedCfg)
	leech := startUTPNode(t, f.mi, t.TempDir(), nil, encConfig(peer.EncryptionPrefer))
	leech.s.AddPeers([]netip.AddrPort{seed.ln.Addr()})

	for name, n := range map[string]*node{"seed": seed, "leecher": leech} {
		deadline := time.Now().Add(5 * time.Second)
		for n.s.Stats().UTP != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d uTP peers, want 1", name, n.s.Stats().UTP)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ps := n.s.Peers(); len(ps) != 1 || !ps[0].UTP || !ps[0].Encrypted {
			t.Fatalf("%s: peers %+v", name, ps)
		}
	}
	leech.waitComplete(t, 30*time.Second)
	checkOnDisk(t, f, leech.dir)
}

// A TCP-only peer: one uTP attempt (three SYNs), then TCP, and later
// reconnects go straight to TCP (D64).
func TestTCPOnlyPeerRemembered(t *testing.T) {
	f := makeTorrent(t, pl, 6*pl)
	sd := newSeeder(t, f, seederOpts{dropAfter: 2}) // reconnects several times
	// Something on the seeder's UDP port number, counting SYNs.
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(sd.addr.Port())})
	if err != nil {
		t.Skipf("UDP port taken: %v", err)
	}
	defer udp.Close()
	syns := make(chan int, 1)
	go func() {
		n := 0
		buf := make([]byte, 2048)
		for {
			udp.SetReadDeadline(time.Now().Add(3 * time.Second))
			k, err := udp.Read(buf)
			if err != nil {
				syns <- n
				return
			}
			if k >= 20 && buf[0] == 4<<4|1 {
				n++
			}
		}
	}()
	leech := startUTPNode(t, f.mi, t.TempDir(), nil, testConfig())
	leech.s.AddPeers([]netip.AddrPort{sd.addr})
	leech.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, leech.dir)
	leech.stopAndWait(t)
	if !leech.s.swarm.get(sd.addr).tcpOnly {
		t.Fatal("the TCP-only peer isn't remembered")
	}
	if n := sd.accepts.Load(); n < 3 {
		t.Fatalf("only %d TCP connections", n)
	}
	if n := <-syns; n != 3 {
		t.Fatalf("%d SYNs, want 3 (one uTP attempt)", n)
	}
}

// PEX tells others which peers speak uTP.
func TestPEXUTPFlag(t *testing.T) {
	s := &Session{peers: map[netip.AddrPort]*peerState{}}
	a := netip.MustParseAddrPort("10.0.0.1:1")
	b := netip.MustParseAddrPort("10.0.0.2:2")
	s.peers[a] = &peerState{addr: a, has: bitfield.New(4), conn: &peer.Conn{Outgoing: true, UTP: true}}
	s.peers[b] = &peerState{addr: b, has: bitfield.New(4), conn: &peer.Conn{Outgoing: true}}
	flags := s.pexPeers()
	if flags[a]&peer.PEXUTP == 0 || flags[b]&peer.PEXUTP != 0 {
		t.Fatalf("flags %v", flags)
	}
}

func ratelimit150k() *ratelimit.Limiter { return ratelimit.New(150_000) }

// Transport utp: the Listener hangs up on TCP peers but serves uTP ones.
func TestListenerRefusesTCP(t *testing.T) {
	f := makeTorrent(t, pl, 2*pl)
	dir := t.TempDir()
	n := startUTPNode(t, f.mi, dir, writeFixture(t, f, dir), testConfig())
	n.ln.RefuseTCP()
	if _, err := peer.Dial(context.Background(), n.ln.Addr(), f.mi.InfoHash, peer.Options{PeerID: peerid.New()}); err == nil {
		t.Fatal("TCP peer served")
	}
	pc, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	sock := utp.NewSocket(pc, utp.Config{})
	defer sock.Close()
	c, err := peer.Dial(context.Background(), n.ln.Addr(), f.mi.InfoHash,
		peer.Options{PeerID: peerid.New(), UTP: &utp.Dialer{V4: sock}, Transport: peer.TransportUTP})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}
