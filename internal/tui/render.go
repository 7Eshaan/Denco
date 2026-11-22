package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/format"
)

// Rendering: the model becomes exactly h lines, each exactly w cells wide
// (pure functions of the model; the screen writes them).
//
// Dashboard layout:
//
//	row 0      title bar: Denco, connection, total speeds
//	row 1      summary: torrents by state, peers, DHT, limits
//	row 2      rule
//	row 3      column headings
//	rows 4…    torrents (scrolling)
//	row h-2    message: prompt, confirmation, result or error
//	row h-1    key hints

const (
	minWidth   = 30
	minHeight  = 7
	listChrome = 6 // rows that aren't torrents
)

// listRows is how many torrents fit.
func (m *model) listRows() int { return max(1, m.h-listChrome) }

func (m *model) render() []string {
	st := styler{m.color}
	if m.w < minWidth || m.h < minHeight {
		return m.tooSmall(st)
	}
	var lines []string
	if m.view == viewDetails {
		lines = m.renderDetails(st)
	} else {
		lines = m.renderList(st)
	}
	if m.mode == modeHelp {
		m.overlayHelp(st, lines)
	}
	return lines
}

func (m *model) tooSmall(st styler) []string {
	lines := make([]string, m.h)
	msg := fmt.Sprintf("Terminal too small (%d×%d); need %d×%d", m.w, m.h, minWidth, minHeight)
	lines[m.h/2] = center(st.paint(truncate(msg, m.w), cYellow), min(width(msg), m.w), m.w)
	if m.h/2+1 < m.h {
		lines[m.h/2+1] = center(truncate("q quits", m.w), min(7, m.w), m.w)
	}
	return lines
}

func (m *model) renderList(st styler) []string {
	lines := []string{
		m.titleBar(st, nil),
		m.summary(st),
		st.paint(strings.Repeat("─", m.w), cGray),
	}
	cols := listColumns(m.w)
	body := m.listBody(st, cols)
	lines = append(lines, body...)
	lines = append(lines, m.messageLine(st), m.hints(st, listHints, m.position()))
	return lines
}

// --- title bar and summary -------------------------------------------------------------------

// seg is a run of text in one style.
type seg struct {
	text  string
	codes []string
}

func s(text string, codes ...string) seg { return seg{text, codes} }

// segLine lays out left and right segments on one w-cell line; the right
// side is dropped first when they don't fit, then the left is cut.
func segLine(st styler, w int, left, right []seg) string {
	lw, rw := segsWidth(left), segsWidth(right)
	if lw+rw+1 > w {
		right, rw = nil, 0
	}
	var b strings.Builder
	used := 0
	for _, sg := range left {
		t := truncate(sg.text, w-rw-used)
		b.WriteString(st.paint(t, sg.codes...))
		used += width(t)
	}
	b.WriteString(strings.Repeat(" ", max(0, w-used-rw)))
	for _, sg := range right {
		b.WriteString(st.paint(sg.text, sg.codes...))
	}
	return b.String()
}

func segsWidth(segs []seg) int {
	n := 0
	for _, sg := range segs {
		n += width(sg.text)
	}
	return n
}

// titleBar: brand, an optional crumb (details view), connection and speeds.
func (m *model) titleBar(st styler, crumb []seg) string {
	left := []seg{s(" DENCO ", sBold, sReverse, cBlue), s(" ")}
	left = append(left, crumb...)
	var right []seg
	switch m.conn {
	case connUp:
		right = append(right, s("● ", cGreen), s("connected", cGreen))
		if m.status.PID > 0 {
			right = append(right, s(fmt.Sprintf(" · pid %d · up %s", m.status.PID,
				format.Duration(m.now.Sub(m.status.StartedAt))), cGray))
		}
	case connDown:
		right = append(right, s("● disconnected", cRed, sBold))
	default:
		right = append(right, s("● connecting…", cYellow))
	}
	if crumb == nil && m.haveData {
		right = append(right,
			s("   ↓ ", cCyan), s(padLeft(format.Speed(m.status.DownloadRate), 10), sBold),
			s("  ↑ ", cMagenta), s(padLeft(format.Speed(m.status.UploadRate), 10), sBold))
	}
	right = append(right, s(" "))
	return segLine(st, m.w, left, right)
}

