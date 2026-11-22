package tui

import (
	"fmt"
	"slices"
	"strings"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/webseed"
)

// Details layout:
//
//	row 0     title bar with "#ID name"
//	row 1     rule
//	info      label: value lines (two columns on wide terminals)
//	          tab bar: Files | Peers
//	          table heading, then rows (scrolling)
//	row h-2   message
//	row h-1   key hints

const (
	detailChrome  = 6 // title, rule, tab bar, table heading, message, hints
	minTableRows  = 3 // info lines give way to keep at least this many rows
	labelWidth    = 11
	twoColumnsMin = 110
)

var detailHints = []hint{{"esc", "back"}, {"tab", "files/peers"}, {"↑↓", "scroll"}, {"p", "pause"}, {"r", "resume"}, {"d", "remove"}, {"K/J", "queue"}, {"?", "help"}, {"q", "quit"}}

// infoItem is one "label: value" line; a value with bar ≥ 0 gets a
// progress bar in front of its text.
type infoItem struct {
	label string
	text  string
	codes []string
	bar   float64
	done  bool
}

func item(label, text string, codes ...string) infoItem {
	return infoItem{label: label, text: text, codes: codes, bar: -1}
}

// detailInfo lists what is known about the torrent, most important first
// (short terminals show only the first few).
func (m *model) detailInfo() []infoItem {
	t := m.detail
	status := stateLabel(t.State)
	if t.State == engine.StateMetadata {
		status = "Fetching metadata from peers"
	}
	if t.UploadsPaused {
		status += " · uploads paused"
	}
	items := []infoItem{item("Status", status, stateColor(t.State), sBold)}
	if t.Error != "" {
		items = append(items, item("Error", clean(t.Error), cRed))
	}
	if m.detailErr != "" {
		items = append(items, item("Refresh", m.detailErr, cRed))
	}
	if !t.HasMetadata {
		meta := "waiting for peers that have it"
		if t.MetaTotal > 0 {
			meta = fmt.Sprintf("%d / %d pieces from %d peer%s", t.MetaHave, t.MetaTotal, t.Peers, plural(t.Peers))
		}
		items = append(items, item("Metadata", meta))
	} else {
		p := format.Percent(t.DoneBytes, t.WantedBytes) + " · " + format.Bytes(t.DoneBytes) + " of " + format.Bytes(t.WantedBytes)
		if t.SkippedFiles > 0 {
			p += fmt.Sprintf(" (%d file%s skipped)", t.SkippedFiles, plural(t.SkippedFiles))
		}
		frac := 0.0
		if t.WantedBytes > 0 {
			frac = float64(t.DoneBytes) / float64(t.WantedBytes)
		}
		items = append(items, infoItem{label: "Progress", text: p, bar: frac, done: t.Complete})
	}
	switch {
	case t.Forced && !t.Complete:
		items = append(items, item("Queue", fmt.Sprintf("forced, %d of %d (f queues it again)", t.Queue, t.QueueLength), cYellow))
	case t.Queue > 0:
		items = append(items, item("Queue", fmt.Sprintf("%d of %d (K J T B move it, f forces it)", t.Queue, t.QueueLength)))
	}
	if t.DownLimit > 0 || t.UpLimit > 0 {
		items = append(items, item("Limits", fmt.Sprintf("↓ %s · ↑ %s (its own; s changes them)", ownLimitText(t.DownLimit), ownLimitText(t.UpLimit)), cYellow))
	}
	if running(t) {
		items = append(items, item("Speed", "↓ "+format.Speed(t.DownloadRate)+"   ↑ "+format.Speed(t.UploadRate)))
		if !t.Complete {
			items = append(items, item("ETA", format.Duration(t.ETA)))
		}
		peers := fmt.Sprintf("%d connected (%d seeds, %d leechers)", t.Peers, t.Seeds, t.Leechers)
		if t.TrackerSeeders >= 0 {
			peers += fmt.Sprintf(" · swarm %d seeds, %d leechers", t.TrackerSeeders, max(t.TrackerLeechers, 0))
		}
		if t.DHTPeers > 0 {
			peers += fmt.Sprintf(" · DHT found %d", t.DHTPeers)
		}
		items = append(items, item("Peers", peers))
		if t.WebSeeds > 0 {
			items = append(items, item("Web seeds", fmt.Sprintf("%d (%d downloading)", t.WebSeeds, t.WebSeedsActive)))
		}
	}
	items = append(items, item("Ratio", fmt.Sprintf("%s · ↑ %s uploaded · ↓ %s downloaded",
		ratioText(t.Ratio), format.Bytes(t.Uploaded), format.Bytes(t.Downloaded))))
	if t.HasMetadata {
		items = append(items, item("Pieces", fmt.Sprintf("%d / %d × %s · %d file%s", t.PiecesHave, t.PiecesTotal,
			format.Bytes(t.PieceLength), t.Files, plural(t.Files))))
	}
	seeding := t.SeedPolicy
	if t.State == engine.StateFinished {
		seeding = "finished"
		if t.SeedNote != "" {
			seeding += ": " + t.SeedNote
		}
	}
	if seeding != "" {
		items = append(items, item("Seeding", seeding))
	}
	if t.TrackerError != "" {
		first, _, _ := strings.Cut(t.TrackerError, "\n")
		items = append(items, item("Trackers", "failing: "+clean(first), cYellow))
	}
	items = append(items, item("Location", clean(t.Dir)), item("Info hash", t.InfoHash, cGray))
	added := "-"
	if !t.AddedAt.IsZero() {
		added = t.AddedAt.Local().Format("2006-01-02 15:04")
	}
	if !t.CompletedAt.IsZero() {
		added += " · completed " + t.CompletedAt.Local().Format("2006-01-02 15:04")
	}
	return append(items, item("Added", added, cGray))
}

