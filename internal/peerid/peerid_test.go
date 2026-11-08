package peerid

import (
	"strings"
	"testing"
)

func TestNew(t *testing.T) {
	a, b := New(), New()
	if a == b {
		t.Fatal("two IDs should differ")
	}
	s := a.String()
	if len(s) != 20 || !strings.HasPrefix(s, Prefix) {
		t.Fatalf("bad id %q", s)
	}
	for _, c := range s[len(Prefix):] {
		if !strings.ContainsRune(alphabet, c) {
			t.Fatalf("non-alphanumeric char %q in %q", c, s)
		}
	}
}
