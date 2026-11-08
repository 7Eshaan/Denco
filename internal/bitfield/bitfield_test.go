package bitfield

import (
	"bytes"
	"errors"
	"testing"
)

func TestSetHasClear(t *testing.T) {
	b := New(10)
	if b.Len() != 10 || b.Count() != 0 || len(b.Bytes()) != 2 {
		t.Fatal("new bitfield wrong")
	}
	b.Set(0)
	b.Set(9)
	b.Set(-1) // ignored
	b.Set(10) // ignored
	if !b.Has(0) || !b.Has(9) || b.Has(1) || b.Has(10) || b.Has(-1) {
		t.Fatal("Has wrong")
	}
	// Piece 0 is the high bit of byte 0; piece 9 is 0x40 of byte 1.
	if !bytes.Equal(b.Bytes(), []byte{0x80, 0x40}) {
		t.Fatalf("bytes = %x", b.Bytes())
	}
	if b.Count() != 2 {
		t.Fatalf("Count = %d", b.Count())
	}
	b.Clear(0)
	b.Clear(99)
	if b.Has(0) || b.Count() != 1 {
		t.Fatal("Clear wrong")
	}
}

func TestSetAllAndFull(t *testing.T) {
	for _, n := range []int{1, 7, 8, 9, 16, 17} {
		b := New(n)
		b.SetAll()
		if !b.Full() || b.Count() != n {
			t.Errorf("n=%d: Full=%v Count=%d", n, b.Full(), b.Count())
		}
		// Spare bits must stay zero so the result is valid on the wire.
		if _, err := FromBytes(b.Bytes(), n); err != nil {
			t.Errorf("n=%d: SetAll produced invalid wire bytes %x", n, b.Bytes())
		}
	}
	if New(0).Full() != true {
		t.Error("empty bitfield is trivially full")
	}
}

func TestFromBytes(t *testing.T) {
	b, err := FromBytes([]byte{0xa0, 0x80}, 9)
	if err != nil {
		t.Fatal(err)
	}
	if !b.Has(0) || !b.Has(2) || !b.Has(8) || b.Count() != 3 {
		t.Fatalf("decoded wrong: %x", b.Bytes())
	}
	for name, tc := range map[string]struct {
		b []byte
		n int
	}{
		"too short":     {[]byte{0xff}, 9},
		"too long":      {[]byte{0xff, 0, 0}, 9},
		"spare bit set": {[]byte{0xff, 0xc0}, 9},
	} {
		if _, err := FromBytes(tc.b, tc.n); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestCloneIndependent(t *testing.T) {
	a := New(8)
	a.Set(1)
	c := a.Clone()
	c.Set(2)
	if a.Has(2) || !c.Has(1) {
		t.Fatal("clone not independent")
	}
	// Bytes is a copy too.
	raw := a.Bytes()
	raw[0] = 0xff
	if a.Count() != 1 {
		t.Fatal("Bytes aliased internal storage")
	}
}

func TestCovers(t *testing.T) {
	b := New(10)
	b.Set(1)
	b.Set(9)
	sub := New(10)
	sub.Set(9)
	if !b.Covers(sub) || !b.Covers(New(10)) {
		t.Error("b should cover its subset and the empty set")
	}
	sub.Set(2)
	if b.Covers(sub) {
		t.Error("b covers a set with a bit it lacks")
	}
	if b.Covers(nil) {
		t.Error("nil means every bit; b isn't full")
	}
	b.SetAll()
	if !b.Covers(nil) || b.Covers(New(11)) {
		t.Error("full b must cover nil; different lengths never cover")
	}
}