// detailInfoLines lays the info out: two columns on wide terminals, and
// only as many lines as leave room for the table.
func (m *model) detailInfoLines(st styler) []string {
	items := m.detailInfo()
	maxLines := max(0, m.h-detailChrome-minTableRows) // tiny terminals: the table only
	var lines []string
	if m.w >= twoColumnsMin {
		cw := (m.w - 1) / 2
		for i := 0; i < len(items) && len(lines) < maxLines; i += 2 {
			left := infoLine(st, items[i], cw)
			right := ""
			if i+1 < len(items) {
				right = infoLine(st, items[i+1], m.w-cw-1)
			}
			lines = append(lines, left+" "+right)
		}
		return lines
	}
	for _, it := range items {
		if len(lines) == maxLines {
			break
		}
		lines = append(lines, infoLine(st, it, m.w))
	}
	return lines
}

// infoLine draws one item in exactly w cells.
func infoLine(st styler, it infoItem, w int) string {
	room := max(0, w-labelWidth-1)
	label := " " + pad(it.label, labelWidth-1)
	value := ""
	used := 0
	if it.bar >= 0 && room >= 30 {
		value = progressBar(st, it.bar, 16, it.done, true) + " "
		used = 17
	}
	text := truncate(it.text, room-used)
	value += st.paint(text, it.codes...)
	used += width(text)
	return st.paint(label, cGray) + value + strings.Repeat(" ", max(0, w-labelWidth-used))
}

// detailRows is how many files or peers fit under the info.
func (m *model) detailRows() int {
	return max(1, m.h-detailChrome-len(m.detailInfoLines(styler{m.color})))
}

func (m *model) renderDetails(st styler) []string {
	t := m.detail
	crumb := []seg{s(fmt.Sprintf("#%d ", t.ID), cGray), s(clean(t.Name), sBold)}
	lines := []string{m.titleBar(st, crumb), st.paint(strings.Repeat("─", m.w), cGray)}
	lines = append(lines, m.detailInfoLines(st)...)
	lines = append(lines, m.tabBar(st))
	lines = append(lines, m.detailTable(st)...)
	pos := ""
	if n := m.tabLen(); n > 0 {
		pos = fmt.Sprintf("%d-%d/%d", m.scroll+1, min(n, m.scroll+m.detailRows()), n)
	}
	return append(lines, m.messageLine(st), m.hints(st, detailHints, pos))
}