// summary counts torrents by what they do.
func (m *model) summary(st styler) string {
	if !m.haveData {
		return ""
	}
	o := m.status.Overview
	active := o.ByState[engine.StateDownloading] + o.ByState[engine.StateStalled] +
		o.ByState[engine.StateMetadata] + o.ByState[engine.StateChecking]
	segs := []seg{s(" "), s(strconv.Itoa(o.Torrents), sBold), s(" torrent" + plural(o.Torrents))}
	add := func(n int, label, color string) {
		if n > 0 {
			segs = append(segs, s(" · ", cGray), s(strconv.Itoa(n)+" "+label, color))
		}
	}
	add(active, "downloading", cCyan)
	add(o.ByState[engine.StateSeeding], "seeding", cGreen)
	add(o.ByState[engine.StateQueued], "queued", cGray)
	add(o.ByState[engine.StatePaused], "paused", cYellow)
	add(o.ByState[engine.StateFinished], "finished", cGray)
	add(o.ByState[engine.StateError], "failed", cRed)
	segs = append(segs, s(" · ", cGray), s(fmt.Sprintf("%d peer%s", o.Peers, plural(o.Peers))))
	if o.DHT {
		segs = append(segs, s(" · ", cGray), s(fmt.Sprintf("DHT %d nodes", o.DHTNodes+o.DHT6Nodes)))
	}
	if o.DownLimit > 0 || o.UpLimit > 0 {
		segs = append(segs, s(" · ", cGray), s("limit ↓ "+limitText(o.DownLimit)+" ↑ "+limitText(o.UpLimit), cYellow))
	}
	return segLine(st, m.w, segs, nil)
}

// ownLimitText is a torrent's own limit in words (D59).
func ownLimitText(n int64) string {
	if n <= 0 {
		return "no limit"
	}
	return format.Speed(n)
}

