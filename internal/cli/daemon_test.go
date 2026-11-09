package cli

import (
	"bytes"
	"context"
	"crypto/sha1"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// daemonHarness runs `denco daemon` in-process with a temporary home.
type daemonHarness struct {
	t    *testing.T
	home string
	out  *syncBuffer
	done chan int
	stop context.CancelFunc
}

func startDaemon(t *testing.T, home, dl string) *daemonHarness {
	t.Helper()
	return startDaemonArgs(t, home, "-port", "0", "-dir", dl)
}

// startDaemonArgs starts a daemon with exactly these flags.
func startDaemonArgs(t *testing.T, home string, args ...string) *daemonHarness {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h := &daemonHarness{t: t, home: home, out: &syncBuffer{}, done: make(chan int, 1), stop: cancel}
	e := &env{home: home, stdout: h.out, stderr: h.out}
	go func() { h.done <- runDaemon(ctx, e, args, nil) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-h.done:
		case <-time.After(15 * time.Second):
			t.Error("daemon did not exit")
		}
	})
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := ipc.Connect(ctx, filepath.Join(home, ipc.DaemonFile)); err == nil {
			return h
		}
		select {
		case code := <-h.done:
			t.Fatalf("daemon exited with %d:\n%s", code, h.out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon did not come up:\n%s", h.out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cmd runs one CLI command against the harness's home.
func (h *daemonHarness) cmd(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = runEnv(&env{home: h.home, stdout: &out, stderr: &errOut}, args)
	return code, out.String(), errOut.String()
}

// mustCmd runs a command that must succeed and returns its stdout.
func (h *daemonHarness) mustCmd(args ...string) string {
	h.t.Helper()
	code, out, errOut := h.cmd(args...)
	if code != 0 {
		h.t.Fatalf("denco %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), code, out, errOut)
	}
	return out
}

// waitList polls `denco list` until its output contains want.
func (h *daemonHarness) waitList(want string) string {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		out := h.mustCmd("list")
		if strings.Contains(out, want) {
			return out
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("list never contained %q:\n%s", want, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// writeTorrent writes a trackerless torrent (single file, or multi-file
// when files > 1) and returns its path and content.
func writeTorrent(t *testing.T, dir, name string, files int) (string, []byte) {
	t.Helper()
	data := []byte(strings.Repeat(name+"-data-", 3000))
	var hashes []byte
	const pl = 16384
	for off := 0; off < len(data); off += pl {
		h := sha1.Sum(data[off:min(off+pl, len(data))])
		hashes = append(hashes, h[:]...)
	}
	info := map[string]any{"name": name, "piece length": pl, "pieces": string(hashes)}
	if files <= 1 {
		info["length"] = len(data)
	} else {
		var list []any
		per := len(data) / files
		for i := 0; i < files; i++ {
			n := per
			if i == files-1 {
				n = len(data) - per*(files-1)
			}
			list = append(list, map[string]any{"length": n, "path": []any{"part" + string(rune('A'+i)) + ".bin"}})
		}
		info["files"] = list
	}
	raw, err := bencode.Encode(map[string]any{"info": info})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name+".torrent")
	if err := os.WriteFile(p, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	return p, data
}

func TestDaemonWorkflow(t *testing.T) {
	tmp := t.TempDir()
	home, dl := filepath.Join(tmp, "home"), filepath.Join(tmp, "downloads")

	// Without a daemon, client commands explain what to do.
	noDaemon := &daemonHarness{t: t, home: home}
	if code, _, errOut := noDaemon.cmd("list"); code != 1 || !strings.Contains(errOut, "denco daemon") {
		t.Fatalf("list without daemon: %d %q", code, errOut)
	}

	h := startDaemon(t, home, dl)
	if !strings.Contains(h.out.String(), "Denco daemon running") {
		t.Errorf("daemon banner:\n%s", h.out.String())
	}
	if code, _, errOut := h.cmd("daemon", "-port", "0", "-dir", dl); code != 1 || !strings.Contains(errOut, "already running") {
		t.Fatalf("second daemon: %d %q", code, errOut)
	}
	if code, _, errOut := h.cmd("download", filepath.Join(tmp, "x.torrent")); code != 1 || !strings.Contains(errOut, "daemon is running") {
		t.Fatalf("download while daemon runs: %d %q", code, errOut)
	}

	if out := h.mustCmd("list"); !strings.Contains(out, "No torrents") {
		t.Fatalf("empty list:\n%s", out)
	}

	// Add two torrents (and one missing file) in one go.
	one, _ := writeTorrent(t, tmp, "alpha", 1)
	two, _ := writeTorrent(t, tmp, "beta", 3)
	code, out, errOut := h.cmd("add", one, filepath.Join(tmp, "missing.torrent"), two)
	if code != 1 || !strings.Contains(errOut, "missing.torrent") {
		t.Fatalf("add with a missing file: exit %d, stderr %q", code, errOut)
	}
	for _, want := range []string{"alpha", "beta", "3 files", "partB.bin", "Added as #1", "Added as #2"} {
		if !strings.Contains(out, want) {
			t.Errorf("add output missing %q:\n%s", want, out)
		}
	}
	if code, out, _ := h.cmd("add", one); code != 1 || !strings.Contains(out, "already added as #1") {
		t.Fatalf("duplicate add: %d\n%s", code, out)
	}

	// A torrent whose data is already here seeds right away.
	three, data3 := writeTorrent(t, tmp, "gamma", 1)
	os.MkdirAll(dl, 0o755)
	os.WriteFile(filepath.Join(dl, "gamma"), data3, 0o644)
	if code, out, _ := h.cmd("add", three); code != 1 || !strings.Contains(out, "-existing") {
		t.Fatalf("add over existing data without -existing: %d\n%s", code, out)
	}
	h.mustCmd("add", "-existing", three)

	list := h.waitList("Seeding")
	for _, want := range []string{"Downloading (2)", "Completed (1)", "alpha", "beta", "gamma", "Stalled", "100%"} {
		if !strings.Contains(list, want) {
			t.Errorf("list missing %q:\n%s", want, list)
		}
	}
	if strings.Index(list, "alpha") > strings.Index(list, "gamma") {
		t.Errorf("completed torrents should be listed after incomplete ones:\n%s", list)
	}

	info := h.mustCmd("info", "1")
	for _, want := range []string{"Name:", "alpha", "Status:", "Stalled", "Pieces:", "0 / 3", "Connected Peers:"} {
		if !strings.Contains(info, want) {
			t.Errorf("info missing %q:\n%s", want, info)
		}
	}
	if files := h.mustCmd("files", "2"); !strings.Contains(files, "beta/partC.bin") || !strings.Contains(files, "3 files") {
		t.Errorf("files:\n%s", files)
	}
	if peers := h.mustCmd("peers", "1"); !strings.Contains(peers, "0 peers") {
		t.Errorf("peers:\n%s", peers)
	}
	if code, _, errOut := h.cmd("info", "99"); code != 1 || !strings.Contains(errOut, "no such torrent") {
		t.Errorf("info 99: %d %q", code, errOut)
	}

	if out := h.mustCmd("pause", "1"); !strings.Contains(out, "Paused #1 alpha") {
		t.Errorf("pause:\n%s", out)
	}
	h.waitList("Paused")
	if out := h.mustCmd("resume", "all"); !strings.Contains(out, "Resuming #1 alpha") || !strings.Contains(out, "#3 gamma") {
		t.Errorf("resume all:\n%s", out)
	}

	status := h.mustCmd("status")
	for _, want := range []string{"Torrents:", "3 (", "Completed:", "1 of 3", "Peer port:", "Port forwarding:", "off (port_forwarding)"} {
		if !strings.Contains(status, want) {
			t.Errorf("status missing %q:\n%s", want, status)
		}
	}

	if out := h.mustCmd("remove", "-delete", "3"); !strings.Contains(out, "files deleted") {
		t.Errorf("remove:\n%s", out)
	}
	if _, err := os.Stat(filepath.Join(dl, "gamma")); !errors.Is(err, os.ErrNotExist) {
		t.Error("remove -delete left the data behind")
	}
	// A torrent that never started owns no files: -delete must say so.
	four, _ := writeTorrent(t, tmp, "delta", 1)
	h.mustCmd("add", "-paused", four)
	if out := h.mustCmd("remove", "-delete", "4"); !strings.Contains(out, "nothing deleted") {
		t.Errorf("remove -delete of a never-started torrent:\n%s", out)
	}
	// Leave a gap in the IDs, so the restart check below can tell saved IDs
	// from torrents merely renumbered in order.
	h.mustCmd("remove", "1")

	if out := h.mustCmd("stop"); !strings.Contains(out, "stopped") {
		t.Errorf("stop:\n%s", out)
	}
	select {
	case code := <-h.done:
		h.done <- code // for the cleanup
		if code != 0 {
			t.Fatalf("daemon exit code %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("daemon still running after stop")
	}
	if _, err := os.Stat(filepath.Join(home, ipc.DaemonFile)); !errors.Is(err, os.ErrNotExist) {
		t.Error("daemon file left behind")
	}

	// Restart: exactly the remaining torrent comes back, with its saved ID
	// (#2, not renumbered to #1). Checked through the API, not table text.
	startDaemon(t, home, dl)
	c, err := ipc.Connect(context.Background(), filepath.Join(home, ipc.DaemonFile))
	if err != nil {
		t.Fatal(err)
	}
	torrents, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tr := range torrents {
		got = append(got, fmt.Sprintf("%d %s", tr.ID, tr.Name))
	}
	if want := []string{"2 beta"}; !slices.Equal(got, want) {
		t.Errorf("after restart: %q, want %q", got, want)
	}
}

func TestRenderList(t *testing.T) {
	var b bytes.Buffer
	renderList(&b, []engine.Torrent{
		{ID: 1, Name: "ubuntu.iso", State: engine.StateDownloading, HasMetadata: true, TotalBytes: 5_200_000_000, WantedBytes: 5_200_000_000, DoneBytes: 3_400_800_000,
			DownloadRate: 4_200_000, UploadRate: 120_000, ETA: 7*time.Minute + 42*time.Second, Peers: 18},
		{ID: 2, Name: "sample.zip", State: engine.StateSeeding, Complete: true, HasMetadata: true, TotalBytes: 850_000_000, WantedBytes: 850_000_000, DoneBytes: 850_000_000},
		{ID: 3, Name: strings.Repeat("long name ", 10), State: engine.StateError, Error: "disk full", HasMetadata: true, TotalBytes: 1, WantedBytes: 1},
		{ID: 4, Name: "from a magnet", State: engine.StateMetadata, Peers: 3},
	})
	out := b.String()
	for _, want := range []string{"Downloading (3)", "Completed (1)", "5.2 GB", "65.4%", "4.2 MB/s", "120.0 KB/s",
		"7m 42s", "18", "Seeding", "100%", "#3 failed: disk full", "…", "Fetching metadata"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestPeerStatus(t *testing.T) {
	cases := []struct {
		p    engine.Peer
		want string
	}{
		{engine.Peer{PeerChoking: true, AmChoking: true}, "idle"},
		{engine.Peer{Interested: true, PeerChoking: true, AmChoking: true}, "choked by peer"},
		{engine.Peer{Interested: true, PeerChoking: false, AmChoking: true}, "downloading"},
		{engine.Peer{Interested: true, PeerChoking: true, PeerWants: true, AmChoking: false}, "choked by peer, uploading"},
		{engine.Peer{PeerChoking: true, PeerWants: true, AmChoking: true}, "peer waiting"},
	}
	for _, c := range cases {
		if got := peerStatus(c.p); got != c.want {
			t.Errorf("peerStatus(%+v) = %q, want %q", c.p, got, c.want)
		}
	}
}

func TestRenderStatusCountsEveryState(t *testing.T) {
	var b bytes.Buffer
	st := ipc.Status{Overview: engine.Overview{Torrents: 3, StartedAt: time.Now(),
		ByState: map[engine.State]int{engine.StateMetadata: 1, engine.StateDownloading: 2}}}
	renderStatus(&b, st)
	if want := "3 (1 fetching metadata, 2 downloading)"; !strings.Contains(b.String(), want) {
		t.Errorf("missing %q in:\n%s", want, b.String())
	}
}
