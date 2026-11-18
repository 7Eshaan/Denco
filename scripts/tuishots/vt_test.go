package main

import (
	"strings"
	"testing"
)

func TestVT(t *testing.T) {
	v := newVT(10, 3)
	// What the TUI writes: clear, positioned lines, colors, clear-to-end,
	// and a sequence split across two writes.
	v.Write([]byte("\x1b[?1049h\x1b[2J\x1b[1;1Hab\x1b[1;31mcd\x1b[0m\x1b[K"))
	v.Write([]byte("\x1b[2;3Hx\x1b["))
	v.Write([]byte("7my\x1b[0m"))
	if got := v.text(); got != "abcd      \n  xy      \n          \n" {
		t.Fatalf("text:\n%q", got)
	}
	if st := v.grid[0][2].st; st.fg != 31 || !st.bold {
		t.Errorf("c should be bold red: %+v", st)
	}
	if st := v.grid[0][1].st; st != (style{}) {
		t.Errorf("b should be plain: %+v", st)
	}
	if !v.grid[1][3].st.rev {
		t.Error("y should be reversed")
	}

	img := svg(v, "a <title> & more")
	for _, want := range []string{`<svg `, `a &lt;title&gt; &amp; more`, `fill="#ff7b72"`, `>cd</text>`} {
		if !strings.Contains(img, want) {
			t.Errorf("svg missing %q", want)
		}
	}
	// Words are placed by column, never by spaces (viewers collapse them).
	if strings.Contains(img, "> ") || strings.Contains(img, " </text>") {
		t.Error("a text element starts or ends with a space")
	}
}
