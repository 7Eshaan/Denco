package peer

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/utp"
)

func utpSocket(t *testing.T) *utp.Socket {
	t.Helper()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	s := utp.NewSocket(pc, utp.Config{})
	t.Cleanup(func() { s.Close() })
	return s
}

// A BitTorrent connection over uTP, encrypted too: the peer wire protocol
// doesn't care what carries it (D64).
func TestDialAcceptOverUTP(t *testing.T) {
	server, client := utpSocket(t), utpSocket(t)
	type result struct {
		c   *Conn
		err error
	}
	res := make(chan result, 1)
	go func() {
		nc, err := server.Accept()
		if err != nil {
			res <- result{nil, err}
			return
		}
		c, err := AcceptFrom(nc, oneTorrent{}, Options{PeerID: peerid.New(), Encryption: EncryptionPrefer})
		res <- result{c, err}
	}()
	addr := server.Addr().(*net.UDPAddr).AddrPort()
	opts := Options{PeerID: peerid.New(), Encryption: EncryptionPrefer, UTP: &utp.Dialer{V4: client}, Transport: TransportBoth}
	d, err := Dial(context.Background(), addr, testInfoHash, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	r := <-res
	if r.err != nil {
		t.Fatal(r.err)
	}
	defer r.c.Close()
	if !d.UTP || !r.c.UTP || !d.Encrypted || !r.c.Encrypted {
		t.Fatalf("dialer utp %v enc %v, acceptor utp %v enc %v", d.UTP, d.Encrypted, r.c.UTP, r.c.Encrypted)
	}
	if r.c.Remote != client.Addr().(*net.UDPAddr).AddrPort() {
		t.Fatalf("acceptor sees %v", r.c.Remote)
	}
	d.Send(Have(3))
	if m := recv(t, r.c); m.ID != MsgHave {
		t.Fatalf("got %v", m.ID)
	}
	r.c.Send(Have(4))
	if m := recv(t, d); m.ID != MsgHave {
		t.Fatalf("got %v", m.ID)
	}
}

// A TCP-only peer: uTP gets no answer, so Dial falls back to TCP — unless
// the transport is uTP only.
func TestTransportFallback(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			nc, err := ln.Accept()
			if err != nil {
				return
			}
			go Accept(nc, func(h metainfo.Hash) bool { return h == testInfoHash }, Options{PeerID: peerid.New()})
		}
	}()
	addr := netip.MustParseAddrPort(ln.Addr().String()) // nothing on its UDP port
	pc, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	client := utp.NewSocket(pc, utp.Config{ConnectTimeout: 300 * time.Millisecond})
	defer client.Close()
	opts := Options{PeerID: peerid.New(), UTP: &utp.Dialer{V4: client}, Transport: TransportBoth}
	c, err := Dial(context.Background(), addr, testInfoHash, opts)
	if err != nil {
		t.Fatal(err)
	}
	if c.UTP {
		t.Fatal("connected over uTP to a TCP-only peer?")
	}
	c.Close()
	opts.Transport = TransportUTP
	if _, err := Dial(context.Background(), addr, testInfoHash, opts); err == nil {
		t.Fatal("transport utp fell back to TCP")
	}
}

func TestParseTransport(t *testing.T) {
	for _, tr := range []Transport{TransportTCP, TransportBoth, TransportUTP} {
		if got, err := ParseTransport(tr.String()); err != nil || got != tr {
			t.Errorf("%v: %v %v", tr, got, err)
		}
	}
	if _, err := ParseTransport("udp"); err == nil {
		t.Error("accepted udp")
	}
}
