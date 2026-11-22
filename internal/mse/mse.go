// Package mse implements Message Stream Encryption (MSE, also called
// Protocol Encryption or PE): the encrypted handshake mainstream
// BitTorrent clients use so that ISPs can't recognise and throttle
// BitTorrent traffic by looking at it.
//
// The two sides agree on a secret with a 768-bit Diffie-Hellman exchange,
// prove they want the same torrent without sending its info hash in the
// clear, and then encrypt everything with RC4 (the first 1 KiB of each
// key stream thrown away). Random padding hides the message lengths.
//
// It is obfuscation, not security: nobody is authenticated, and RC4 is
// broken. It stops simple traffic shaping, not a determined observer.
//
// The handshake, with A the side that connects and B the side that
// accepts (S is the DH secret, SKEY the torrent's info hash):
//
//	1 A→B  Ya, PadA
//	2 B→A  Yb, PadB
//	3 A→B  HASH("req1", S), HASH("req2", SKEY) xor HASH("req3", S),
//	       ENCRYPT(VC, crypto_provide, len(PadC), PadC, len(IA)), ENCRYPT(IA)
//	4 B→A  ENCRYPT(VC, crypto_select, len(PadD), PadD), ENCRYPT2(payload…)
//	5 A→B  ENCRYPT2(payload…)
//
// ENCRYPT is RC4; ENCRYPT2 is the method B selected (RC4, or plaintext
// after the encrypted header). IA, the initial payload, lets A send its
// BitTorrent handshake without waiting a round trip.
package mse

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rc4"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
)

// Methods for crypto_provide / crypto_select.
const (
	Plaintext uint32 = 0x01 // only the handshake is encrypted
	RC4       uint32 = 0x02 // everything is encrypted
)

const (
	keyLen = 96  // a 768-bit public key on the wire
	maxPad = 512 // PadA … PadD are at most this long
	vcLen  = 8   // the verification constant: 8 zero bytes
)

var (
	// ErrNotMSE: the other side doesn't speak MSE (or speaks it wrongly) —
	// e.g. it didn't answer with a key, or the synchronisation marker never
	// came.
	ErrNotMSE = errors.New("mse: handshake failed")
	// ErrUnknownTorrent: the connecting peer asked for a torrent we don't
	// have.
	ErrUnknownTorrent = errors.New("mse: unknown torrent")
	// ErrNoCommonMethod: no encryption method both sides accept.
	ErrNoCommonMethod = errors.New("mse: no common encryption method")
)

// The DH group: a 768-bit prime and generator 2 (from the MSE spec).
var (
	prime = mustHex("FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74" +
		"020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F1437" +
		"4FE1356D6D51C245E485B576625E7EC6F44C42E9A63A36210000000000090563")
	generator = big.NewInt(2)
	primeLess = new(big.Int).Sub(prime, big.NewInt(1))
)

func mustHex(s string) *big.Int {
	n, ok := new(big.Int).SetString(s, 16)
	if !ok {
		panic("mse: bad constant")
	}
	return n
}

// Req2 is HASH("req2", infoHash): what a connecting peer's torrent is
// recognised by. An accepting side indexes its torrents by it.
func Req2(infoHash [20]byte) [20]byte { return hash([]byte("req2"), infoHash[:]) }

func hash(parts ...[]byte) [20]byte {
	h := sha1.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out [20]byte
	h.Sum(out[:0])
	return out
}

// keyPair is one side's DH private key and the public key to send.
type keyPair struct {
	x   *big.Int
	pub [keyLen]byte
}

func newKeyPair() (*keyPair, error) {
	// 160 random bits, as the spec recommends.
	var b [20]byte
	if _, err := rand.Read(b[:]); err != nil {
		return nil, err
	}
	k := &keyPair{x: new(big.Int).SetBytes(b[:])}
	new(big.Int).Exp(generator, k.x, prime).FillBytes(k.pub[:])
	return k, nil
}

