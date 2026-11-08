// Package metadata implements BEP 9 (ut_metadata): fetching a torrent's
// info dictionary from peers, which is how a magnet link — just an info
// hash — becomes a full torrent.
//
// The info dict ("metadata") is split into 16 KiB pieces. A peer that has
// it advertises "metadata_size" in its BEP 10 extension handshake; we ask
// for pieces with request messages and get data (or reject) messages back.
// The assembled bytes must hash to the info hash, so a peer can't feed us a
// different torrent.
//
// Files:
//
//	metadata.go  – message encoding and the Assembler
//	fetcher.go   – Fetcher: finds peers (trackers, magnet x.pe) and downloads the metadata
//
// Serving metadata to others is done by torrent.Session, which has it.
package metadata

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"fmt"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// PieceSize is the size of every metadata piece but the last (BEP 9).
const PieceSize = 16 << 10

// MaxSize caps the metadata we accept. An info dict holds 20 bytes per
// piece, so 16 MiB covers torrents with ~800,000 pieces; larger claims are
// almost certainly hostile.
const MaxSize = 16 << 20

// MsgType is a ut_metadata message type.
type MsgType int

const (
	Request MsgType = 0
	Data    MsgType = 1
	Reject  MsgType = 2
)

// ErrBadMessage is wrapped by every decoding or validation error.
var ErrBadMessage = errors.New("metadata: bad message")

// Message is one ut_metadata message.
type Message struct {
	Type      MsgType
	Piece     int
	TotalSize int    // Data only
	Data      []byte // Data only: the piece, sent after the bencoded dict
}

// Encode returns the message body (to send as an extended message).
func (m Message) Encode() []byte {
	d := map[string]any{"msg_type": int(m.Type), "piece": m.Piece}
	if m.Type == Data {
		d["total_size"] = m.TotalSize
	}
	b, _ := bencode.Encode(d)
	if m.Type == Data {
		b = append(b, m.Data...)
	}
	return b
}

// Decode parses a message body.
func Decode(b []byte) (Message, error) {
	v, n, err := bencode.DecodePrefix(b)
	if err != nil {
		return Message{}, fmt.Errorf("%w: %v", ErrBadMessage, err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return Message{}, fmt.Errorf("%w: not a dict", ErrBadMessage)
	}
	t, ok1 := bencode.GetInt(d, "msg_type")
	p, ok2 := bencode.GetInt(d, "piece")
	if !ok1 || !ok2 || t < 0 || t > 2 || p < 0 || p > MaxSize/PieceSize {
		return Message{}, fmt.Errorf("%w: missing or invalid msg_type/piece", ErrBadMessage)
	}
	m := Message{Type: MsgType(t), Piece: int(p)}
	if m.Type == Data {
		size, ok := bencode.GetInt(d, "total_size")
		if !ok || size <= 0 || size > MaxSize {
			return Message{}, fmt.Errorf("%w: invalid total_size", ErrBadMessage)
		}
		m.TotalSize = int(size)
		m.Data = b[n:]
	}
	return m, nil
}

// NumPieces is how many pieces metadata of size bytes has.
func NumPieces(size int) int { return (size + PieceSize - 1) / PieceSize }

// pieceLen is the length of piece i of metadata of size bytes.
func pieceLen(size, i int) int { return min(PieceSize, size-i*PieceSize) }

// Piece returns piece i of metadata (for serving it), or false if i is
// out of range.
func Piece(meta []byte, i int) ([]byte, bool) {
	if i < 0 || i >= NumPieces(len(meta)) {
		return nil, false
	}
	return meta[i*PieceSize : i*PieceSize+pieceLen(len(meta), i)], true
}

// Assembler collects metadata pieces and checks the result. Not safe for
// concurrent use.
type Assembler struct {
	hash   metainfo.Hash
	size   int
	pieces [][]byte
	have   int
}

// NewAssembler prepares for metadata of size bytes that must hash to hash.
func NewAssembler(hash metainfo.Hash, size int) (*Assembler, error) {
	if size <= 0 || size > MaxSize {
		return nil, fmt.Errorf("%w: metadata size %d", ErrBadMessage, size)
	}
	return &Assembler{hash: hash, size: size, pieces: make([][]byte, NumPieces(size))}, nil
}

// Size is the expected metadata size.
func (a *Assembler) Size() int { return a.size }

// NumPieces is the number of pieces.
func (a *Assembler) NumPieces() int { return len(a.pieces) }

// Have reports how many pieces are stored.
func (a *Assembler) Have() int { return a.have }

// Has reports whether piece i is stored.
func (a *Assembler) Has(i int) bool { return i >= 0 && i < len(a.pieces) && a.pieces[i] != nil }

// Add stores piece i. Duplicates are ignored; a wrong index or length is
// an error (the sender is broken or lying).
func (a *Assembler) Add(i int, data []byte) error {
	if i < 0 || i >= len(a.pieces) || len(data) != pieceLen(a.size, i) {
		return fmt.Errorf("%w: piece %d with %d bytes", ErrBadMessage, i, len(data))
	}
	if a.pieces[i] == nil {
		a.pieces[i] = bytes.Clone(data)
		a.have++
	}
	return nil
}

// Complete reports whether every piece is stored.
func (a *Assembler) Complete() bool { return a.have == len(a.pieces) }

// Verify joins the pieces and checks the SHA-1. On a mismatch everything
// is discarded so it can be fetched again.
func (a *Assembler) Verify() ([]byte, bool) {
	out := bytes.Join(a.pieces, nil)
	if sha1.Sum(out) == a.hash {
		return out, true
	}
	clear(a.pieces)
	a.have = 0
	return nil, false
}
