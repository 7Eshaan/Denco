// Package peerid generates the 20-byte peer ID that identifies this client to
// trackers and peers. It uses the common Azureus style: "-DN0100-" followed by
// 12 random alphanumeric characters, where "DN" is Denco's client code and
// "0100" the version (0.1.0 → 0.1.0.0; see version.PeerIDDigits).
package peerid

import (
	"crypto/rand"
	"fmt"

	"github.com/7Eshaan/Denco/internal/version"
)

// Prefix identifies Denco and its version to other clients, e.g. "-DN0100-".
var Prefix = "-DN" + version.PeerIDDigits() + "-"

// ID is a 20-byte peer ID.
type ID [20]byte

const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// New returns a fresh random peer ID. A new one is generated each time the
// daemon starts, which is standard and avoids being tracked across sessions.
func New() ID {
	var id ID
	n := copy(id[:], Prefix)
	var random [20]byte
	if _, err := rand.Read(random[:]); err != nil {
		// crypto/rand only fails if the OS entropy source is broken.
		panic(fmt.Sprintf("peerid: crypto/rand failed: %v", err))
	}
	for i := n; i < len(id); i++ {
		id[i] = alphabet[int(random[i])%len(alphabet)]
	}
	return id
}

// String returns the ID as text (it is always printable ASCII).
func (id ID) String() string { return string(id[:]) }
