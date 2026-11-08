package tracker

// UDP tracker protocol (BEP 15), with URL data (BEP 41).
//
// Every exchange is one request datagram and one response datagram:
//
//	connect:  → [protocol_id u64][action=0 u32][txid u32]
//	          ← [action=0][txid][connection_id u64]
//	announce: → [connection_id][action=1][txid][info_hash 20][peer_id 20]
//	            [downloaded u64][left u64][uploaded u64][event u32][ip u32]
//	            [key u32][num_want i32][port u16] [BEP 41 options…]
//	          ← [action=1][txid][interval u32][leechers u32][seeders u32]
//	            [peers: 6 bytes each (IPv4 tracker) or 18 (IPv6 tracker)]
//	error:    ← [action=3][txid][message…]
//
// UDP can drop packets, so each request is retransmitted after 15·2ⁿ seconds
// (n = 0…8). A connection ID is valid for one minute and is cached per
// tracker address so regular announces skip the connect step.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"net/url"
	"sync"
	"time"
)

const (
	udpProtocolID = 0x41727101980 // magic constant from BEP 15

	actionConnect  = 0
	actionAnnounce = 1
	actionError    = 3

	connIDLifetime = time.Minute
	maxUDPRetries  = 8 // 15·2⁸ s ≈ 1 h total; contexts normally cut this short
	maxUDPPacket   = 64 << 10
)

// errUDPTimeout is returned when no response arrived after all retries.
var errUDPTimeout = errors.New("tracker: udp timeout")

// UDPClient announces to udp:// trackers. The zero value is usable and safe
// for concurrent use; one is shared by all torrents so connection IDs are
// reused across them.
type UDPClient struct {
	// RetryBase is the first retransmit timeout (BEP 15: 15s). Tests shrink it.
	RetryBase time.Duration

	mu    sync.Mutex
	conns map[string]connID // tracker "host:port" → cached connection ID
}

type connID struct {
	id      uint64
	expires time.Time
}

// Announce performs a BEP 15 announce to a udp:// URL.
func (c *UDPClient) Announce(ctx context.Context, trackerURL string, req Request) (*Response, error) {
	u, err := url.Parse(trackerURL)
	if err != nil || u.Host == "" || u.Port() == "" {
		return nil, fmt.Errorf("tracker: bad UDP tracker URL %q", trackerURL)
	}
	var d net.Dialer
	nc, err := d.DialContext(ctx, "udp", u.Host)
	if err != nil {
		return nil, fmt.Errorf("tracker %s: %w", u.Host, err)
	}
	defer nc.Close()
	stop := context.AfterFunc(ctx, func() { nc.SetDeadline(time.Now()) })
	defer stop()

	ipv6 := false
	if ra, ok := nc.RemoteAddr().(*net.UDPAddr); ok {
		ipv6 = ra.IP.To4() == nil
	}
	resp, err := c.announce(ctx, nc, u, req, ipv6)
	if err != nil {
		if ctx.Err() != nil {
			err = ctx.Err()
		}
		return nil, fmt.Errorf("tracker %s: %w", u.Host, err)
	}
	return resp, nil
}

func (c *UDPClient) announce(ctx context.Context, nc net.Conn, u *url.URL, req Request, ipv6 bool) (*Response, error) {
	id, cached, err := c.connectionID(ctx, nc, u.Host)
	if err != nil {
		return nil, err
	}
	resp, err := c.doAnnounce(ctx, nc, id, u, req, ipv6)
	var fe *FailureError
	if errors.As(err, &fe) && cached {
		// A cached ID can be rejected (tracker restarted, clocks disagree).
		// Forget it and retry once with a fresh connect.
		c.forget(u.Host)
		if id, _, err = c.connectionID(ctx, nc, u.Host); err != nil {
			return nil, err
		}
		return c.doAnnounce(ctx, nc, id, u, req, ipv6)
	}
	return resp, err
}

// connectionID returns a valid connection ID, from cache or by connecting.
func (c *UDPClient) connectionID(ctx context.Context, nc net.Conn, host string) (uint64, bool, error) {
	c.mu.Lock()
	cid, ok := c.conns[host]
	c.mu.Unlock()
	if ok && time.Now().Before(cid.expires) {
		return cid.id, true, nil
	}

	tx := rand.Uint32()
	pkt := make([]byte, 16)
	binary.BigEndian.PutUint64(pkt[0:], udpProtocolID)
	binary.BigEndian.PutUint32(pkt[8:], actionConnect)
	binary.BigEndian.PutUint32(pkt[12:], tx)

	body, err := c.roundTrip(ctx, nc, pkt, tx, actionConnect)
	if err != nil {
		return 0, false, fmt.Errorf("connect: %w", err)
	}
	if len(body) < 8 {
		return 0, false, errors.New("connect: short response")
	}
	id := binary.BigEndian.Uint64(body)

	c.mu.Lock()
	if c.conns == nil {
		c.conns = make(map[string]connID)
	}
	c.conns[host] = connID{id: id, expires: time.Now().Add(connIDLifetime)}
	c.mu.Unlock()
	return id, false, nil
}