func (m *model) tabBar(st styler) string {
	files := fmt.Sprintf(" Files (%d) ", len(m.files))
	peers := fmt.Sprintf(" Peers (%d) ", len(m.peers))
	a, b := s(files, sBold, sReverse), s(peers, cGray)
	if m.tab == tabPeers {
		a, b = s(files, cGray), s(peers, sBold, sReverse)
	}
	used := 1 + width(files) + 1 + width(peers) + 1
	return segLine(st, m.w, []seg{s(" "), a, s(" "), b, s(" "), s(strings.Repeat("─", max(0, m.w-used)), cGray)}, nil)
}

// --- files and peers tables -------------------------------------------------------------

// tcol is a column of the files/peers tables. drop orders which columns
// go first on narrow terminals (0 = never); flex takes the spare width.
type tcol struct {
	title       string
	w           int
	right, flex bool
	drop        int
}

// tcell is one cell; bar ≥ 0 makes it a progress cell.
type tcell struct {
	text  string
	codes []string
	bar   float64
	done  bool
}

func cell(text string, codes ...string) tcell { return tcell{text: text, codes: codes, bar: -1} }

// fitColumns keeps the columns that fit in w (dropping by drop, highest
// first) and gives the flex column the rest. It returns the kept indexes.
func fitColumns(cols []tcol, w int) ([]tcol, []int) {
	keep := make([]int, len(cols))
	for i := range cols {
		keep[i] = i
	}
	need := func() int {
		n := 1
		for _, i := range keep {
			n += cols[i].w + 2
		}
		return n
	}
	for need() > w {
		worst := -1
		for k, i := range keep {
			if cols[i].drop > 0 && (worst < 0 || cols[i].drop > cols[keep[worst]].drop) {
				worst = k
			}
		}
		if worst < 0 {
			break
		}
		keep = append(keep[:worst], keep[worst+1:]...)
	}
	out := make([]tcol, len(keep))
	for k, i := range keep {
		out[k] = cols[i]
	}
	for k := range out {
		if out[k].flex {
			out[k].w = max(out[k].w, out[k].w+w-need())
		}
	}
	return out, keep
}

func tableLine(st styler, cols []tcol, idx []int, cells []tcell, w int) string {
	var b strings.Builder
	b.WriteString(" ")
	used := 1
	for k, c := range cols {
		if k > 0 {
			if used+2 >= w {
				break // no room for another column
			}
			b.WriteString("  ")
			used += 2
		}
		if c.w > w-used { // last column cut short on very narrow screens
			c.w = max(0, w-used)
		}
		cl := cells[idx[k]]
		switch {
		case cl.bar >= 0 && c.w >= barCellWidth:
			b.WriteString(progressBar(st, cl.bar, c.w-pctWidth-1, cl.done, true) + " " + padLeft(cl.text, pctWidth))
		case c.right:
			b.WriteString(st.paint(padLeft(cl.text, c.w), cl.codes...))
		default:
			b.WriteString(st.paint(pad(cl.text, c.w), cl.codes...))
		}
		used += c.w
	}
	return b.String() + strings.Repeat(" ", max(0, w-used))
}

func heading(st styler, cols []tcol, w int) string {
	cells := make([]tcell, len(cols))
	idx := make([]int, len(cols))
	for i, c := range cols {
		cells[i], idx[i] = cell(c.title), i
	}
	return st.paint(stripANSI(tableLine(styler{}, cols, idx, cells, w)), sBold, cGray)
}

// maxAddrWidth is the longest address: "[" + 39 + "]:" + 5 digits.
const maxAddrWidth = 47

