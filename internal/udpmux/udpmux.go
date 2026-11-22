// Package udpmux shares one UDP socket between protocols. BitTorrent
// clients run the DHT and uTP on the same UDP port (the peer port's
// number), so one socket must serve both: a reader goroutine hands each
// datagram to the first Endpoint whose match function accepts it.
//
// An Endpoint looks like a UDP socket to its user (ReadFromUDPAddrPort,
// WriteToUDPAddrPort, LocalAddr, Close). Its queue is bounded: when its
// reader falls behind, datagrams are dropped, as the network would.
package udpmux

import (
	"errors"
	"net"
	"net/netip"
	"sync"
)

// Mux owns a UDP socket and its reader goroutine.
type Mux struct {
	conn *net.UDPConn

	mu        sync.Mutex
	endpoints []*Endpoint
	closed    bool
	done      chan struct{} // closed when the reader has stopped
}

// Listen opens a UDP socket ("udp4" or "udp6", addr like ":6881").
func Listen(network, addr string) (*Mux, error) {
	ua, err := net.ResolveUDPAddr(network, addr)
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP(network, ua)
	if err != nil {
		return nil, err
	}
	m := &Mux{conn: conn, done: make(chan struct{})}
	go m.read()
	return m, nil
}

// Port is the UDP port listened on.
func (m *Mux) Port() uint16 { return m.conn.LocalAddr().(*net.UDPAddr).AddrPort().Port() }

// Addr is the local address.
func (m *Mux) Addr() netip.AddrPort { return m.conn.LocalAddr().(*net.UDPAddr).AddrPort() }

// Endpoint adds a protocol: datagrams for which match returns true (and no
// earlier endpoint's did) go to it, up to queue of them waiting.
func (m *Mux) Endpoint(match func([]byte) bool, queue int) *Endpoint {
	e := &Endpoint{m: m, match: match, in: make(chan packet, queue), done: make(chan struct{})}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		e.close()
	}
	m.endpoints = append(m.endpoints, e)
	return e
}

// Close closes the socket and every endpoint, and waits for the reader.
func (m *Mux) Close() error {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
	err := m.conn.Close()
	<-m.done
	return err
}

type packet struct {
	b    []byte
	from netip.AddrPort
}

func (m *Mux) read() {
	defer close(m.done)
	buf := make([]byte, 64<<10)
	for {
		n, from, err := m.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) || m.isClosed() {
				m.mu.Lock()
				for _, e := range m.endpoints {
					e.close()
				}
				m.mu.Unlock()
				return
			}
			continue // e.g. an ICMP error reported on the socket (Windows)
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		b := buf[:n]
		m.mu.Lock()
		eps := m.endpoints
		m.mu.Unlock()
		for _, e := range eps {
			if !e.match(b) {
				continue
			}
			select {
			case e.in <- packet{append([]byte(nil), b...), from}:
			default: // its reader is behind: drop, like the network would
			}
			break
		}
	}
}

func (m *Mux) isClosed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.closed
}

// Endpoint is one protocol's share of the socket. Safe for concurrent use.
type Endpoint struct {
	m     *Mux
	match func([]byte) bool
	in    chan packet

	once sync.Once
	done chan struct{}
}

// ReadFromUDPAddrPort waits for the next datagram for this endpoint. It
// returns net.ErrClosed once the endpoint or the mux is closed.
func (e *Endpoint) ReadFromUDPAddrPort(b []byte) (int, netip.AddrPort, error) {
	select {
	case p := <-e.in:
		return copy(b, p.b), p.from, nil
	case <-e.done:
		return 0, netip.AddrPort{}, net.ErrClosed
	}
}

// WriteToUDPAddrPort sends a datagram from the shared socket.
func (e *Endpoint) WriteToUDPAddrPort(b []byte, to netip.AddrPort) (int, error) {
	select {
	case <-e.done:
		return 0, net.ErrClosed
	default:
	}
	return e.m.conn.WriteToUDPAddrPort(b, to)
}

// LocalAddr is the shared socket's address.
func (e *Endpoint) LocalAddr() net.Addr { return e.m.conn.LocalAddr() }

// Close stops delivering to this endpoint; the socket stays open for the
// others (Mux.Close closes it).
func (e *Endpoint) Close() error {
	e.close()
	return nil
}

func (e *Endpoint) close() { e.once.Do(func() { close(e.done) }) }

// IsDHT matches KRPC messages: bencoded dictionaries.
func IsDHT(b []byte) bool { return len(b) > 0 && b[0] == 'd' }

// IsUTP matches uTP packets (BEP 29): version 1, a known type, a full
// header.
func IsUTP(b []byte) bool { return len(b) >= 20 && b[0]&0x0f == 1 && b[0]>>4 <= 4 }
