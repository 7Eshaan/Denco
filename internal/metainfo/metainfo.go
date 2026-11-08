// Package metainfo parses and validates .torrent files (BEP 3), exposing the
// info hash, piece hashes, file layout and tracker list.
//
// Validation is deliberately strict about anything that could be malicious
// (path traversal) or that would make the download impossible (piece count
// mismatch). See docs/DECISIONS.md D8 and D9.
package metainfo

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
)

// HashSize is the length of a SHA-1 digest (info hash and piece hashes).
const HashSize = sha1.Size

// Hash is a 20-byte SHA-1 digest.
type Hash [HashSize]byte

// String renders the hash as 40 lowercase hex characters.
func (h Hash) String() string { return hex.EncodeToString(h[:]) }

// ErrInvalid is wrapped by every validation error.
var ErrInvalid = errors.New("invalid torrent")

// MetaInfo is a parsed .torrent file.
type MetaInfo struct {
	Announce     string
	AnnounceList [][]string // BEP 12 tiers; may be empty
	Comment      string
	CreatedBy    string
	CreationDate time.Time // zero if absent
	URLList      []string  // BEP 19 web seeds (parsed, not used yet)

	InfoHash  Hash
	InfoBytes []byte // raw bencoded info dict, exactly as in the file
	Info      Info
}

// Load reads and parses a .torrent file from disk.
func Load(path string) (*MetaInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mi, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return mi, nil
}

// Parse decodes and validates a .torrent file.
func Parse(data []byte) (*MetaInfo, error) {
	spans, err := bencode.DictSpans(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	infoBytes, ok := spans["info"]
	if !ok {
		return nil, invalid("missing info dictionary")
	}
	info, err := ParseInfo(infoBytes)
	if err != nil {
		return nil, err
	}

	// The whole file already decoded once inside DictSpans' validation, so
	// this can only fail on a bug; still handle it rather than panic.
	v, err := bencode.Decode(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	root := v.(map[string]any)

	mi := &MetaInfo{
		InfoHash:  sha1.Sum(infoBytes),
		InfoBytes: append([]byte(nil), infoBytes...),
		Info:      *info,
	}
	mi.Announce, _ = bencode.GetString(root, "announce")
	mi.Comment, _ = bencode.GetString(root, "comment")
	mi.CreatedBy, _ = bencode.GetString(root, "created by")
	if ts, ok := bencode.GetInt(root, "creation date"); ok && ts > 0 {
		mi.CreationDate = time.Unix(ts, 0).UTC()
	}
	mi.AnnounceList = parseAnnounceList(root)
	mi.URLList = parseURLList(root)
	return mi, nil
}

// Trackers returns all tracker URLs (announce-list tiers first, then
// announce), de-duplicated, in priority order.
func (m *MetaInfo) Trackers() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(u string) {
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	for _, tier := range m.AnnounceList {
		for _, u := range tier {
			add(u)
		}
	}
	add(m.Announce)
	return out
}

func parseAnnounceList(root map[string]any) [][]string {
	tiers, _ := bencode.GetList(root, "announce-list")
	var out [][]string
	for _, t := range tiers {
		list, ok := t.([]any)
		if !ok {
			continue // tolerate junk: trackers are optional
		}
		var tier []string
		for _, u := range list {
			if s, ok := u.(string); ok && s != "" {
				tier = append(tier, s)
			}
		}
		if len(tier) > 0 {
			out = append(out, tier)
		}
	}
	return out
}

// url-list may be a single string or a list of strings.
func parseURLList(root map[string]any) []string {
	switch v := root["url-list"].(type) {
	case string:
		if v != "" {
			return []string{v}
		}
	case []any:
		var out []string
		for _, u := range v {
			if s, ok := u.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
