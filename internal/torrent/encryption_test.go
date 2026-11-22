package torrent

import (
	"net/netip"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/ratelimit"
)

func encConfig(e peer.Encryption) Config {
	cfg := testConfig()
	cfg.PeerOpts.Encryption = e
	return cfg
}

// Two Denco nodes, one preferring and one requiring encryption, transfer a
// torrent over an encrypted connection (D63), and both count it.
func TestEncryptedTransfer(t *testing.T) {
	f := makeTorrent(t, pl*2, 300_000, 77)
	seedDir := t.TempDir()
	seedCfg := encConfig(peer.EncryptionRequire)
	// Slow enough to look at the connection before the download completes
	// (two seeds then drop each other).
	seedCfg.UploadLimiter = ratelimit.New(150_000)
	seed := startNode(t, f.mi, seedDir, writeFixture(t, f, seedDir), seedCfg)
	leech := startNode(t, f.mi, t.TempDir(), nil, encConfig(peer.EncryptionPrefer))
	leech.s.AddPeers([]netip.AddrPort{seed.ln.Addr()})

	for name, n := range map[string]*node{"seed": seed, "leecher": leech} {
		deadline := time.Now().Add(5 * time.Second)
		for n.s.Stats().Encrypted != 1 {
			if time.Now().After(deadline) {
				t.Fatalf("%s: %d encrypted peers, want 1", name, n.s.Stats().Encrypted)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if ps := n.s.Peers(); len(ps) != 1 || !ps[0].Encrypted {
			t.Fatalf("%s: peers %+v", name, ps)
		}
	}
	if leech.s.Stats().State == StateSeeding {
		t.Fatal("finished before the check: slow the seed down more")
	}
	leech.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, leech.dir)
}

// A peer that only speaks plain BitTorrent is reached by falling back, and
// is dialed plain from then on: no second wasted MSE attempt (D63).
func TestPlainPeerRemembered(t *testing.T) {
	f := makeTorrent(t, pl, 6*pl)
	// dropAfter makes the leecher reconnect several times.
	sd := newSeeder(t, f, seederOpts{dropAfter: 2})
	leech := start(t, f.mi, t.TempDir(), nil, encConfig(peer.EncryptionPrefer), false)
	leech.s.AddPeers([]netip.AddrPort{sd.addr})
	leech.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, leech.dir)
	leech.stopAndWait(t) // the swarm is ours to read now

	if !leech.s.swarm.get(sd.addr).plain {
		t.Fatal("the plain peer isn't remembered as plain")
	}
	// 6 pieces at 2 blocks per connection: several connections, and
	// exactly one refused MSE attempt (before the first).
	if n := sd.accepts.Load(); n < 4 {
		t.Fatalf("only %d connections", n)
	}
	if n := sd.refusedMSE.Load(); n != 1 {
		t.Fatalf("%d MSE attempts, want 1", n)
	}
}

// PEX tells others which peers speak MSE.
func TestPEXEncryptionFlag(t *testing.T) {
	s := &Session{peers: map[netip.AddrPort]*peerState{}}
	add := func(addr string, encrypted bool) netip.AddrPort {
		a := netip.MustParseAddrPort(addr)
		s.peers[a] = &peerState{addr: a, has: bitfield.New(4),
			conn: &peer.Conn{Outgoing: true, Encrypted: encrypted}}
		return a
	}
	enc, plain := add("10.0.0.1:1", true), add("10.0.0.2:2", false)
	flags := s.pexPeers()
	if flags[enc]&peer.PEXEncryption == 0 || flags[plain]&peer.PEXEncryption != 0 {
		t.Fatalf("flags %v", flags)
	}
}
