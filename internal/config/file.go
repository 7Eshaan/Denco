package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads the config file at path over base (normally Defaults). A
// missing file is not an error: base is returned unchanged.
func Load(path string, base Config) (Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return base, nil
	}
	if err != nil {
		return base, fmt.Errorf("config: %w", err)
	}
	c, err := Parse(string(data), base)
	if err != nil {
		return base, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Parse applies the settings in a config file's text to base.
func Parse(text string, base Config) (Config, error) {
	c := base
	seen := map[string]int{}
	for i, line := range strings.Split(text, "\n") {
		n := i + 1
		key, value, ok, err := splitLine(line)
		if err != nil {
			return base, fmt.Errorf("line %d: %w", n, err)
		}
		if !ok {
			continue // blank or comment
		}
		if prev, dup := seen[key]; dup {
			return base, fmt.Errorf("line %d: %s is already set on line %d", n, key, prev)
		}
		seen[key] = n
		if err := c.Set(key, value); err != nil {
			return base, fmt.Errorf("line %d: %w", n, err)
		}
	}
	return c, nil
}

// splitLine parses one line into key and value. ok is false for blank and
// comment lines. Values may be quoted ("…" or '…') to contain '#' or
// surrounding spaces; otherwise " #" starts a trailing comment.
func splitLine(line string) (key, value string, ok bool, err error) {
	t := strings.TrimSpace(strings.TrimSuffix(line, "\r"))
	if t == "" || strings.HasPrefix(t, "#") {
		return "", "", false, nil
	}
	k, v, found := strings.Cut(t, ":")
	if !found {
		return "", "", false, fmt.Errorf("expected 'key: value', got %q", t)
	}
	key = strings.TrimSpace(k)
	v = strings.TrimSpace(v)
	if v != "" && (v[0] == '"' || v[0] == '\'') {
		end := strings.IndexByte(v[1:], v[0])
		if end < 0 {
			return "", "", false, fmt.Errorf("%s: unterminated quote", key)
		}
		rest := strings.TrimSpace(v[end+2:])
		if rest != "" && !strings.HasPrefix(rest, "#") {
			return "", "", false, fmt.Errorf("%s: unexpected text after the quoted value", key)
		}
		return key, v[1 : end+1], true, nil
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = v[:i]
	} else if i := strings.Index(v, "\t#"); i >= 0 {
		v = v[:i]
	}
	return key, strings.TrimSpace(v), true, nil
}

// KeysInFile reports which settings the file sets explicitly (the rest
// are defaults). A missing or unreadable file sets none.
func KeysInFile(path string) map[string]bool {
	out := map[string]bool{}
	lines, _ := readLines(path)
	for _, line := range lines {
		if k, _, ok, err := splitLine(line); ok && err == nil {
			out[k] = true
		}
	}
	return out
}

// SetInFile validates value for key and writes it to the file at path,
// replacing that key's line (keeping its trailing comment) or appending
// one. Other lines and comments are kept as they are. The file is created
// if missing. Returns the resulting configuration.
func SetInFile(path, key, value string, base Config) (Config, error) {
	c, err := Load(path, base) // refuse to edit a file that's already broken
	if err != nil {
		return base, err
	}
	if err := c.Set(key, value); err != nil {
		return base, err
	}
	lines, err := readLines(path)
	if err != nil {
		return base, err
	}
	newLine := key + ": " + quoteIfNeeded(strings.TrimSpace(value))
	replaced := false
	for i, line := range lines {
		k, _, ok, _ := splitLine(line)
		if ok && k == key {
			lines[i] = newLine + trailingComment(line)
			replaced = true
			break
		}
	}
	if !replaced {
		if len(lines) == 0 {
			lines = append(lines, header...)
		}
		lines = append(lines, newLine)
	}
	return c, writeFile(path, lines)
}

// UnsetInFile removes key from the file, so its default applies again.
func UnsetInFile(path, key string, base Config) (Config, error) {
	if _, err := Lookup(key); err != nil {
		return base, err
	}
	lines, err := readLines(path)
	if err != nil {
		return base, err
	}
	out := lines[:0]
	for _, line := range lines {
		if k, _, ok, _ := splitLine(line); ok && k == key {
			continue
		}
		out = append(out, line)
	}
	if err := writeFile(path, out); err != nil {
		return base, err
	}
	return Load(path, base)
}

// Init writes a file listing every setting with its help text and
// current value. It refuses to overwrite an existing file.
func Init(path string, c Config) error {
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("config: %s already exists", path)
	}
	lines := append([]string(nil), header...)
	for _, s := range settings {
		lines = append(lines, "", "# "+s.Help, s.Name+": "+quoteIfNeeded(s.get(&c)))
	}
	return writeFile(path, lines)
}

var header = []string{
	"# Denco configuration. Format: key: value   (# starts a comment)",
	"# Edit here or use 'denco config set <key> <value>'. Restart the daemon",
	"# to apply; speed limits set with 'denco config set' apply at once.",
}

// trailingComment returns a setting line's trailing comment (" # …"), or "".
func trailingComment(line string) string {
	_, v, _ := strings.Cut(line, ":")
	v = strings.TrimSpace(v)
	if v != "" && (v[0] == '"' || v[0] == '\'') { // skip the quoted value
		if end := strings.IndexByte(v[1:], v[0]); end >= 0 {
			v = v[end+2:]
		}
		if rest := strings.TrimSpace(v); strings.HasPrefix(rest, "#") {
			return "  " + rest
		}
		return ""
	}
	for _, sep := range []string{" #", "\t#"} {
		if i := strings.Index(v, sep); i >= 0 {
			return "  " + strings.TrimSpace(v[i:])
		}
	}
	return ""
}

func quoteIfNeeded(v string) string {
	if v == "" || strings.Contains(v, "#") || v[0] == '"' || v[0] == '\'' {
		if strings.Contains(v, `"`) {
			return "'" + v + "'"
		}
		return `"` + v + `"`
	}
	return v
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	text := strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// writeFile replaces the file atomically (temp file + rename).
func writeFile(path string, lines []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-config-*")
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	_, err = tmp.WriteString(strings.Join(lines, "\n") + "\n")
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	return nil
}
