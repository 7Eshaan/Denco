package torrent

import (
	"context"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/storage"
)

// A peer that connects after the session is registered with the listener
// but before Run has started must be served once Run starts, not hung up
// on. The engine registers a session just before running it, and CI once
// failed TestSessionRejectsBadMetadataRequests on exactly this gap.
func TestIncomingBeforeRunIsServed(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl)
	dir := t.TempDir()
	have := writeFixture(t, f, dir)
	cfg := testConfig()
	layout, err := storage.NewLayout(dir, &f.mi.Info)
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(layout, storage.Options{AllowExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := New(f.mi, st, have, cfg)

	opts := cfg.PeerOpts
	opts.PeerID = cfg.PeerID
	ln, err := Listen("127.0.0.1:0", opts, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ln.Add(s) // registered, but Run hasn't started

	c, err := peer.Dial(context.Background(), ln.Addr(), f.mi.InfoHash, peer.Options{PeerID: peerid.New()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(100 * time.Millisecond) // the listener hands the connection to the session meanwhile

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() { cancel(); <-done }()

	// A seed's first message is its bitfield.
	select {
	case m, ok := <-c.Incoming():
		if !ok {
			t.Fatalf("the session hung up on a peer that connected before Run: %v", c.Err())
		}
		if m.ID != peer.MsgBitfield {
			t.Fatalf("first message %s, want bitfield", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no bitfield from the session")
	}
}
