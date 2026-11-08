package peer

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func roundTrip(t *testing.T, m *Message) *Message {
	t.Helper()
	got, err := ReadMessage(bytes.NewReader(m.Marshal()))
	if err != nil {
		t.Fatalf("%s: %v", m, err)
	}
	return got
}

func TestSimpleMessages(t *testing.T) {
	for _, m := range []*Message{Choke(), Unchoke(), Interested(), NotInterested()} {
		wire := m.Marshal()
		if len(wire) != 5 || binary.BigEndian.Uint32(wire) != 1 || wire[4] != byte(m.ID) {
			t.Errorf("%s wire = % x", m.ID, wire)
		}
		if got := roundTrip(t, m); got.ID != m.ID || len(got.Payload) != 0 {
			t.Errorf("%s round trip = %+v", m.ID, got)
		}
	}
}

func TestKeepAlive(t *testing.T) {
	var ka *Message
	wire := ka.Marshal()
	if !bytes.Equal(wire, []byte{0, 0, 0, 0}) {
		t.Fatalf("keep-alive wire = % x", wire)
	}
	m, err := ReadMessage(bytes.NewReader(wire))
	if m != nil || err != nil {
		t.Fatalf("got %v, %v", m, err)
	}
	if ka.String() != "keep-alive" {
		t.Error("String")
	}
}

func TestHave(t *testing.T) {
	got := roundTrip(t, Have(123456))
	idx, err := ParseHave(got)
	if err != nil || idx != 123456 {
		t.Fatalf("idx=%d err=%v", idx, err)
	}
	if _, err := ParseHave(&Message{ID: MsgHave, Payload: []byte{1}}); !errors.Is(err, ErrBadPayload) {
		t.Error("short have accepted")
	}
	if _, err := ParseHave(Choke()); !errors.Is(err, ErrBadPayload) {
		t.Error("wrong type accepted")
	}
}

func TestRequestCancel(t *testing.T) {
	b := Block{Index: 7, Begin: 16384, Length: BlockSize}
	for _, m := range []*Message{Request(b), Cancel(b)} {
		got, err := ParseBlock(roundTrip(t, m))
		if err != nil || got != b {
			t.Errorf("%s: got %+v err %v", m.ID, got, err)
		}
	}
	if _, err := ParseBlock(&Message{ID: MsgRequest, Payload: make([]byte, 11)}); !errors.Is(err, ErrBadPayload) {
		t.Error("short request accepted")
	}
	if _, err := ParseBlock(Have(1)); !errors.Is(err, ErrBadPayload) {
		t.Error("wrong type accepted")
	}
}

func TestPiece(t *testing.T) {
	data := bytes.Repeat([]byte{0xab}, 100)
	idx, begin, got, err := ParsePiece(roundTrip(t, Piece(3, 32768, data)))
	if err != nil || idx != 3 || begin != 32768 || !bytes.Equal(got, data) {
		t.Fatalf("idx=%d begin=%d len=%d err=%v", idx, begin, len(got), err)
	}
	if _, _, _, err := ParsePiece(&Message{ID: MsgPiece, Payload: make([]byte, 7)}); !errors.Is(err, ErrBadPayload) {
		t.Error("short piece accepted")
	}
}

func TestBitfieldPortExtended(t *testing.T) {
	src := []byte{0xf0, 0x01}
	bm := BitfieldMsg(src)
	src[0] = 0 // constructor must copy
	if got := roundTrip(t, bm); got.ID != MsgBitfield || !bytes.Equal(got.Payload, []byte{0xf0, 0x01}) {
		t.Errorf("bitfield = %+v", got)
	}

	p, err := ParsePort(roundTrip(t, Port(6881)))
	if err != nil || p != 6881 {
		t.Errorf("port=%d err=%v", p, err)
	}
	if _, err := ParsePort(&Message{ID: MsgPort}); !errors.Is(err, ErrBadPayload) {
		t.Error("empty port accepted")
	}

	id, body, err := ParseExtended(roundTrip(t, Extended(0, []byte("d1:md11:ut_metadatai1eee"))))
	if err != nil || id != 0 || string(body) != "d1:md11:ut_metadatai1eee" {
		t.Errorf("ext id=%d body=%q err=%v", id, body, err)
	}
	if _, _, err := ParseExtended(&Message{ID: MsgExtended}); !errors.Is(err, ErrBadPayload) {
		t.Error("empty extended accepted")
	}
}

func TestReadMessageErrors(t *testing.T) {
	tooBig := make([]byte, 4)
	binary.BigEndian.PutUint32(tooBig, MaxMessageLen+1)
	if _, err := ReadMessage(bytes.NewReader(tooBig)); !errors.Is(err, ErrMessageTooLarge) {
		t.Errorf("too big: %v", err)
	}
	truncated := Have(1).Marshal()[:6]
	if _, err := ReadMessage(bytes.NewReader(truncated)); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated body: %v", err)
	}
	if _, err := ReadMessage(bytes.NewReader([]byte{0, 0})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated length: %v", err)
	}
	if _, err := ReadMessage(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("clean EOF: %v", err)
	}
}

func TestUnknownMessagePassesThrough(t *testing.T) {
	// Unknown IDs (e.g. BEP 6 fast extension) must decode, not error, so the
	// session can ignore them instead of dropping the peer.
	got := roundTrip(t, &Message{ID: 13, Payload: []byte{1, 2}})
	if got.ID != 13 || got.ID.String() != "unknown(13)" {
		t.Errorf("got %+v", got)
	}
}

func FuzzReadMessage(f *testing.F) {
	f.Add(Have(1).Marshal())
	f.Add(Piece(1, 2, []byte("abc")).Marshal())
	f.Add([]byte{0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ReadMessage(bytes.NewReader(data))
		if err != nil || m == nil {
			return
		}
		// Parsers must never panic on arbitrary payloads.
		ParseHave(m)
		ParseBlock(m)
		ParsePiece(m)
		ParsePort(m)
		ParseExtended(m)
		_ = m.String()
	})
}
