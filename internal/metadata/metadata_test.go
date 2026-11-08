package metadata

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// infoDict returns a valid info dict spanning several metadata pieces.
func infoDict(t *testing.T, name string, numPieces int) ([]byte, metainfo.Hash) {
	t.Helper()
	raw, err := bencode.Encode(map[string]any{
		"name": name, "piece length": 16384, "length": 16384 * numPieces,
		"pieces": strings.Repeat("\x01\x02\x03\x04\x05\x06\x07\x08\x09\x0a\x0b\x0c\x0d\x0e\x0f\x10\x11\x12\x13\x14", numPieces),
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw, sha1.Sum(raw)
}

func TestMessageRoundTrip(t *testing.T) {
	for _, m := range []Message{
		{Type: Request, Piece: 3},
		{Type: Reject, Piece: 0},
		{Type: Data, Piece: 1, TotalSize: 20000, Data: []byte("raw bytes after the dict")},
	} {
		got, err := Decode(m.Encode())
		if err != nil || got.Type != m.Type || got.Piece != m.Piece || got.TotalSize != m.TotalSize || !bytes.Equal(got.Data, m.Data) {
			t.Errorf("round trip %+v → %+v, %v", m, got, err)
		}
	}
	for _, bad := range []string{"", "le", "d8:msg_typei1ee", "d8:msg_typei9e5:piecei0ee", "d8:msg_typei1e5:piecei0ee",
		"d8:msg_typei1e5:piecei0e10:total_sizei99999999ee", "d8:msg_typei0e5:piecei-1ee"} {
		if _, err := Decode([]byte(bad)); !errors.Is(err, ErrBadMessage) {
			t.Errorf("Decode(%q): %v", bad, err)
		}
	}
}

func TestAssembler(t *testing.T) {
	meta, hash := infoDict(t, "asm", 2000) // ~40 KB → 3 pieces
	a, err := NewAssembler(hash, len(meta))
	if err != nil || a.NumPieces() != 3 {
		t.Fatalf("%v, %d pieces", err, a.NumPieces())
	}
	for i := 2; i >= 0; i-- { // any order
		p, _ := Piece(meta, i)
		if err := a.Add(i, p); err != nil {
			t.Fatal(err)
		}
		a.Add(i, p) // duplicates are ignored
	}
	if !a.Complete() || a.Have() != 3 {
		t.Fatal("not complete")
	}
	if got, ok := a.Verify(); !ok || !bytes.Equal(got, meta) {
		t.Fatal("verify failed")
	}
	// Wrong lengths/indexes are errors; a bad hash resets everything.
	b, _ := NewAssembler(hash, len(meta))
	if b.Add(0, make([]byte, 10)) == nil || b.Add(3, nil) == nil || b.Add(-1, nil) == nil {
		t.Fatal("bad pieces accepted")
	}
	for i := 0; i < 3; i++ {
		p, _ := Piece(meta, i)
		p = bytes.Clone(p)
		p[0] ^= 1
		b.Add(i, p)
	}
	if _, ok := b.Verify(); ok || b.Have() != 0 || b.Complete() {
		t.Fatal("corrupt metadata verified or not reset")
	}
	if _, err := NewAssembler(hash, MaxSize+1); err == nil {
		t.Fatal("oversized metadata accepted")
	}
	if _, ok := Piece(meta, 3); ok {
		t.Fatal("piece out of range")
	}
}

// --- fake peers ------------------------------------------------------------------------

type fakeOpts struct {
	meta     []byte // what it serves (nil = no metadata_size, no ut_metadata)
	noExt    bool   // don't set the extension bit
	reject   bool   // reject every request
	silent   bool   // handshake, then never answer
	reqCount atomic.Int64
}

// fakePeer accepts connections for hash and serves metadata per o.
func fakePeer(t *testing.T, hash metainfo.Hash, o *fakeOpts) netip.AddrPort {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				c, err := peer.Accept(nc, func(h metainfo.Hash) bool { return h == hash },
					peer.Options{PeerID: peerid.New(), Extensions: !o.noExt})
				if err != nil {
					return
				}
				defer c.Close()
				const theirID = 7 // our (the fake's) ID for ut_metadata: anything but 1
				hs := peer.ExtHandshake{M: map[string]int{}}
				if o.meta != nil {
					hs.M[peer.ExtMetadata] = theirID
					hs.MetadataSize = len(o.meta)
				}
				c.Send(peer.Extended(0, hs.Encode()))
				peerID := 0
				for m := range c.Incoming() {
					id, body, err := peer.ParseExtended(m)
					if err != nil {
						continue
					}
					if id == 0 {
						h, _ := peer.ParseExtHandshake(body)
						peerID = h.M[peer.ExtMetadata]
						continue
					}
					if id != theirID || o.silent {
						continue
					}
					msg, err := Decode(body)
					if err != nil || msg.Type != Request {
						continue
					}
					o.reqCount.Add(1)
					if o.reject {
						c.Send(peer.Extended(uint8(peerID), Message{Type: Reject, Piece: msg.Piece}.Encode()))
						continue
					}
					p, _ := Piece(o.meta, msg.Piece)
					c.Send(peer.Extended(uint8(peerID), Message{Type: Data, Piece: msg.Piece, TotalSize: len(o.meta), Data: p}.Encode()))
				}
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func testConfig() Config {
	return Config{
		PeerOpts:       peer.Options{PeerID: peerid.New()},
		Port:           6881,
		Retry:          50 * time.Millisecond,
		RequestTimeout: 500 * time.Millisecond,
	}
}

func fetch(t *testing.T, hash metainfo.Hash, tiers [][]string, peers []netip.AddrPort, timeout time.Duration) ([]byte, error, *Fetcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	f := NewFetcher(hash, tiers, peers, testConfig())
	data, err := f.Run(ctx)
	return data, err, f
}

func TestFetchFromPeers(t *testing.T) {
	meta, hash := infoDict(t, "good", 2000)
	other, _ := infoDict(t, "other torrent, same size!", 2000)
	other = other[:len(meta)] // same claimed size, wrong content

	good := &fakeOpts{meta: meta}
	liar := &fakeOpts{meta: other}
	peers := []netip.AddrPort{
		fakePeer(t, hash, &fakeOpts{noExt: true}),
		fakePeer(t, hash, &fakeOpts{}), // extensions but no metadata
		fakePeer(t, hash, &fakeOpts{meta: meta, reject: true}),
		fakePeer(t, hash, &fakeOpts{meta: meta, silent: true}),
		fakePeer(t, hash, liar),
		fakePeer(t, hash, good),
		netip.MustParseAddrPort("127.0.0.1:1"), // nothing listening
	}
	got, err, f := fetch(t, hash, nil, peers, 10*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, meta) {
		t.Fatal("wrong metadata returned")
	}
	if good.reqCount.Load() == 0 {
		t.Fatal("the honest peer was never asked")
	}
	// Regression (D38): honest peers must never be blamed for a liar's data.
	if f.isBanned(peers[5]) {
		t.Fatal("the honest peer was banned")
	}
	if st := f.Status(); st.Total != 3 || st.Have != 3 {
		t.Errorf("status = %+v", st)
	}
}

func TestFetchFindsPeersThroughTracker(t *testing.T) {
	meta, hash := infoDict(t, "tracked", 100)
	addr := fakePeer(t, hash, &fakeOpts{meta: meta})
	var announces atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		announces.Add(1)
		ip := addr.Addr().As4()
		body, _ := bencode.Encode(map[string]any{"interval": 60,
			"peers": string(binary.BigEndian.AppendUint16(ip[:], addr.Port()))})
		w.Write(body)
	}))
	defer srv.Close()
	got, err, _ := fetch(t, hash, [][]string{{srv.URL}}, nil, 10*time.Second)
	if err != nil || !bytes.Equal(got, meta) {
		t.Fatalf("err = %v", err)
	}
	if announces.Load() == 0 {
		t.Fatal("tracker never asked")
	}
}

