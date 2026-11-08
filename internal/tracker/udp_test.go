package tracker

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/peerid"
)

// fakeUDPTracker is an in-process BEP 15 tracker.
type fakeUDPTracker struct {
	t    *testing.T
	conn net.PacketConn
	url  string

	mu        sync.Mutex
	dropNext  int    // drop this many incoming packets
	silent    bool   // never answer
	failWith  string // answer announces with an error
	strayTx   bool   // send a wrong-txid packet before each real reply
	validIDs  map[uint64]bool
	connects  int
	announces int
	last      announceFields
	peers     []netip.AddrPort
}

type announceFields struct {
	infoHash, peerID      [20]byte
	downloaded, left, upl uint64
	event, key            uint32
	numWant               int32
	port                  uint16
	urlData               string
}

func newUDPTracker(t *testing.T, network, addr string) *fakeUDPTracker {
	t.Helper()
	pc, err := net.ListenPacket(network, addr)
	if err != nil {
		t.Skipf("cannot listen on %s %s: %v", network, addr, err)
	}
	ft := &fakeUDPTracker{t: t, conn: pc, validIDs: map[uint64]bool{},
		url: "udp://" + pc.LocalAddr().String() + "/announce"}
	t.Cleanup(func() { pc.Close() })
	go ft.serve()
	return ft
}

func (ft *fakeUDPTracker) serve() {
	buf := make([]byte, 2048)
	for {
		n, from, err := ft.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		pkt := append([]byte(nil), buf[:n]...)
		ft.mu.Lock()
		drop := ft.dropNext > 0 || ft.silent
		if ft.dropNext > 0 {
			ft.dropNext--
		}
		ft.mu.Unlock()
		if drop || n < 16 {
			continue
		}
		ft.reply(pkt, from)
	}
}

func (ft *fakeUDPTracker) reply(pkt []byte, from net.Addr) {
	action := binary.BigEndian.Uint32(pkt[8:])
	tx := binary.BigEndian.Uint32(pkt[12:])
	send := func(b []byte) {
		ft.mu.Lock()
		stray := ft.strayTx
		ft.mu.Unlock()
		if stray {
			junk := append([]byte(nil), b...)
			binary.BigEndian.PutUint32(junk[4:], tx+1)
			ft.conn.WriteTo(junk, from)
		}
		ft.conn.WriteTo(b, from)
	}
	errorReply := func(msg string) {
		b := binary.BigEndian.AppendUint32(nil, actionError)
		b = binary.BigEndian.AppendUint32(b, tx)
		send(append(b, msg...))
	}

	ft.mu.Lock()
	defer ft.mu.Unlock()
	switch action {
	case actionConnect:
		if binary.BigEndian.Uint64(pkt[0:]) != udpProtocolID {
			return
		}
		ft.connects++
		id := rand.Uint64()
		ft.validIDs[id] = true
		b := binary.BigEndian.AppendUint32(nil, actionConnect)
		b = binary.BigEndian.AppendUint32(b, tx)
		b = binary.BigEndian.AppendUint64(b, id)
		go send(b)

	case actionAnnounce:
		if len(pkt) < 98 {
			return
		}
		if !ft.validIDs[binary.BigEndian.Uint64(pkt[0:])] {
			go errorReply("Connection ID mismatch.")
			return
		}
		ft.announces++
		f := announceFields{
			downloaded: binary.BigEndian.Uint64(pkt[56:]),
			left:       binary.BigEndian.Uint64(pkt[64:]),
			upl:        binary.BigEndian.Uint64(pkt[72:]),
			event:      binary.BigEndian.Uint32(pkt[80:]),
			key:        binary.BigEndian.Uint32(pkt[88:]),
			numWant:    int32(binary.BigEndian.Uint32(pkt[92:])),
			port:       binary.BigEndian.Uint16(pkt[96:]),
		}
		copy(f.infoHash[:], pkt[16:36])
		copy(f.peerID[:], pkt[36:56])
		for opts := pkt[98:]; len(opts) >= 2 && opts[0] == 0x2; opts = opts[2+int(opts[1]):] {
			f.urlData += string(opts[2 : 2+int(opts[1])])
		}
		ft.last = f
		if ft.failWith != "" {
			go errorReply(ft.failWith)
			return
		}
		b := binary.BigEndian.AppendUint32(nil, actionAnnounce)
		b = binary.BigEndian.AppendUint32(b, tx)
		b = binary.BigEndian.AppendUint32(b, 1800) // interval
		b = binary.BigEndian.AppendUint32(b, 3)    // leechers
		b = binary.BigEndian.AppendUint32(b, 9)    // seeders
		for _, p := range ft.peers {
			b = append(b, p.Addr().AsSlice()...)
			b = binary.BigEndian.AppendUint16(b, p.Port())
		}
		go send(b)
	}
}

