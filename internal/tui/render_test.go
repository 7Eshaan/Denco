package tui

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/engine"
)

// plainScreen renders m and returns the lines without escape codes.
func plainScreen(m *model) []string {
	lines := m.render()
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = stripANSI(l)
	}
	return out
}

func screenText(m *model) string { return strings.Join(plainScreen(m), "\n") }

// detailsModel is a model showing torrent #2 with files and peers.
func detailsModel(w, h int) *model {
	m := newModel(w, h, false, true, t0)
	ts := fakeTorrents(6)
	connected(m, ts)
	press(m, kDown, kEnter)
	var files []engine.File
	for i := 0; i < 25; i++ {
		files = append(files, engine.File{Index: i, Path: fmt.Sprintf("Season 1/Episode %02d — 日本語.mkv", i+1), Size: 700_000_000, Done: int64(i) * 28_000_000, Skipped: i == 3})
	}
	peers := []engine.Peer{{Addr: netip.MustParseAddrPort("203.0.113.7:51413"), Client: "qBittorrent 4.6.3.0", Progress: 0.5,
		DownloadRate: 1_200_000, UploadRate: 30_000, Interested: true, PeerWants: true}}
	m.update(dataEvent{snap: snapshot{torrents: ts, detailID: 2, detail: ts[1], files: files, peers: peers}})
	return m
}

// Every screen, at every size, is exactly h lines of at most w cells.
func TestEveryScreenFitsEverySize(t *testing.T) {
	type setup struct {
		name string
		make func(w, h int) *model
	}
	setups := []setup{
		{"list", func(w, h int) *model { m := newModel(w, h, false, true, t0); connected(m, fakeTorrents(30)); return m }},
		{"empty", func(w, h int) *model { m := newModel(w, h, false, true, t0); connected(m, nil); return m }},
		{"connecting", func(w, h int) *model { return newModel(w, h, true, false, t0) }},
		{"disconnected", func(w, h int) *model {
			m := newModel(w, h, false, true, t0)
			m.update(dataEvent{err: errors.New("the denco daemon is not running")})
			return m
		}},
		{"details", detailsModel},
		{"details-peers", func(w, h int) *model { m := detailsModel(w, h); press(m, Key{Code: KeyTab}); return m }},
		{"help", func(w, h int) *model {
			m := newModel(w, h, false, true, t0)
			connected(m, fakeTorrents(3))
			press(m, runeKey('?'))
			return m
		}},
		{"confirm", func(w, h int) *model {
			m := newModel(w, h, false, true, t0)
			connected(m, fakeTorrents(3))
			press(m, runeKey('d'))
			return m
		}},
		{"add", func(w, h int) *model {
			m := newModel(w, h, false, true, t0)
			connected(m, fakeTorrents(3))
			press(m, runeKey('a'))
			for _, r := range strings.Repeat("magnet:?xt=urn:btih:0123456789abcdef", 4) {
				press(m, runeKey(r))
			}
			return m
		}},
	}
	for _, s := range setups {
		for _, w := range []int{1, 10, 29, 30, 40, 55, 70, 80, 100, 109, 110, 140, 250} {
			for _, h := range []int{1, 3, 6, 7, 8, 12, 24, 60} {
				m := s.make(w, h)
				lines := plainScreen(m)
				if len(lines) != h {
					t.Fatalf("%s %dx%d: %d lines", s.name, w, h, len(lines))
				}
				for i, l := range lines {
					if width(l) > w {
						t.Fatalf("%s %dx%d: line %d is %d cells:\n%q", s.name, w, h, i, width(l), l)
					}
				}
			}
		}
	}
}

func TestDashboardContent(t *testing.T) {
	m := newModel(140, 20, false, true, t0)
	ts := fakeTorrents(6)
	connected(m, ts)
	m.status.DownloadRate, m.status.UploadRate, m.status.Peers = 4_200_000, 120_000, 46
	got := screenText(m)
	for _, want := range []string{
		"DENCO", "connected", "pid 4242", "up 2h 0m", "↓", "4.2 MB/s", "120.0 KB/s",
		"6 torrents", "2 downloading", "1 seeding", "1 paused", "1 failed", "46 peers",
		"ID", "NAME", "SIZE", "PROGRESS", "STATUS", "DOWN", "UP", "ETA", "PEERS", "RATIO",
		"torrent-01-downloading.iso", "5.2 GB", "5.8%", "Downloading", "7m 42s", "0.12",
		"Seeding", "100%", "Metadata", "Error",
		"enter details", "q quit", "1/6",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("dashboard is missing %q:\n%s", want, got)
		}
	}
	// A seed shows no download speed or ETA; a paused torrent no speeds.
	for _, l := range plainScreen(m) {
		if strings.Contains(l, "seeding.iso") && strings.Contains(l, "7m 42s") {
			t.Errorf("a seed shows an ETA: %q", l)
		}
	}
}

func TestNarrowDropsColumnsNotNames(t *testing.T) {
	m := newModel(48, 12, false, true, t0)
	connected(m, fakeTorrents(3))
	got := screenText(m)
	for _, gone := range []string{"RATIO", "PEERS", "ETA"} {
		if strings.Contains(got, gone) {
			t.Errorf("48 columns should drop %s:\n%s", gone, got)
		}
	}
	for _, want := range []string{"ID", "NAME", "torrent-0", "%"} {
		if !strings.Contains(got, want) {
			t.Errorf("48 columns lost %q:\n%s", want, got)
		}
	}
}

