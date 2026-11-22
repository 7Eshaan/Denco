package udpmux_test

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/dht"
	"github.com/7Eshaan/Denco/internal/udpmux"
	"github.com/7Eshaan/Denco/internal/utp"
)

func TestMatchers(t *testing.T) {
	utpHdr := make([]byte, 20)
	for typ := byte(0); typ <= 4; typ++ {
		utpHdr[0] = typ<<4 | 1
		if !udpmux.IsUTP(utpHdr) || udpmux.IsDHT(utpHdr) {
			t.Errorf("uTP type %d misrouted", typ)
		}
	}
	krpc := []byte("d1:ad2:id20:aaaaaaaaaaaaaaaaaaaae1:q4:ping1:t2:aa1:y1:qe")
	if !udpmux.IsDHT(krpc) || udpmux.IsUTP(krpc) {
		t.Error("KRPC misrouted")
	}
	for _, junk := range [][]byte{nil, {0x51}, append([]byte{0x02}, make([]byte, 19)...), {0x01, 0, 0}} {
		if udpmux.IsUTP(junk) || udpmux.IsDHT(junk) {
			t.Errorf("matched %x", junk)
		}
	}
}

// One UDP port serves a DHT node and uTP at once (D64): another node
// pings ours while a uTP stream runs over the same port.
func TestDHTAndUTPShareAPort(t *testing.T) {
	m, err := udpmux.Listen("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	node, err := dht.Open(dht.Config{Conn: m.Endpoint(udpmux.IsDHT, 256), StateFile: ""})
	if err != nil {
		t.Fatal(err)
	}
	defer node.Close()
	if node.Port() != m.Port() {
		t.Fatalf("DHT on %d, mux on %d", node.Port(), m.Port())
	}
	sock := utp.NewSocket(m.Endpoint(udpmux.IsUTP, 1024), utp.Config{})
	defer sock.Close()

	other, err := dht.Open(dht.Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	pc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	client := utp.NewSocket(pc, utp.Config{})
	defer client.Close()

	data := bytes.Repeat([]byte("0123456789"), 50_000)
	got := make(chan []byte, 1)
	go func() {
		c, err := sock.Accept()
		if err != nil {
			got <- nil
			return
		}
		b, _ := io.ReadAll(c)
		got <- b
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := client.Dial(ctx, m.Addr())
	if err != nil {
		t.Fatal(err)
	}
	go func() { c.Write(data); c.Close() }()

	// Meanwhile the DHT answers: the other node learns ours.
	other.AddNode(m.Addr())
	for deadline := time.Now().Add(10 * time.Second); other.Stats().Nodes == 0; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the DHT node behind the mux never answered")
		}
	}
	select {
	case b := <-got:
		if !bytes.Equal(b, data) {
			t.Fatalf("uTP got %d of %d bytes", len(b), len(data))
		}
	case <-time.After(20 * time.Second):
		t.Fatal("uTP stream didn't finish")
	}
}

// Datagrams nobody matches are dropped; a closed mux closes its endpoints.
func TestUnmatchedDroppedAndClose(t *testing.T) {
	m, err := udpmux.Listen("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := m.Endpoint(udpmux.IsDHT, 4)
	pc, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer pc.Close()
	pc.WriteToUDPAddrPort([]byte("junk"), m.Addr())
	pc.WriteToUDPAddrPort([]byte("d1:y1:qe"), m.Addr())
	buf := make([]byte, 100)
	n, from, err := e.ReadFromUDPAddrPort(buf)
	if err != nil || string(buf[:n]) != "d1:y1:qe" || from.Port() != pc.LocalAddr().(*net.UDPAddr).AddrPort().Port() {
		t.Fatalf("got %q from %v, %v", buf[:n], from, err)
	}
	m.Close()
	if _, _, err := e.ReadFromUDPAddrPort(buf); err != net.ErrClosed {
		t.Fatalf("after close: %v", err)
	}
}
