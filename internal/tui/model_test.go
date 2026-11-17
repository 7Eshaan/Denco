package tui

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
)

var t0 = time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)

// fakeTorrents returns n torrents with IDs 1…n in assorted states.
func fakeTorrents(n int) []engine.Torrent {
	states := []engine.State{engine.StateDownloading, engine.StateSeeding, engine.StatePaused, engine.StateMetadata, engine.StateError, engine.StateQueued}
	var out []engine.Torrent
	for i := 1; i <= n; i++ {
		st := states[(i-1)%len(states)]
		t := engine.Torrent{
			ID: i, Name: fmt.Sprintf("torrent-%02d-%s.iso", i, st), State: st, InfoHash: fmt.Sprintf("%040x", i),
			HasMetadata: st != engine.StateMetadata, TotalBytes: 5_200_000_000, WantedBytes: 5_200_000_000,
			DoneBytes: int64(i) * 300_000_000 % 5_200_000_000, DownloadRate: 4_200_000, UploadRate: 120_000,
			ETA: 7*time.Minute + 42*time.Second, Peers: 18, Seeds: 8, Leechers: 10, Ratio: 0.12, Files: 3,
			PiecesHave: 860, PiecesTotal: 1315, PieceLength: 4 << 20, TrackerSeeders: -1,
		}
		if st == engine.StateSeeding {
			t.DoneBytes, t.Complete = t.WantedBytes, true
		}
		if st == engine.StateError {
			t.Error = "storage: disk full"
		}
		out = append(out, t)
	}
	return out
}

func connected(m *model, ts []engine.Torrent) {
	st := ipc.Status{PID: 4242}
	st.StartedAt = t0.Add(-2 * time.Hour)
	st.Torrents = len(ts)
	st.ByState = map[engine.State]int{}
	for _, t := range ts {
		st.ByState[t.State]++
	}
	m.update(dataEvent{snap: snapshot{status: st, torrents: ts}})
}

func press(m *model, keys ...Key) []action {
	var acts []action
	for _, k := range keys {
		acts = append(acts, m.update(keyEvent{k})...)
	}
	return acts
}

var (
	kDown  = Key{Code: KeyDown}
	kUp    = Key{Code: KeyUp}
	kEnter = Key{Code: KeyEnter}
	kEsc   = Key{Code: KeyEsc}
)

func TestNavigationStaysInBounds(t *testing.T) {
	m := newModel(100, 10, false, true, t0) // 4 rows of torrents
	connected(m, fakeTorrents(9))
	press(m, kUp, runeKey('k'))
	if m.sel != 0 {
		t.Fatalf("up at the top: sel %d", m.sel)
	}
	press(m, kDown, runeKey('j'), kDown, kDown, kDown)
	if m.sel != 5 || m.selID != 6 {
		t.Fatalf("after 5 downs: sel %d id %d", m.sel, m.selID)
	}
	if m.offset != 5-m.listRows()+1 {
		t.Errorf("selection scrolled out of view: offset %d, rows %d", m.offset, m.listRows())
	}
	press(m, runeKey('G'))
	if m.sel != 8 {
		t.Errorf("G: sel %d", m.sel)
	}
	press(m, kDown)
	if m.sel != 8 {
		t.Errorf("down at the bottom: sel %d", m.sel)
	}
	press(m, runeKey('g'))
	if m.sel != 0 || m.offset != 0 {
		t.Errorf("g: sel %d offset %d", m.sel, m.offset)
	}
	press(m, Key{Code: KeyPgDn})
	if m.sel != m.listRows() {
		t.Errorf("page down: sel %d", m.sel)
	}
}

func TestSelectionFollowsTorrentAcrossRefreshes(t *testing.T) {
	m := newModel(100, 20, false, true, t0)
	ts := fakeTorrents(5)
	connected(m, ts)
	press(m, kDown, kDown) // #3
	// #1 was removed elsewhere: #3 is now row 1, and stays selected.
	connected(m, ts[1:])
	if got, _ := m.selected(); got.ID != 3 {
		t.Fatalf("selected #%d, want #3", got.ID)
	}
	// #3 itself goes away: the selection stays on the same row.
	connected(m, []engine.Torrent{ts[1], ts[3], ts[4]})
	if got, _ := m.selected(); got.ID != 4 {
		t.Fatalf("after removing the selected one: #%d, want #4", got.ID)
	}
	connected(m, nil)
	if _, ok := m.selected(); ok || m.selID != 0 {
		t.Fatal("empty list should select nothing")
	}
	if acts := press(m, runeKey('p')); acts != nil {
		t.Fatalf("pause with nothing selected: %v", acts)
	}
	if msg, isErr := m.message(); !isErr || msg != "No torrent selected" {
		t.Errorf("message %q", msg)
	}
}

