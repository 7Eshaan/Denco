package cli

import (
	"bytes"
	"encoding/binary"
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

func compactPeer(ap netip.AddrPort) string {
	b := ap.Addr().As4()
	var p [2]byte
	binary.BigEndian.PutUint16(p[:], ap.Port())
	return string(b[:]) + string(p[:])
}

func TestProbeCommand(t *testing.T) {
	const numPieces = 3
	data, _ := bencode.Encode(map[string]any{
		"info": map[string]any{
			"name": "demo", "piece length": 16384, "length": 16384*2 + 10,
			"pieces": strings.Repeat("x", 20*numPieces),
		},
	})
	mi, err := metainfo.Parse(data)
	if err != nil {
		t.Fatal(err)
	}

	// Fake seeder: accepts one connection, sends a full bitfield.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, err := ln.Accept()
		if err != nil {
			return
		}
		var seederID peerid.ID
		copy(seederID[:], "-qB4630-abcdefghijkl")
		c, err := peer.Accept(nc, func(h metainfo.Hash) bool { return h == mi.InfoHash }, peer.Options{PeerID: seederID})
		if err != nil {
			return
		}
		bf := bitfield.New(numPieces)
		bf.SetAll()
		c.Send(peer.BitfieldMsg(bf.Bytes()))
		<-c.Done()
	}()
	seeder := netip.MustParseAddrPort(ln.Addr().String())

	// Dead peer: a port nobody listens on.
	dl, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := netip.MustParseAddrPort(dl.Addr().String())
	dl.Close()

	tr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := bencode.Encode(map[string]any{"interval": 1800, "peers": compactPeer(seeder) + compactPeer(dead)})
		w.Write(body)
	}))
	defer tr.Close()

	data, _ = bencode.Encode(map[string]any{"announce": tr.URL, "info": bencode.Raw(mi.InfoBytes)})
	path := filepath.Join(t.TempDir(), "demo.torrent")
	os.WriteFile(path, data, 0o644)

	var out, errOut bytes.Buffer
	if code := run([]string{"probe", "-wait", "2s", path}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	s := out.String()
	for _, want := range []string{"Probing 2 peers", "qBittorrent 4.6.3.0", "100% (3/3)", "✗", "1 of 2 peers handshook"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// Successful peer listed before the failed one.
	if strings.Index(s, seeder.String()) > strings.Index(s, dead.String()) {
		t.Errorf("successes should come first:\n%s", s)
	}
}

func TestProbeNoPeers(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"probe", "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a"}, &out, &errOut); code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no peers") {
		t.Errorf("stderr = %q", errOut.String())
	}
	if code := run([]string{"probe"}, &out, &errOut); code != 2 {
		t.Errorf("no args exit = %d", code)
	}
}
