package peer

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/7Eshaan/Denco/internal/peerid"
)

func TestHandshakeRoundTrip(t *testing.T) {
	h := &Handshake{InfoHash: [20]byte{1, 2, 3}, PeerID: peerid.New()}
	h.SetExtensions()
	h.SetDHT()
	var buf bytes.Buffer
	if _, err := h.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != HandshakeLen || buf.Bytes()[0] != 19 {
		t.Fatalf("wire form wrong: % x", buf.Bytes())
	}
	got, err := ReadHandshake(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if *got != *h {
		t.Fatalf("round trip: %+v vs %+v", got, h)
	}
	if !got.SupportsExtensions() || !got.SupportsDHT() {
		t.Fatal("reserved bits lost")
	}
	if (&Handshake{}).SupportsExtensions() || (&Handshake{}).SupportsDHT() {
		t.Fatal("zero handshake should support nothing")
	}
}

func TestReadHandshakeErrors(t *testing.T) {
	good := (&Handshake{}).MarshalBinary()

	wrongLen := append([]byte{18}, good[1:]...)
	if _, err := ReadHandshake(bytes.NewReader(wrongLen)); !errors.Is(err, ErrBadHandshake) {
		t.Errorf("wrong pstrlen: %v", err)
	}
	wrongProto := append([]byte(nil), good...)
	copy(wrongProto[1:], "BitTorrent protocoX")
	if _, err := ReadHandshake(bytes.NewReader(wrongProto)); !errors.Is(err, ErrBadHandshake) {
		t.Errorf("wrong protocol: %v", err)
	}
	if _, err := ReadHandshake(bytes.NewReader(good[:30])); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("truncated: %v", err)
	}
	if _, err := ReadHandshake(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Errorf("empty: %v", err)
	}
}
