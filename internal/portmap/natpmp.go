package portmap

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// NAT-PMP (RFC 6886). Requests go to the gateway's port 5351; each is
// sent again if no answer comes, waiting 250 ms, then 500 ms, then 1 s
// (the RFC keeps doubling up to 64 s; a router that hasn't answered after
// ~2 s is taken not to speak NAT-PMP, and UPnP is tried instead).

const (
	natpmpPort     = 5351
	natpmpLifetime = 7200 // seconds, the RFC's recommendation
)

var natpmpTries = []time.Duration{250 * time.Millisecond, 500 * time.Millisecond, time.Second}

var errNoAnswer = errors.New("no answer")

type natpmp struct {
	gw netip.AddrPort
}

func (n *natpmp) method() string { return "NAT-PMP" }
func (n *natpmp) addr() string   { return n.gw.Addr().String() }

// natpmpResultText explains the RFC's result codes.
var natpmpResultText = map[uint16]string{
	1: "unsupported version",
	2: "not authorized (port mapping is off on the router)",
	3: "network failure",
	4: "out of resources",
	5: "unsupported opcode",
}

// call sends req and returns the first answer to it (same opcode + 128,
// at least size bytes long, result code 0).
func (n *natpmp) call(ctx context.Context, req []byte, size int) ([]byte, error) {
	conn, err := net.DialUDP("udp", nil, net.UDPAddrFromAddrPort(n.gw))
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	// Stopping (ctx) wakes a pending read at once.
	stop := context.AfterFunc(ctx, func() { conn.SetReadDeadline(time.Now()) })
	defer stop()
	buf := make([]byte, 64)
	for _, wait := range natpmpTries {
		if _, err := conn.Write(req); err != nil {
			return nil, err
		}
		deadline := time.Now().Add(wait)
		if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
			deadline = d
		}
		conn.SetReadDeadline(deadline)
		for {
			k, err := conn.Read(buf)
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				break // send again
			}
			if err != nil {
				return nil, err // e.g. "connection refused": nothing listens on 5351
			}
			if k < 4 || buf[0] != 0 || buf[1] != req[1]+128 {
				continue // not an answer to this request
			}
			if code := binary.BigEndian.Uint16(buf[2:4]); code != 0 {
				if text, ok := natpmpResultText[code]; ok {
					return nil, fmt.Errorf("router says: %s", text)
				}
				return nil, fmt.Errorf("router error %d", code)
			}
			if k < size {
				continue
			}
			return buf[:k], nil
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return nil, errNoAnswer
}

func (n *natpmp) externalIP(ctx context.Context) (netip.Addr, error) {
	resp, err := n.call(ctx, []byte{0, 0}, 12)
	if err != nil {
		return netip.Addr{}, err
	}
	return netip.AddrFrom4([4]byte(resp[8:12])), nil
}

func natpmpOp(p Proto) byte {
	if p == UDP {
		return 1
	}
	return 2
}

func (n *natpmp) request(ctx context.Context, proto Proto, internal, external uint16, lifetime uint32) ([]byte, error) {
	req := make([]byte, 12)
	req[1] = natpmpOp(proto)
	binary.BigEndian.PutUint16(req[4:], internal)
	binary.BigEndian.PutUint16(req[6:], external)
	binary.BigEndian.PutUint32(req[8:], lifetime)
	resp, err := n.call(ctx, req, 16)
	if err != nil {
		return nil, err
	}
	if binary.BigEndian.Uint16(resp[8:10]) != internal {
		return nil, fmt.Errorf("router answered for port %d, not %d", binary.BigEndian.Uint16(resp[8:10]), internal)
	}
	return resp, nil
}

func (n *natpmp) add(ctx context.Context, p Port, external uint16) (uint16, time.Duration, error) {
	resp, err := n.request(ctx, p.Proto, p.Internal, external, natpmpLifetime)
	if err != nil {
		return 0, 0, err
	}
	ext := binary.BigEndian.Uint16(resp[10:12])
	life := binary.BigEndian.Uint32(resp[12:16])
	if ext == 0 || life == 0 {
		return 0, 0, errors.New("router granted no mapping")
	}
	return ext, time.Duration(life) * time.Second, nil
}

// remove: lifetime 0 and external port 0 delete the mapping (RFC 6886 §3.4).
func (n *natpmp) remove(ctx context.Context, m Mapping) error {
	_, err := n.request(ctx, m.Proto, m.Internal, 0, 0)
	return err
}