// secret computes S from the other side's public key. Keys of 0, 1 and
// P-1 (or ≥ P) are refused: they would make S predictable.
func (k *keyPair) secret(theirs []byte) ([]byte, error) {
	y := new(big.Int).SetBytes(theirs)
	if y.Cmp(big.NewInt(1)) <= 0 || y.Cmp(primeLess) >= 0 {
		return nil, fmt.Errorf("%w: invalid public key", ErrNotMSE)
	}
	s := make([]byte, keyLen)
	new(big.Int).Exp(y, k.x, prime).FillBytes(s)
	return s, nil
}

// newCipher returns RC4 keyed with HASH(name, S, SKEY), its first 1024
// bytes of key stream discarded (the spec's fix for RC4's weak start).
func newCipher(name string, s []byte, skey [20]byte) *rc4.Cipher {
	key := hash([]byte(name), s, skey[:])
	c, _ := rc4.NewCipher(key[:]) // a 20-byte key is always valid
	var discard [1024]byte
	c.XORKeyStream(discard[:], discard[:])
	return c
}

// randomPad returns 0–512 random bytes.
func randomPad() ([]byte, error) {
	var n [2]byte
	if _, err := rand.Read(n[:]); err != nil {
		return nil, err
	}
	pad := make([]byte, int(binary.BigEndian.Uint16(n[:]))%(maxPad+1))
	_, err := rand.Read(pad)
	return pad, err
}

// Initiate runs the handshake as the connecting side over nc, for the
// torrent infoHash, offering the methods in provide and sending ia (the
// BitTorrent handshake) as the initial payload. The caller sets nc's
// deadline. It returns the connection to use from now on — reads and
// writes go through the selected method — and the method chosen.
func Initiate(nc net.Conn, infoHash [20]byte, provide uint32, ia []byte) (*Conn, uint32, error) {
	if len(ia) > 0xffff {
		return nil, 0, errors.New("mse: initial payload too long")
	}
	k, err := newKeyPair()
	if err != nil {
		return nil, 0, err
	}
	padA, err := randomPad()
	if err != nil {
		return nil, 0, err
	}
	// 1: Ya, PadA.
	if _, err := nc.Write(append(k.pub[:], padA...)); err != nil {
		return nil, 0, err
	}
	// 2: Yb (PadB is skipped while looking for step 4's marker).
	br := bufio.NewReaderSize(nc, 4096)
	var yb [keyLen]byte
	if _, err := io.ReadFull(br, yb[:]); err != nil {
		return nil, 0, notMSE(err)
	}
	s, err := k.secret(yb[:])
	if err != nil {
		return nil, 0, err
	}
	enc := newCipher("keyA", s, infoHash)
	dec := newCipher("keyB", s, infoHash)

	// 3: the sync hash, the obfuscated torrent, then the encrypted part.
	req1, req2, req3 := hash([]byte("req1"), s), Req2(infoHash), hash([]byte("req3"), s)
	var msg bytes.Buffer
	msg.Write(req1[:])
	for i := range req2 {
		msg.WriteByte(req2[i] ^ req3[i])
	}
	plain := make([]byte, vcLen+4+2+0+2, vcLen+8+len(ia))
	binary.BigEndian.PutUint32(plain[vcLen:], provide)
	// len(PadC) = 0: the padding before it already hides the length.
	binary.BigEndian.PutUint16(plain[vcLen+6:], uint16(len(ia)))
	plain = append(plain, ia...)
	enc.XORKeyStream(plain, plain)
	msg.Write(plain)
	if _, err := nc.Write(msg.Bytes()); err != nil {
		return nil, 0, err
	}

	// 4: find ENCRYPT(VC) — VC is zeros, so it is the key stream itself —
	// within PadB's 512 bytes, then read the rest of the header.
	var marker [vcLen]byte
	dec.XORKeyStream(marker[:], marker[:])
	if err := scanFor(br, marker[:], maxPad+vcLen); err != nil {
		return nil, 0, err
	}
	var hdr [6]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, 0, notMSE(err)
	}
	dec.XORKeyStream(hdr[:], hdr[:])
	sel := binary.BigEndian.Uint32(hdr[:4])
	if sel != RC4 && sel != Plaintext || sel&provide == 0 {
		return nil, 0, fmt.Errorf("%w: peer selected method %#x", ErrNoCommonMethod, sel)
	}
	padD := int(binary.BigEndian.Uint16(hdr[4:]))
	if padD > maxPad {
		return nil, 0, fmt.Errorf("%w: padding of %d bytes", ErrNotMSE, padD)
	}
	pad := make([]byte, padD)
	if _, err := io.ReadFull(br, pad); err != nil {
		return nil, 0, notMSE(err)
	}
	dec.XORKeyStream(pad, pad)

	c := &Conn{Conn: nc}
	if sel == RC4 {
		c.enc, c.dec = enc, dec
	}
	c.pending = c.decrypt(buffered(br))
	return c, sel, nil
}

