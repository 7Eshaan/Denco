package mse

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"
)

var testHash = [20]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20}

// pair returns the two ends of a loopback TCP connection.
func pair(t *testing.T) (a, b net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() { c, err := ln.Accept(); ch <- res{c, err} }()
	a, err = net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	for _, c := range []net.Conn{a, r.c} {
		c.SetDeadline(time.Now().Add(10 * time.Second))
		t.Cleanup(func() { c.Close() })
	}
	return a, r.c
}

func findTest(req2 [20]byte) ([20]byte, bool) {
	if req2 == Req2(testHash) {
		return testHash, true
	}
	return [20]byte{}, false
}

func prefer(provide uint32) uint32 {
	if provide&RC4 != 0 {
		return RC4
	}
	return provide & Plaintext
}

type recvResult struct {
	c   *Conn
	ih  [20]byte
	sel uint32
	err error
}

func receive(b net.Conn, find Torrents, choose func(uint32) uint32) chan recvResult {
	ch := make(chan recvResult, 1)
	go func() {
		var prefix [20]byte // as peer.Accept does: 20 bytes already read
		if _, err := io.ReadFull(b, prefix[:]); err != nil {
			ch <- recvResult{err: err}
			return
		}
		c, ih, sel, err := Receive(b, prefix[:], find, choose)
		ch <- recvResult{c, ih, sel, err}
	}()
	return ch
}

// Both sides of this package agree, the initial payload arrives first,
// and a lot of data goes both ways at once.
func TestHandshakeAndStream(t *testing.T) {
	for _, tc := range []struct {
		name    string
		provide uint32
		choose  func(uint32) uint32
		want    uint32
	}{
		{"rc4", RC4 | Plaintext, prefer, RC4},
		{"plaintext", RC4 | Plaintext, func(uint32) uint32 { return Plaintext }, Plaintext},
		{"rc4 only", RC4, prefer, RC4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, b := pair(t)
			ch := receive(b, findTest, tc.choose)
			ia := []byte("initial payload: the BitTorrent handshake")
			ca, sel, err := Initiate(a, testHash, tc.provide, ia)
			if err != nil {
				t.Fatal(err)
			}
			r := <-ch
			if r.err != nil {
				t.Fatal(r.err)
			}
			if sel != tc.want || r.sel != tc.want || r.ih != testHash {
				t.Fatalf("selected %#x / %#x, torrent %x", sel, r.sel, r.ih)
			}
			if ca.Encrypted() != (tc.want == RC4) || r.c.Encrypted() != (tc.want == RC4) {
				t.Fatal("Encrypted() disagrees with the method")
			}
			got := make([]byte, len(ia))
			if _, err := io.ReadFull(r.c, got); err != nil || !bytes.Equal(got, ia) {
				t.Fatalf("initial payload %q, %v", got, err)
			}
			exchange(t, ca, r.c)
		})
	}
}

// exchange sends 1 MiB each way at the same time, in odd-sized writes.
func exchange(t *testing.T, x, y net.Conn) {
	t.Helper()
	data := make([]byte, 1<<20)
	rand.Read(data)
	errs := make(chan error, 2)
	for _, w := range []net.Conn{x, y} {
		go func() {
			for off := 0; off < len(data); off += 1000 {
				if _, err := w.Write(data[off:min(off+1000, len(data))]); err != nil {
					errs <- err
					return
				}
			}
			errs <- nil
		}()
	}
	for _, r := range []net.Conn{x, y} {
		got := make([]byte, len(data))
		if _, err := io.ReadFull(r, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("data corrupted")
		}
	}
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
}

// --- the spec, written out independently -------------------------------
//
// These tests play the other side by hand from the spec's formulas (not
// this package's helpers), with padding everywhere, so a mistake made the
// same way on both sides of the package — keys swapped, a field in the
// wrong order — still fails.

var specP, _ = new(big.Int).SetString("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A63A36210000000000090563", 16)

func sha(parts ...[]byte) []byte {
	h := sha1.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func specRC4(name string, s []byte) *rc4.Cipher {
	c, _ := rc4.NewCipher(sha([]byte(name), s, testHash[:]))
	skip := make([]byte, 1024)
	c.XORKeyStream(skip, skip)
	return c
}

type specKey struct{ x, y *big.Int }

func newSpecKey() specKey {
	x, _ := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 160))
	return specKey{x, new(big.Int).Exp(big.NewInt(2), x, specP)}
}

func pad96(n *big.Int) []byte { return n.FillBytes(make([]byte, 96)) }