func TestActionsTargetTheSelectedTorrent(t *testing.T) {
	m := newModel(100, 20, false, true, t0)
	connected(m, fakeTorrents(5))
	press(m, kDown)
	if acts := press(m, runeKey('p')); !reflect.DeepEqual(acts, []action{{kind: actPause, id: 2, name: "torrent-02-seeding.iso"}}) {
		t.Errorf("p: %+v", acts)
	}
	if acts := press(m, runeKey('r')); len(acts) != 1 || acts[0].kind != actResume || acts[0].id != 2 {
		t.Errorf("r: %+v", acts)
	}
	if m.pending != 2 {
		t.Errorf("pending %d", m.pending)
	}
	m.update(resultEvent{text: "Paused #2"})
	m.update(resultEvent{err: errors.New("not seeding: ratio reached")})
	if msg, isErr := m.message(); !isErr || msg != "not seeding: ratio reached" || m.pending != 0 {
		t.Errorf("after results: %q %v pending %d", msg, isErr, m.pending)
	}
	m.now = m.now.Add(flashErrFor + time.Second)
	if msg, _ := m.message(); msg != "" {
		t.Errorf("message should have expired: %q", msg)
	}
}

func TestRemoveAsksFirst(t *testing.T) {
	m := newModel(100, 20, false, true, t0)
	connected(m, fakeTorrents(3))
	if acts := press(m, runeKey('d')); acts != nil || m.mode != modeConfirm || m.confirmID != 1 {
		t.Fatalf("d should only ask: %v mode %d", acts, m.mode)
	}
	if acts := press(m, runeKey('n')); acts != nil || m.mode != modeNormal {
		t.Fatalf("n should cancel: %v", acts)
	}
	press(m, runeKey('d'))
	if acts := press(m, runeKey('d')); acts != nil {
		t.Fatalf("a second lowercase d must not delete anything: %v", acts)
	}
	press(m, runeKey('d'))
	if acts := press(m, runeKey('y')); len(acts) != 1 || acts[0] != (action{kind: actRemove, id: 1, name: "torrent-01-downloading.iso"}) {
		t.Fatalf("y: %+v", acts)
	}
	press(m, runeKey('d'))
	if acts := press(m, runeKey('D')); len(acts) != 1 || !acts[0].deleteFiles {
		t.Fatalf("D should remove and delete files: %+v", acts)
	}
}

func TestAddPrompt(t *testing.T) {
	m := newModel(100, 20, false, true, t0)
	connected(m, nil)
	press(m, runeKey('a'))
	if m.mode != modeAdd {
		t.Fatal("a should open the prompt")
	}
	// Keys that are commands elsewhere are just text here.
	for _, r := range "~/x y.torrentq" {
		press(m, runeKey(r))
	}
	press(m, Key{Code: KeyBackspace})
	if string(m.input) != "~/x y.torrent" || m.quit {
		t.Fatalf("input %q quit %v", string(m.input), m.quit)
	}
	press(m, Key{Code: KeyCtrlW})
	if string(m.input) != "~/x " {
		t.Fatalf("ctrl-w: %q", string(m.input))
	}
	press(m, Key{Code: KeyCtrlU})
	for _, r := range "magnet:?xt=urn:btih:abc" {
		press(m, runeKey(r))
	}
	acts := press(m, kEnter)
	if len(acts) != 1 || acts[0] != (action{kind: actAdd, arg: "magnet:?xt=urn:btih:abc"}) || m.mode != modeNormal {
		t.Fatalf("enter: %+v mode %d", acts, m.mode)
	}
	press(m, runeKey('a'), runeKey('x'), kEsc)
	if m.mode != modeNormal || len(m.input) != 0 {
		t.Error("esc should cancel and clear")
	}
	press(m, runeKey('a'), runeKey(' '))
	if acts := press(m, kEnter); acts != nil {
		t.Errorf("blank add should do nothing: %v", acts)
	}
}