func (c *UDPClient) forget(host string) {
	c.mu.Lock()
	delete(c.conns, host)
	c.mu.Unlock()
}

func (c *UDPClient) doAnnounce(ctx context.Context, nc net.Conn, id uint64, u *url.URL, req Request, ipv6 bool) (*Response, error) {
	tx := rand.Uint32()
	pkt := make([]byte, 98, 98+2+300)
	binary.BigEndian.PutUint64(pkt[0:], id)
	binary.BigEndian.PutUint32(pkt[8:], actionAnnounce)
	binary.BigEndian.PutUint32(pkt[12:], tx)
	copy(pkt[16:36], req.InfoHash[:])
	copy(pkt[36:56], req.PeerID[:])
	binary.BigEndian.PutUint64(pkt[56:], uint64(req.Downloaded))
	binary.BigEndian.PutUint64(pkt[64:], uint64(req.Left))
	binary.BigEndian.PutUint64(pkt[72:], uint64(req.Uploaded))
	binary.BigEndian.PutUint32(pkt[80:], udpEvent(req.Event))
	binary.BigEndian.PutUint32(pkt[84:], 0) // IP: 0 = use the packet's source address
	binary.BigEndian.PutUint32(pkt[88:], req.Key)
	numWant := int32(req.NumWant)
	if numWant <= 0 {
		numWant = defaultNumWant
	}
	binary.BigEndian.PutUint32(pkt[92:], uint32(numWant))
	binary.BigEndian.PutUint16(pkt[96:], req.Port)
	pkt = appendURLData(pkt, u)

	body, err := c.roundTrip(ctx, nc, pkt, tx, actionAnnounce)
	if err != nil {
		return nil, err
	}
	if len(body) < 12 {
		return nil, errors.New("announce: short response")
	}
	r := &Response{
		Interval: time.Duration(binary.BigEndian.Uint32(body[0:])) * time.Second,
		Leechers: int(binary.BigEndian.Uint32(body[4:])),
		Seeders:  int(binary.BigEndian.Uint32(body[8:])),
	}
	if r.Interval <= 0 {
		r.Interval = defaultInterval
	}
	r.Interval = max(r.Interval, minInterval)
	ipLen := 4
	if ipv6 {
		ipLen = 16
	}
	r.Peers = parseCompact(string(body[12:]), ipLen)
	return r, nil
}

// roundTrip sends pkt and waits for a response with matching transaction ID
// and action, retransmitting with exponential backoff. It returns the
// response body after the 8-byte action/txid header.
func (c *UDPClient) roundTrip(ctx context.Context, nc net.Conn, pkt []byte, tx, action uint32) ([]byte, error) {
	base := c.RetryBase
	if base <= 0 {
		base = 15 * time.Second
	}
	buf := make([]byte, maxUDPPacket)
	for n := 0; n <= maxUDPRetries; n++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := nc.Write(pkt); err != nil {
			return nil, err
		}
		nc.SetReadDeadline(time.Now().Add(base << n))
		// Setting the deadline may have overwritten the one a cancellation
		// just set (see Announce); re-check so we don't wait a full period.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for {
			m, err := nc.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					break // retransmit
				}
				return nil, err // e.g. ICMP port unreachable → connection refused
			}
			if m < 8 || binary.BigEndian.Uint32(buf[4:]) != tx {
				continue // stray or stale datagram; keep waiting
			}
			got := binary.BigEndian.Uint32(buf[0:])
			if got == actionError {
				return nil, &FailureError{Reason: string(buf[8:m])}
			}
			if got != action {
				continue
			}
			return append([]byte(nil), buf[8:m]...), nil
		}
	}
	return nil, errUDPTimeout
}

func udpEvent(e Event) uint32 {
	switch e {
	case EventCompleted:
		return 1
	case EventStarted:
		return 2
	case EventStopped:
		return 3
	default:
		return 0
	}
}

// appendURLData adds the BEP 41 URLData option carrying the URL's path and
// query (some trackers use it for passkeys). Trackers that don't understand
// options ignore trailing bytes.
func appendURLData(pkt []byte, u *url.URL) []byte {
	data := u.EscapedPath()
	if u.RawQuery != "" {
		data += "?" + u.RawQuery
	}
	if data == "" || data == "/" {
		return pkt
	}
	const optURLData = 0x2
	for len(data) > 0 {
		chunk := data[:min(len(data), 255)]
		data = data[len(chunk):]
		pkt = append(pkt, optURLData, byte(len(chunk)))
		pkt = append(pkt, chunk...)
	}
	return pkt
}
