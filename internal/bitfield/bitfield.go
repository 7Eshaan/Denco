// Package bitfield is a fixed-size set of piece indexes, stored the way the
// BitTorrent wire protocol sends it: one bit per piece, high bit first, so
// piece 0 is the 0x80 bit of byte 0.
//
// It is used for our own completed pieces, each peer's advertised pieces,
// and resume data. A Bitfield is not safe for concurrent use; its owner
// (the torrent session) serialises access.
package bitfield

import (
	"errors"
	"math/bits"
)

// Bitfield holds n bits.
type Bitfield struct {
	bits []byte
	n    int
}

// ErrInvalid means a received bitfield has the wrong length or has spare bits set.
var ErrInvalid = errors.New("bitfield: invalid")

// New returns an empty bitfield of n bits.
func New(n int) *Bitfield {
	return &Bitfield{bits: make([]byte, (n+7)/8), n: n}
}

// FromBytes validates and copies a wire-format bitfield of n bits. Per BEP 3
// the length must be exactly ceil(n/8) bytes and the spare trailing bits zero.
func FromBytes(b []byte, n int) (*Bitfield, error) {
	if len(b) != (n+7)/8 {
		return nil, ErrInvalid
	}
	if rem := n % 8; rem != 0 && b[len(b)-1]&(0xff>>rem) != 0 {
		return nil, ErrInvalid
	}
	return &Bitfield{bits: append([]byte(nil), b...), n: n}, nil
}

// Len returns the number of bits.
func (b *Bitfield) Len() int { return b.n }

// Has reports whether bit i is set. Out-of-range indexes report false.
func (b *Bitfield) Has(i int) bool {
	if i < 0 || i >= b.n {
		return false
	}
	return b.bits[i/8]&(0x80>>(i%8)) != 0
}

// Set sets bit i. Out-of-range indexes are ignored.
func (b *Bitfield) Set(i int) {
	if i >= 0 && i < b.n {
		b.bits[i/8] |= 0x80 >> (i % 8)
	}
}

// Clear clears bit i. Out-of-range indexes are ignored.
func (b *Bitfield) Clear(i int) {
	if i >= 0 && i < b.n {
		b.bits[i/8] &^= 0x80 >> (i % 8)
	}
}

// SetAll sets every bit (used for seeders / "have all").
func (b *Bitfield) SetAll() {
	for i := range b.bits {
		b.bits[i] = 0xff
	}
	if rem := b.n % 8; rem != 0 {
		b.bits[len(b.bits)-1] = 0xff << (8 - rem)
	}
}

// Count returns the number of set bits.
func (b *Bitfield) Count() int {
	c := 0
	for _, x := range b.bits {
		c += bits.OnesCount8(x)
	}
	return c
}

// Full reports whether every bit is set.
func (b *Bitfield) Full() bool { return b.Count() == b.n }

// Bytes returns a copy in wire format.
func (b *Bitfield) Bytes() []byte { return append([]byte(nil), b.bits...) }

// Clone returns an independent copy.
func (b *Bitfield) Clone() *Bitfield {
	return &Bitfield{bits: b.Bytes(), n: b.n}
}
