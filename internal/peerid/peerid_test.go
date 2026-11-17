package peerid

import (
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/version"
)

func TestNew(t *testing.T) {
	a, b := New(), New()
	if a == b {
		t.Fatal("two IDs should differ")
	}
	s := a.String()
	t.Logf("peer ID %s (prefix %s)", s, Prefix)
	if len(s) != 20 || !strings.HasPrefix(s, Prefix) {
		t.Fatalf("bad id %q", s)
	}
	for _, c := range s[len(Prefix):] {
		if !strings.ContainsRune(alphabet, c) {
			t.Fatalf("non-alphanumeric char %q in %q", c, s)
		}
	}
}

func TestClientName(t *testing.T) {
	mk := func(s string) ID {
		var id ID
		copy(id[:], s)
		return id
	}
	tests := map[string]string{
		"-qB4630-abcdefghijkl": "qBittorrent 4.6.3.0",
		"-TR4060-abcdefghijkl": "Transmission 4.0.6.0",
		"-ZZ0100-abcdefghijkl": "unknown (ZZ) 0.1.0.0",
		"M7-2-0--abcdefghijkl": "unknown",
	}
	for in, want := range tests {
		if got := ClientName(mk(in)); got != want {
			t.Errorf("ClientName(%q) = %q, want %q", in, got, want)
		}
	}
	// Our own ID decodes to Denco and the version digits (0.1.0 → 0.1.0.0).
	d := version.PeerIDDigits()
	if got, want := ClientName(New()), "Denco "+string([]byte{d[0], '.', d[1], '.', d[2], '.', d[3]}); got != want {
		t.Errorf("our own ID = %q, want %q", got, want)
	}
}
