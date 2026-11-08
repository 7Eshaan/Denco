// Package peer implements the BitTorrent peer wire protocol (BEP 3): the
// handshake, message encoding, and a Conn type that runs one TCP connection
// to a peer with its own read and write goroutines.
//
// This package moves bytes; it does not decide what to request or whom to
// unchoke. That logic lives in the torrent session (Phase 5).
package peer

import (
	"errors"
	"fmt"
	"io"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
)

const protocolID = "BitTorrent protocol"

// HandshakeLen is the size of a handshake on the wire:
// 1 (pstrlen) + 19 (pstr) + 8 (reserved) + 20 (info hash) + 20 (peer id).
const HandshakeLen = 1 + len(protocolID) + 8 + 20 + 20

// Reserved-bit flags we advertise or check.
const (
	reservedExtByte = 5    // BEP 10 extension protocol: reserved[5] & 0x10
	reservedExtBit  = 0x10 //
	reservedDHTByte = 7    // BEP 5 DHT: reserved[7] & 0x01
	reservedDHTBit  = 0x01 //
)

// ErrBadHandshake is wrapped by handshake parsing errors.
var ErrBadHandshake = errors.New("peer: bad handshake")

// Handshake is the first message on every connection.
type Handshake struct {
	Reserved [8]byte
	InfoHash metainfo.Hash
	PeerID   peerid.ID
}

// SupportsExtensions reports whether the peer set the BEP 10 bit.
func (h *Handshake) SupportsExtensions() bool {
	return h.Reserved[reservedExtByte]&reservedExtBit != 0
}

// SupportsDHT reports whether the peer set the BEP 5 DHT bit.
func (h *Handshake) SupportsDHT() bool {
	return h.Reserved[reservedDHTByte]&reservedDHTBit != 0
}

// SetExtensions / SetDHT set the matching reserved bits on our handshake.
func (h *Handshake) SetExtensions() { h.Reserved[reservedExtByte] |= reservedExtBit }
func (h *Handshake) SetDHT()        { h.Reserved[reservedDHTByte] |= reservedDHTBit }

// MarshalBinary returns the 68-byte wire form.
func (h *Handshake) MarshalBinary() []byte {
	buf := make([]byte, 0, HandshakeLen)
	buf = append(buf, byte(len(protocolID)))
	buf = append(buf, protocolID...)
	buf = append(buf, h.Reserved[:]...)
	buf = append(buf, h.InfoHash[:]...)
	buf = append(buf, h.PeerID[:]...)
	return buf
}

// WriteTo writes the handshake to w.
func (h *Handshake) WriteTo(w io.Writer) (int64, error) {
	n, err := w.Write(h.MarshalBinary())
	return int64(n), err
}

// ReadHandshake reads and validates a handshake from r.
func ReadHandshake(r io.Reader) (*Handshake, error) {
	var buf [HandshakeLen]byte
	// Read the length byte first so a non-BitTorrent peer is rejected without
	// waiting for 67 more bytes that may never come.
	if _, err := io.ReadFull(r, buf[:1]); err != nil {
		return nil, fmt.Errorf("peer: reading handshake: %w", err)
	}
	if int(buf[0]) != len(protocolID) {
		return nil, fmt.Errorf("%w: protocol length %d", ErrBadHandshake, buf[0])
	}
	if _, err := io.ReadFull(r, buf[1:]); err != nil {
		return nil, fmt.Errorf("peer: reading handshake: %w", err)
	}
	if string(buf[1:1+len(protocolID)]) != protocolID {
		return nil, fmt.Errorf("%w: unknown protocol %q", ErrBadHandshake, buf[1:1+len(protocolID)])
	}
	h := &Handshake{}
	off := 1 + len(protocolID)
	copy(h.Reserved[:], buf[off:off+8])
	copy(h.InfoHash[:], buf[off+8:off+28])
	copy(h.PeerID[:], buf[off+28:])
	return h, nil
}