func TestFetchGivesUpOnlyWhenCancelled(t *testing.T) {
	_, hash := infoDict(t, "nobody", 10)
	silent := fakePeer(t, hash, &fakeOpts{noExt: true})
	_, err, f := fetch(t, hash, nil, []netip.AddrPort{silent}, 300*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if st := f.Status(); st.Peers != 0 || st.Total != 0 {
		t.Errorf("status = %+v", st)
	}
	// No trackers and no peers: Status explains why nothing happens.
	f2 := NewFetcher(hash, nil, nil, testConfig())
	if st := f2.Status(); !strings.Contains(st.TrackerError, "DHT") {
		t.Errorf("status = %+v", st)
	}
}

func TestFetchRecoversFromWrongMetadata(t *testing.T) {
	meta, hash := infoDict(t, "real", 2000)
	other, _ := infoDict(t, "fake torrent with equal size", 2000)
	other = other[:len(meta)]
	liar := &fakeOpts{meta: other}
	good := &fakeOpts{meta: meta}
	cfg := testConfig()
	cfg.Parallel = 1 // the liar (first in line) delivers everything first
	f := NewFetcher(hash, nil, []netip.AddrPort{fakePeer(t, hash, liar), fakePeer(t, hash, good)}, cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := f.Run(ctx)
	if err != nil || !bytes.Equal(got, meta) {
		t.Fatalf("err = %v", err)
	}
	if liar.reqCount.Load() < 3 {
		t.Fatalf("liar answered %d requests; the test didn't exercise the bad-hash path", liar.reqCount.Load())
	}
	f.mu.Lock()
	banned := len(f.banned)
	f.mu.Unlock()
	if banned != 1 {
		t.Fatalf("banned = %d, want the liar only", banned)
	}
}
