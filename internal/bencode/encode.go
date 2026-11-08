package bencode

import (
	"bytes"
	"fmt"
	"sort"
	"strconv"
)

// Encode serialises v into canonical bencode (dictionary keys sorted).
//
// Supported types: int, int64, uint32, string, []byte, []any, []string,
// map[string]any, map[string]string and Raw.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	if err := encodeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// Raw is already-encoded bencode that Encode copies through verbatim.
// Useful for embedding an original info dictionary without re-encoding it.
type Raw []byte

func encodeValue(buf *bytes.Buffer, v any) error {
	switch x := v.(type) {
	case int:
		writeInt(buf, int64(x))
	case int64:
		writeInt(buf, x)
	case uint32:
		writeInt(buf, int64(x))
	case string:
		writeString(buf, x)
	case []byte:
		writeString(buf, string(x))
	case Raw:
		buf.Write(x)
	case []string:
		buf.WriteByte('l')
		for _, s := range x {
			writeString(buf, s)
		}
		buf.WriteByte('e')
	case []any:
		buf.WriteByte('l')
		for _, item := range x {
			if err := encodeValue(buf, item); err != nil {
				return err
			}
		}
		buf.WriteByte('e')
	case map[string]string:
		buf.WriteByte('d')
		for _, k := range sortedKeys(x) {
			writeString(buf, k)
			writeString(buf, x[k])
		}
		buf.WriteByte('e')
	case map[string]any:
		buf.WriteByte('d')
		for _, k := range sortedKeys(x) {
			writeString(buf, k)
			if err := encodeValue(buf, x[k]); err != nil {
				return fmt.Errorf("key %q: %w", k, err)
			}
		}
		buf.WriteByte('e')
	default:
		return fmt.Errorf("bencode: cannot encode type %T", v)
	}
	return nil
}

func writeInt(buf *bytes.Buffer, n int64) {
	buf.WriteByte('i')
	buf.WriteString(strconv.FormatInt(n, 10))
	buf.WriteByte('e')
}

func writeString(buf *bytes.Buffer, s string) {
	buf.WriteString(strconv.Itoa(len(s)))
	buf.WriteByte(':')
	buf.WriteString(s)
}

// sortedKeys returns map keys in raw byte order, as BEP 3 requires.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
