package torrent

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/storage"
)

// --- torrent fixture --------------------------------------------------------------

type fixture struct {
	data []byte
	mi   *metainfo.MetaInfo
}

// makeTorrent builds a multi-file torrent with random content.
func makeTorrent(t *testing.T, pieceLen int, fileSizes ...int) *fixture {
	t.Helper()
	total := 0
	for _, n := range fileSizes {
		total += n
	}
	data := make([]byte, total)
	r := rand.New(rand.NewPCG(uint64(total), 7))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	var hashes []byte
	for off := 0; off < total; off += pieceLen {
		h := sha1.Sum(data[off:min(off+pieceLen, total)])
		hashes = append(hashes, h[:]...)
	}
	var files []any
	for i, n := range fileSizes {
		files = append(files, map[string]any{"length": n, "path": []any{"dir", "file" + string(rune('A'+i))}})
	}
	raw, err := bencode.Encode(map[string]any{"info": map[string]any{
		"name": "fixture", "piece length": pieceLen, "pieces": string(hashes), "files": files,
	}})
	if err != nil {
		t.Fatal(err)
	}
	mi, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return &fixture{data: data, mi: mi}
}

// withTracker returns a copy of the metainfo announcing to trackerURL.
func (f *fixture) withTracker(t *testing.T, trackerURL string) *metainfo.MetaInfo {
	t.Helper()
	raw, _ := bencode.Encode(map[string]any{"announce": trackerURL, "info": bencode.Raw(f.mi.InfoBytes)})
	mi, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return mi
}

// --- fake seeder -----------------------------------------------------------------

type seederOpts struct {
	pieces       []int // pieces it has; nil = all
	corrupt      bool  // flip a byte in every block it sends
	dropAfter    int   // close each connection after this many blocks (0 = never)
	neverRespond bool  // unchoke but ignore requests
}

type seeder struct {
	t    *testing.T
	f    *fixture
	opts seederOpts
	ln   net.Listener
	addr netip.AddrPort

	accepts    atomic.Int64
	blocksSent atomic.Int64
	mu         sync.Mutex
	requested  map[uint32]bool // piece indexes ever requested
}

func newSeeder(t *testing.T, f *fixture, opts seederOpts) *seeder {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &seeder{t: t, f: f, opts: opts, ln: ln, requested: map[uint32]bool{},
		addr: netip.MustParseAddrPort(ln.Addr().String())}
	t.Cleanup(func() { ln.Close() })
	go s.serve()
	return s
}

func (s *seeder) has() *bitfield.Bitfield {
	bf := bitfield.New(s.f.mi.Info.NumPieces())
	if s.opts.pieces == nil {
		bf.SetAll()
	}
	for _, i := range s.opts.pieces {
		bf.Set(i)
	}
	return bf
}

func (s *seeder) serve() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.accepts.Add(1)
		go s.handle(nc)
	}
}

func (s *seeder) handle(nc net.Conn) {
	known := func(h metainfo.Hash) bool { return h == s.f.mi.InfoHash }
	c, err := peer.Accept(nc, known, peer.Options{PeerID: peerid.New()})
	if err != nil {
		return
	}
	defer c.Close()
	has := s.has()
	c.Send(peer.BitfieldMsg(has.Bytes()))
	sent := 0
	info := &s.f.mi.Info
	for m := range c.Incoming() {
		switch m.ID {
		case peer.MsgInterested:
			c.Send(peer.Unchoke())
		case peer.MsgRequest:
			b, err := peer.ParseBlock(m)
			if err != nil {
				return
			}
			s.mu.Lock()
			s.requested[b.Index] = true
			s.mu.Unlock()
			if s.opts.neverRespond || !has.Has(int(b.Index)) {
				continue
			}
			off := int64(b.Index)*info.PieceLength + int64(b.Begin)
			block := bytes.Clone(s.f.data[off : off+int64(b.Length)])
			if s.opts.corrupt {
				block[0] ^= 0xff
			}
			c.Send(peer.Piece(b.Index, b.Begin, block))
			s.blocksSent.Add(1)
			sent++
			if s.opts.dropAfter > 0 && sent >= s.opts.dropAfter {
				time.Sleep(10 * time.Millisecond) // let the piece flush
				return
			}
		}
	}
}

func (s *seeder) wasRequested(i int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.requested[uint32(i)]
}

// --- fake tracker ------------------------------------------------------------------

type fakeTracker struct {
	srv      *httptest.Server
	mu       sync.Mutex
	events   []string
	peers    []netip.AddrPort
	seeders  int
	leechers int
}

func newTracker(t *testing.T, peers ...netip.AddrPort) *fakeTracker {
	ft := &fakeTracker{peers: peers, seeders: len(peers)}
	ft.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		ft.mu.Lock()
		ft.events = append(ft.events, q.Get("event"))
		var compact []byte
		for _, p := range ft.peers {
			ip := p.Addr().As4()
			compact = append(compact, ip[:]...)
			compact = binary.BigEndian.AppendUint16(compact, p.Port())
		}
		body, _ := bencode.Encode(map[string]any{
			"interval": 60, "complete": ft.seeders, "incomplete": ft.leechers, "peers": string(compact),
		})
		ft.mu.Unlock()
		w.Write(body)
	}))
	t.Cleanup(ft.srv.Close)
	return ft
}

func (ft *fakeTracker) eventLog() []string {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return append([]string(nil), ft.events...)
}

// --- running a session ------------------------------------------------------------------

func testConfig() Config {
	return Config{
		PeerID:         peerid.New(),
		Port:           6881,
		Tick:           20 * time.Millisecond,
		RequestTimeout: 2 * time.Second,
		RetryBase:      50 * time.Millisecond,
		AnnounceRetry:  50 * time.Millisecond,
	}
}

type running struct {
	s    *Session
	st   *storage.Storage
	dir  string
	stop context.CancelFunc
	done chan error
}

func start(t *testing.T, mi *metainfo.MetaInfo, dir string, have *bitfield.Bitfield, cfg Config, allowExisting bool) *running {
	t.Helper()
	layout, err := storage.NewLayout(dir, &mi.Info)
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(layout, storage.Options{AllowExisting: allowExisting})
	if err != nil {
		t.Fatal(err)
	}
	s := New(mi, st, have, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	r := &running{s: s, st: st, dir: dir, stop: cancel, done: make(chan error, 1)}
	go func() { r.done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-r.done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return after cancel")
		}
		st.Close()
	})
	return r
}

func (r *running) waitComplete(t *testing.T, timeout time.Duration) {
	t.Helper()
	select {
	case <-r.s.Completed():
	case err := <-r.done:
		t.Fatalf("Run returned early: %v", err)
	case <-time.After(timeout):
		t.Fatalf("download did not complete in %v; stats: %+v", timeout, r.s.Stats())
	}
}

// stopAndWait cancels and waits for Run, returning its error.
func (r *running) stopAndWait(t *testing.T) error {
	t.Helper()
	r.stop()
	select {
	case err := <-r.done:
		r.done <- err // keep it for the cleanup
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after cancel")
		return nil
	}
}

// checkOnDisk compares every file under dir with the fixture's data.
func checkOnDisk(t *testing.T, f *fixture, dir string) {
	t.Helper()
	for _, file := range f.mi.Info.Files {
		p := filepath.Join(append([]string{dir, f.mi.Info.Name}, file.Path...)...)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		if !bytes.Equal(got, f.data[file.Offset:file.Offset+file.Length]) {
			t.Fatalf("%s: content differs", p)
		}
	}
}
