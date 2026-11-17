package tui

import (
	"strings"
	"unicode/utf8"
)

// Keyboard input. In raw mode the terminal sends bytes: printable
// characters as UTF-8, control keys as single bytes (Enter = CR, Ctrl-C =
// 0x03, Backspace = DEL), and arrows and friends as escape sequences
// (ESC [ A, or ESC O A in "application" mode). Windows consoles in
// virtual-terminal input mode send the same.
//
// One read normally holds whole keys: a terminal writes each sequence at
// once. A lone ESC at the end of a read is therefore the Esc key itself.

// KeyCode identifies a key; KeyRune carries a character in Key.Rune.
type KeyCode int

const (
	KeyRune KeyCode = iota
	KeyUp
	KeyDown
	KeyLeft
	KeyRight
	KeyHome
	KeyEnd
	KeyPgUp
	KeyPgDn
	KeyEnter
	KeyEsc
	KeyTab
	KeyBackTab
	KeyBackspace
	KeyDelete
	KeyCtrlC
	KeyCtrlU
	KeyCtrlW
)

// Key is one key press.
type Key struct {
	Code KeyCode
	Rune rune // for KeyRune
}

func runeKey(r rune) Key { return Key{Code: KeyRune, Rune: r} }

// parseKeys decodes one read's worth of input. Unknown sequences and
// stray control bytes are dropped.
func parseKeys(b []byte) []Key {
	var out []Key
	for len(b) > 0 {
		k, n, ok := nextKey(b)
		if ok {
			out = append(out, k)
		}
		b = b[n:]
	}
	return out
}

func nextKey(b []byte) (Key, int, bool) {
	switch c := b[0]; {
	case c == 0x1b:
		return escape(b)
	case c == '\r' || c == '\n':
		if c == '\r' && len(b) > 1 && b[1] == '\n' {
			return Key{Code: KeyEnter}, 2, true // CRLF: one Enter
		}
		return Key{Code: KeyEnter}, 1, true
	case c == '\t':
		return Key{Code: KeyTab}, 1, true
	case c == 0x7f || c == 0x08:
		return Key{Code: KeyBackspace}, 1, true
	case c == 0x03:
		return Key{Code: KeyCtrlC}, 1, true
	case c == 0x15:
		return Key{Code: KeyCtrlU}, 1, true
	case c == 0x17:
		return Key{Code: KeyCtrlW}, 1, true
	case c < 0x20:
		return Key{}, 1, false
	}
	r, n := utf8.DecodeRune(b)
	if r == utf8.RuneError {
		return Key{}, n, false
	}
	return runeKey(r), n, true
}

// escape decodes a sequence starting with ESC.
func escape(b []byte) (Key, int, bool) {
	if len(b) == 1 {
		return Key{Code: KeyEsc}, 1, true
	}
	switch b[1] {
	case '[': // CSI: parameters (digits, ';'), then a final byte
		i := 2
		for i < len(b) && (b[i] >= '0' && b[i] <= '9' || b[i] == ';') {
			i++
		}
		if i == len(b) {
			return Key{}, len(b), false // cut off; drop it
		}
		k := csiKey(string(b[2:i]), b[i])
		return k, i + 1, k.Code >= 0 // unknown sequences (Code -1) are dropped
	case 'O': // SS3: ESC O A (arrows in application cursor mode)
		if len(b) < 3 {
			return Key{Code: KeyEsc}, 1, true
		}
		if k, ok := letterKey(b[2]); ok {
			return k, 3, true
		}
		return Key{}, 3, false
	}
	// ESC then something else: Alt+key, or Esc typed before another key.
	return Key{Code: KeyEsc}, 1, true
}

func csiKey(params string, final byte) Key {
	if final == '~' { // ESC [ n ~  (modifiers after ';' are ignored)
		n, _, _ := strings.Cut(params, ";")
		switch n {
		case "1", "7":
			return Key{Code: KeyHome}
		case "4", "8":
			return Key{Code: KeyEnd}
		case "3":
			return Key{Code: KeyDelete}
		case "5":
			return Key{Code: KeyPgUp}
		case "6":
			return Key{Code: KeyPgDn}
		}
		return Key{Code: -1}
	}
	if final == 'Z' {
		return Key{Code: KeyBackTab}
	}
	if k, ok := letterKey(final); ok {
		return k // ESC [ A, and ESC [ 1 ; 5 A (Ctrl-Up) alike
	}
	return Key{Code: -1}
}

func letterKey(c byte) (Key, bool) {
	switch c {
	case 'A':
		return Key{Code: KeyUp}, true
	case 'B':
		return Key{Code: KeyDown}, true
	case 'C':
		return Key{Code: KeyRight}, true
	case 'D':
		return Key{Code: KeyLeft}, true
	case 'H':
		return Key{Code: KeyHome}, true
	case 'F':
		return Key{Code: KeyEnd}, true
	}
	return Key{}, false
}
