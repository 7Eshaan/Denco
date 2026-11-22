package peer

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/mse"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// oneTorrent serves testInfoHash, plain and encrypted.
type oneTorrent struct{}

func (oneTorrent) Has(h metainfo.Hash) bool { return h == testInfoHash }
func (oneTorrent) FindObfuscated(req2 [20]byte) (metainfo.Hash, bool) {
	return testInfoHash, req2 == mse.Req2(testInfoHash)
}

// TestEncryptionPolicies dials with every policy to an acceptor with every
// policy. Prefer → disable falls back to a second, plain connection.
func TestEncryptionPolicies(t *testing.T) {
	off, prefer, require := EncryptionOff, EncryptionPrefer, EncryptionRequire
	for _, tc := range []struct {
		dial, accept Encryption
		ok           bool // a connection is made
		encrypted    bool
		attempts     int // connections the acceptor sees
	}{
		{off, off, true, false, 1},
		{off, prefer, true, false, 1},
		{off, require, false, false, 1},
		{prefer, off, true, false, 2},
		{prefer, prefer, true, true, 1},
		{prefer, require, true, true, 1},
		{require, off, false, false, 1},
		{require, prefer, true, true, 1},
		{require, require, true, true, 1},
	} {
		t.Run(tc.dial.String()+"→"+tc.accept.String(), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			type result struct {
				c   *Conn
				err error
			}
			results := make(chan result, 4)
			go func() {
				for {
					nc, err := ln.Accept()
					if err != nil {
						return
					}
					c, err := AcceptFrom(nc, oneTorrent{}, Options{PeerID: peerid.New(), Encryption: tc.accept})
					results <- result{c, err}
				}
			}()
			addr := netip.MustParseAddrPort(ln.Addr().String())
			d, err := Dial(context.Background(), addr, testInfoHash, Options{PeerID: peerid.New(), Encryption: tc.dial})
			if (err == nil) != tc.ok {
				t.Fatalf("dial: %v", err)
			}
			var acc *Conn
			for i := range tc.attempts {
				r := <-results
				if i < tc.attempts-1 || !tc.ok {
					if r.err == nil {
						t.Fatalf("attempt %d accepted", i+1)
					}
					continue
				}
				if r.err != nil {
					t.Fatalf("accept: %v", r.err)
				}
				acc = r.c
			}
			if !tc.ok {
				return
			}
			defer d.Close()
			defer acc.Close()
			if d.Encrypted != tc.encrypted || acc.Encrypted != tc.encrypted {
				t.Fatalf("encrypted: dialer %v, acceptor %v, want %v", d.Encrypted, acc.Encrypted, tc.encrypted)
			}
			// Messages flow both ways after the handshake.
			d.Send(Have(7))
			acc.Send(Have(9))
			if m := recv(t, acc); m.ID != MsgHave {
				t.Fatalf("acceptor got %v", m.ID)
			}
			if m := recv(t, d); m.ID != MsgHave {
				t.Fatalf("dialer got %v", m.ID)
			}
			select {
			case r := <-results:
				t.Fatalf("an extra connection: %v", r.err)
			default:
			}
		})
	}
}

// Requiring encryption refuses a plain incoming peer, and a dialer whose
// peer doesn't answer MSE says so.
func TestEncryptionRefusalErrors(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errs := make(chan error, 1)
	go func() {
		nc, err := ln.Accept()
		if err == nil {
			_, err = AcceptFrom(nc, oneTorrent{}, Options{PeerID: peerid.New(), Encryption: EncryptionRequire})
		}
		errs <- err
	}()
	addr := netip.MustParseAddrPort(ln.Addr().String())
	if _, err := Dial(context.Background(), addr, testInfoHash, Options{PeerID: peerid.New()}); err == nil {
		t.Fatal("plain dial to a require-only peer succeeded")
	}
	if err := <-errs; !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("acceptor: %v", err)
	}

	// A plain peer that hangs up on MSE: require doesn't fall back.
	addr = rawPeer(t, func(nc net.Conn) {
		ReadHandshake(nc) // fails on MSE's random key
	})
	_, err = Dial(context.Background(), addr, testInfoHash, Options{PeerID: peerid.New(), Encryption: EncryptionRequire})
	if !errors.Is(err, ErrPlaintextRefused) {
		t.Fatalf("dialer: %v", err)
	}
}

// An encrypted peer must ask for the same torrent in the MSE handshake and
// the BitTorrent one.
func TestAcceptEncryptedHashMismatch(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	errs := make(chan error, 1)
	go func() {
		nc, err := ln.Accept()
		if err == nil {
			_, err = AcceptFrom(nc, oneTorrent{}, Options{PeerID: peerid.New(), Encryption: EncryptionPrefer})
		}
		errs <- err
	}()
	nc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	other := (&Handshake{InfoHash: metainfo.Hash{9}, PeerID: peerid.New()}).MarshalBinary()
	mse.Initiate(nc, testInfoHash, mse.RC4, other) // the acceptor hangs up after reading IA
	if err := <-errs; !errors.Is(err, ErrInfoHashMismatch) {
		t.Fatalf("got %v", err)
	}
}

func TestParseEncryption(t *testing.T) {
	for _, e := range []Encryption{EncryptionOff, EncryptionPrefer, EncryptionRequire} {
		if got, err := ParseEncryption(e.String()); err != nil || got != e {
			t.Errorf("%v: %v, %v", e, got, err)
		}
	}
	if _, err := ParseEncryption("on"); err == nil {
		t.Error("accepted \"on\"")
	}
}

// A peer offering only plaintext-after-the-handshake is served when
// encryption is preferred (unencrypted), and refused when it is required.
func TestAcceptPlaintextMethod(t *testing.T) {
	for _, policy := range []Encryption{EncryptionPrefer, EncryptionRequire} {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer ln.Close()
		type result struct {
			c   *Conn
			err error
		}
		res := make(chan result, 1)
		go func() {
			nc, err := ln.Accept()
			if err != nil {
				res <- result{nil, err}
				return
			}
			c, err := AcceptFrom(nc, oneTorrent{}, Options{PeerID: peerid.New(), Encryption: policy})
			res <- result{c, err}
		}()
		nc, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer nc.Close()
		nc.SetDeadline(time.Now().Add(5 * time.Second))
		hs := (&Handshake{InfoHash: testInfoHash, PeerID: peerid.New()}).MarshalBinary()
		go mse.Initiate(nc, testInfoHash, mse.Plaintext, hs)
		r := <-res
		switch {
		case policy == EncryptionRequire && !errors.Is(r.err, mse.ErrNoCommonMethod):
			t.Errorf("require: %v", r.err)
		case policy == EncryptionPrefer && (r.err != nil || r.c.Encrypted):
			t.Errorf("prefer: %v, encrypted %v", r.err, r.c != nil && r.c.Encrypted)
		}
		if r.c != nil {
			r.c.Close()
		}
	}
}
