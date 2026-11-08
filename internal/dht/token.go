package dht

import (
	"crypto/rand"
	"crypto/sha1"
	"net/netip"
	"sync"
	"time"
)

// tokens hands out and checks announce tokens (BEP 5): get_peers returns a
// token bound to the asker's IP, and announce_peer must present it. A token
// is SHA-1(secret ‖ IP), truncated; the secret changes every rotateEvery and
// the previous one is still accepted, so a token lives 5–10 minutes.
type tokens struct {
	mu      sync.Mutex
	now     func() time.Time
	cur     [16]byte
	prev    [16]byte
	rotated time.Time
}

const rotateEvery = 5 * time.Minute

func newTokens(now func() time.Time) *tokens {
	t := &tokens{now: now, rotated: now()}
	rand.Read(t.cur[:])
	rand.Read(t.prev[:])
	return t
}

func (t *tokens) rotateLocked() {
	now := t.now()
	switch age := now.Sub(t.rotated); {
	case age >= 2*rotateEvery: // idle for long: no old token may survive
		rand.Read(t.cur[:])
		t.prev = t.cur
		t.rotated = now
	case age >= rotateEvery:
		t.prev = t.cur
		rand.Read(t.cur[:])
		t.rotated = now
	}
}

func token(secret [16]byte, ip netip.Addr) string {
	h := sha1.New()
	h.Write(secret[:])
	h.Write(ip.Unmap().AsSlice())
	return string(h.Sum(nil)[:8])
}

// issue returns a token for ip.
func (t *tokens) issue(ip netip.Addr) string {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rotateLocked()
	return token(t.cur, ip)
}

// valid reports whether tok was issued to ip recently.
func (t *tokens) valid(tok string, ip netip.Addr) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.rotateLocked()
	return tok == token(t.cur, ip) || tok == token(t.prev, ip)
}
