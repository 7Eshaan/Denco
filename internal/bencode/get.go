package bencode

// Typed accessors for decoded dictionaries. Each returns ok=false when the
// key is missing or holds a different type, so callers can decide whether a
// field is required or optional.

// GetString returns d[key] as a byte string.
func GetString(d map[string]any, key string) (string, bool) {
	s, ok := d[key].(string)
	return s, ok
}

// GetInt returns d[key] as an integer.
func GetInt(d map[string]any, key string) (int64, bool) {
	n, ok := d[key].(int64)
	return n, ok
}

// GetList returns d[key] as a list.
func GetList(d map[string]any, key string) ([]any, bool) {
	l, ok := d[key].([]any)
	return l, ok
}

// GetDict returns d[key] as a dictionary.
func GetDict(d map[string]any, key string) (map[string]any, bool) {
	m, ok := d[key].(map[string]any)
	return m, ok
}
