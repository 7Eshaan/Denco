// Package utp implements uTP, the Micro Transport Protocol (BEP 29): a
// reliable, ordered byte stream over UDP, which BitTorrent clients use
// next to TCP. Its point is LEDBAT congestion control: it measures the
// one-way queuing delay its own packets cause and backs off when it rises
// above a target (100 ms), so a big download doesn't make the user's other
// traffic (calls, games, browsing) laggy, and it yields to TCP.
//
// A Socket runs over one UDP socket (usually a udpmux.Endpoint shared with
// the DHT) and is both a dialer and a net.Listener; a Conn is a net.Conn.
//
// Packets: a 20-byte header (type/version, extension, connection ID,
// timestamp, timestamp difference, window, sequence and ack numbers),
// optionally a selective-ack extension, then data. Sequence numbers count
// packets (16 bits, wrapping), not bytes.
package utp

import (
	"encoding/binary"
	"errors"
)

// Packet types.
const (
	stData  = 0
	stFin   = 1
	stState = 2 // a pure ack
	stReset = 3
	stSyn   = 4
)

const (
	version   = 1
	headerLen = 20
	extSACK   = 1
)

var errBadPacket = errors.New("utp: bad packet")

type header struct {
	typ    byte
	connID uint16
	ts     uint32 // sender's clock, microseconds
	tsDiff uint32 // receiver's last measured (arrival − send) time
	wnd    uint32 // free receive buffer, bytes
	seq    uint16
	ack    uint16
	// sack, if set, is the selective ack bitmask: bit i (LSB first in each
	// byte) means packet ack+2+i has arrived. A multiple of 4 bytes.
	sack []byte
}

// marshal appends the packet to dst.
func (h *header) marshal(dst, payload []byte) []byte {
	ext := byte(0)
	if len(h.sack) > 0 {
		ext = extSACK
	}
	var b [headerLen]byte
	b[0] = h.typ<<4 | version
	b[1] = ext
	binary.BigEndian.PutUint16(b[2:], h.connID)
	binary.BigEndian.PutUint32(b[4:], h.ts)
	binary.BigEndian.PutUint32(b[8:], h.tsDiff)
	binary.BigEndian.PutUint32(b[12:], h.wnd)
	binary.BigEndian.PutUint16(b[16:], h.seq)
	binary.BigEndian.PutUint16(b[18:], h.ack)
	dst = append(dst, b[:]...)
	if ext != 0 {
		dst = append(dst, 0, byte(len(h.sack))) // no next extension
		dst = append(dst, h.sack...)
	}
	return append(dst, payload...)
}

// parse reads a packet. Unknown extensions are skipped.
func parse(b []byte) (header, []byte, error) {
	var h header
	if len(b) < headerLen || b[0]&0x0f != version || b[0]>>4 > stSyn {
		return h, nil, errBadPacket
	}
	h.typ = b[0] >> 4
	ext := b[1]
	h.connID = binary.BigEndian.Uint16(b[2:])
	h.ts = binary.BigEndian.Uint32(b[4:])
	h.tsDiff = binary.BigEndian.Uint32(b[8:])
	h.wnd = binary.BigEndian.Uint32(b[12:])
	h.seq = binary.BigEndian.Uint16(b[16:])
	h.ack = binary.BigEndian.Uint16(b[18:])
	b = b[headerLen:]
	for ext != 0 {
		if len(b) < 2 || len(b) < 2+int(b[1]) {
			return h, nil, errBadPacket
		}
		next, n := b[0], int(b[1])
		if ext == extSACK {
			if n == 0 || n%4 != 0 {
				return h, nil, errBadPacket
			}
			h.sack = b[2 : 2+n]
		}
		ext, b = next, b[2+n:]
	}
	return h, b, nil
}

// seqLess reports a < b in 16-bit wrapping sequence space.
func seqLess(a, b uint16) bool { return int16(a-b) < 0 }
