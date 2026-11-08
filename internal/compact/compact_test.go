package compact

import (
	"net/netip"
	"slices"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	v4 := netip.MustParseAddrPort("1.2.3.4:6881")
	v6 := netip.MustParseAddrPort("[2001:db8::1]:51413")
	mapped := netip.MustParseAddrPort("[::ffff:5.6.7.8]:80")

	b := Append(Append(nil, v4), mapped)
	if len(b) != 2*IPv4Len {
		t.Fatalf("IPv4 entries are %d bytes, want %d", len(b), 2*IPv4Len)
	}
	got := Parse(string(b)+"\x01\x02", 4) // trailing partial entry ignored
	if want := []netip.AddrPort{v4, netip.MustParseAddrPort("5.6.7.8:80")}; !slices.Equal(got, want) {
		t.Errorf("IPv4: %v, want %v", got, want)
	}
	if got := Parse(string(Append(nil, v6)), 16); len(got) != 1 || got[0] != v6 {
		t.Errorf("IPv6: %v", got)
	}
}

func TestUsable(t *testing.T) {
	for s, want := range map[string]bool{
		"1.2.3.4:1":       true,
		"1.2.3.4:0":       false,
		"0.0.0.0:80":      false,
		"224.0.0.1:80":    false,
		"[::]:80":         false,
		"[2001:db8::1]:9": true,
	} {
		if got := Usable(netip.MustParseAddrPort(s)); got != want {
			t.Errorf("Usable(%s) = %v", s, got)
		}
	}
	if Usable(netip.AddrPort{}) {
		t.Error("zero AddrPort usable")
	}
}