func TestEmptyAndDisconnectedScreens(t *testing.T) {
	m := newModel(100, 15, false, true, t0)
	connected(m, nil)
	if got := screenText(m); !strings.Contains(got, "No torrents yet") || !strings.Contains(got, "Press a to add") {
		t.Errorf("empty list:\n%s", got)
	}
	m = newModel(100, 15, false, true, t0)
	m.update(dataEvent{err: errors.New("the denco daemon is not running")})
	if got := screenText(m); !strings.Contains(got, "disconnected") || !strings.Contains(got, "Can't reach the Denco daemon") || !strings.Contains(got, "denco start") {
		t.Errorf("never connected:\n%s", got)
	}
	m = newModel(100, 15, false, true, t0)
	connected(m, fakeTorrents(2))
	m.update(dataEvent{err: errors.New("connection refused")})
	got := screenText(m)
	if !strings.Contains(got, "torrent-01") || !strings.Contains(got, "daemon unreachable") {
		t.Errorf("lost connection should keep the list and say so:\n%s", got)
	}
	m = newModel(20, 5, false, true, t0)
	if got := screenText(m); !strings.Contains(got, "too small") && !strings.Contains(got, "Terminal") {
		t.Errorf("tiny terminal:\n%s", got)
	}
}

func TestSelectedRowIsHighlighted(t *testing.T) {
	m := newModel(100, 12, false, true, t0)
	connected(m, fakeTorrents(3))
	press(m, kDown)
	lines := m.render()
	for i, l := range lines {
		isSel := strings.Contains(l, "torrent-02")
		if isSel != strings.HasPrefix(l, "\x1b[7;1m") {
			t.Errorf("line %d selected=%v but highlight mismatch: %q", i, isSel, l)
		}
		if isSel && width(stripANSI(l)) != 100 {
			t.Errorf("the highlight should span the full width: %d", width(stripANSI(l)))
		}
	}
}

func TestNoColor(t *testing.T) {
	colorCode := regexp.MustCompile(`\x1b\[[0-9;]*(3[0-9]|4[0-9]|9[0-9])[0-9;]*m`)
	for _, m := range []*model{detailsModel(120, 40), func() *model {
		m := newModel(120, 20, false, false, t0)
		connected(m, fakeTorrents(6))
		return m
	}()} {
		m.color = false
		for _, l := range m.render() {
			if colorCode.MatchString(l) {
				t.Fatalf("color code with NO_COLOR: %q", l)
			}
		}
	}
	// The progress bar stays readable without color.
	m := newModel(120, 12, false, false, t0)
	connected(m, fakeTorrents(2))
	if got := screenText(m); !strings.Contains(got, "█") || !strings.Contains(got, "░") {
		t.Errorf("no-color progress bar:\n%s", got)
	}
}

func TestDetailsContent(t *testing.T) {
	m := detailsModel(120, 40)
	got := screenText(m)
	for _, want := range []string{
		"#2", "torrent-02-seeding.iso", "Status", "Seeding", "Progress", "100%", "Speed", "Peers", "18 connected",
		"Ratio", "0.12", "Pieces", "860 / 1315", "Info hash", fmt.Sprintf("%040x", 2),
		"Files (25)", "Peers (1)", "Episode 01", "skip", "esc back",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("details missing %q:\n%s", want, got)
		}
	}
	press(m, Key{Code: KeyTab})
	got = screenText(m)
	for _, want := range []string{"ADDRESS", "203.0.113.7:51413", "qBittorrent", "1.2 MB/s", "downloading, uploading"} {
		if !strings.Contains(got, want) {
			t.Errorf("peers tab missing %q:\n%s", want, got)
		}
	}
	// A short terminal keeps the table and gives up info lines.
	m = detailsModel(80, 14)
	if got := screenText(m); !strings.Contains(got, "Files (25)") || !strings.Contains(got, "Episode") {
		t.Errorf("short details lost the table:\n%s", got)
	}
}

func TestHelpListsEveryShortcut(t *testing.T) {
	m := newModel(100, 30, false, true, t0)
	press(m, runeKey('?'))
	got := screenText(m)
	for _, want := range []string{"↑ ↓   j k", "Enter", "add a .torrent file", "pause", "resume", "remove", "this help", "quit", "daemon keeps running", "Esc"} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing %q:\n%s", want, got)
		}
	}
}

// Names, paths and errors from torrents can't send escape sequences to
// the terminal: only the TUI's own SGR codes reach it.
func TestRenderedTextCantDriveTheTerminal(t *testing.T) {
	evil := "x\x1b]0;owned\x07\x1b[2J\x1b[?1049l"
	m := newModel(120, 30, false, true, t0)
	ts := fakeTorrents(2)
	ts[0].Name, ts[0].Error, ts[0].Dir = evil, evil, evil
	connected(m, ts)
	m.update(resultEvent{err: errors.New(evil)})
	check := func(where string) {
		for _, l := range m.render() {
			for _, bad := range []string{"\x1b]", "\x07", "\x1b[2J", "\x1b[?1049l"} {
				if strings.Contains(l, bad) {
					t.Fatalf("%s: %q reached the screen: %q", where, bad, l)
				}
			}
		}
	}
	check("list")
	press(m, kEnter)
	m.update(dataEvent{snap: snapshot{torrents: ts, detailID: 1, detail: ts[0],
		files: []engine.File{{Path: evil, Size: 1}}, peers: []engine.Peer{{Client: evil}}}})
	check("details")
	press(m, Key{Code: KeyTab})
	check("peers")
	press(m, runeKey('d'))
	check("confirm")
}