// Torrents finds the torrent a connecting peer asks for by Req2 of its
// info hash.
type Torrents func(req2 [20]byte) (infoHash [20]byte, ok bool)

// Receive runs the handshake as the accepting side. prefix holds bytes
// already read from nc (to tell MSE from a plain handshake); choose picks
// the method from the peer's crypto_provide, returning 0 to refuse. It
// returns the connection — whose first bytes are the peer's initial
// payload, if it sent one — the torrent asked for, and the method chosen.
func Receive(nc net.Conn, prefix []byte, find Torrents, choose func(provide uint32) uint32) (*Conn, [20]byte, uint32, error) {
	var infoHash [20]byte
	br := bufio.NewReaderSize(io.MultiReader(bytes.NewReader(prefix), nc), 4096)
	// 1: Ya.
	var ya [keyLen]byte
	if _, err := io.ReadFull(br, ya[:]); err != nil {
		return nil, infoHash, 0, notMSE(err)
	}
	k, err := newKeyPair()
	if err != nil {
		return nil, infoHash, 0, err
	}
	s, err := k.secret(ya[:])
	if err != nil {
		return nil, infoHash, 0, err
	}
	// 2: Yb, PadB.
	padB, err := randomPad()
	if err != nil {
		return nil, infoHash, 0, err
	}
	if _, err := nc.Write(append(k.pub[:], padB...)); err != nil {
		return nil, infoHash, 0, err
	}
	// 3: skip PadA up to HASH("req1", S), then find the torrent.
	req1 := hash([]byte("req1"), s)
	if err := scanFor(br, req1[:], maxPad+len(req1)); err != nil {
		return nil, infoHash, 0, err
	}
	var obf [20]byte
	if _, err := io.ReadFull(br, obf[:]); err != nil {
		return nil, infoHash, 0, notMSE(err)
	}
	req3 := hash([]byte("req3"), s)
	for i := range obf {
		obf[i] ^= req3[i]
	}
	infoHash, ok := find(obf)
	if !ok {
		return nil, infoHash, 0, ErrUnknownTorrent
	}
	dec := newCipher("keyA", s, infoHash)
	enc := newCipher("keyB", s, infoHash)
	var hdr [vcLen + 6]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return nil, infoHash, 0, notMSE(err)
	}
	dec.XORKeyStream(hdr[:], hdr[:])
	if !bytes.Equal(hdr[:vcLen], make([]byte, vcLen)) {
		return nil, infoHash, 0, fmt.Errorf("%w: bad verification constant", ErrNotMSE)
	}
	provide := binary.BigEndian.Uint32(hdr[vcLen:])
	padC := int(binary.BigEndian.Uint16(hdr[vcLen+4:]))
	if padC > maxPad {
		return nil, infoHash, 0, fmt.Errorf("%w: padding of %d bytes", ErrNotMSE, padC)
	}
	// PadC, then len(IA): read together, decrypted in order.
	rest := make([]byte, padC+2)
	if _, err := io.ReadFull(br, rest); err != nil {
		return nil, infoHash, 0, notMSE(err)
	}
	dec.XORKeyStream(rest, rest)
	ia := make([]byte, binary.BigEndian.Uint16(rest[padC:]))
	if _, err := io.ReadFull(br, ia); err != nil {
		return nil, infoHash, 0, notMSE(err)
	}
	dec.XORKeyStream(ia, ia) // IA is always RC4, whatever is selected

	sel := choose(provide)
	if sel != RC4 && sel != Plaintext || sel&provide == 0 {
		return nil, infoHash, 0, fmt.Errorf("%w: peer offers %#x", ErrNoCommonMethod, provide)
	}
	// 4: ENCRYPT(VC, crypto_select, len(PadD) = 0).
	var reply [vcLen + 6]byte
	binary.BigEndian.PutUint32(reply[vcLen:], sel)
	enc.XORKeyStream(reply[:], reply[:])
	if _, err := nc.Write(reply[:]); err != nil {
		return nil, infoHash, 0, err
	}

	c := &Conn{Conn: nc}
	if sel == RC4 {
		c.enc, c.dec = enc, dec
	}
	c.pending = append(ia, c.decrypt(buffered(br))...)
	return c, infoHash, sel, nil
}

