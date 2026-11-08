package torrent

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/metadata"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// A session serves its info dict to a magnet-link fetcher (BEP 9).
func TestSessionServesMetadata(t *testing.T) {
	// Many pieces, so the info dict spans several 16 KiB metadata pieces.
	f := makeTorrent(t, pl, 2000*pl)
	if len(f.mi.InfoBytes) <= metadata.PieceSize {
		t.Fatalf("info dict only %d bytes; want several metadata pieces", len(f.mi.InfoBytes))
	}
	cfg := testConfig()
	cfg.PeerOpts.Extensions = true
	seedDir := t.TempDir()
	seed := startNode(t, f.mi, seedDir, writeFixture(t, f, seedDir), cfg)

	fetcher := metadata.NewFetcher(f.mi.InfoHash, nil, []netip.AddrPort{seed.ln.Addr()}, metadata.Config{
		PeerOpts: peer.Options{PeerID: peerid.New()}, Retry: 50 * time.Millisecond,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := fetcher.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, f.mi.InfoBytes) {
		t.Fatal("served metadata differs from the info dict")
	}
}

// Requests for pieces that don't exist are rejected; peers without the
// extension bit never get an extension handshake.
func TestSessionRejectsBadMetadataRequests(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	cfg := testConfig()
	cfg.PeerOpts.Extensions = true
	seedDir := t.TempDir()
	seed := startNode(t, f.mi, seedDir, writeFixture(t, f, seedDir), cfg)

	c, err := peer.Dial(context.Background(), seed.ln.Addr(), f.mi.InfoHash, peer.Options{PeerID: peerid.New(), Extensions: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const myID = 9
	c.Send(peer.Extended(0, peer.ExtHandshake{M: map[string]int{peer.ExtMetadata: myID}}.Encode()))
	var hs peer.ExtHandshake
	got := waitExtended(t, c, func(id uint8, body []byte) bool {
		if id == 0 {
			hs, _ = peer.ParseExtHandshake(body)
			return true
		}
		return false
	})
	if !got || hs.MetadataSize != len(f.mi.InfoBytes) || hs.M[peer.ExtMetadata] != peer.ExtMetadataID {
		t.Fatalf("handshake = %+v", hs)
	}
	c.Send(peer.Extended(peer.ExtMetadataID, metadata.Message{Type: metadata.Request, Piece: 5}.Encode()))
	var reply metadata.Message
	waitExtended(t, c, func(id uint8, body []byte) bool {
		if id != myID {
			return false
		}
		reply, _ = metadata.Decode(body)
		return true
	})
	if reply.Type != metadata.Reject || reply.Piece != 5 {
		t.Fatalf("reply = %+v, want reject of piece 5", reply)
	}

	// A peer without the extension bit: no extended messages at all.
	plain, err := peer.Dial(context.Background(), seed.ln.Addr(), f.mi.InfoHash, peer.Options{PeerID: peerid.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	timeout := time.After(300 * time.Millisecond)
	for {
		select {
		case m := <-plain.Incoming():
			if m != nil && m.ID == peer.MsgExtended {
				t.Fatal("sent an extended message to a peer without extension support")
			}
		case <-timeout:
			return
		}
	}
}

// waitExtended reads until match accepts an extended message (true) or
// the connection is quiet for 5s (false).
func waitExtended(t *testing.T, c *peer.Conn, match func(uint8, []byte) bool) bool {
	t.Helper()
	for {
		select {
		case m, ok := <-c.Incoming():
			if !ok {
				t.Fatalf("connection closed: %v", c.Err())
			}
			if m.ID != peer.MsgExtended {
				continue
			}
			id, body, _ := peer.ParseExtended(m)
			if match(id, body) {
				return true
			}
		case <-time.After(5 * time.Second):
			return false
		}
	}
}
