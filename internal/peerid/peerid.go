// Package peerid generates the 20-byte peer ID that identifies this client to
// trackers and peers. It uses the common Azureus style: "-DN0001-" followed by
// 12 random alphanumeric characters, where "DN" is Denco's client code and
// "0001" the version (0.0.0.1).
package peerid

import (
	"crypto/rand"
	"fmt"
)

// Prefix identifies Denco and its version to other clients.
const Prefix = "-DN0001-"

// ID is a 20-byte peer ID.
type ID [20]byte

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// New returns a fresh random peer ID. A new one is generated each time the
// daemon starts, which is standard and avoids being tracked across sessions.
func New() ID {
	var id ID
	copy(id[:], Prefix)
	var random [20 - len(Prefix)]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand only fails if the OS entropy source is broken.
		panic(fmt.Sprintf("peerid: crypto/rand failed: %v", err))
	}
	for i, b := range random {
		id[len(Prefix)+i] = alphabet[int(b)%len(alphabet)]
	}
	return id
}

// String returns the ID as text (it is always printable ASCII).
func (id ID) String() string { return string(id[:]) }
