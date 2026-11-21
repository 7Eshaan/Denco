package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// The model is the TUI's whole state. It is owned by the run loop's
// goroutine and changed only by update, which takes one event and returns
// the daemon operations to start. It does no I/O, so it is tested directly.

type view int

const (
	viewList view = iota
	viewDetails
)

// mode is what keys currently go to, on top of the view.
type mode int

const (
	modeNormal  mode = iota
	modeHelp         // the shortcut list; any key closes it
	modeAdd          // typing a torrent to add
	modeConfirm      // confirming a removal
)

type tab int

const (
	tabFiles tab = iota
	tabPeers
)

type connState int

const (
	connConnecting connState = iota
	connUp
	connDown
)

// snapshot is one refresh's worth of daemon data.
type snapshot struct {
	status   ipc.Status
	torrents []engine.Torrent
	// The details view's torrent (detailID 0: none was asked for).
	detailID  int
	detail    engine.Torrent
	files     []engine.File
	peers     []engine.Peer
	detailErr error
}

// Events the run loop feeds to update.
type (
	keyEvent  struct{ k Key }
	lineEvent struct{ text string } // line-mode fallback: one line typed
	dataEvent struct {
		snap snapshot
		err  error // the daemon couldn't be reached
	}
	resultEvent struct { // a daemon operation finished
		text string
		err  error
	}
	resizeEvent struct{ w, h int }
	tickEvent   struct{ now time.Time }
)

type actionKind int

const (
	actPause actionKind = iota
	actResume
	actRemove
	actAdd
	actForce   // force start (D58)
	actUnforce // back under the queue's rules (a normal resume)
	actMove    // move in the download queue
)

// action is a daemon operation for the run loop to start.
type action struct {
	kind        actionKind
	id          int
	name        string
	deleteFiles bool
	arg         string // actAdd: file path, magnet link or URL
	move        string // actMove: "top", "up", "down" or "bottom"
}

const (
	flashFor    = 6 * time.Second
	flashErrFor = 12 * time.Second
)

type model struct {
	w, h     int
	lineMode bool
	color    bool
	now      time.Time

	conn     connState
	connErr  string
	status   ipc.Status
	haveData bool
	torrents []engine.Torrent
	sel      int // selected row in torrents
	selID    int // its torrent ID, so the selection follows it when the list changes
	offset   int // first row shown

	view      view
	mode      mode
	detailID  int
	detail    engine.Torrent
	files     []engine.File
	peers     []engine.Peer
	detailErr string
	tab       tab
	scroll    int // first file/peer shown in the details view

	input       []rune // modeAdd
	confirmID   int    // modeConfirm
	confirmName string

	flash      string
	flashErr   bool
	flashUntil time.Time

	pending int // operations in flight
	quit    bool
}

func newModel(w, h int, lineMode, color bool, now time.Time) *model {
	return &model{w: w, h: h, lineMode: lineMode, color: color, now: now}
}

// update applies one event and returns the operations to start.
func (m *model) update(ev any) []action {
	switch e := ev.(type) {
	case keyEvent:
		return m.key(e.k)
	case lineEvent:
		return m.line(e.text)
	case dataEvent:
		m.data(e)
	case resultEvent:
		m.pending = max(0, m.pending-1)
		if e.err != nil {
			m.setFlash(clean(e.err.Error()), true)
		} else {
			m.setFlash(e.text, false)
		}
	case resizeEvent:
		m.w, m.h = e.w, e.h
		m.keepVisible()
		m.clampScroll()
	case tickEvent:
		m.now = e.now
	}
	return nil
}

func (m *model) setFlash(text string, isErr bool) {
	d := flashFor
	if isErr {
		d = flashErrFor
	}
	m.flash, m.flashErr, m.flashUntil = text, isErr, m.now.Add(d)
}

// message is the flash text still showing, if any.
func (m *model) message() (string, bool) {
	if m.flash == "" || m.now.After(m.flashUntil) {
		return "", false
	}
	return m.flash, m.flashErr
}

// data takes in a refresh.
func (m *model) data(e dataEvent) {
	if e.err != nil {
		m.conn, m.connErr = connDown, clean(e.err.Error())
		return
	}
	m.conn, m.connErr = connUp, ""
	m.status, m.torrents, m.haveData = e.snap.status, e.snap.torrents, true
	m.reselect()
	if m.view == viewDetails && e.snap.detailID == m.detailID && e.snap.detailID != 0 {
		var apiErr *ipc.Error
		switch {
		case errors.As(e.snap.detailErr, &apiErr) && apiErr.Code == ipc.CodeNotFound:
			m.view, m.mode = viewList, modeNormal
			m.setFlash(fmt.Sprintf("#%d is no longer there", m.detailID), false)
			m.detailID = 0
		case e.snap.detailErr != nil:
			m.detailErr = clean(e.snap.detailErr.Error())
		default:
			m.detail, m.files, m.peers, m.detailErr = e.snap.detail, e.snap.files, e.snap.peers, ""
			m.clampScroll()
		}
	}
}