func TestNothingWhileDisconnected(t *testing.T) {
	m := newModel(100, 20, false, true, t0)
	connected(m, fakeTorrents(2))
	m.update(dataEvent{err: errors.New("talking to the daemon: connection refused")})
	if m.conn != connDown || !m.haveData || len(m.torrents) != 2 {
		t.Fatalf("lost connection should keep the last data: conn %d", m.conn)
	}
	if acts := press(m, runeKey('p')); acts != nil {
		t.Fatalf("pause while disconnected: %v", acts)
	}
	if msg, isErr := m.message(); !isErr || msg != "Not connected to the daemon" {
		t.Errorf("message %q", msg)
	}
	connected(m, fakeTorrents(2))
	if m.conn != connUp || m.connErr != "" {
		t.Error("reconnect not shown")
	}
}

func TestDetailsView(t *testing.T) {
	m := newModel(100, 30, false, true, t0)
	ts := fakeTorrents(3)
	connected(m, ts)
	press(m, kDown, kEnter)
	if m.view != viewDetails || m.detailID != 2 || m.detail.Name != ts[1].Name {
		t.Fatalf("enter: view %d id %d", m.view, m.detailID)
	}
	files := make([]engine.File, 40)
	for i := range files {
		files[i] = engine.File{Index: i, Path: fmt.Sprintf("dir/file%02d", i), Size: 100, Done: 50}
	}
	snap := snapshot{torrents: ts, detailID: 2, detail: ts[1], files: files}
	m.update(dataEvent{snap: snap})
	if len(m.files) != 40 {
		t.Fatal("files not taken")
	}
	press(m, runeKey('G'))
	if m.scroll != 40-m.detailRows() {
		t.Errorf("G: scroll %d, want %d", m.scroll, 40-m.detailRows())
	}
	press(m, Key{Code: KeyTab})
	if m.tab != tabPeers || m.scroll != 0 {
		t.Errorf("tab: %d scroll %d", m.tab, m.scroll)
	}
	if acts := press(m, runeKey('p')); len(acts) != 1 || acts[0].id != 2 {
		t.Errorf("p in details: %+v", acts)
	}
	// Data for another torrent (a stale refresh) is ignored.
	m.update(dataEvent{snap: snapshot{torrents: ts, detailID: 3, detail: ts[2]}})
	if m.detail.ID != 2 {
		t.Error("stale detail data replaced the shown torrent")
	}
	press(m, kEsc)
	if m.view != viewList || m.detailID != 0 {
		t.Error("esc should go back")
	}
	// The torrent disappears while its details are open.
	press(m, kEnter)
	m.update(dataEvent{snap: snapshot{torrents: ts, detailID: 2,
		detailErr: &ipc.Error{Code: ipc.CodeNotFound, Message: "no such torrent"}}})
	if m.view != viewList {
		t.Error("a removed torrent should close its details")
	}
}

func TestHelpAndQuit(t *testing.T) {
	m := newModel(100, 30, false, true, t0)
	press(m, runeKey('?'))
	if m.mode != modeHelp {
		t.Fatal("? should open help")
	}
	press(m, runeKey('x'))
	if m.mode != modeNormal || m.quit {
		t.Fatal("any key should close help")
	}
	press(m, runeKey('q'))
	if !m.quit {
		t.Fatal("q should quit")
	}
	m = newModel(100, 30, false, true, t0)
	press(m, runeKey('a'), Key{Code: KeyCtrlC})
	if !m.quit {
		t.Fatal("ctrl-c should quit even in the prompt")
	}
}

func TestLineModeFallback(t *testing.T) {
	m := newModel(100, 30, true, true, t0)
	connected(m, fakeTorrents(4))
	m.update(lineEvent{"jj"})
	if m.sel != 2 {
		t.Fatalf("jj: sel %d", m.sel)
	}
	m.update(lineEvent{""}) // Enter
	if m.view != viewDetails {
		t.Fatal("empty line should be Enter")
	}
	m.update(lineEvent{"\x1b"}) // what Esc types in line mode
	if m.view != viewList {
		t.Fatal("esc line should go back")
	}
	if acts := m.update(lineEvent{"a magnet:?xt=urn:btih:abc"}); len(acts) != 1 || acts[0].arg != "magnet:?xt=urn:btih:abc" {
		t.Fatalf("a + text on one line: %+v", acts)
	}
	m.update(lineEvent{"a"})
	if acts := m.update(lineEvent{"/tmp/x.torrent"}); len(acts) != 1 || acts[0].arg != "/tmp/x.torrent" {
		t.Fatalf("a, then the text on its own line: %+v", acts)
	}
	m.update(lineEvent{"qp"})
	if !m.quit {
		t.Fatal("q should quit")
	}
}