// scanFor reads from r until it has consumed marker, which must end
// within the first limit bytes.
func scanFor(r *bufio.Reader, marker []byte, limit int) error {
	window := make([]byte, 0, limit)
	for len(window) < limit {
		b, err := r.ReadByte()
		if err != nil {
			return notMSE(err)
		}
		window = append(window, b)
		if bytes.HasSuffix(window, marker) {
			return nil
		}
	}
	return fmt.Errorf("%w: no synchronisation marker in %d bytes", ErrNotMSE, limit)
}

// buffered takes whatever r has already read past the handshake.
func buffered(r *bufio.Reader) []byte {
	b, _ := r.Peek(r.Buffered())
	return bytes.Clone(b)
}

// notMSE wraps a read failure mid-handshake: a peer that doesn't speak MSE
// typically hangs up (EOF or a reset) when it sees our key.
func notMSE(err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: connection closed", ErrNotMSE)
	}
	return fmt.Errorf("%w: %w", ErrNotMSE, err)
}

// Conn is a connection after the handshake: reads are decrypted and writes
// encrypted when RC4 was selected, and passed through otherwise.
// Deadlines and Close go to the underlying connection. One goroutine may
// read while another writes.
type Conn struct {
	net.Conn
	enc, dec *rc4.Cipher // nil: plaintext after the handshake

	rmu     sync.Mutex
	pending []byte // read past the handshake, already decrypted

	wmu  sync.Mutex
	wbuf []byte
}

// Encrypted reports whether the payload is RC4-encrypted.
func (c *Conn) Encrypted() bool { return c.enc != nil }

func (c *Conn) decrypt(b []byte) []byte {
	if c.dec != nil {
		c.dec.XORKeyStream(b, b)
	}
	return b
}

func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if len(c.pending) > 0 {
		n := copy(p, c.pending)
		c.pending = c.pending[n:]
		return n, nil
	}
	n, err := c.Conn.Read(p)
	c.decrypt(p[:n])
	return n, err
}

func (c *Conn) Write(p []byte) (int, error) {
	if c.enc == nil {
		return c.Conn.Write(p)
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	// Encrypt into our own buffer: p belongs to the caller.
	if cap(c.wbuf) < len(p) {
		c.wbuf = make([]byte, len(p))
	}
	buf := c.wbuf[:len(p)]
	c.enc.XORKeyStream(buf, p)
	// A short write leaves the key stream ahead of the peer; the
	// connection is broken then anyway, and net.Conn reports an error.
	return c.Conn.Write(buf)
}
