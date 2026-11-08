package peer

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MessageID identifies a peer wire message. On the wire every message is
// <4-byte big-endian length><1-byte id><payload>; length 0 is a keep-alive.
type MessageID uint8

const (
	MsgChoke         MessageID = 0
	MsgUnchoke       MessageID = 1
	MsgInterested    MessageID = 2
	MsgNotInterested MessageID = 3
	MsgHave          MessageID = 4 // payload: piece index
	MsgBitfield      MessageID = 5 // payload: bitfield bytes
	MsgRequest       MessageID = 6 // payload: index, begin, length
	MsgPiece         MessageID = 7 // payload: index, begin, block
	MsgCancel        MessageID = 8 // payload: index, begin, length
	MsgPort          MessageID = 9 // payload: DHT port (BEP 5)
	MsgExtended      MessageID = 20
)

var messageNames = map[MessageID]string{
	MsgChoke: "choke", MsgUnchoke: "unchoke", MsgInterested: "interested",
	MsgNotInterested: "not-interested", MsgHave: "have", MsgBitfield: "bitfield",
	MsgRequest: "request", MsgPiece: "piece", MsgCancel: "cancel",
	MsgPort: "port", MsgExtended: "extended",
}

func (id MessageID) String() string {
	if s, ok := messageNames[id]; ok {
		return s
	}
	return fmt.Sprintf("unknown(%d)", uint8(id))
}

// MaxMessageLen caps an incoming message (id + payload). The largest
// legitimate messages are bitfields of huge torrents (~1 MB for 8M pieces)
// and 16 KiB piece blocks, so 2 MiB leaves headroom while stopping a peer
// from making us allocate gigabytes.
const MaxMessageLen = 2 << 20

// BlockSize is the standard request size. Nearly every client uses 16 KiB
// and many disconnect peers that request more.
const BlockSize = 16 * 1024

// ErrMessageTooLarge is returned by ReadMessage for oversized messages.
var ErrMessageTooLarge = errors.New("peer: message too large")

// ErrBadPayload is wrapped when a message's payload doesn't fit its type.
var ErrBadPayload = errors.New("peer: bad message payload")

// Message is one decoded wire message. A nil *Message means keep-alive.
type Message struct {
	ID      MessageID
	Payload []byte
}

