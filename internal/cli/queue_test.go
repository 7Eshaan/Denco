package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestQueueCommands(t *testing.T) {
	tmp := t.TempDir()
	home, dl := filepath.Join(tmp, "home"), filepath.Join(tmp, "downloads")
	h := startDaemonArgs(t, home, "-port", "0", "-dir", dl, "-max-active", "1")
	var paths []string
	for _, name := range []string{"qa", "qb", "qc", "qd"} {
		p, _ := writeTorrent(t, tmp, name, 1)
		paths = append(paths, p)
	}
	h.mustCmd(append([]string{"add"}, paths...)...)
	h.waitList("Queued")

	// Several at once keep the order they were named in.
	out := h.mustCmd("queue", "top", "4", "3")
	if !strings.Contains(out, "#4 qd: queue position 1 of 4") || !strings.Contains(out, "#3 qc: queue position 2 of 4") {
		t.Fatalf("queue top 4 3:\n%s", out)
	}
	list := h.mustCmd("list")
	if !(strings.Index(list, "qd") < strings.Index(list, "qc") && strings.Index(list, "qc") < strings.Index(list, "qa")) {
		t.Fatalf("list isn't in queue order:\n%s", list)
	}
	if !strings.Contains(list, "ID  Q") {
		t.Fatalf("list has no Q column:\n%s", list)
	}
	h.mustCmd("queue", "down", "4", "3") // both one step down, order kept
	if info := h.mustCmd("info", "4"); !strings.Contains(info, "Queue:") || !strings.Contains(info, "2 of 4") {
		t.Fatalf("info after down:\n%s", info)
	}
	if info := h.mustCmd("info", "3"); !strings.Contains(info, "3 of 4") {
		t.Fatalf("#3 after down:\n%s", info)
	}

	// Force start past max_active_downloads.
	out = h.mustCmd("resume", "-force", "2")
	if !strings.Contains(out, "Force-started #2") {
		t.Fatalf("resume -force:\n%s", out)
	}
	if info := h.mustCmd("info", "2"); !strings.Contains(info, "force-started") {
		t.Fatalf("info of a forced torrent:\n%s", info)
	}

	for _, bad := range [][]string{
		{"queue", "sideways", "1"}, {"queue", "top"}, {"queue", "top", "all"}, {"resume", "-force", "-uploads", "1"},
	} {
		if code, _, _ := h.cmd(bad...); code != 2 {
			t.Errorf("denco %v: exit %d, want 2 (usage)", bad, code)
		}
	}
	if code, _, errOut := h.cmd("queue", "top", "9"); code != 1 || !strings.Contains(errOut, "no such torrent") {
		t.Errorf("unknown torrent: %d %q", code, errOut)
	}
}
