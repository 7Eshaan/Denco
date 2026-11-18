package main

import (
	"strconv"
	"strings"
	"unicode/utf8"
)

// vt is a tiny terminal emulator: just enough of what the TUI writes
// (cursor positioning, clear screen, clear to end of line, colors) to
// rebuild the screen as a grid of styled cells.

type style struct {
	fg, bg         int // ANSI color numbers (31…36, 90; 40…47); 0 = default
	bold, dim, rev bool
}

type cell struct {
	r  rune
	st style
}

type vt struct {
	w, h     int
	grid     [][]cell
	row, col int
	cur      style
	pending  []byte // an escape sequence cut off at the end of a write
}

func newVT(w, h int) *vt {
	v := &vt{w: w, h: h}
	v.clear()
	return v
}

func (v *vt) clear() {
	v.grid = make([][]cell, v.h)
	for i := range v.grid {
		v.grid[i] = make([]cell, v.w)
		for j := range v.grid[i] {
			v.grid[i][j] = cell{r: ' '}
		}
	}
}

func (v *vt) Write(p []byte) (int, error) {
	b := append(v.pending, p...)
	v.pending = nil
	for len(b) > 0 {
		if b[0] == 0x1b {
			n, ok := v.escape(b)
			if !ok {
				v.pending = append([]byte(nil), b...)
				break
			}
			b = b[n:]
			continue
		}
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n < 4 && !utf8.FullRune(b) {
			v.pending = append([]byte(nil), b...)
			break
		}
		b = b[n:]
		v.put(r)
	}
	return len(p), nil
}

func (v *vt) put(r rune) {
	switch r {
	case '\r':
		v.col = 0
		return
	case '\n':
		v.row++
		return
	}
	if v.row >= 0 && v.row < v.h && v.col >= 0 && v.col < v.w {
		v.grid[v.row][v.col] = cell{r: r, st: v.cur}
	}
	v.col++
}

// escape handles one CSI sequence; ok is false if it isn't complete yet.
func (v *vt) escape(b []byte) (int, bool) {
	if len(b) < 2 {
		return 0, false
	}
	if b[1] != '[' {
		return 2, true
	}
	i := 2
	for i < len(b) && (b[i] < 0x40 || b[i] > 0x7e) {
		i++
	}
	if i == len(b) {
		return 0, false
	}
	params, final := string(b[2:i]), b[i]
	if strings.HasPrefix(params, "?") { // modes: alternate screen, cursor, sync
		return i + 1, true
	}
	nums := func() []int {
		var out []int
		for _, f := range strings.Split(params, ";") {
			n, _ := strconv.Atoi(f)
			out = append(out, n)
		}
		return out
	}
	switch final {
	case 'H':
		n := nums()
		v.row, v.col = 0, 0
		if len(n) >= 1 && n[0] > 0 {
			v.row = n[0] - 1
		}
		if len(n) >= 2 && n[1] > 0 {
			v.col = n[1] - 1
		}
	case 'J':
		v.clear()
	case 'K':
		if v.row >= 0 && v.row < v.h {
			for c := max(v.col, 0); c < v.w; c++ {
				v.grid[v.row][c] = cell{r: ' '}
			}
		}
	case 'm':
		v.sgr(nums())
	}
	return i + 1, true
}

func (v *vt) sgr(codes []int) {
	for _, c := range codes {
		switch {
		case c == 0:
			v.cur = style{}
		case c == 1:
			v.cur.bold = true
		case c == 2:
			v.cur.dim = true
		case c == 7:
			v.cur.rev = true
		case c >= 30 && c <= 37, c >= 90 && c <= 97:
			v.cur.fg = c
		case c >= 40 && c <= 47:
			v.cur.bg = c
		}
	}
}

// text is the screen as plain text (to wait for content to appear).
func (v *vt) text() string {
	var b strings.Builder
	for _, row := range v.grid {
		for _, c := range row {
			b.WriteRune(c.r)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