// reselect keeps the selection on the same torrent when the list changes.
func (m *model) reselect() {
	for i, t := range m.torrents {
		if t.ID == m.selID {
			m.sel = i
			m.keepVisible()
			return
		}
	}
	m.sel = min(m.sel, len(m.torrents)-1)
	m.sel = max(m.sel, 0)
	m.selID = 0
	if m.sel < len(m.torrents) {
		m.selID = m.torrents[m.sel].ID
	}
	m.keepVisible()
}

func (m *model) selected() (engine.Torrent, bool) {
	if m.sel < 0 || m.sel >= len(m.torrents) {
		return engine.Torrent{}, false
	}
	return m.torrents[m.sel], true
}

func (m *model) moveTo(i int) {
	if len(m.torrents) == 0 {
		return
	}
	m.sel = min(max(i, 0), len(m.torrents)-1)
	m.selID = m.torrents[m.sel].ID
	m.keepVisible()
}

// keepVisible scrolls the list so the selected row is on screen.
func (m *model) keepVisible() {
	rows := m.listRows()
	if m.sel < m.offset {
		m.offset = m.sel
	}
	if m.sel >= m.offset+rows {
		m.offset = m.sel - rows + 1
	}
	m.offset = max(0, min(m.offset, len(m.torrents)-rows))
}

// --- keys ------------------------------------------------------------------------------

func (m *model) key(k Key) []action {
	if k.Code == KeyCtrlC {
		m.quit = true
		return nil
	}
	switch m.mode {
	case modeAdd:
		return m.addKey(k)
	case modeConfirm:
		return m.confirmKey(k)
	case modeHelp:
		if isRune(k, 'q') {
			m.quit = true
		}
		m.mode = modeNormal
		return nil
	}
	switch {
	case isRune(k, 'q'):
		m.quit = true
		return nil
	case isRune(k, '?'):
		m.mode = modeHelp
		return nil
	case isRune(k, 'a'):
		m.mode, m.input = modeAdd, nil
		return nil
	}
	if m.view == viewDetails {
		return m.detailsKey(k)
	}
	return m.listKey(k)
}

func isRune(k Key, r rune) bool { return k.Code == KeyRune && k.Rune == r }

func (m *model) listKey(k Key) []action {
	page := m.listRows()
	switch {
	case k.Code == KeyUp || isRune(k, 'k'):
		m.moveTo(m.sel - 1)
	case k.Code == KeyDown || isRune(k, 'j'):
		m.moveTo(m.sel + 1)
	case k.Code == KeyPgUp:
		m.moveTo(m.sel - page)
	case k.Code == KeyPgDn:
		m.moveTo(m.sel + page)
	case k.Code == KeyHome || isRune(k, 'g'):
		m.moveTo(0)
	case k.Code == KeyEnd || isRune(k, 'G'):
		m.moveTo(len(m.torrents) - 1)
	case k.Code == KeyEnter:
		if t, ok := m.selected(); ok {
			m.view, m.detailID, m.detail = viewDetails, t.ID, t
			m.files, m.peers, m.detailErr, m.tab, m.scroll = nil, nil, "", tabFiles, 0
		}
	case isTorrentKey(k):
		t, ok := m.selected()
		if !ok {
			m.setFlash("No torrent selected", true)
			return nil
		}
		return m.torrentKey(k.Rune, t)
	}
	return nil
}

func (m *model) detailsKey(k Key) []action {
	page := max(1, m.detailRows()-1)
	switch {
	case k.Code == KeyEsc || k.Code == KeyBackspace || k.Code == KeyLeft || isRune(k, 'h'):
		m.view, m.detailID = viewList, 0
	case k.Code == KeyTab || k.Code == KeyBackTab || k.Code == KeyRight || isRune(k, 'l'):
		m.tab, m.scroll = 1-m.tab, 0
	case k.Code == KeyUp || isRune(k, 'k'):
		m.scroll--
	case k.Code == KeyDown || isRune(k, 'j'):
		m.scroll++
	case k.Code == KeyPgUp:
		m.scroll -= page
	case k.Code == KeyPgDn:
		m.scroll += page
	case k.Code == KeyHome || isRune(k, 'g'):
		m.scroll = 0
	case k.Code == KeyEnd || isRune(k, 'G'):
		m.scroll = m.tabLen()
	case isTorrentKey(k):
		return m.torrentKey(k.Rune, m.detail)
	}
	m.clampScroll()
	return nil
}

func (m *model) tabLen() int {
	if m.tab == tabPeers {
		return len(m.peers)
	}
	return len(m.files)
}

func (m *model) clampScroll() {
	m.scroll = max(0, min(m.scroll, m.tabLen()-m.detailRows()))
}

