package tui

import (
	"bytes"
	"io"
	"strconv"
)

// Escape sequences for the whole screen.
const (
	enterScreen = "\x1b[?1049h\x1b[?25l" // alternate screen (the shell's text is kept), hide cursor
	leaveScreen = "\x1b[?25h\x1b[?1049l" // show cursor, back to the normal screen
	clearScreen = "\x1b[2J"
	syncStart   = "\x1b[?2026h" // synchronized update: terminals that know it
	syncEnd     = "\x1b[?2026l" // draw the frame at once; others ignore it
)

// screen draws frames, rewriting only the lines that changed since the
// last frame, all in one write. That, and no full clears except after a
// resize, is what keeps the display from flickering.
type screen struct {
	w    io.Writer
	prev []string // last frame drawn; nil = draw everything
	buf  bytes.Buffer
}

func newScreen(w io.Writer) *screen { return &screen{w: w} }

// invalidate makes the next draw clear the screen and redraw every line
// (after a resize the terminal may have moved or wrapped what was there).
func (s *screen) invalidate() { s.prev = nil }

// draw shows lines (one per row, each already fitted to the width).
func (s *screen) draw(lines []string) error {
	s.buf.Reset()
	full := s.prev == nil
	if full {
		s.buf.WriteString(clearScreen)
	}
	for i, l := range lines {
		if !full && i < len(s.prev) && s.prev[i] == l {
			continue
		}
		s.buf.WriteString("\x1b[")
		s.buf.WriteString(strconv.Itoa(i + 1))
		s.buf.WriteString(";1H")
		s.buf.WriteString(l)
		s.buf.WriteString("\x1b[0m\x1b[K") // reset style, clear what's left of the row
	}
	s.prev = append(s.prev[:0], lines...)
	if s.buf.Len() == 0 {
		return nil // nothing changed: no write at all
	}
	out := append([]byte(syncStart), s.buf.Bytes()...)
	out = append(out, syncEnd...)
	_, err := s.w.Write(out)
	return err
}