func (ft *fakeUDPTracker) set(f func(ft *fakeUDPTracker)) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	f(ft)
}

func (ft *fakeUDPTracker) counts() (connects, announces int) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	return ft.connects, ft.announces
}

func fastUDP() *UDPClient { return &UDPClient{RetryBase: 50 * time.Millisecond} }

func TestUDPAnnounce(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	ft.set(func(ft *fakeUDPTracker) {
		ft.peers = []netip.AddrPort{
			netip.MustParseAddrPort("10.0.0.1:6881"),
			netip.MustParseAddrPort("192.168.1.2:51413"),
		}
	})
	req := baseRequest()
	req.Event = EventCompleted
	req.NumWant = 77

	c := &Client{UDP: fastUDP()}
	resp, err := c.Announce(context.Background(), ft.url+"?passkey=abc", req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Interval != 30*time.Minute || resp.Seeders != 9 || resp.Leechers != 3 {
		t.Errorf("resp = %+v", resp)
	}
	if len(resp.Peers) != 2 || resp.Peers[0].String() != "10.0.0.1:6881" || resp.Peers[1].String() != "192.168.1.2:51413" {
		t.Errorf("peers = %v", resp.Peers)
	}

	ft.mu.Lock()
	f := ft.last
	ft.mu.Unlock()
	if f.infoHash != req.InfoHash || f.peerID != req.PeerID {
		t.Error("info hash / peer id not sent correctly")
	}
	if f.downloaded != 20 || f.left != 30 || f.upl != 10 || f.port != 6881 || f.key != 0xbeef || f.numWant != 77 {
		t.Errorf("fields = %+v", f)
	}
	if f.event != 1 { // completed
		t.Errorf("event = %d, want 1 (completed)", f.event)
	}
	if f.urlData != "/announce?passkey=abc" {
		t.Errorf("BEP 41 URL data = %q", f.urlData)
	}
}

func TestUDPEventCodes(t *testing.T) {
	for ev, want := range map[Event]uint32{EventNone: 0, EventCompleted: 1, EventStarted: 2, EventStopped: 3} {
		if got := udpEvent(ev); got != want {
			t.Errorf("udpEvent(%q) = %d, want %d", ev, got, want)
		}
	}
}

func TestUDPConnectionIDCached(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	c := &Client{UDP: fastUDP()}
	for i := 0; i < 3; i++ {
		if _, err := c.Announce(context.Background(), ft.url, baseRequest()); err != nil {
			t.Fatal(err)
		}
	}
	if conns, anns := ft.counts(); conns != 1 || anns != 3 {
		t.Errorf("connects=%d announces=%d, want 1 and 3", conns, anns)
	}
}

func TestUDPStaleConnectionIDRetried(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	c := &Client{UDP: fastUDP()}
	if _, err := c.Announce(context.Background(), ft.url, baseRequest()); err != nil {
		t.Fatal(err)
	}
	// Tracker "restarts": all issued IDs become invalid.
	ft.set(func(ft *fakeUDPTracker) { ft.validIDs = map[uint64]bool{} })
	if _, err := c.Announce(context.Background(), ft.url, baseRequest()); err != nil {
		t.Fatalf("stale ID not recovered: %v", err)
	}
	if conns, _ := ft.counts(); conns != 2 {
		t.Errorf("connects = %d, want 2 (reconnect after rejection)", conns)
	}
}

func TestUDPRetransmitsLostPackets(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	ft.set(func(ft *fakeUDPTracker) { ft.dropNext = 2 }) // lose the first connect + its retry
	c := &Client{UDP: fastUDP()}
	if _, err := c.Announce(context.Background(), ft.url, baseRequest()); err != nil {
		t.Fatalf("did not survive packet loss: %v", err)
	}
}

func TestUDPIgnoresStrayPackets(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	ft.set(func(ft *fakeUDPTracker) { ft.strayTx = true })
	c := &Client{UDP: fastUDP()}
	if _, err := c.Announce(context.Background(), ft.url, baseRequest()); err != nil {
		t.Fatal(err)
	}
}

func TestUDPTrackerError(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	ft.set(func(ft *fakeUDPTracker) { ft.failWith = "torrent not registered" })
	c := &Client{UDP: fastUDP()}
	_, err := c.Announce(context.Background(), ft.url, baseRequest())
	var fe *FailureError
	if !errors.As(err, &fe) || fe.Reason != "torrent not registered" {
		t.Fatalf("err = %v", err)
	}
}

func TestUDPSilentTrackerRespectsContext(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	ft.set(func(ft *fakeUDPTracker) { ft.silent = true })
	c := &Client{UDP: &UDPClient{}} // real 15s base: only the context can end this
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := c.Announce(ctx, ft.url, baseRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("context deadline not honoured")
	}
}

func TestUDPGivesUpAfterRetries(t *testing.T) {
	ft := newUDPTracker(t, "udp4", "127.0.0.1:0")
	ft.set(func(ft *fakeUDPTracker) { ft.silent = true })
	c := &Client{UDP: &UDPClient{RetryBase: time.Millisecond}} // 1+2+…+256 ms ≈ 0.5s
	_, err := c.Announce(context.Background(), ft.url, baseRequest())
	if !errors.Is(err, errUDPTimeout) {
		t.Fatalf("err = %v", err)
	}
}

func TestUDPIPv6Tracker(t *testing.T) {
	ft := newUDPTracker(t, "udp6", "[::1]:0")
	ft.set(func(ft *fakeUDPTracker) { ft.peers = []netip.AddrPort{netip.MustParseAddrPort("[2001:db8::1]:6881")} })
	c := &Client{UDP: fastUDP()}
	resp, err := c.Announce(context.Background(), ft.url, baseRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Peers) != 1 || resp.Peers[0].String() != "[2001:db8::1]:6881" {
		t.Errorf("peers = %v (IPv6 tracker must use 18-byte entries)", resp.Peers)
	}
}

func TestUDPBadURLs(t *testing.T) {
	c := &Client{UDP: fastUDP()}
	for _, u := range []string{"udp://", "udp://host-without-port", "udp:///announce"} {
		if _, err := c.Announce(context.Background(), u, baseRequest()); err == nil || !strings.Contains(err.Error(), "bad UDP tracker URL") {
			t.Errorf("%q: err = %v", u, err)
		}
	}
}

func TestUDPConnectionRefusedFailsFast(t *testing.T) {
	// A UDP port with nobody listening: the OS usually reports ICMP
	// "port unreachable" as a read error, which should end the attempt
	// immediately instead of waiting out retransmits.
	pc, _ := net.ListenPacket("udp4", "127.0.0.1:0")
	addr := pc.LocalAddr().String()
	pc.Close()
	c := &Client{UDP: &UDPClient{RetryBase: time.Second}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if _, err := c.Announce(ctx, "udp://"+addr+"/announce", baseRequest()); err == nil {
		t.Fatal("expected an error")
	}
	t.Logf("closed UDP port failed after %v", time.Since(start).Round(time.Millisecond))
}

func TestURLData(t *testing.T) {
	mk := func(raw string) string {
		u, _ := url.Parse(raw)
		pkt := appendURLData(nil, u)
		var out string
		for len(pkt) >= 2 {
			n := int(pkt[1])
			out += string(pkt[2 : 2+n])
			pkt = pkt[2+n:]
		}
		return out
	}
	if got := mk("udp://t:1"); got != "" {
		t.Errorf("no path → %q", got)
	}
	if got := mk("udp://t:1/"); got != "" {
		t.Errorf("root path → %q", got)
	}
	long := "/" + strings.Repeat("x", 600)
	if got := mk("udp://t:1" + long); got != long {
		t.Errorf("long path split/rejoin failed (len %d)", len(got))
	}
}

func TestUDPWithList(t *testing.T) {
	// End to end through List: a dead UDP tracker falls back to a live one
	// in the same tier.
	live := newUDPTracker(t, "udp4", "127.0.0.1:0")
	dead := newUDPTracker(t, "udp4", "127.0.0.1:0")
	dead.set(func(ft *fakeUDPTracker) { ft.silent = true })
	l := NewList(nil)
	l.tiers = [][]string{{dead.url, live.url}}
	l.AttemptTimeout = 200 * time.Millisecond
	res := l.AnnounceAll(context.Background(), &Client{UDP: &UDPClient{}}, Request{PeerID: peerid.New(), Port: 1})
	if res[0].URL != live.url || res[0].Response.Seeders != 9 {
		t.Fatalf("result = %+v", res[0])
	}
}