// queueMoves are the keys that move a torrent in the download queue:
// Shift plus the movement keys (K up, J down), T top and B bottom.
var queueMoves = map[rune]string{'K': "up", 'J': "down", 'T': "top", 'B': "bottom"}

// isTorrentKey: a key that acts on the selected (or shown) torrent.
func isTorrentKey(k Key) bool {
	if k.Code != KeyRune {
		return false
	}
	_, move := queueMoves[k.Rune]
	return move || k.Rune == 'p' || k.Rune == 'r' || k.Rune == 'd' || k.Rune == 'f'
}

// torrentKey handles p, r, d, f and the queue moves for torrent t.
func (m *model) torrentKey(r rune, t engine.Torrent) []action {
	if m.conn != connUp {
		m.setFlash("Not connected to the daemon", true)
		return nil
	}
	name := clean(t.Name)
	if move, ok := queueMoves[r]; ok {
		if t.Complete {
			m.setFlash(fmt.Sprintf("#%d is complete: the queue only orders downloads", t.ID), true)
			return nil
		}
		return m.start(action{kind: actMove, id: t.ID, name: name, move: move}, "Moving "+move)
	}
	switch r {
	case 'p':
		return m.start(action{kind: actPause, id: t.ID, name: name}, "Pausing")
	case 'r':
		return m.start(action{kind: actResume, id: t.ID, name: name}, "Resuming")
	case 'f':
		if t.Forced {
			return m.start(action{kind: actUnforce, id: t.ID, name: name}, "Queuing again")
		}
		return m.start(action{kind: actForce, id: t.ID, name: name}, "Force-starting")
	default: // 'd'
		m.mode, m.confirmID, m.confirmName = modeConfirm, t.ID, name
		return nil
	}
}

func (m *model) start(a action, verb string) []action {
	m.pending++
	if a.kind == actAdd {
		m.setFlash(verb+"…", false)
	} else {
		m.setFlash(fmt.Sprintf("%s #%d %s…", verb, a.id, a.name), false)
	}
	return []action{a}
}

func (m *model) confirmKey(k Key) []action {
	m.mode = modeNormal
	a := action{kind: actRemove, id: m.confirmID, name: m.confirmName}
	switch {
	case isRune(k, 'y'), isRune(k, 'Y'):
		return m.start(a, "Removing")
	case isRune(k, 'D'):
		a.deleteFiles = true
		return m.start(a, "Removing (and deleting the files of)")
	}
	m.setFlash("Not removed", false)
	return nil
}

func (m *model) addKey(k Key) []action {
	switch k.Code {
	case KeyEsc:
		m.mode, m.input = modeNormal, nil
	case KeyEnter:
		return m.submitAdd()
	case KeyBackspace:
		if len(m.input) > 0 {
			m.input = m.input[:len(m.input)-1]
		}
	case KeyCtrlU:
		m.input = nil
	case KeyCtrlW: // delete the last word
		s := strings.TrimRight(string(m.input), " ")
		if i := strings.LastIndexByte(s, ' '); i >= 0 {
			m.input = []rune(s[:i+1])
		} else {
			m.input = nil
		}
	case KeyTab:
		m.input = append(m.input, ' ')
	case KeyRune:
		m.input = append(m.input, k.Rune)
	}
	return nil
}

func (m *model) submitAdd() []action {
	arg := strings.TrimSpace(string(m.input))
	m.mode, m.input = modeNormal, nil
	if arg == "" {
		return nil
	}
	if m.conn != connUp {
		m.setFlash("Not connected to the daemon", true)
		return nil
	}
	return m.start(action{kind: actAdd, arg: arg}, "Adding")
}

// line handles a line typed in the line-mode fallback, where keys only
// arrive after Enter: an empty line is Enter, the add prompt takes the
// whole line, and anything else is a sequence of keys ("jjp").
func (m *model) line(text string) []action {
	if m.mode == modeAdd {
		m.input = []rune(text)
		return m.submitAdd()
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "":
		return m.key(Key{Code: KeyEnter})
	case "esc", "back":
		return m.key(Key{Code: KeyEsc})
	case "tab":
		return m.key(Key{Code: KeyTab})
	}
	var acts []action
	for i, r := range text {
		k := runeKey(r)
		switch r {
		case 0x1b: // Esc typed into the line
			k = Key{Code: KeyEsc}
		case '\t':
			k = Key{Code: KeyTab}
		}
		acts = append(acts, m.key(k)...)
		if m.mode == modeAdd { // "a magnet:?…": the rest of the line is what to add
			if rest := strings.TrimSpace(text[i+1:]); rest != "" {
				m.input = []rune(rest)
				acts = append(acts, m.submitAdd()...)
			}
			break
		}
		if m.quit {
			break
		}
	}
	return acts
}