func u16(n int) []byte    { return binary.BigEndian.AppendUint16(nil, uint16(n)) }
func u32(n uint32) []byte { return binary.BigEndian.AppendUint32(nil, n) }

// Receive against a hand-made initiator with 300 bytes of PadA, 17 of
// PadC, and plaintext selected (IA still RC4, the rest in the clear).
func TestReceiveMatchesSpec(t *testing.T) {
	a, b := pair(t)
	ch := receive(b, findTest, func(p uint32) uint32 {
		if p != RC4|Plaintext {
			t.Errorf("crypto_provide %#x", p)
		}
		return Plaintext
	})
	k := newSpecKey()
	a.Write(append(pad96(k.y), make([]byte, 300)...)) // Ya, PadA
	yb := make([]byte, 96)
	br := bufio.NewReader(a)
	if _, err := io.ReadFull(br, yb); err != nil {
		t.Fatal(err)
	}
	s := pad96(new(big.Int).Exp(new(big.Int).SetBytes(yb), k.x, specP))
	keyA, keyB := specRC4("keyA", s), specRC4("keyB", s)
	req2, req3 := sha([]byte("req2"), testHash[:]), sha([]byte("req3"), s)
	for i := range req2 {
		req2[i] ^= req3[i]
	}
	ia := []byte("IA")
	enc := bytes.Join([][]byte{make([]byte, 8), u32(RC4 | Plaintext), u16(17), make([]byte, 17), u16(len(ia)), ia}, nil)
	keyA.XORKeyStream(enc, enc)
	a.Write(bytes.Join([][]byte{sha([]byte("req1"), s), req2, enc}, nil))

	// Step 4 after PadB: find ENCRYPT(VC), check crypto_select.
	vc := make([]byte, 8)
	keyB.XORKeyStream(vc, vc)
	var window []byte
	for !bytes.HasSuffix(window, vc) {
		c, err := br.ReadByte()
		if err != nil || len(window) > 520 {
			t.Fatalf("no VC from Receive: %v", err)
		}
		window = append(window, c)
	}
	hdr := make([]byte, 6)
	io.ReadFull(br, hdr)
	keyB.XORKeyStream(hdr, hdr)
	if binary.BigEndian.Uint32(hdr) != Plaintext {
		t.Fatalf("crypto_select %x", hdr[:4])
	}
	io.ReadFull(br, make([]byte, binary.BigEndian.Uint16(hdr[4:]))) // PadD (encrypted, ignored)

	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	a.Write([]byte(" then plain")) // ENCRYPT2 = plaintext
	got := make([]byte, len("IA then plain"))
	if _, err := io.ReadFull(r.c, got); err != nil || string(got) != "IA then plain" {
		t.Fatalf("got %q, %v", got, err)
	}
	r.c.Write([]byte("reply"))
	got = make([]byte, 5)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "reply" {
		t.Fatalf("reply %q, %v", got, err)
	}
}

// Initiate against a hand-made receiver with 400 bytes of PadB and 33 of
// PadD, selecting RC4, which then sends payload in the same packet.
func TestInitiateMatchesSpec(t *testing.T) {
	a, b := pair(t)
	type res struct {
		c   *Conn
		sel uint32
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, sel, err := Initiate(a, testHash, RC4|Plaintext, []byte("hello"))
		ch <- res{c, sel, err}
	}()
	br := bufio.NewReader(b)
	ya := make([]byte, 96)
	if _, err := io.ReadFull(br, ya); err != nil {
		t.Fatal(err)
	}
	k := newSpecKey()
	b.Write(append(pad96(k.y), make([]byte, 400)...)) // Yb, PadB
	s := pad96(new(big.Int).Exp(new(big.Int).SetBytes(ya), k.x, specP))
	keyA, keyB := specRC4("keyA", s), specRC4("keyB", s)

	req1 := sha([]byte("req1"), s)
	var window []byte
	for !bytes.HasSuffix(window, req1) {
		c, err := br.ReadByte()
		if err != nil || len(window) > 532 {
			t.Fatalf("no req1 from Initiate: %v", err)
		}
		window = append(window, c)
	}
	obf := make([]byte, 20)
	io.ReadFull(br, obf)
	req3 := sha([]byte("req3"), s)
	for i := range obf {
		obf[i] ^= req3[i]
	}
	if !bytes.Equal(obf, sha([]byte("req2"), testHash[:])) {
		t.Fatal("wrong obfuscated torrent hash")
	}
	hdr := make([]byte, 14)
	io.ReadFull(br, hdr)
	keyA.XORKeyStream(hdr, hdr)
	if !bytes.Equal(hdr[:8], make([]byte, 8)) || binary.BigEndian.Uint32(hdr[8:]) != RC4|Plaintext {
		t.Fatalf("VC / crypto_provide: %x", hdr[:12])
	}
	padC := make([]byte, binary.BigEndian.Uint16(hdr[12:])+2)
	io.ReadFull(br, padC)
	keyA.XORKeyStream(padC, padC)
	ia := make([]byte, binary.BigEndian.Uint16(padC[len(padC)-2:]))
	io.ReadFull(br, ia)
	keyA.XORKeyStream(ia, ia)
	if string(ia) != "hello" {
		t.Fatalf("IA = %q", ia)
	}
	reply := bytes.Join([][]byte{make([]byte, 8), u32(RC4), u16(33), make([]byte, 33), []byte("payload")}, nil)
	keyB.XORKeyStream(reply, reply)
	b.Write(reply)

	r := <-ch
	if r.err != nil || r.sel != RC4 {
		t.Fatalf("Initiate: %#x, %v", r.sel, r.err)
	}
	got := make([]byte, 7)
	if _, err := io.ReadFull(r.c, got); err != nil || string(got) != "payload" {
		t.Fatalf("payload %q, %v", got, err)
	}
	r.c.Write([]byte("back"))
	got = make([]byte, 4)
	io.ReadFull(br, got)
	keyA.XORKeyStream(got, got)
	if string(got) != "back" {
		t.Fatalf("A's payload decrypts to %q", got)
	}
}

