package peer

import (
	"fmt"
	"net/netip"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/compact"
)

// BEP 11 peer exchange (ut_pex). About once a minute, peers tell each other
// which peers they connected to ("added") and lost ("dropped") since the
// last message. The body is a bencoded dict of compact address lists:
//
//	added, added.f, dropped        IPv4: 6 bytes per peer; added.f = 1 flag byte per added peer
//	added6, added6.f, dropped6     IPv6: 18 bytes per peer
//
// https://www.bittorrent.org/beps/bep_0011.html

// PEX flags (added.f), one byte per added peer.
const (
	PEXEncryption = 0x01 // prefers encrypted connections
	PEXSeed       = 0x02 // is a seed / upload-only
	PEXUTP        = 0x04 // supports uTP
	PEXHolepunch  = 0x08 // supports ut_holepunch
	PEXReachable  = 0x10 // the sender connected *to* it, so it accepts connections
)

// MaxPEXPeers is the most added (and, separately, dropped) peers per
// message, the limit mainstream clients use.
const MaxPEXPeers = 50

// maxPEXMessage bounds a message we accept: 2 × 50 IPv4 + 2 × 50 IPv6
// entries plus flags is under 3 KB, so 64 KiB is generous.
const maxPEXMessage = 64 << 10

// PEX is one ut_pex message. AddedFlags is parallel to Added (0 where the
// sender gave no flag). IPv4 and IPv6 peers share the lists; Encode splits them.
type PEX struct {
	Added      []netip.AddrPort
	AddedFlags []byte
	Dropped    []netip.AddrPort
}

// Flags returns the flags of Added[i] (0 if unknown).
func (m *PEX) Flags(i int) byte {
	if i < len(m.AddedFlags) {
		return m.AddedFlags[i]
	}
	return 0
}

// Empty reports whether the message carries nothing.
func (m *PEX) Empty() bool { return len(m.Added) == 0 && len(m.Dropped) == 0 }

// Encode returns the bencoded body (without the extended-message header).
func (m PEX) Encode() []byte {
	var a4, f4, a6, f6, d4, d6 []byte
	for i, a := range m.Added {
		a = unmapped(a)
		if a.Addr().Is4() {
			a4, f4 = compact.Append(a4, a), append(f4, m.Flags(i))
		} else {
			a6, f6 = compact.Append(a6, a), append(f6, m.Flags(i))
		}
	}
	for _, a := range m.Dropped {
		a = unmapped(a)
		if a.Addr().Is4() {
			d4 = compact.Append(d4, a)
		} else {
			d6 = compact.Append(d6, a)
		}
	}
	// BEP 11 clients expect all three IPv4 keys even when empty.
	d := map[string]any{"added": a4, "added.f": f4, "dropped": d4}
	if len(a6) > 0 {
		d["added6"], d["added6.f"] = a6, f6
	}
	if len(d6) > 0 {
		d["dropped6"] = d6
	}
	b, _ := bencode.Encode(d) // only supported types above
	return b
}

// ParsePEX decodes a ut_pex body. Like the extension handshake it is strict
// about the shape (a dict) and lenient inside it: malformed lists, partial
// trailing entries and addresses with port 0 are skipped.
func ParsePEX(b []byte) (PEX, error) {
	var m PEX
	if len(b) > maxPEXMessage {
		return m, fmt.Errorf("%w: ut_pex message of %d bytes", ErrBadPayload, len(b))
	}
	v, err := bencode.Decode(b)
	if err != nil {
		return m, fmt.Errorf("%w: ut_pex: %v", ErrBadPayload, err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return m, fmt.Errorf("%w: ut_pex message is not a dict", ErrBadPayload)
	}
	get := func(k string) string { s, _ := bencode.GetString(d, k); return s }
	for _, fam := range []struct {
		added, flags, dropped string
		ipLen                 int
	}{{"added", "added.f", "dropped", 4}, {"added6", "added6.f", "dropped6", 16}} {
		flags := get(fam.flags)
		for i, a := range compact.Parse(get(fam.added), fam.ipLen) {
			if !compact.Usable(a) {
				continue // its flag (index i) is skipped with it
			}
			var f byte
			if i < len(flags) {
				f = flags[i]
			}
			m.Added = append(m.Added, a)
			m.AddedFlags = append(m.AddedFlags, f)
		}
		for _, a := range compact.Parse(get(fam.dropped), fam.ipLen) {
			if compact.Usable(a) {
				m.Dropped = append(m.Dropped, a)
			}
		}
	}
	return m, nil
}

func unmapped(a netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(a.Addr().Unmap(), a.Port())
}
