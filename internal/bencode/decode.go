// Package bencode implements the bencoding format used by BitTorrent
// (.torrent files, tracker responses, extension messages and DHT packets).
//
// Decoded values use plain Go types:
//
//	integer     -> int64
//	byte string -> string   (may contain arbitrary binary data)
//	list        -> []any
//	dictionary  -> map[string]any
//
// See BEP 3: https://www.bittorrent.org/beps/bep_0003.html
package bencode

import (
	"errors"
	"fmt"
	"strconv"
)

// maxDepth bounds list/dict nesting so hostile input can't exhaust the stack.
const maxDepth = 256

// ErrSyntax is wrapped by every decoding error, so callers can test with errors.Is.
var ErrSyntax = errors.New("bencode: syntax error")

// Decode parses exactly one bencoded value. Trailing bytes are an error.
func Decode(data []byte) (any, error) {
	v, n, err := DecodePrefix(data)
	if err != nil {
		return nil, err
	}
	if n != len(data) {
		return nil, syntaxErr(n, "trailing data after value")
	}
	return v, nil
}

// DecodePrefix parses one bencoded value from the start of data and returns
// it with the number of bytes consumed. Bytes after the value are ignored;
// this is needed for messages that append raw data after a dictionary.
func DecodePrefix(data []byte) (any, int, error) {
	d := decoder{data: data}
	v, err := d.value(0)
	if err != nil {
		return nil, 0, err
	}
	return v, d.pos, nil
}

// DictSpans parses a top-level dictionary and returns the raw encoded bytes of
// each value, keyed by dictionary key. The returned slices alias data.
//
// This is how the info hash is computed: sha1(DictSpans(torrent)["info"]).
// Hashing the original bytes (rather than re-encoding) keeps the hash correct
// even for torrents that are not canonically encoded.
func DictSpans(data []byte) (map[string][]byte, error) {
	d := decoder{data: data}
	if d.peek() != 'd' {
		return nil, syntaxErr(0, "expected dictionary")
	}
	d.pos++
	spans := make(map[string][]byte)
	for d.peek() != 'e' {
		if d.eof() {
			return nil, syntaxErr(d.pos, "unterminated dictionary")
		}
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		start := d.pos
		if _, err := d.value(1); err != nil {
			return nil, err
		}
		spans[key] = data[start:d.pos]
	}
	d.pos++ // 'e'
	if d.pos != len(data) {
		return nil, syntaxErr(d.pos, "trailing data after value")
	}
	return spans, nil
}

type decoder struct {
	data []byte
	pos  int
}

func (d *decoder) eof() bool { return d.pos >= len(d.data) }

// peek returns the current byte, or 0 at end of input.
func (d *decoder) peek() byte {
	if d.eof() {
		return 0
	}
	return d.data[d.pos]
}

func (d *decoder) value(depth int) (any, error) {
	if depth > maxDepth {
		return nil, syntaxErr(d.pos, "nesting too deep")
	}
	switch c := d.peek(); {
	case c == 'i':
		return d.integer()
	case c >= '0' && c <= '9':
		return d.str()
	case c == 'l':
		return d.list(depth)
	case c == 'd':
		return d.dict(depth)
	case d.eof():
		return nil, syntaxErr(d.pos, "unexpected end of input")
	default:
		return nil, syntaxErr(d.pos, fmt.Sprintf("unexpected byte %q", c))
	}
}

// integer parses i<digits>e. Leading zeros and "-0" are invalid per BEP 3.
func (d *decoder) integer() (int64, error) {
	start := d.pos
	d.pos++ // 'i'
	end := d.indexFrom('e')
	if end < 0 {
		return 0, syntaxErr(start, "unterminated integer")
	}
	digits := string(d.data[d.pos:end])
	if !validIntText(digits) {
		return 0, syntaxErr(start, fmt.Sprintf("invalid integer %q", digits))
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, syntaxErr(start, "integer out of range")
	}
	d.pos = end + 1
	return n, nil
}

func validIntText(s string) bool {
	if s == "" || s == "-" {
		return false
	}
	body := s
	if s[0] == '-' {
		body = s[1:]
		if body == "0" {
			return false // "-0"
		}
	}
	if len(body) > 1 && body[0] == '0' {
		return false // leading zero
	}
	for i := 0; i < len(body); i++ {
		if body[i] < '0' || body[i] > '9' {
			return false
		}
	}
	return true
}

// str parses <length>:<bytes>.
func (d *decoder) str() (string, error) {
	start := d.pos
	colon := d.indexFrom(':')
	if colon < 0 {
		return "", syntaxErr(start, "string length without ':'")
	}
	lenText := string(d.data[d.pos:colon])
	if lenText == "" || (len(lenText) > 1 && lenText[0] == '0') {
		return "", syntaxErr(start, fmt.Sprintf("invalid string length %q", lenText))
	}
	n, err := strconv.ParseUint(lenText, 10, 63)
	if err != nil {
		return "", syntaxErr(start, fmt.Sprintf("invalid string length %q", lenText))
	}
	begin := colon + 1
	if n > uint64(len(d.data)-begin) {
		return "", syntaxErr(start, "string length exceeds input")
	}
	end := begin + int(n)
	d.pos = end
	return string(d.data[begin:end]), nil
}

func (d *decoder) list(depth int) ([]any, error) {
	start := d.pos
	d.pos++ // 'l'
	list := []any{}
	for d.peek() != 'e' {
		if d.eof() {
			return nil, syntaxErr(start, "unterminated list")
		}
		v, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		list = append(list, v)
	}
	d.pos++ // 'e'
	return list, nil
}

func (d *decoder) dict(depth int) (map[string]any, error) {
	start := d.pos
	d.pos++ // 'd'
	m := make(map[string]any)
	for d.peek() != 'e' {
		if d.eof() {
			return nil, syntaxErr(start, "unterminated dictionary")
		}
		if c := d.peek(); c < '0' || c > '9' {
			return nil, syntaxErr(d.pos, "dictionary key must be a string")
		}
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		v, err := d.value(depth + 1)
		if err != nil {
			return nil, err
		}
		m[key] = v // duplicate keys: last one wins (lenient, see DECISIONS D7)
	}
	d.pos++ // 'e'
	return m, nil
}

// indexFrom returns the absolute index of b at or after d.pos, or -1.
func (d *decoder) indexFrom(b byte) int {
	for i := d.pos; i < len(d.data); i++ {
		if d.data[i] == b {
			return i
		}
	}
	return -1
}

func syntaxErr(pos int, msg string) error {
	return fmt.Errorf("%w at offset %d: %s", ErrSyntax, pos, msg)
}
