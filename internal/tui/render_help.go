package tui

import "strings"

// helpLines is the shortcut list shown by '?'.
var helpLines = []struct{ keys, what string }{
	{"", "Torrent list"},
	{"↑ ↓   j k", "move the selection"},
	{"PgUp PgDn", "move a page"},
	{"g G  Home End", "first / last torrent"},
	{"Enter", "open the torrent's details"},
	{"a", "add a .torrent file, magnet link or http(s) URL"},
	{"p", "pause the selected torrent"},
	{"r", "resume it (also retries a failed one)"},
	{"d", "remove it — asks first; D there also deletes its files"},
	{"K J  T B", "move it up / down / to the top / bottom of the queue"},
	{"f", "force start past max_active_downloads (f again undoes)"},
	{"s", "its own speed limits: down up, e.g. 500K 2M (0 = none)"},
	{"", ""},
	{"", "Details"},
	{"Esc ← h", "back to the list"},
	{"Tab → l", "switch between files and peers"},
	{"↑ ↓   j k", "scroll"},
	{"p r d f s K J T B", "the same for this torrent"},
	{"", ""},
	{"?", "this help"},
	{"q  Ctrl-C", "quit (the daemon keeps running)"},
}

// overlayHelp draws the shortcut list in a box over lines, centered.
func (m *model) overlayHelp(st styler, lines []string) {
	const keyW = 19
	inner := 0
	for _, h := range helpLines {
		inner = max(inner, keyW+width(h.what))
	}
	inner = min(inner+2, m.w-4)
	body := []string{}
	lines0 := helpLines
	if len(helpLines)+4 > m.h { // short terminal: drop the spacer lines first
		lines0 = nil
		for _, h := range helpLines {
			if h.keys != "" || h.what != "" {
				lines0 = append(lines0, h)
			}
		}
	}
	for _, h := range lines0 {
		switch {
		case h.keys == "" && h.what != "": // section title
			body = append(body, st.paint(pad(" "+h.what, inner), sBold, cCyan))
		default:
			body = append(body, " "+st.paint(pad(h.keys, keyW-1), sBold)+st.paint(pad(h.what, inner-keyW), cGray))
		}
	}
	footer := truncate("press any key to close", inner)
	body = append(body, "", st.paint(center(footer, width(footer), inner)+strings.Repeat(" ", max(0, inner-width(footer)-(inner-width(footer))/2)), cGray))

	top := max(0, (m.h-len(body)-2)/2)
	left := strings.Repeat(" ", max(0, (m.w-inner-2)/2))
	rows := []string{st.paint("╭"+strings.Repeat("─", inner)+"╮", cBlue)}
	for _, b := range body {
		if b == "" {
			b = strings.Repeat(" ", inner)
		}
		rows = append(rows, st.paint("│", cBlue)+b+st.paint("│", cBlue))
	}
	rows = append(rows, st.paint("╰"+strings.Repeat("─", inner)+"╯", cBlue))
	for i, r := range rows {
		if top+i < len(lines) {
			lines[top+i] = left + r
		}
	}
}
