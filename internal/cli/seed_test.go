package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/config"
)

func TestSeedingCommands(t *testing.T) {
	home, tmp := t.TempDir(), t.TempDir()
	h := startDaemonArgs(t, home, "-port", "0", "-dir", tmp, "-seed-time", "3d")
	if out := h.mustCmd("status"); !strings.Contains(out, "for 3d 0h") {
		t.Errorf("the daemon's -seed-time didn't reach the engine:\n%s", out)
	}
	path, data := writeTorrent(t, tmp, "song", 1)
	if err := os.WriteFile(filepath.Join(tmp, "song"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	h.mustCmd("add", "-existing", path)
	h.waitList("Seeding")

	if out := h.mustCmd("pause", "-uploads", "1"); !strings.Contains(out, "Uploads paused for #1") {
		t.Errorf("pause -uploads:\n%s", out)
	}
	if out := h.mustCmd("list"); !strings.Contains(out, "paused") || !strings.Contains(out, "RATIO") {
		t.Errorf("list with uploads paused:\n%s", out)
	}
	if out := h.mustCmd("info", "1"); !strings.Contains(out, "resume -uploads 1") || !strings.Contains(out, "until 3d 0h") {
		t.Errorf("info with uploads paused:\n%s", out)
	}
	h.mustCmd("resume", "-uploads", "all")

	if out := h.mustCmd("seed", "off", "1"); !strings.Contains(out, "seeding off") {
		t.Errorf("seed off:\n%s", out)
	}
	h.waitList("Finished")
	if out := h.mustCmd("info", "1"); !strings.Contains(out, "finished: seeding is off") {
		t.Errorf("info when finished:\n%s", out)
	}
	if code, _, errOut := h.cmd("resume", "1"); code != 1 || !strings.Contains(errOut, "denco seed on 1") {
		t.Errorf("resume a finished torrent: exit %d, %s", code, errOut)
	}
	if code, _, _ := h.cmd("seed", "maybe", "1"); code != 2 {
		t.Errorf("seed maybe: exit %d", code)
	}
	h.mustCmd("seed", "on", "1")
	h.waitList("Seeding")

	if out := h.mustCmd("config", "set", "seed_ratio", "3"); !strings.Contains(out, "Applied to the running daemon") {
		t.Errorf("config set seed_ratio:\n%s", out)
	}
	h.mustCmd("config", "set", "seed_time", "2d")
	if out := h.mustCmd("status"); !strings.Contains(out, "until ratio 3 or for 2d 0h") {
		t.Errorf("status after setting limits:\n%s", out)
	}
	if out := h.mustCmd("seed", "default", "1"); !strings.Contains(out, "until ratio 3 or 2d 0h") {
		t.Errorf("seed default:\n%s", out)
	}
}

func TestDaemonSeedFlags(t *testing.T) {
	var errOut bytes.Buffer
	e := &env{home: t.TempDir(), stdout: &errOut, stderr: &errOut}
	cfg, _, code := daemonFlags(e, config.Defaults(e.home), []string{"-seed-ratio", "1.5", "-seed-time", "1d12h"})
	if code != 0 || cfg.SeedRatio != 1.5 || cfg.SeedTime != 36*time.Hour {
		t.Fatalf("flags = %v %v, code %d: %s", cfg.SeedRatio, cfg.SeedTime, code, errOut.String())
	}
	if _, _, code := daemonFlags(e, config.Defaults(e.home), []string{"-seed-time", "10s"}); code != 2 {
		t.Errorf("-seed-time 10s: code %d", code)
	}
}
