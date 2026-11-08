package main

import (
	"bytes"
	"crypto/sha1"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// serveSeeder runs a minimal seeder for data on a loopback port.
func serveSeeder(t *testing.T, mi *metainfo.MetaInfo, data []byte) netip.AddrPort {
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
				c, err := peer.Accept(nc, func(h metainfo.Hash) bool { return h == mi.InfoHash }, peer.Options{PeerID: peerid.New()})
				if err != nil {
					return
				}
				defer c.Close()
				bf := bitfield.New(mi.Info.NumPieces())
				bf.SetAll()
				c.Send(peer.BitfieldMsg(bf.Bytes()))
				for m := range c.Incoming() {
					switch m.ID {
					case peer.MsgInterested:
						c.Send(peer.Unchoke())
					case peer.MsgRequest:
						b, _ := peer.ParseBlock(m)
						off := int64(b.Index)*mi.Info.PieceLength + int64(b.Begin)
						c.Send(peer.Piece(b.Index, b.Begin, data[off:off+int64(b.Length)]))
					}
				}
			}()
		}
	}()
	return netip.MustParseAddrPort(ln.Addr().String())
}

func TestDownloadCommand(t *testing.T) {
	const pl = 16384
	data := make([]byte, pl*5+321)
	r := rand.New(rand.NewPCG(3, 4))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	var hashes []byte
	for off := 0; off < len(data); off += pl {
		h := sha1.Sum(data[off:min(off+pl, len(data))])
		hashes = append(hashes, h[:]...)
	}
	info := map[string]any{"name": "movie.bin", "piece length": pl, "length": len(data), "pieces": string(hashes)}
	raw, _ := bencode.Encode(map[string]any{"info": info})
	mi, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	seed := serveSeeder(t, mi, data)

	tr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		ip := seed.Addr().As4()
		p := []byte{ip[0], ip[1], ip[2], ip[3], byte(seed.Port() >> 8), byte(seed.Port())}
		body, _ := bencode.Encode(map[string]any{"interval": 60, "complete": 1, "peers": string(p)})
		w.Write(body)
	}))
	defer tr.Close()
	raw, _ = bencode.Encode(map[string]any{"announce": tr.URL, "info": info})
	tmp := t.TempDir()
	tpath := filepath.Join(tmp, "movie.torrent")
	os.WriteFile(tpath, raw, 0o644)
	dl := filepath.Join(tmp, "new", "dir") // must be created automatically

	var out, errOut bytes.Buffer
	if code := run([]string{"download", "-dir", dl, tpath}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d\nstdout:\n%s\nstderr:\n%s", code, out.String(), errOut.String())
	}
	got, err := os.ReadFile(filepath.Join(dl, "movie.bin"))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("downloaded file wrong (err=%v)", err)
	}
	if !strings.Contains(out.String(), "Done: movie.bin") {
		t.Errorf("stdout:\n%s", out.String())
	}

	// Second run without -existing refuses to touch the file...
	out.Reset()
	errOut.Reset()
	if code := run([]string{"download", "-dir", dl, tpath}, &out, &errOut); code != 1 {
		t.Fatalf("second run exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "-existing") {
		t.Errorf("stderr should suggest -existing:\n%s", errOut.String())
	}
	// ...and with -existing it verifies the data and is done immediately.
	out.Reset()
	if code := run([]string{"download", "-existing", "-dir", dl, tpath}, &out, &errOut); code != 0 {
		t.Fatalf("-existing exit = %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "6 / 6 pieces already present") {
		t.Errorf("stdout:\n%s", out.String())
	}
}

func TestDownloadCommandErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"download"}, &out, &errOut); code != 2 {
		t.Errorf("no args exit = %d", code)
	}
	if code := run([]string{"download", "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a"}, &out, &errOut); code != 1 {
		t.Errorf("magnet exit = %d", code)
	}
	if !strings.Contains(errOut.String(), "Phase 10") {
		t.Errorf("stderr = %q", errOut.String())
	}
}