func limitText(n int64) string {
	if n <= 0 {
		return "∞"
	}
	return format.Speed(n)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// --- torrent table ------------------------------------------------------------------------

// column is one column of the torrent table; cell returns plain text and
// its style for a torrent.
type column struct {
	title string
	w     int
	right bool
	cell  func(t engine.Torrent) (string, []string)
	bar   bool // the progress column: a bar plus a percentage when wide enough
}

const (
	barCellWidth = 17 // bar (10) + space + "100.0%"
	pctWidth     = 6
	nameMin      = 12
	nameMax      = 80
)

// listColumns picks the columns that fit in w, dropping the least
// important first, and gives the name column the rest.
func listColumns(w int) []column {
	all := []column{
		{title: "ID", w: 3, right: true, cell: func(t engine.Torrent) (string, []string) { return strconv.Itoa(t.ID), []string{cGray} }},
		{title: "Q", w: 3, right: true, cell: queueCell},
		{title: "NAME", cell: func(t engine.Torrent) (string, []string) { return clean(t.Name), nil }},
		{title: "SIZE", w: 9, right: true, cell: sizeCell},
		{title: "PROGRESS", w: barCellWidth, bar: true},
		{title: "STATUS", w: 11, cell: statusCell},
		{title: "DOWN", w: 10, right: true, cell: downCell},
		{title: "UP", w: 10, right: true, cell: upCell},
		{title: "ETA", w: 7, right: true, cell: etaCell},
		{title: "PEERS", w: 5, right: true, cell: peersCell},
		{title: "RATIO", w: 5, right: true, cell: func(t engine.Torrent) (string, []string) { return ratioText(t.Ratio), nil }},
	}
	need := func(cols []column) int {
		n := 1 + nameMin // left margin
		for _, c := range cols {
			if c.title != "NAME" {
				n += c.w + 2
			}
		}
		return n
	}
	drop := func(cols []column, title string) []column {
		out := cols[:0:0]
		for _, c := range cols {
			if c.title != title {
				out = append(out, c)
			}
		}
		return out
	}
	cols := all
	for _, step := range []string{"RATIO", "Q", "PEERS", "ETA", "UP", "bar", "SIZE", "DOWN", "STATUS"} {
		if need(cols) <= w {
			break
		}
		if step == "bar" {
			for i := range cols {
				if cols[i].bar {
					cols[i].w = pctWidth
				}
			}
			continue
		}
		cols = drop(cols, step)
	}
	for i := range cols {
		if cols[i].title == "NAME" {
			cols[i].w = max(4, min(nameMax, w-need(cols)+nameMin))
		}
	}
	return cols
}

func (m *model) listBody(st styler, cols []column) []string {
	rows := m.listRows()
	heading := " "
	for i, c := range cols {
		if i > 0 {
			heading += "  "
		}
		if c.right {
			heading += padLeft(c.title, c.w)
		} else {
			heading += pad(c.title, c.w)
		}
	}
	out := []string{st.paint(pad(heading, m.w), sBold, cGray)}

	if len(m.torrents) == 0 {
		return append(out, m.emptyBody(st, rows)...)
	}
	for i := m.offset; i < m.offset+rows; i++ {
		if i >= len(m.torrents) {
			out = append(out, "")
			continue
		}
		out = append(out, m.torrentRow(st, cols, m.torrents[i], i == m.sel))
	}
	return out
}

// torrentRow draws one torrent; the selected row is one reverse-video band.
func (m *model) torrentRow(st styler, cols []column, t engine.Torrent, selected bool) string {
	var b strings.Builder
	b.WriteString(" ")
	for i, c := range cols {
		if i > 0 {
			b.WriteString("  ")
		}
		if c.bar {
			b.WriteString(progressCell(st, t, c.w, !selected))
			continue
		}
		text, codes := c.cell(t)
		cell := pad(text, c.w)
		if c.right {
			cell = padLeft(text, c.w)
		}
		if selected {
			b.WriteString(cell)
		} else {
			b.WriteString(st.paint(cell, codes...))
		}
	}
	line := b.String()
	if selected {
		plain := stripANSI(line)
		return st.paint(pad(plain, m.w), sReverse, sBold)
	}
	return line
}

func (m *model) emptyBody(st styler, rows int) []string {
	out := make([]string, rows)
	var msg, hint string
	switch {
	case m.haveData:
		msg, hint = "No torrents yet.", "Press a to add a .torrent file, a magnet link or an http(s) URL."
	case m.conn == connDown:
		msg, hint = "Can't reach the Denco daemon.", "Retrying every few seconds — start it with 'denco start'. ("+m.connErr+")"
	default:
		msg, hint = "Connecting to the daemon…", ""
	}
	mid := max(0, rows/2-1)
	out[mid] = center(st.paint(truncate(msg, m.w), sBold), min(width(msg), m.w), m.w)
	if mid+1 < rows && hint != "" {
		out[mid+1] = center(st.paint(truncate(hint, m.w), cGray), min(width(hint), m.w), m.w)
	}
	return out
}

func center(painted string, textWidth, w int) string {
	return strings.Repeat(" ", max(0, (w-textWidth)/2)) + painted
}

// --- message and hint lines ----------------------------------------------------------------

// messageLine shows, by priority: the add prompt, a removal confirmation,
// the latest result or error, a lost connection, or work in progress.
func (m *model) messageLine(st styler) string {
	switch m.mode {
	case modeAdd, modeLimits:
		label, hint := " Add torrent: ", "  enter add · esc cancel"
		if m.mode == modeLimits {
			label = fmt.Sprintf(" Limits for #%d (down up): ", m.limitID)
			hint = "  e.g. 500K 2M · 0 = none · enter · esc"
			if width(label)+width(hint)+12 > m.w {
				hint = "  enter · esc"
			}
		}
		room := max(1, m.w-width(label)-width(hint)-1)
		text := tail(clean(string(m.input)), room-1)
		return segLine(st, m.w, []seg{s(label, sBold, cCyan), s(text), s(" ", sReverse)}, []seg{s(hint, cGray)})
	case modeConfirm:
		return segLine(st, m.w, []seg{
			s(fmt.Sprintf(" Remove #%d %s? ", m.confirmID, m.confirmName), sBold, cYellow),
			s("y", sBold), s(" remove · ", cGray), s("D", sBold, cRed), s(" remove and delete its files · ", cGray),
			s("any other key", sBold), s(" cancels", cGray),
		}, nil)
	}
	if text, isErr := m.message(); text != "" {
		if isErr {
			return segLine(st, m.w, []seg{s(" ✗ "+text, cRed)}, nil)
		}
		return segLine(st, m.w, []seg{s(" ✓ "+text, cGreen)}, nil)
	}
	if m.conn == connDown && m.haveData {
		return segLine(st, m.w, []seg{s(" ⚠ daemon unreachable, retrying — showing the last data: "+m.connErr, cRed)}, nil)
	}
	if m.pending > 0 {
		return segLine(st, m.w, []seg{s(" working…", cGray)}, nil)
	}
	return ""
}

type hint struct{ key, what string }

var listHints = []hint{{"↑↓", "move"}, {"enter", "details"}, {"a", "add"}, {"p", "pause"}, {"r", "resume"}, {"d", "remove"}, {"K/J", "queue"}, {"?", "help"}, {"q", "quit"}}

func (m *model) hints(st styler, hs []hint, right string) string {
	segs := []seg{s(" ")}
	if m.lineMode {
		segs = append(segs, s("line mode: type keys then Enter (esc = back) · ", cYellow))
	}
	for i, h := range hs {
		if i > 0 {
			segs = append(segs, s("  "))
		}
		segs = append(segs, s(h.key, sBold), s(" "+h.what, cGray))
	}
	return segLine(st, m.w, segs, []seg{s(right+" ", cGray)})
}

// position is "selected/total" for the list's hint line.
func (m *model) position() string {
	if len(m.torrents) == 0 {
		return ""
	}
	return fmt.Sprintf("%d/%d", m.sel+1, len(m.torrents))
}
