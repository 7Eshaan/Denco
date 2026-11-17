package tui

import (
	"strings"
	"unicode"
)

// Text helpers. Everything the TUI draws goes through these, so every line
// fits the terminal exactly: widths are counted in terminal cells (wide
// CJK characters and emoji take two, combining marks none), and text from
// the daemon is cleaned of control characters first.

// clean replaces control characters with '?'. Torrent names, file paths
// and error messages come from torrents and peers; an escape sequence in a
// name must not reach the terminal as a command.
func clean(s string) string {
	// Invalid UTF-8 first: a raw 0x9b byte is a control introducer to
	// terminals that read 8-bit codes.
	s = strings.ToValidUTF8(s, "?")
	if strings.IndexFunc(s, isControl) < 0 {
		return s
	}
	return strings.Map(func(r rune) rune {
		if isControl(r) {
			return '?'
		}
		return r
	}, s)
}

func isControl(r rune) bool { return r < 0x20 || (r >= 0x7f && r < 0xa0) }

// runeWidth is how many terminal cells r takes.
func runeWidth(r rune) int {
	switch {
	case isControl(r):
		return 0
	case unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf):
		return 0
	case isWide(r):
		return 2
	}
	return 1
}

// isWide covers the East Asian wide/fullwidth ranges and the common emoji
// blocks, which terminals draw two cells wide.
func isWide(r rune) bool {
	return r >= 0x1100 && (r <= 0x115f ||
		(r >= 0x2e80 && r <= 0xa4cf && r != 0x303f) ||
		(r >= 0xac00 && r <= 0xd7a3) ||
		(r >= 0xf900 && r <= 0xfaff) ||
		(r >= 0xfe30 && r <= 0xfe4f) ||
		(r >= 0xff00 && r <= 0xff60) ||
		(r >= 0xffe0 && r <= 0xffe6) ||
		(r >= 0x1f300 && r <= 0x1f64f) ||
		(r >= 0x1f900 && r <= 0x1f9ff) ||
		(r >= 0x20000 && r <= 0x3fffd))
}

// width is the number of cells s takes (s must not contain escape codes).
func width(s string) int {
	n := 0
	for _, r := range s {
		n += runeWidth(r)
	}
	return n
}

// truncate shortens s to at most w cells, marking a cut with "…".
func truncate(s string, w int) string {
	if w <= 0 {
		return ""
	}
	if width(s) <= w {
		return s
	}
	var b strings.Builder
	n := 0
	for _, r := range s {
		rw := runeWidth(r)
		if n+rw > w-1 {
			break
		}
		b.WriteRune(r)
		n += rw
	}
	b.WriteString("…")
	return b.String()
}

// pad truncates or space-fills s to exactly w cells, left-aligned.
func pad(s string, w int) string {
	s = truncate(s, w)
	return s + strings.Repeat(" ", max(0, w-width(s)))
}

// padLeft is pad, right-aligned (numbers).
func padLeft(s string, w int) string {
	s = truncate(s, w)
	return strings.Repeat(" ", max(0, w-width(s))) + s
}

// tail keeps the last w cells of s, marking a cut with "…" at the start
// (the add prompt shows the end of what is being typed).
func tail(s string, w int) string {
	if width(s) <= w {
		return s
	}
	rs := []rune(s)
	n := 1 // the "…"
	i := len(rs)
	for i > 0 && n+runeWidth(rs[i-1]) <= w {
		i--
		n += runeWidth(rs[i])
	}
	return "…" + string(rs[i:])
}

// --- styles -------------------------------------------------------------------------

// SGR codes. Colors are dropped when the terminal shouldn't get them
// (NO_COLOR); bold, dim and reverse are kept, since they carry meaning
// (the selected row) and aren't colors.
const (
	sBold    = "1"
	sDim     = "2"
	sReverse = "7"
	cRed     = "31"
	cGreen   = "32"
	cYellow  = "33"
	cBlue    = "34"
	cMagenta = "35"
	cCyan    = "36"
	cGray    = "90"
	bgBlue   = "44"
)

// styler applies SGR codes, or only the non-color ones when color is off.
type styler struct{ color bool }

func (st styler) paint(s string, codes ...string) string {
	if s == "" || len(codes) == 0 {
		return s
	}
	var keep []string
	for _, c := range codes {
		if st.color || c == sBold || c == sDim || c == sReverse {
			keep = append(keep, c)
		}
	}
	if len(keep) == 0 {
		return s
	}
	return "\x1b[" + strings.Join(keep, ";") + "m" + s + "\x1b[0m"
}

// stripANSI removes SGR sequences (tests and width checks of painted lines).
func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
