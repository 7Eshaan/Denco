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

// nodeLen is one entry of the compact node format: ID + IPv4 + port.
const nodeLen = 20 + compact.IPv4Len

// encodeNodes packs IPv4 nodes in the compact node format (IPv6 nodes,
// BEP 32, aren't supported and are skipped).
func encodeNodes(nodes []NodeInfo) string {
	b := make([]byte, 0, len(nodes)*nodeLen)
	for _, n := range nodes {
		if n.Addr.Addr().Unmap().Is4() {
			b = append(b, n.ID[:]...)
			b = compact.Append(b, n.Addr)
		}
	}
	return string(b)
}

// decodeNodes unpacks compact node info, skipping unusable addresses.
func decodeNodes(s string) []NodeInfo {
	var out []NodeInfo
	for i := 0; i+nodeLen <= len(s); i += nodeLen {
		var n NodeInfo
		copy(n.ID[:], s[i:i+20])
		addrs := compact.Parse(s[i+20:i+nodeLen], 4)
		if len(addrs) == 1 && compact.Usable(addrs[0]) {
			n.Addr = addrs[0]
			out = append(out, n)
		}
	}
	return out
}

// decodeValues reads get_peers "values": a list of 6-byte compact peers.
func decodeValues(body map[string]any) []netip.AddrPort {
	list, _ := bencode.GetList(body, "values")
	var out []netip.AddrPort
	for _, v := range list {
		s, ok := v.(string)
		if !ok || len(s) != compact.IPv4Len {
			continue
		}
		if a := compact.Parse(s, 4); compact.Usable(a[0]) {
			out = append(out, a[0])
		}
	}
	return out
}
