package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/format"
)

// Cells of the torrent table, status words and colors, and progress bars.

// running: the torrent has live speeds and peers.
func running(t engine.Torrent) bool {
	switch t.State {
	case engine.StateDownloading, engine.StateStalled, engine.StateSeeding:
		return true
	}
	return false
}

func sizeCell(t engine.Torrent) (string, []string) {
	if !t.HasMetadata {
		return "?", []string{cGray}
	}
	return format.Bytes(t.WantedBytes), nil
}

func statusCell(t engine.Torrent) (string, []string) {
	return stateLabel(t.State), []string{stateColor(t.State)}
}

func downCell(t engine.Torrent) (string, []string) {
	if !running(t) || t.Complete {
		return "-", []string{cGray}
	}
	return format.Speed(t.DownloadRate), []string{cCyan}
}

func upCell(t engine.Torrent) (string, []string) {
	switch {
	case !running(t):
		return "-", []string{cGray}
	case t.UploadsPaused:
		return "paused", []string{cYellow}
	}
	return format.Speed(t.UploadRate), []string{cMagenta}
}

func etaCell(t engine.Torrent) (string, []string) {
	if !running(t) || t.Complete {
		return "-", []string{cGray}
	}
	return format.Duration(t.ETA), nil
}

func peersCell(t engine.Torrent) (string, []string) {
	if !running(t) && t.State != engine.StateMetadata {
		return "-", []string{cGray}
	}
	return strconv.Itoa(t.Peers), nil
}

func ratioText(r float64) string {
	if r < 0 {
		return "-"
	}
	return fmt.Sprintf("%.2f", r)
}

// stateLabel is a short status word that fits the STATUS column.
func stateLabel(s engine.State) string {
	if s == engine.StateMetadata {
		return "Metadata"
	}
	if s == "" {
		return "Unknown"
	}
	return strings.ToUpper(string(s[:1])) + string(s[1:])
}

func stateColor(s engine.State) string {
	switch s {
	case engine.StateDownloading, engine.StateMetadata:
		return cCyan
	case engine.StateSeeding:
		return cGreen
	case engine.StatePaused, engine.StateChecking:
		return cYellow
	case engine.StateStalled:
		return cMagenta
	case engine.StateError:
		return cRed
	}
	return cGray // queued, finished
}

// progressCell is a bar and percentage in w cells (just the percentage
// when w is narrow).
func progressCell(st styler, t engine.Torrent, w int, colored bool) string {
	if !t.HasMetadata {
		return padLeft("-", w)
	}
	pct := padLeft(format.Percent(t.DoneBytes, t.WantedBytes), pctWidth)
	if w < barCellWidth {
		return padLeft(pct, w)
	}
	frac := 0.0
	if t.WantedBytes > 0 {
		frac = float64(t.DoneBytes) / float64(t.WantedBytes)
	}
	return progressBar(st, frac, w-pctWidth-1, t.Complete, colored) + " " + pct
}

// progressBar draws frac (0…1) in w cells: a colored line with half-cell
// steps, or block characters without color (still readable with NO_COLOR,
// and inside the reverse-video selected row).
func progressBar(st styler, frac float64, w int, complete, colored bool) string {
	frac = min(max(frac, 0), 1)
	halves := int(frac*float64(2*w) + 0.5)
	if !complete && halves == 2*w && frac < 1 {
		halves-- // never look finished before it is
	}
	full, half := halves/2, halves%2
	if !st.color || !colored {
		return strings.Repeat("█", full) + strings.Repeat("▌", half) + strings.Repeat("░", w-full-half)
	}
	color := cCyan
	if complete {
		color = cGreen
	}
	bar := st.paint(strings.Repeat("━", full)+strings.Repeat("╸", half), color)
	return bar + st.paint(strings.Repeat("━", w-full-half), cGray)
}