// ReadMessage reads one message. It returns (nil, nil) for a keep-alive.
func ReadMessage(r io.Reader) (*Message, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(r, lenBuf[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n == 0 {
		return nil, nil
	}
	if n > MaxMessageLen {
		return nil, fmt.Errorf("%w: %d bytes", ErrMessageTooLarge, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return &Message{ID: MessageID(buf[0]), Payload: buf[1:]}, nil
}

// Marshal returns the full wire form, including the length prefix.
// A nil message marshals to a keep-alive.
func (m *Message) Marshal() []byte {
	if m == nil {
		return make([]byte, 4)
	}
	buf := make([]byte, 5+len(m.Payload))
	binary.BigEndian.PutUint32(buf, uint32(1+len(m.Payload)))
	buf[4] = byte(m.ID)
	copy(buf[5:], m.Payload)
	return buf
}

func (m *Message) String() string {
	if m == nil {
		return "keep-alive"
	}
	return fmt.Sprintf("%s(%d bytes)", m.ID, len(m.Payload))
}

// --- Constructors --------------------------------------------------------

func simple(id MessageID) *Message { return &Message{ID: id} }

func Choke() *Message         { return simple(MsgChoke) }
func Unchoke() *Message       { return simple(MsgUnchoke) }
func Interested() *Message    { return simple(MsgInterested) }
func NotInterested() *Message { return simple(MsgNotInterested) }

// Have announces that we completed piece index.
func Have(index uint32) *Message {
	p := make([]byte, 4)
	binary.BigEndian.PutUint32(p, index)
	return &Message{ID: MsgHave, Payload: p}
}

// BitfieldMsg sends our full piece set (wire-format bytes).
func BitfieldMsg(bits []byte) *Message {
	return &Message{ID: MsgBitfield, Payload: append([]byte(nil), bits...)}
}

// Block identifies a range inside a piece (used by request and cancel).
type Block struct {
	Index, Begin, Length uint32
}

func blockMsg(id MessageID, b Block) *Message {
	p := make([]byte, 12)
	binary.BigEndian.PutUint32(p[0:], b.Index)
	binary.BigEndian.PutUint32(p[4:], b.Begin)
	binary.BigEndian.PutUint32(p[8:], b.Length)
	return &Message{ID: id, Payload: p}
}

// Request asks the peer for block b.
func Request(b Block) *Message { return blockMsg(MsgRequest, b) }

// Cancel withdraws an earlier request.
func Cancel(b Block) *Message { return blockMsg(MsgCancel, b) }

// Piece sends block data.
func Piece(index, begin uint32, data []byte) *Message {
	p := make([]byte, 8+len(data))
	binary.BigEndian.PutUint32(p[0:], index)
	binary.BigEndian.PutUint32(p[4:], begin)
	copy(p[8:], data)
	return &Message{ID: MsgPiece, Payload: p}
}

// Port tells the peer our DHT port.
func Port(port uint16) *Message {
	p := make([]byte, 2)
	binary.BigEndian.PutUint16(p, port)
	return &Message{ID: MsgPort, Payload: p}
}

// Extended sends a BEP 10 extension message; extID 0 is the extension handshake.
func Extended(extID uint8, payload []byte) *Message {
	return &Message{ID: MsgExtended, Payload: append([]byte{extID}, payload...)}
}

// --- Parsers ---------------------------------------------------------------

func expect(m *Message, id MessageID) error {
	if m == nil || m.ID != id {
		return fmt.Errorf("%w: expected %s, got %s", ErrBadPayload, id, m)
	}
	return nil
}

// ParseHave returns the piece index of a have message.
func ParseHave(m *Message) (uint32, error) {
	if err := expect(m, MsgHave); err != nil {
		return 0, err
	}
	if len(m.Payload) != 4 {
		return 0, fmt.Errorf("%w: have payload %d bytes", ErrBadPayload, len(m.Payload))
	}
	return binary.BigEndian.Uint32(m.Payload), nil
}

// ParseBlock decodes a request or cancel message.
func ParseBlock(m *Message) (Block, error) {
	if m == nil || (m.ID != MsgRequest && m.ID != MsgCancel) {
		return Block{}, fmt.Errorf("%w: expected request/cancel, got %s", ErrBadPayload, m)
	}
	if len(m.Payload) != 12 {
		return Block{}, fmt.Errorf("%w: %s payload %d bytes", ErrBadPayload, m.ID, len(m.Payload))
	}
	return Block{
		Index:  binary.BigEndian.Uint32(m.Payload[0:]),
		Begin:  binary.BigEndian.Uint32(m.Payload[4:]),
		Length: binary.BigEndian.Uint32(m.Payload[8:]),
	}, nil
}

// ParsePiece decodes a piece message. data aliases m.Payload.
func ParsePiece(m *Message) (index, begin uint32, data []byte, err error) {
	if err := expect(m, MsgPiece); err != nil {
		return 0, 0, nil, err
	}
	if len(m.Payload) < 8 {
		return 0, 0, nil, fmt.Errorf("%w: piece payload %d bytes", ErrBadPayload, len(m.Payload))
	}
	return binary.BigEndian.Uint32(m.Payload[0:]), binary.BigEndian.Uint32(m.Payload[4:]), m.Payload[8:], nil
}

// ParsePort decodes a port message.
func ParsePort(m *Message) (uint16, error) {
	if err := expect(m, MsgPort); err != nil {
		return 0, err
	}
	if len(m.Payload) != 2 {
		return 0, fmt.Errorf("%w: port payload %d bytes", ErrBadPayload, len(m.Payload))
	}
	return binary.BigEndian.Uint16(m.Payload), nil
}

// ParseExtended splits an extended message into its extension ID and body.
func ParseExtended(m *Message) (extID uint8, body []byte, err error) {
	if err := expect(m, MsgExtended); err != nil {
		return 0, nil, err
	}
	if len(m.Payload) < 1 {
		return 0, nil, fmt.Errorf("%w: empty extended message", ErrBadPayload)
	}
	return m.Payload[0], m.Payload[1:], nil
}
