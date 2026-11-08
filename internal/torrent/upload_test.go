package torrent

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// writeFixture puts the whole torrent on disk under dir and returns a full
// "have" bitfield, i.e. prepares a complete seed.
func writeFixture(t *testing.T, f *fixture, dir string) *bitfield.Bitfield {
	t.Helper()
	for _, file := range f.mi.Info.Files {
		p := filepath.Join(append([]string{dir, f.mi.Info.Name}, file.Path...)...)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, f.data[file.Offset:file.Offset+file.Length], 0o644); err != nil {
			t.Fatal(err)
		}
	}
	have := bitfield.New(f.mi.Info.NumPieces())
	have.SetAll()
	return have
}

// node is a Denco session reachable through its own Listener.
type node struct {
	*running
	ln *Listener
}

func startNode(t *testing.T, mi *metainfo.MetaInfo, dir string, have *bitfield.Bitfield, cfg Config) *node {
	t.Helper()
	opts := cfg.PeerOpts // same options (extensions…) for incoming peers
	opts.PeerID = cfg.PeerID
	ln, err := Listen("127.0.0.1:0", opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	cfg.Port = ln.Port()
	r := start(t, mi, dir, have, cfg, have != nil)
	ln.Add(r.s)
	return &node{running: r, ln: ln}
}

func TestDencoToDencoTransfer(t *testing.T) {
	f := makeTorrent(t, pl*2, 300_000, 77, 123_456)
	seedDir := t.TempDir()
	seed := startNode(t, f.mi, seedDir, writeFixture(t, f, seedDir), testConfig())

	leech := startNode(t, f.mi, t.TempDir(), nil, testConfig())
	leech.s.AddPeers([]netip.AddrPort{seed.ln.Addr()})
	leech.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, leech.dir)

	// The seed counted what it uploaded; the leecher is now a seed itself.
	deadline := time.Now().Add(5 * time.Second)
	for seed.s.Stats().Uploaded < int64(len(f.data)) {
		if time.Now().After(deadline) {
			t.Fatalf("seed uploaded %d of %d bytes", seed.s.Stats().Uploaded, len(f.data))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if st := leech.s.Stats(); st.State != StateSeeding {
		t.Errorf("leecher state after completion = %s, want seeding", st.State)
	}
	if st := seed.s.Stats(); st.State != StateSeeding {
		t.Errorf("seed state = %s", st.State)
	}
}

func TestFinishedDownloadSeedsToOthers(t *testing.T) {
	f := makeTorrent(t, pl, 25*pl)
	seedDir := t.TempDir()
	orig := startNode(t, f.mi, seedDir, writeFixture(t, f, seedDir), testConfig())

	first := startNode(t, f.mi, t.TempDir(), nil, testConfig())
	first.s.AddPeers([]netip.AddrPort{orig.ln.Addr()})
	first.waitComplete(t, 20*time.Second)

	// The original seed leaves. The only copy left is the one `first` made.
	orig.stopAndWait(t)
	orig.ln.Close()

	second := startNode(t, f.mi, t.TempDir(), nil, testConfig())
	second.s.AddPeers([]netip.AddrPort{first.ln.Addr()})
	second.waitComplete(t, 20*time.Second)
	checkOnDisk(t, f, second.dir) // `first` was the only possible source
	// Stats are snapshotted once per tick, so give the counter a moment.
	deadline := time.Now().Add(5 * time.Second)
	for first.s.Stats().Uploaded < int64(len(f.data)) {
		if time.Now().After(deadline) {
			t.Fatalf("first uploaded %d of %d bytes", first.s.Stats().Uploaded, len(f.data))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNoSeedStopsAfterDownload(t *testing.T) {
	f := makeTorrent(t, pl, 5*pl)
	sd := newSeeder(t, f, seederOpts{})
	cfg := testConfig()
	cfg.NoSeed = true
	r := start(t, f.mi, t.TempDir(), nil, cfg, false)
	r.s.AddPeers([]netip.AddrPort{sd.addr})
	select {
	case err := <-r.done:
		r.done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("NoSeed session did not return after completing")
	}
	checkOnDisk(t, f, r.dir)
	if st := r.s.Stats(); st.State != StateComplete {
		t.Errorf("state = %s, want complete", st.State)
	}
}

// --- a hand-driven leecher, to test the upload protocol precisely --------------

type rawLeecher struct {
	t *testing.T
	c *peer.Conn
}

func dialRaw(t *testing.T, addr netip.AddrPort, mi *metainfo.MetaInfo) *rawLeecher {
	t.Helper()
	c, err := peer.Dial(context.Background(), addr, mi.InfoHash, peer.Options{PeerID: peerid.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &rawLeecher{t: t, c: c}
}

// next waits for the next message with one of the given IDs, skipping others.
func (r *rawLeecher) next(ids ...peer.MessageID) *peer.Message {
	r.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case m, ok := <-r.c.Incoming():
			if !ok {
				r.t.Fatalf("connection closed: %v", r.c.Err())
			}
			for _, id := range ids {
				if m.ID == id {
					return m
				}
			}
		case <-timeout:
			r.t.Fatalf("timed out waiting for %v", ids)
			return nil
		}
	}
}

func (r *rawLeecher) expectClosed() {
	r.t.Helper()
	select {
	case <-r.c.Done():
	case <-time.After(5 * time.Second):
		r.t.Fatal("expected the seed to disconnect us")
	}
}

func (r *rawLeecher) expectSilence(d time.Duration, id peer.MessageID) {
	r.t.Helper()
	timeout := time.After(d)
	for {
		select {
		case m, ok := <-r.c.Incoming():
			if !ok {
				return
			}
			if m.ID == id {
				r.t.Fatalf("unexpected %s", id)
			}
		case <-timeout:
			return
		}
	}
}

func seedNode(t *testing.T, f *fixture, cfg Config) *node {
	dir := t.TempDir()
	return startNode(t, f.mi, dir, writeFixture(t, f, dir), cfg)
}

func TestUploadProtocol(t *testing.T) {
	f := makeTorrent(t, pl*2, pl*5+999) // 2 blocks per piece, short last piece
	seed := seedNode(t, f, testConfig())
	l := dialRaw(t, seed.ln.Addr(), f.mi)

	bf := l.next(peer.MsgBitfield)
	got, err := bitfield.FromBytes(bf.Payload, f.mi.Info.NumPieces())
	if err != nil || !got.Full() {
		t.Fatalf("seed bitfield wrong: %v", err)
	}

	l.c.Send(peer.Interested())
	l.next(peer.MsgUnchoke) // free slot → unchoked immediately

	// Request a block from the middle and the short last block.
	last := f.mi.Info.NumPieces() - 1
	lastLen := uint32(f.mi.Info.PieceSize(last))
	for _, b := range []peer.Block{{Index: 1, Begin: pl, Length: pl}, {Index: uint32(last), Begin: 0, Length: lastLen}} {
		l.c.Send(peer.Request(b))
		idx, begin, data, err := peer.ParsePiece(l.next(peer.MsgPiece))
		if err != nil || idx != b.Index || begin != b.Begin {
			t.Fatalf("piece header wrong: %d/%d %v", idx, begin, err)
		}
		off := int64(b.Index)*f.mi.Info.PieceLength + int64(b.Begin)
		if !bytes.Equal(data, f.data[off:off+int64(b.Length)]) {
			t.Fatalf("block %+v content wrong", b)
		}
	}

	// Pausing uploads chokes us; requests while choked are ignored.
	seed.s.SetUploadsPaused(true)
	l.next(peer.MsgChoke)
	l.c.Send(peer.Request(peer.Block{Index: 0, Begin: 0, Length: pl}))
	l.expectSilence(200*time.Millisecond, peer.MsgPiece)

	seed.s.SetUploadsPaused(false)
	l.next(peer.MsgUnchoke) // back in a slot (next rechoke or wake)

	deadline := time.Now().Add(5 * time.Second)
	for seed.s.Stats().Uploaded != int64(pl)+int64(lastLen) {
		if time.Now().After(deadline) {
			t.Fatalf("Uploaded = %d", seed.s.Stats().Uploaded)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestUploadRejectsBadRequests(t *testing.T) {
	f := makeTorrent(t, pl, pl*3)
	seed := seedNode(t, f, testConfig())
	for name, b := range map[string]peer.Block{
		"too long":       {Index: 0, Begin: 0, Length: maxRequestLen + 1},
		"past piece end": {Index: 0, Begin: pl - 10, Length: 100},
		"no such piece":  {Index: 99, Begin: 0, Length: 100},
		"zero length":    {Index: 0, Begin: 0, Length: 0},
	} {
		t.Run(name, func(t *testing.T) {
			l := dialRaw(t, seed.ln.Addr(), f.mi)
			l.c.Send(peer.Interested())
			l.next(peer.MsgUnchoke)
			l.c.Send(peer.Request(b))
			l.expectClosed()
		})
	}
}

func TestCancelledRequestNotSent(t *testing.T) {
	f := makeTorrent(t, pl, pl*40)
	seed := seedNode(t, f, testConfig())
	l := dialRaw(t, seed.ln.Addr(), f.mi)
	l.c.Send(peer.Interested())
	l.next(peer.MsgUnchoke)
	// Request 40 blocks and cancel every odd one right after requesting it.
	for i := 0; i < 40; i++ {
		b := peer.Block{Index: uint32(i), Begin: 0, Length: pl}
		l.c.Send(peer.Request(b))
		if i%2 == 1 {
			l.c.Send(peer.Cancel(b))
		}
	}
	// Collect until 300ms pass with nothing new.
	got := map[uint32]bool{}
	for {
		select {
		case m, ok := <-l.c.Incoming():
			if !ok {
				t.Fatalf("disconnected: %v", l.c.Err())
			}
			if m.ID == peer.MsgPiece {
				idx, _, _, _ := peer.ParsePiece(m)
				got[idx] = true
			}
			continue
		case <-time.After(300 * time.Millisecond):
		}
		break
	}
	cancelledSent := 0
	for i := 0; i < 40; i++ {
		switch {
		case i%2 == 0 && !got[uint32(i)]:
			t.Errorf("uncancelled block %d never arrived", i)
		case i%2 == 1 && got[uint32(i)]:
			cancelledSent++
		}
	}
	// A cancel may lose the race against a read that already finished (the
	// protocol allows that), but it must be the exception.
	if cancelledSent > 3 {
		t.Errorf("%d of 20 cancelled blocks were still sent", cancelledSent)
	}
}

func TestChokerLimitsSlots(t *testing.T) {
	f := makeTorrent(t, pl, pl*3)
	cfg := testConfig()
	cfg.UploadSlots = 1 // 1 regular + 1 optimistic = 2 unchoked at most
	seed := seedNode(t, f, cfg)
	var ls []*rawLeecher
	for i := 0; i < 4; i++ {
		l := dialRaw(t, seed.ln.Addr(), f.mi)
		l.c.Send(peer.Interested())
		ls = append(ls, l)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		unchoked, total := 0, 0
		for _, p := range seed.s.Peers() {
			total++
			if !p.AmChoking {
				unchoked++
			}
		}
		if total == 4 && unchoked == 2 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("unchoked %d of %d peers, want 2 of 4", unchoked, total)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestChooseUnchoked(t *testing.T) {
	a := func(i int) netip.AddrPort { return netip.AddrPortFrom(netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}), 1) }
	cands := []chokeCand{{a(1), 100}, {a(2), 500}, {a(3), 300}, {a(4), 0}, {a(5), 50}}
	first := func(int) int { return 0 }

	set, opt := chooseUnchoked(append([]chokeCand(nil), cands...), 2, netip.AddrPort{}, true, first)
	if !set[a(2)] || !set[a(3)] || len(set) != 3 {
		t.Fatalf("set = %v", set)
	}
	if set[opt] != true || opt == a(2) || opt == a(3) {
		t.Fatalf("optimistic %v should be outside the top 2", opt)
	}

	// Without rotation the current optimistic peer is kept.
	set, opt2 := chooseUnchoked(append([]chokeCand(nil), cands...), 2, a(4), false, first)
	if opt2 != a(4) || !set[a(4)] {
		t.Fatalf("optimistic not kept: %v", opt2)
	}
	// If the current optimistic peer is now in the top set, pick another.
	_, opt3 := chooseUnchoked(append([]chokeCand(nil), cands...), 2, a(2), false, first)
	if opt3 == a(2) || !opt3.IsValid() {
		t.Fatalf("opt3 = %v", opt3)
	}
	// Fewer candidates than slots: all unchoked, no optimistic.
	set, opt4 := chooseUnchoked([]chokeCand{{a(1), 1}}, 4, netip.AddrPort{}, true, first)
	if !set[a(1)] || opt4.IsValid() {
		t.Fatalf("small set: %v %v", set, opt4)
	}
	// No candidates.
	if set, _ := chooseUnchoked(nil, 4, a(1), true, first); len(set) != 0 {
		t.Fatal("empty set expected")
	}
}

func TestListenerEdgeCases(t *testing.T) {
	f := makeTorrent(t, pl, pl*2)
	seed := seedNode(t, f, testConfig())

	// Unknown torrent: the handshake is refused.
	other := makeTorrent(t, pl, pl*3)
	if _, err := peer.Dial(context.Background(), seed.ln.Addr(), other.mi.InfoHash, peer.Options{PeerID: peerid.New()}); err == nil {
		t.Error("unknown torrent accepted")
	}
	// Not BitTorrent at all: connection closed, listener keeps working.
	nc, err := net.Dial("tcp", seed.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	nc.Write([]byte("GET / HTTP/1.1\r\n\r\n"))
	nc.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := nc.Read(make([]byte, 1)); err == nil {
		t.Error("garbage connection not closed")
	}
	nc.Close()
	dialRaw(t, seed.ln.Addr(), f.mi).next(peer.MsgBitfield) // still serving

	// After Remove, the torrent is unknown.
	seed.ln.Remove(seed.s)
	if _, err := peer.Dial(context.Background(), seed.ln.Addr(), f.mi.InfoHash, peer.Options{PeerID: peerid.New()}); err == nil {
		t.Error("removed torrent still accepted")
	}
}

func TestIncomingRefusedWhenStopped(t *testing.T) {
	f := makeTorrent(t, pl, pl*2)
	seed := seedNode(t, f, testConfig())
	seed.stopAndWait(t) // listener still has it registered, but Run is over
	c, err := peer.Dial(context.Background(), seed.ln.Addr(), f.mi.InfoHash, peer.Options{PeerID: peerid.New()})
	if err != nil {
		return // also acceptable
	}
	defer c.Close()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("stopped session kept an incoming connection")
	}
}
