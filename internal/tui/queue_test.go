package tui

import (
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/engine"
)

// queuedTorrents: #1 downloading at position 1, #2 complete (seeding),
// #3 forced at position 2.
func queuedTorrents() []engine.Torrent {
	ts := fakeTorrents(3)
	ts[0].Queue, ts[0].QueueLength = 1, 2
	ts[2].State, ts[2].Forced, ts[2].Queue, ts[2].QueueLength = engine.StateDownloading, true, 2, 2
	return ts
}

func TestQueueKeys(t *testing.T) {
	m := newModel(120, 24, false, true, t0)
	connected(m, queuedTorrents())

	for _, tc := range []struct {
		key  rune
		move string
	}{{'K', "up"}, {'J', "down"}, {'T', "top"}, {'B', "bottom"}} {
		acts := press(m, runeKey(tc.key))
		if len(acts) != 1 || acts[0].kind != actMove || acts[0].move != tc.move || acts[0].id != 1 {
			t.Errorf("%c: actions %+v, want move %s of #1", tc.key, acts, tc.move)
		}
	}
	if acts := press(m, runeKey('f')); len(acts) != 1 || acts[0].kind != actForce {
		t.Errorf("f on a queued torrent: %+v, want force", acts)
	}

	// A complete torrent isn't queued: refused here, without a daemon call.
	press(m, kDown)
	if acts := press(m, runeKey('K')); len(acts) != 0 {
		t.Errorf("K on a complete torrent: %+v", acts)
	}
	if text, isErr := m.message(); !isErr || !strings.Contains(text, "complete") {
		t.Errorf("message = %q (err %v)", text, isErr)
	}

	// f on a forced torrent puts it back in the queue.
	press(m, kDown)
	if acts := press(m, runeKey('f')); len(acts) != 1 || acts[0].kind != actUnforce || acts[0].id != 3 {
		t.Errorf("f on a forced torrent: %+v, want unforce #3", acts)
	}

	// The same keys work in the details view, on the torrent shown.
	press(m, kEnter)
	if acts := press(m, runeKey('T')); len(acts) != 1 || acts[0].kind != actMove || acts[0].id != 3 {
		t.Errorf("T in details: %+v", acts)
	}
}

func TestQueueShownInListDetailsAndHelp(t *testing.T) {
	m := newModel(160, 30, false, true, t0)
	connected(m, queuedTorrents())
	got := screenText(m)
	if !strings.Contains(got, " Q ") && !strings.Contains(got, "  Q  ") {
		t.Errorf("no Q column:\n%s", got)
	}
	var row1, row3 string
	for _, l := range plainScreen(m) {
		switch {
		case strings.Contains(l, "torrent-01"):
			row1 = l
		case strings.Contains(l, "torrent-03"):
			row3 = l
		}
	}
	if !strings.Contains(row1, "  1  torrent-01") || !strings.Contains(row3, "  F  torrent-03") {
		t.Errorf("queue cells wrong:\n%s\n%s", row1, row3)
	}

	press(m, kEnter)
	if got := screenText(m); !strings.Contains(got, "1 of 2") || !strings.Contains(got, "K J T B") {
		t.Errorf("details lack the queue line:\n%s", got)
	}
	press(m, kEsc, runeKey('?'))
	got = screenText(m)
	for _, want := range []string{"K J  T B", "force start"} {
		if !strings.Contains(got, want) {
			t.Errorf("help missing %q:\n%s", want, got)
		}
	}
	// On a 24-row terminal the whole help box fits, quit line included.
	m.update(resizeEvent{80, 24})
	if got := screenText(m); !strings.Contains(got, "daemon keeps running") || !strings.Contains(got, "press any key") {
		t.Errorf("help cut off at 80×24:\n%s", got)
	}
}