var (
	fileCols = []tcol{
		{title: "#", w: 4, right: true}, {title: "WANT", w: 4, drop: 2}, {title: "SIZE", w: 9, right: true, drop: 3},
		{title: "PROGRESS", w: barCellWidth, right: true}, {title: "PATH", w: 12, flex: true},
	}
	peerCols = []tcol{
		{title: "ADDRESS", w: 21}, {title: "CLIENT", w: 18, drop: 4}, {title: "DIR", w: 3, drop: 3},
		{title: "HAS", w: 6, right: true, drop: 2}, {title: "DOWN", w: 10, right: true},
		{title: "UP", w: 10, right: true, drop: 1}, {title: "STATUS", w: 8, flex: true, drop: 5},
	}
)

func (m *model) detailTable(st styler) []string {
	rows := m.detailRows()
	var cols []tcol
	var data [][]tcell
	empty := ""
	if m.tab == tabFiles {
		cols = fileCols
		for _, f := range m.files {
			want, wc := "yes", []string{cGreen}
			if f.Skipped {
				want, wc = "skip", []string{cGray}
			}
			frac, done := 1.0, true
			if f.Size > 0 {
				frac, done = float64(f.Done)/float64(f.Size), f.Done >= f.Size
			}
			pct := format.Percent(f.Done, f.Size)
			if f.Size == 0 {
				pct = "100%"
			}
			data = append(data, []tcell{cell(fmt.Sprint(f.Index+1), cGray), cell(want, wc...),
				cell(format.Bytes(f.Size)), {text: pct, bar: frac, done: done}, cell(clean(f.Path))})
		}
		switch {
		case !m.detail.HasMetadata:
			empty = "The file list arrives with the torrent's metadata."
		case m.files == nil:
			empty = "Loading…"
		}
	} else {
		// IPv6 addresses are up to 47 cells: the column grows to the
		// longest one shown (to a cap), and narrow screens cut it (D60).
		cols = slices.Clone(peerCols)
		for _, p := range m.peers {
			cols[0].w = max(cols[0].w, min(maxAddrWidth, width(peerAddr(p))))
		}
		for _, p := range m.peers {
			dir, status := "out", peerStatus(p)
			if p.Incoming {
				dir = "in"
			}
			if p.WebSeed != "" {
				dir, status = "web", clean(p.WebSeedNote)
			}
			data = append(data, []tcell{cell(peerAddr(p)), cell(clean(p.Client), cGray), cell(dir, cGray),
				cell(fmt.Sprintf("%.1f%%", p.Progress*100)), cell(format.Speed(p.DownloadRate), cCyan),
				cell(format.Speed(p.UploadRate), cMagenta), cell(status, cGray)})
		}
		switch {
		case !running(m.detail):
			empty = "No peers: the torrent is " + strings.ToLower(stateLabel(m.detail.State)) + "."
		case len(m.peers) == 0:
			empty = "No peers connected yet."
		}
	}
	fit, idx := fitColumns(cols, m.w)
	out := []string{heading(st, fit, m.w)}
	if len(data) == 0 {
		out = append(out, st.paint(pad("  "+empty, m.w), cGray))
	}
	for i := m.scroll; i < len(data) && len(out) <= rows; i++ {
		out = append(out, tableLine(st, fit, idx, data[i], m.w))
	}
	for len(out) <= rows {
		out = append(out, "")
	}
	return out
}

// peerAddr is a peer's address, or a web seed's host (D61).
func peerAddr(p engine.Peer) string {
	if p.WebSeed != "" {
		return clean(webseed.Host(p.WebSeed))
	}
	return p.Addr.String()
}

// peerStatus says in words what flows over a connection.
func peerStatus(p engine.Peer) string {
	var parts []string
	switch {
	case p.Interested && !p.PeerChoking:
		parts = append(parts, "downloading")
	case p.Interested:
		parts = append(parts, "choked by peer")
	}
	switch {
	case p.PeerWants && !p.AmChoking:
		parts = append(parts, "uploading")
	case p.PeerWants:
		parts = append(parts, "peer waiting")
	}
	if len(parts) == 0 {
		return "idle"
	}
	return strings.Join(parts, ", ")
}
