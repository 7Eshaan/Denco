package tui

import (
	"strings"
	"testing"
)

func TestWidthTruncatePad(t *testing.T) {
	tests := []struct {
		in        string
		w         int
		trunc     string
		widthOfIn int
	}{
		{"ubuntu.iso", 20, "ubuntu.iso", 10},
		{"ubuntu-24.04-desktop-amd64.iso", 10, "ubuntu-24…", 30},
		{"日本語のファイル", 7, "日本語…", 16}, // wide: 2 cells each, never split
		{"café", 4, "café", 4},
		{"étude", 5, "étude", 5}, // combining accent takes no cell
		{"🎬 movie", 4, "🎬 …", 8},
		{"x", 0, "", 1},
	}
	for _, tt := range tests {
		if got := width(tt.in); got != tt.widthOfIn {
			t.Errorf("width(%q) = %d, want %d", tt.in, got, tt.widthOfIn)
		}
		got := truncate(tt.in, tt.w)
		if got != tt.trunc {
			t.Errorf("truncate(%q, %d) = %q, want %q", tt.in, tt.w, got, tt.trunc)
		}
		if width(got) > tt.w {
			t.Errorf("truncate(%q, %d) is %d cells wide", tt.in, tt.w, width(got))
		}
		if p := pad(tt.in, tt.w); width(p) != tt.w {
			t.Errorf("pad(%q, %d) = %q (%d cells)", tt.in, tt.w, p, width(p))
		}
		if p := padLeft(tt.in, tt.w); width(p) != tt.w {
			t.Errorf("padLeft(%q, %d) = %q (%d cells)", tt.in, tt.w, p, width(p))
		}
	}
	if got := padLeft("7", 3); got != "  7" {
		t.Errorf("padLeft = %q", got)
	}
}

func TestTail(t *testing.T) {
	if got := tail("magnet:?xt=urn:btih:abcdef", 10); got != "…:abcdef" && width(got) != 10 {
		t.Errorf("tail = %q", got)
	}
	if got := tail("magnet:?xt=urn:btih:abcdef", 10); !strings.HasSuffix(got, "abcdef") || width(got) > 10 || !strings.HasPrefix(got, "…") {
		t.Errorf("tail = %q", got)
	}
	if got := tail("short", 10); got != "short" {
		t.Errorf("tail = %q", got)
	}
}

func TestCleanStopsEscapeSequences(t *testing.T) {
	// A torrent named like this must not be able to drive the terminal.
	name := "evil\x1b]0;pwned\x07\x1b[2Jname\r\n\u009b\x9b"
	got := clean(name)
	if strings.ContainsAny(got, "\x1b\x07\r\n\u009b") {
		t.Fatalf("clean left control characters: %q", got)
	}
	if got != "evil?]0;pwned??[2Jname????" {
		t.Errorf("clean = %q", got)
	}
	if got := clean("only a raw \x9b byte"); got != "only a raw ? byte" {
		t.Errorf("clean(raw 0x9b) = %q", got)
	}
	if clean("plain") != "plain" {
		t.Error("clean changed plain text")
	}
}

func TestPaint(t *testing.T) {
	color, plain := styler{color: true}, styler{color: false}
	if got := color.paint("x", sBold, cRed); got != "\x1b[1;31mx\x1b[0m" {
		t.Errorf("color paint = %q", got)
	}
	// NO_COLOR keeps bold/dim/reverse (they aren't colors) but drops colors.
	if got := plain.paint("x", sBold, cRed); got != "\x1b[1mx\x1b[0m" {
		t.Errorf("no-color paint = %q", got)
	}
	if got := plain.paint("x", cRed); got != "x" {
		t.Errorf("no-color, color only = %q", got)
	}
	if got := stripANSI(color.paint("ab", cRed) + "c"); got != "abc" {
		t.Errorf("stripANSI = %q", got)
	}
}
