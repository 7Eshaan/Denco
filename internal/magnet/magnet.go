// Package magnet parses magnet URIs (BEP 9), e.g.
//
//	magnet:?xt=urn:btih:<info-hash>&dn=<name>&tr=<tracker>&tr=<tracker>
//
// The info hash may be 40 hex characters or 32 base32 characters.
// A magnet link carries no file list or piece hashes; those are fetched from
// peers later (Phase 10) and validated against the info hash.
package magnet

import (
	"encoding/base32"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strings"

	"github.com/7Eshaan/Denco/internal/metainfo"
)

// ErrInvalid is wrapped by every parse error.
var ErrInvalid = errors.New("invalid magnet link")

// Magnet is a parsed magnet URI.
type Magnet struct {
	InfoHash metainfo.Hash
	Name     string           // "dn"; may be empty
	Trackers []string         // "tr", de-duplicated, in order
	Peers    []netip.AddrPort // "x.pe" with literal IP addresses (hostnames skipped)
}

const btihPrefix = "urn:btih:"

// Parse parses a magnet URI.
func Parse(uri string) (*Magnet, error) {
	u, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	if !strings.EqualFold(u.Scheme, "magnet") {
		return nil, fmt.Errorf("%w: scheme is %q, want magnet", ErrInvalid, u.Scheme)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}

	m := &Magnet{Name: q.Get("dn")}

	found := false
	for _, xt := range q["xt"] {
		// Other xt kinds (e.g. urn:btmh: for BitTorrent v2) are ignored.
		if len(xt) < len(btihPrefix) || !strings.EqualFold(xt[:len(btihPrefix)], btihPrefix) {
			continue
		}
		h, err := parseInfoHash(xt[len(btihPrefix):])
		if err != nil {
			return nil, err
		}
		if found && h != m.InfoHash {
			return nil, fmt.Errorf("%w: conflicting btih values", ErrInvalid)
		}
		m.InfoHash, found = h, true
	}
	if !found {
		return nil, fmt.Errorf("%w: missing xt=urn:btih:<hash>", ErrInvalid)
	}

	seen := make(map[string]bool)
	for _, tr := range q["tr"] {
		tr = strings.TrimSpace(tr)
		if tr != "" && !seen[tr] {
			seen[tr] = true
			m.Trackers = append(m.Trackers, tr)
		}
	}
	for _, pe := range q["x.pe"] {
		if ap, err := netip.ParseAddrPort(pe); err == nil {
			m.Peers = append(m.Peers, ap)
		}
	}
	return m, nil
}

func parseInfoHash(s string) (metainfo.Hash, error) {
	var h metainfo.Hash
	switch len(s) {
	case 40:
		if _, err := hex.Decode(h[:], []byte(s)); err != nil {
			return h, fmt.Errorf("%w: bad hex info hash", ErrInvalid)
		}
	case 32:
		b, err := base32.StdEncoding.DecodeString(strings.ToUpper(s))
		if err != nil || len(b) != metainfo.HashSize {
			return h, fmt.Errorf("%w: bad base32 info hash", ErrInvalid)
		}
		copy(h[:], b)
	default:
		return h, fmt.Errorf("%w: info hash has %d chars, want 40 (hex) or 32 (base32)", ErrInvalid, len(s))
	}
	return h, nil
}

// String renders the magnet back to a URI (hex hash, dn, then tr params).
func (m *Magnet) String() string {
	var b strings.Builder
	b.WriteString("magnet:?xt=urn:btih:")
	b.WriteString(m.InfoHash.String())
	if m.Name != "" {
		b.WriteString("&dn=")
		b.WriteString(url.QueryEscape(m.Name))
	}
	for _, tr := range m.Trackers {
		b.WriteString("&tr=")
		b.WriteString(url.QueryEscape(tr))
	}
	for _, p := range m.Peers {
		b.WriteString("&x.pe=")
		b.WriteString(url.QueryEscape(p.String()))
	}
	return b.String()
}

// IsMagnet reports whether s looks like a magnet URI (cheap check used by the
// CLI to decide how to interpret an argument).
func IsMagnet(s string) bool {
	return len(s) >= 8 && strings.EqualFold(s[:8], "magnet:?")
}