func TestReceiveRefuses(t *testing.T) {
	t.Run("unknown torrent", func(t *testing.T) {
		a, b := pair(t)
		ch := receive(b, func([20]byte) ([20]byte, bool) { return [20]byte{}, false }, prefer)
		go Initiate(a, testHash, RC4, nil)
		if r := <-ch; !errors.Is(r.err, ErrUnknownTorrent) {
			t.Fatalf("got %v", r.err)
		}
	})
	t.Run("no common method", func(t *testing.T) {
		a, b := pair(t)
		ch := receive(b, findTest, func(p uint32) uint32 { return p & Plaintext }) // plaintext only
		errA := make(chan error, 1)
		go func() {
			_, _, err := Initiate(a, testHash, RC4, nil) // RC4 only
			errA <- err
		}()
		if r := <-ch; !errors.Is(r.err, ErrNoCommonMethod) {
			t.Fatalf("receiver: %v", r.err)
		}
		b.Close()
		if err := <-errA; err == nil {
			t.Fatal("initiator succeeded")
		}
	})
	t.Run("no sync marker", func(t *testing.T) {
		a, b := pair(t)
		ch := receive(b, findTest, prefer)
		k := newSpecKey()
		a.Write(append(pad96(k.y), make([]byte, 600)...)) // never sends req1
		if r := <-ch; !errors.Is(r.err, ErrNotMSE) {
			t.Fatalf("got %v", r.err)
		}
	})
	t.Run("weak public key", func(t *testing.T) {
		for _, y := range []*big.Int{big.NewInt(0), big.NewInt(1), new(big.Int).Sub(specP, big.NewInt(1)), specP} {
			a, b := pair(t)
			ch := receive(b, findTest, prefer)
			a.Write(pad96(y))
			// Refused for the key itself, at once — not later for want
			// of a marker the test never sends.
			if r := <-ch; !errors.Is(r.err, ErrNotMSE) || !strings.Contains(r.err.Error(), "invalid public key") {
				t.Fatalf("Y=%v: got %v", y, r.err)
			}
		}
	})
}

// A peer that doesn't speak MSE — a plain BitTorrent peer reads 68 bytes,
// finds nonsense and hangs up — fails Initiate quickly with ErrNotMSE.
func TestInitiateAgainstPlainPeer(t *testing.T) {
	a, b := pair(t)
	go func() {
		io.ReadFull(b, make([]byte, 68))
		b.Close()
	}()
	start := time.Now()
	if _, _, err := Initiate(a, testHash, RC4, nil); !errors.Is(err, ErrNotMSE) {
		t.Fatalf("got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("took too long")
	}
}

func TestRandomPad(t *testing.T) {
	seen := map[bool]bool{}
	for range 200 {
		p, err := randomPad()
		if err != nil || len(p) > maxPad {
			t.Fatalf("pad of %d: %v", len(p), err)
		}
		seen[len(p) > 256] = true
	}
	if !seen[true] || !seen[false] {
		t.Fatal("pad lengths not spread over 0–512")
	}
}
