package dht

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math/bits"
	"net/netip"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/compact"
)

// KRPC (BEP 5): every message is one bencoded dict in one UDP datagram.
//
//	query:    {"t": <tid>, "y": "q", "q": <method>, "a": {"id": <sender id>, …}}
//	response: {"t": <tid>, "y": "r", "r": {"id": <sender id>, …}}
//	error:    {"t": <tid>, "y": "e", "e": [<code>, <message>]}

// ID is a 160-bit node ID; info hashes live in the same space.
type ID [20]byte

// NewID returns a random node ID.
func NewID() ID {
	var id ID
	if _, err := rand.Read(id[:]); err != nil {
		panic(fmt.Sprintf("dht: crypto/rand failed: %v", err))
	}
	return id
}

func (id ID) String() string { return hex.EncodeToString(id[:]) }

// distance is the XOR metric; comparing distances byte-wise compares them
// as 160-bit numbers.
func distance(a, b ID) ID {
	var d ID
	for i := range d {
		d[i] = a[i] ^ b[i]
	}
	return d
}

// closer reports whether a is closer to target than b.
func closer(target, a, b ID) bool {
	da, db := distance(target, a), distance(target, b)
	for i := range da {
		if da[i] != db[i] {
			return da[i] < db[i]
		}
	}
	return false
}

// commonPrefix is how many leading bits a and b share (160 if equal).
func commonPrefix(a, b ID) int {
	for i := range a {
		if x := a[i] ^ b[i]; x != 0 {
			return i*8 + bits.LeadingZeros8(x)
		}
	}
	return 160
}

// Error codes (BEP 5).
const (
	errGeneric  = 201
	errServer   = 202
	errProtocol = 203
	errMethod   = 204
)

// msg is a decoded KRPC message.
type msg struct {
	tid    string
	kind   string // "q", "r" or "e"
	method string // queries only
	id     ID     // sender's node ID (queries and responses)
	body   map[string]any
	code   int // errors only
	text   string
}

var errBadMessage = errors.New("dht: bad message")

// decode parses a datagram. Queries and responses must carry a 20-byte id.
func decode(b []byte) (msg, error) {
	var m msg
	v, err := bencode.Decode(b)
	if err != nil {
		return m, fmt.Errorf("%w: %v", errBadMessage, err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return m, fmt.Errorf("%w: not a dict", errBadMessage)
	}
	m.tid, _ = bencode.GetString(d, "t")
	m.kind, _ = bencode.GetString(d, "y")
	switch m.kind {
	case "q":
		m.method, _ = bencode.GetString(d, "q")
		m.body, _ = bencode.GetDict(d, "a")
	case "r":
		m.body, _ = bencode.GetDict(d, "r")
	case "e":
		if l, ok := bencode.GetList(d, "e"); ok && len(l) >= 2 {
			c, _ := l[0].(int64)
			m.code = int(c)
			m.text, _ = l[1].(string)
		}
		return m, nil
	default:
		return m, fmt.Errorf("%w: y=%q", errBadMessage, m.kind)
	}
	if m.body == nil {
		return m, fmt.Errorf("%w: no arguments", errBadMessage)
	}
	id, ok := m.hash("id")
	if !ok {
		return m, fmt.Errorf("%w: missing id", errBadMessage)
	}
	m.id = id
	return m, nil
}

// hash reads a 20-byte string from the body.
func (m msg) hash(key string) (ID, bool) {
	var h ID
	s, ok := bencode.GetString(m.body, key)
	if !ok || len(s) != len(h) {
		return h, false
	}
	copy(h[:], s)
	return h, true
}

func encodeQuery(tid, method string, args map[string]any) []byte {
	b, _ := bencode.Encode(map[string]any{"t": tid, "y": "q", "q": method, "a": args})
	return b
}

func encodeResponse(tid string, r map[string]any) []byte {
	b, _ := bencode.Encode(map[string]any{"t": tid, "y": "r", "r": r})
	return b
}

func encodeError(tid string, code int, text string) []byte {
	b, _ := bencode.Encode(map[string]any{"t": tid, "y": "e", "e": []any{code, text}})
	return b
}

// NodeInfo is a node's ID and UDP address.
type NodeInfo struct {
	ID   ID
	Addr netip.AddrPort
}

// Compact node info (BEP 5, BEP 32): a 20-byte ID and a compact address,
// 26 bytes per IPv4 node under "nodes", 38 per IPv6 node under "nodes6".
const (
	nodeLen  = 20 + compact.IPv4Len
	nodeLen6 = 20 + compact.IPv6Len
)

// nodesKey is the key a node list of one address family travels under.
func nodesKey(v6 bool) string {
	if v6 {
		return "nodes6"
	}
	return "nodes"
}

// isFamily reports whether a is an address of the family (v6 or IPv4).
func isFamily(a netip.AddrPort, v6 bool) bool {
	ip := a.Addr().Unmap()
	return ip.Is6() == v6
}

// encodeNodes packs the nodes of one address family in the compact node
// format; nodes of the other family are skipped.
func encodeNodes(nodes []NodeInfo, v6 bool) string {
	n6 := nodeLen
	if v6 {
		n6 = nodeLen6
	}
	b := make([]byte, 0, len(nodes)*n6)
	for _, n := range nodes {
		if isFamily(n.Addr, v6) {
			b = append(b, n.ID[:]...)
			b = compact.Append(b, n.Addr)
		}
	}
	return string(b)
}

// decodeNodes unpacks compact node info of one family, skipping unusable
// addresses.
func decodeNodes(s string, v6 bool) []NodeInfo {
	size, ipLen := nodeLen, 4
	if v6 {
		size, ipLen = nodeLen6, 16
	}
	var out []NodeInfo
	for i := 0; i+size <= len(s); i += size {
		var n NodeInfo
		copy(n.ID[:], s[i:i+20])
		addrs := compact.Parse(s[i+20:i+size], ipLen)
		if len(addrs) == 1 && compact.Usable(addrs[0]) && isFamily(addrs[0], v6) {
			n.Addr = addrs[0]
			out = append(out, n)
		}
	}
	return out
}

// decodeValues reads get_peers "values": a list of compact peers, 6 bytes
// for IPv4 and 18 for IPv6 (BEP 32: an IPv6 node returns IPv6 peers).
func decodeValues(body map[string]any) []netip.AddrPort {
	list, _ := bencode.GetList(body, "values")
	var out []netip.AddrPort
	for _, v := range list {
		s, ok := v.(string)
		var a []netip.AddrPort
		switch {
		case ok && len(s) == compact.IPv4Len:
			a = compact.Parse(s, 4)
		case ok && len(s) == compact.IPv6Len:
			a = compact.Parse(s, 16)
		}
		if len(a) == 1 && compact.Usable(a[0]) {
			out = append(out, a[0])
		}
	}
	return out
}
