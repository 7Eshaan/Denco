package magnet

import (
	"errors"
	"reflect"
	"testing"
)

const (
	hexHash = "c12fe1c06bba254a9dc9f519b335aa7c1367a88a"
	// Same 20 bytes in base32.
	b32Hash = "YEX6DQDLXISUVHOJ6UM3GNNKPQJWPKEK"
)

func TestParseFull(t *testing.T) {
	uri := "magnet:?xt=urn:btih:" + hexHash +
		"&dn=Ubuntu+24.04+Desktop" +
		"&tr=http%3A%2F%2Ftracker.example%2Fannounce" +
		"&tr=udp%3A%2F%2Fudp.example%3A80" +
		"&tr=http%3A%2F%2Ftracker.example%2Fannounce" + // duplicate
		"&x.pe=10.0.0.1:6881&x.pe=[::1]:51413&x.pe=host.example:1"
	m, err := Parse(uri)
	if err != nil {
		t.Fatal(err)
	}
	if m.InfoHash.String() != hexHash {
		t.Errorf("hash = %s", m.InfoHash)
	}
	if m.Name != "Ubuntu 24.04 Desktop" {
		t.Errorf("name = %q", m.Name)
	}
	wantTr := []string{"http://tracker.example/announce", "udp://udp.example:80"}
	if !reflect.DeepEqual(m.Trackers, wantTr) {
		t.Errorf("trackers = %v", m.Trackers)
	}
	if len(m.Peers) != 2 || m.Peers[0].String() != "10.0.0.1:6881" || m.Peers[1].String() != "[::1]:51413" {
		t.Errorf("peers = %v", m.Peers)
	}
}

func TestParseBase32AndCase(t *testing.T) {
	for _, uri := range []string{
		"magnet:?xt=urn:btih:" + b32Hash,
		"magnet:?xt=urn:btih:yex6dqdlxisuvhoj6um3gnnkpqjwpkek", // lowercase base32
		"magnet:?xt=urn:btih:" + "C12FE1C06BBA254A9DC9F519B335AA7C1367A88A",
		"MAGNET:?xt=URN:BTIH:" + hexHash, // scheme and urn prefix are case-insensitive
	} {
		m, err := Parse(uri)
		if err != nil {
			t.Errorf("%s: %v", uri, err)
			continue
		}
		if m.InfoHash.String() != hexHash {
			t.Errorf("%s: hash = %s", uri, m.InfoHash)
		}
	}
}

func TestParseIgnoresOtherXT(t *testing.T) {
	m, err := Parse("magnet:?xt=urn:btmh:1220abcd&xt=urn:btih:" + hexHash)
	if err != nil {
		t.Fatal(err)
	}
	if m.InfoHash.String() != hexHash {
		t.Error("wrong hash")
	}
}

func TestParseInvalid(t *testing.T) {
	for _, uri := range []string{
		"",
		"http://example.com/?xt=urn:btih:" + hexHash,
		"magnet:?dn=nohash",
		"magnet:?xt=urn:btih:abc",
		"magnet:?xt=urn:btih:" + hexHash[:39] + "z",
		"magnet:?xt=urn:btih:" + "1" + b32Hash[1:], // '1' is not base32
		"magnet:?xt=urn:btih:" + hexHash + "&xt=urn:btih:" + b32Hash[:31] + "A",
		"magnet:?xt=urn:btih:%zz",
	} {
		if _, err := Parse(uri); !errors.Is(err, ErrInvalid) {
			t.Errorf("Parse(%q) = %v, want ErrInvalid", uri, err)
		}
	}
}

func TestStringRoundTrip(t *testing.T) {
	in := "magnet:?xt=urn:btih:" + hexHash + "&dn=a+b%26c&tr=http%3A%2F%2Ft%2Fa%3Fx%3D1&x.pe=10.0.0.1%3A1"
	m, err := Parse(in)
	if err != nil {
		t.Fatal(err)
	}
	m2, err := Parse(m.String())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m, m2) {
		t.Errorf("round trip:\n%+v\n%+v", m, m2)
	}
	if m.Name != "a b&c" || m.Trackers[0] != "http://t/a?x=1" {
		t.Errorf("decoding: %+v", m)
	}
}

func TestIsMagnet(t *testing.T) {
	if !IsMagnet("magnet:?xt=x") || !IsMagnet("Magnet:?") || IsMagnet("file.torrent") || IsMagnet("magnet") {
		t.Error("IsMagnet")
	}
}
