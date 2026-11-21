package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLimitCommand(t *testing.T) {
	tmp := t.TempDir()
	home, dl := filepath.Join(tmp, "home"), filepath.Join(tmp, "downloads")
	h := startDaemon(t, home, dl)
	p, _ := writeTorrent(t, tmp, "lim", 1)
	h.mustCmd("add", "-paused", p)

	if out := h.mustCmd("limit", "1"); !strings.Contains(out, "#1 lim: own limits ↓ none  ↑ none") {
		t.Fatalf("show without limits:\n%s", out)
	}
	if out := h.mustCmd("limit", "-down", "500K", "1"); !strings.Contains(out, "↓ 500.0 KB/s  ↑ none") {
		t.Fatalf("set down:\n%s", out)
	}
	// Only -up given: the download limit stays.
	if out := h.mustCmd("limit", "-up", "2M", "1"); !strings.Contains(out, "↓ 500.0 KB/s  ↑ 2.0 MB/s") {
		t.Fatalf("set up:\n%s", out)
	}
	if info := h.mustCmd("info", "1"); !strings.Contains(info, "Speed limits:") || !strings.Contains(info, "↓ 500.0 KB/s") {
		t.Fatalf("info:\n%s", info)
	}
	if st := h.mustCmd("status"); !strings.Contains(st, "↓ unlimited") {
		t.Fatalf("a torrent's limit changed the totals:\n%s", st)
	}
	h.mustCmd("limit", "-down", "0", "-up", "0", "all")
	if info := h.mustCmd("info", "1"); strings.Contains(info, "Speed limits:") {
		t.Fatalf("limits not lifted:\n%s", info)
	}

	for _, bad := range [][]string{{"limit"}, {"limit", "-down", "fast", "1"}, {"limit", "-up", "10B", "1"}} {
		if code, _, _ := h.cmd(bad...); code != 2 {
			t.Errorf("denco %v: exit %d, want 2", bad, code)
		}
	}
	if code, _, errOut := h.cmd("limit", "-down", "1M", "9"); code != 1 || !strings.Contains(errOut, "no such torrent") {
		t.Errorf("unknown torrent: %d %q", code, errOut)
	}
}
