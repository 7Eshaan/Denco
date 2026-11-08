// Package compact reads and writes the "compact" peer address format
// shared by trackers (BEP 23, BEP 7), peer exchange (BEP 11) and the DHT
// (BEP 5): an IPv4 peer is 4 address bytes + 2 port bytes (big-endian),
// an IPv6 peer 16 + 2.
package compact

import (
	"encoding/binary"
	"net/netip"
)

// Sizes of one entry.
const (
	IPv4Len = 4 + 2
	IPv6Len = 16 + 2
)

// Append adds a in compact form: 6 bytes if it is IPv4 (or IPv4-mapped
// IPv6), else 18.
func Append(b []byte, a netip.AddrPort) []byte {
	b = append(b, a.Addr().Unmap().AsSlice()...)
	return binary.BigEndian.AppendUint16(b, a.Port())
}

// Parse decodes every whole entry of s, where each address is ipLen bytes
// (4 or 16), in order and unfiltered (IPv4-mapped addresses are unmapped).
// A trailing partial entry is ignored. Callers drop what they can't use,
// usually with Usable.
func Parse(s string, ipLen int) []netip.AddrPort {
	entry := ipLen + 2
	out := make([]netip.AddrPort, 0, len(s)/entry)
	for i := 0; i+entry <= len(s); i += entry {
		ip, _ := netip.AddrFromSlice([]byte(s[i : i+ipLen]))
		port := binary.BigEndian.Uint16([]byte(s[i+ipLen : i+entry]))
		out = append(out, netip.AddrPortFrom(ip.Unmap(), port))
	}
	return out
}

// Usable reports whether a could be a peer: a real unicast address with a
// non-zero port.
func Usable(a netip.AddrPort) bool {
	ip := a.Addr()
	return a.IsValid() && a.Port() != 0 && !ip.IsUnspecified() && !ip.IsMulticast()
}
