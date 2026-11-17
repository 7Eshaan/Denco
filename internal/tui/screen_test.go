package tui

import (
	"bytes"
	"strings"
	"testing"
)

func TestScreenRedrawsOnlyChangedLines(t *testing.T) {
	var out bytes.Buffer
	s := newScreen(&out)

	if err := s.draw([]string{"one", "two", "three"}); err != nil {
		t.Fatal(err)
	}
	first := out.String()
	if !strings.Contains(first, clearScreen) || !strings.Contains(first, "\x1b[3;1Hthree") {
		t.Fatalf("first frame should clear and draw every line: %q", first)
	}

	out.Reset()
	s.draw([]string{"one", "TWO", "three"})
	got := out.String()
	if !strings.Contains(got, "\x1b[2;1HTWO") || strings.Contains(got, "one") || strings.Contains(got, "three") || strings.Contains(got, clearScreen) {
		t.Fatalf("second frame should only rewrite line 2: %q", got)
	}
	if !strings.HasPrefix(got, syncStart) || !strings.HasSuffix(got, syncEnd) {
		t.Errorf("frame not wrapped in a synchronized update: %q", got)
	}

	out.Reset()
	s.draw([]string{"one", "TWO", "three"})
	if out.Len() != 0 {
		t.Errorf("an unchanged frame wrote %q", out.String())
	}

	out.Reset()
	s.invalidate() // after a resize
	s.draw([]string{"one", "TWO", "three"})
	if got := out.String(); !strings.Contains(got, clearScreen) || !strings.Contains(got, "one") {
		t.Errorf("after invalidate everything should be redrawn: %q", got)
	}
}
