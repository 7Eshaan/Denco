package peer

import (
	"errors"
	"net/netip"
	"slices"
	"testing"

	"github.com/7Eshaan/Denco/internal/bencode"
)

func TestPEXRoundTrip(t *testing.T) {
	in := PEX{
		Added: []netip.AddrPort{
			netip.MustParseAddrPort("1.2.3.4:6881"),
			netip.MustParseAddrPort("[2001:db8::1]:51413"),
			netip.MustParseAddrPort("[::ffff:5.6.7.8]:80"), // IPv4-mapped: sent as IPv4
		},
		AddedFlags: []byte{PEXSeed | PEXReachable, PEXUTP, 0},
		Dropped: []netip.AddrPort{
			netip.MustParseAddrPort("9.9.9.9:1"),
			netip.MustParseAddrPort("[2001:db8::2]:2"),
		},
	}
	out, err := ParsePEX(in.Encode())
	if err != nil {
		t.Fatal(err)
	}
	// Parsed back IPv4 first, then IPv6.
	wantAdded := []netip.AddrPort{in.Added[0], netip.MustParseAddrPort("5.6.7.8:80"), in.Added[1]}
	if !slices.Equal(out.Added, wantAdded) {
		t.Errorf("added = %v, want %v", out.Added, wantAdded)
	}
	if want := []byte{PEXSeed | PEXReachable, 0, PEXUTP}; !slices.Equal(out.AddedFlags, want) {
		t.Errorf("flags = %v, want %v", out.AddedFlags, want)
	}
	if !slices.Equal(out.Dropped, in.Dropped) {
		t.Errorf("dropped = %v, want %v", out.Dropped, in.Dropped)
	}
}

func TestPEXEncodeAlwaysHasIPv4Keys(t *testing.T) {
	v, err := bencode.Decode(PEX{}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	d := v.(map[string]any)
	for _, k := range []string{"added", "added.f", "dropped"} {
		if _, ok := d[k]; !ok {
			t.Errorf("missing key %q", k)
		}
	}
	if _, ok := d["added6"]; ok {
		t.Error("added6 present without IPv6 peers")
	}
}

func TestParsePEXLenient(t *testing.T) {
	body, _ := bencode.Encode(map[string]any{
		// good, port 0 (skipped), unspecified IP (skipped), good, then a partial entry
		"added": "\x01\x02\x03\x04\x1a\xe1" + "\x05\x06\x07\x08\x00\x00" + "\x00\x00\x00\x00\x00\x50" +
			"\x0a\x00\x00\x01\x00\x50" + "\x0b\x0b",
		"added.f": "\x02\x00\x00\x10", // one flag per entry, aligned with the raw list
		"dropped": 7,                  // wrong type: ignored
		"extra":   "whatever",
	})
	m, err := ParsePEX(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.AddrPort{netip.MustParseAddrPort("1.2.3.4:6881"), netip.MustParseAddrPort("10.0.0.1:80")}
	if !slices.Equal(m.Added, want) {
		t.Fatalf("added = %v, want %v", m.Added, want)
	}
	if m.Flags(0) != PEXSeed || m.Flags(1) != PEXReachable {
		t.Errorf("flags = %v; misaligned after skipped entries", m.AddedFlags)
	}
	if len(m.Dropped) != 0 {
		t.Errorf("dropped = %v", m.Dropped)
	}
}

func TestParsePEXRejects(t *testing.T) {
	for name, body := range map[string][]byte{
		"not a dict": []byte("le"),
		"garbage":    []byte("d5:added"),
		"too large":  append([]byte("d5:added70000:"), make([]byte, 70000)...),
	} {
		if _, err := ParsePEX(body); !errors.Is(err, ErrBadPayload) {
			t.Errorf("%s: err = %v, want ErrBadPayload", name, err)
		}
	}
}
