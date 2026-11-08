package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestConfigCommandWithoutDaemon(t *testing.T) {
	home := t.TempDir()
	h := &daemonHarness{t: t, home: home}

	out := h.mustCmd("config")
	for _, want := range []string{"SETTING", "download_dir", "max_active_downloads", "default", "0 (unlimited)", "No config file yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("config list missing %q:\n%s", want, out)
		}
	}
	if out := h.mustCmd("config", "set", "download_limit", "2", "MB/s"); !strings.Contains(out, "download_limit = 2.0 MB/s") {
		t.Errorf("set:\n%s", out)
	}
	if got := strings.TrimSpace(h.mustCmd("config", "get", "download_limit")); got != "2.0 MB/s" {
		t.Errorf("get = %q", got)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.yaml"))
	if err != nil || !strings.Contains(string(data), "download_limit: 2 MB/s") {
		t.Fatalf("file: %q, %v", data, err)
	}
	out = h.mustCmd("config")
	if !strings.Contains(out, "File: ") || !strings.Contains(lineOf(out, "download_limit"), "file") {
		t.Errorf("after set:\n%s", out)
	}
	if out := h.mustCmd("config", "-v"); !strings.Contains(out, "total download speed limit") {
		t.Errorf("config -v lacks descriptions:\n%s", out)
	}

	for _, bad := range [][]string{
		{"config", "set", "listen_port", "lots"},
		{"config", "set", "colour", "blue"},
		{"config", "get", "colour"},
		{"config", "init"}, // file exists now
	} {
		if code, _, errOut := h.cmd(bad...); code != 1 || errOut == "" {
			t.Errorf("%v: exit %d, stderr %q", bad, code, errOut)
		}
	}
	if code, _, _ := h.cmd("config", "bogus"); code != 2 {
		t.Errorf("unknown subcommand exit %d", code)
	}

	if out := h.mustCmd("config", "unset", "download_limit"); !strings.Contains(out, "download_limit = 0 (unlimited) (default)") {
		t.Errorf("unset:\n%s", out)
	}
	if got := strings.TrimSpace(h.mustCmd("config", "path")); got != filepath.Join(home, "config.yaml") {
		t.Errorf("path = %q", got)
	}

	fresh := &daemonHarness{t: t, home: t.TempDir()}
	if out := fresh.mustCmd("config", "init"); !strings.Contains(out, "Wrote") {
		t.Errorf("init:\n%s", out)
	}
	if out := fresh.mustCmd("config"); strings.Contains(out, " default ") {
		t.Errorf("after init every key is in the file:\n%s", out)
	}
}

func lineOf(text, prefix string) string {
	for _, l := range strings.Split(text, "\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	return ""
}

func TestDaemonUsesConfigAndFlagsOverride(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	dl := filepath.Join(tmp, "from-config")
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte(strings.Join([]string{
		"download_dir: " + dl,
		"max_active_downloads: 2",
		"max_connections_per_torrent: 30",
		"upload_limit: 300K",
		"download_limit: 5M   # overridden by the flag below",
		"enable_pex: true",
		"listen_port: 0",
	}, "\n")), 0o644)

	h := startDaemonArgs(t, home, "-down-limit", "1M", "-max-active", "3")
	banner := h.out.String()
	for _, want := range []string{dl, "↓ 1.0 MB/s", "↑ 300.0 KB/s", "enable_pex is on", "enable_dht is on"} {
		if !strings.Contains(banner, want) {
			t.Errorf("banner missing %q:\n%s", want, banner)
		}
	}
	status := h.mustCmd("status")
	for _, want := range []string{"Download dir:", dl, "Max downloading:", "3", "30 per torrent", "↓ 1.0 MB/s  ↑ 300.0 KB/s"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}

	// Speed limits apply at once; the flag-set download limit stays.
	if out := h.mustCmd("config", "set", "upload_limit", "2M"); !strings.Contains(out, "Applied to the running daemon") {
		t.Errorf("live set:\n%s", out)
	}
	if status := h.mustCmd("status"); !strings.Contains(status, "↓ 1.0 MB/s  ↑ 2.0 MB/s") {
		t.Errorf("limits after live set:\n%s", status)
	}
	// Other settings wait for a restart.
	if out := h.mustCmd("config", "set", "max_active_downloads", "4"); !strings.Contains(out, "Restart the daemon") {
		t.Errorf("non-live set:\n%s", out)
	}

	// Torrents without -dir go to the configured directory.
	p, _ := writeTorrent(t, tmp, "cfgdir", 1)
	h.mustCmd("add", p)
	h.waitList("cfgdir")
	if info := h.mustCmd("info", "1"); !strings.Contains(info, dl) {
		t.Errorf("torrent not in config dir:\n%s", info)
	}
}

func TestDaemonRejectsBadConfigAndFlags(t *testing.T) {
	home := t.TempDir()
	h := &daemonHarness{t: t, home: home}
	if code, _, errOut := h.cmd("daemon", "-port", "99999"); code != 2 || !strings.Contains(errOut, "out of range") {
		t.Errorf("bad flag: %d %q", code, errOut)
	}
	if code, _, errOut := h.cmd("daemon", "-down-limit", "fast"); code != 2 || !strings.Contains(errOut, "not a speed") {
		t.Errorf("bad limit flag: %d %q", code, errOut)
	}
	os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen_port: 6881\nmax_actve_downloads: 3\n"), 0o644)
	code, _, errOut := h.cmd("daemon")
	if code != 1 || !strings.Contains(errOut, "line 2") || !strings.Contains(errOut, "max_actve_downloads") {
		t.Errorf("bad config: %d %q", code, errOut)
	}
	if code, _, errOut := h.cmd("download", "x.torrent"); code != 1 || !strings.Contains(errOut, "line 2") {
		t.Errorf("download with bad config: %d %q", code, errOut)
	}
}

func TestResumeDisabledForgetsTorrents(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	resumeDir := filepath.Join(tmp, "resume-here")
	os.MkdirAll(home, 0o755)
	os.WriteFile(filepath.Join(home, "config.yaml"),
		[]byte("resume_enabled: false\nresume_dir: "+resumeDir+"\nlisten_port: 0\n"), 0o644)

	h := startDaemonArgs(t, home, "-dir", filepath.Join(tmp, "dl"))
	if !strings.Contains(h.out.String(), "resume_enabled is off") {
		t.Errorf("banner:\n%s", h.out.String())
	}
	p, _ := writeTorrent(t, tmp, "temp", 1)
	h.mustCmd("add", "-paused", p)
	h.mustCmd("stop")
	waitExit(t, h)

	h2 := startDaemonArgs(t, home, "-dir", filepath.Join(tmp, "dl"))
	if out := h2.mustCmd("list"); !strings.Contains(out, "No torrents") {
		t.Errorf("torrent survived with resume disabled:\n%s", out)
	}
	if _, err := os.Stat(resumeDir); !errors.Is(err, os.ErrNotExist) {
		t.Error("resume_dir was written although resume is disabled")
	}
}

func waitExit(t *testing.T, h *daemonHarness) {
	t.Helper()
	select {
	case code := <-h.done:
		h.done <- code // for the cleanup
	case <-time.After(10 * time.Second):
		t.Fatal("daemon did not exit")
	}
}
