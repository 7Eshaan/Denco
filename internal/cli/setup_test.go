package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakePath records what setup does to PATH.
type fakePath struct {
	has            bool
	added, removed []string
}

func (f *fakePath) Has(string) (bool, error) { return f.has, nil }
func (f *fakePath) Add(dir string) (string, error) {
	f.added, f.has = append(f.added, dir), true
	return "a test PATH", nil
}
func (f *fakePath) Remove(dir string) (bool, error) {
	f.removed = append(f.removed, dir)
	was := f.has
	f.has = false
	return was, nil
}
func (f *fakePath) Reload() string { return "" }

// testInstaller installs a fake executable from a temp "Downloads" into a
// temp bin folder.
func testInstaller(t *testing.T, goos string) (*installer, *fakePath, *bytes.Buffer) {
	t.Helper()
	tmp := t.TempDir()
	exe := filepath.Join(tmp, "Downloads", "denco")
	os.MkdirAll(filepath.Dir(exe), 0o755)
	os.WriteFile(exe, []byte("new version"), 0o644)
	fp := &fakePath{}
	var out bytes.Buffer
	in := &installer{
		goos: goos, exe: exe, dir: filepath.Join(tmp, "bin"), name: "denco", path: fp, out: &out,
		lookPath:   func(string) (string, error) { return "", errors.New("not found") },
		stopDaemon: func() (bool, error) { return false, nil },
	}
	return in, fp, &out
}

func TestSetupInstallsAndUpdates(t *testing.T) {
	in, fp, out := testInstaller(t, runtime.GOOS)
	if in.Installed() {
		t.Fatal("installed before setup")
	}
	if err := in.install(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(in.dest())
	if err != nil || string(b) != "new version" {
		t.Fatalf("installed copy: %q, %v", b, err)
	}
	if fi, _ := os.Stat(in.dest()); runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		t.Errorf("not executable: %v", fi.Mode())
	}
	if len(fp.added) != 1 || fp.added[0] != in.dir || !strings.Contains(out.String(), "Open a new terminal") {
		t.Fatalf("PATH: %v; output:\n%s", fp.added, out)
	}
	if entries, _ := os.ReadDir(in.dir); len(entries) != 1 {
		t.Errorf("left behind: %v", entries) // no temp files
	}

	// Again, from the installed copy: nothing to do.
	out.Reset()
	in.exe = in.dest()
	if err := in.install(); err != nil || !strings.Contains(out.String(), "is installed at") || len(fp.added) != 1 {
		t.Fatalf("%v; added %v; output:\n%s", err, fp.added, out)
	}

	// A newer version replaces it, stopping a running daemon first.
	os.WriteFile(filepath.Join(filepath.Dir(in.dir), "Downloads", "denco"), []byte("newer"), 0o644)
	in.exe = filepath.Join(filepath.Dir(in.dir), "Downloads", "denco")
	stopped := false
	in.stopDaemon = func() (bool, error) { stopped = true; return true, nil }
	out.Reset()
	if err := in.install(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(in.dest()); string(b) != "newer" || !stopped || !strings.Contains(out.String(), "daemon was stopped") {
		t.Fatalf("update: %q, stopped %v; output:\n%s", b, stopped, out)
	}
}

// On Windows the old copy is moved aside first (a running program can't
// be overwritten, only renamed) and removed when possible.
func TestSetupWindowsMovesOldCopyAside(t *testing.T) {
	in, _, _ := testInstaller(t, "windows")
	os.MkdirAll(in.dir, 0o755)
	os.WriteFile(in.dest(), []byte("old"), 0o644)
	os.WriteFile(in.dest()+".old", []byte("older"), 0o644) // from an earlier update
	if err := in.install(); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(in.dest()); string(b) != "new version" {
		t.Fatalf("installed %q", b)
	}
	if _, err := os.Stat(in.dest() + ".old"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".old left behind: %v", err)
	}
	if !in.samePath(`C:\Users\Me\AppData\Local\Programs\Denco\denco.exe`, `c:\users\me\appdata\local\programs\denco\DENCO.EXE`) {
		t.Error("Windows paths compared case-sensitively")
	}
}

func TestSetupWarnsAboutAnotherDenco(t *testing.T) {
	in, _, out := testInstaller(t, runtime.GOOS)
	other := filepath.Join(t.TempDir(), "denco")
	in.lookPath = func(string) (string, error) { return other, nil }
	if err := in.install(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "another denco is on your PATH at "+other) {
		t.Fatalf("no warning:\n%s", out)
	}
}

func TestSetupRemove(t *testing.T) {
	in, fp, out := testInstaller(t, runtime.GOOS)
	in.install()
	out.Reset()
	home := t.TempDir()
	if err := in.remove(home); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(in.dest()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("still installed: %v", err)
	}
	if len(fp.removed) != 1 || !strings.Contains(out.String(), "are kept") {
		t.Fatalf("PATH removals %v; output:\n%s", fp.removed, out)
	}
	// Removing what isn't there just says so.
	out.Reset()
	if err := in.remove(home); err != nil || !strings.Contains(out.String(), "isn't installed") {
		t.Fatalf("%v:\n%s", err, out)
	}

	// The installed copy removing itself (Windows): deleted after exit.
	in, _, _ = testInstaller(t, "windows")
	in.install()
	in.exe = in.dest()
	var deleted string
	in.deleteSelf = func(path, dir string) error { deleted = path; return nil }
	if err := in.remove(home); err != nil || deleted != in.dest() {
		t.Fatalf("%v; deleteSelf(%q)", err, deleted)
	}
}

func TestShellPathFiles(t *testing.T) {
	t.Setenv("ZDOTDIR", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	home := t.TempDir()
	for _, tc := range []struct{ shell, goos, file string }{
		{"/bin/zsh", "darwin", ".zshrc"},
		{"/bin/bash", "darwin", ".bash_profile"},
		{"/usr/bin/bash", "linux", ".bashrc"},
		{"/usr/bin/fish", "linux", filepath.Join(".config", "fish", "conf.d", "denco.fish")},
		{"/bin/ksh", "linux", ".profile"},
		{"", "linux", ".profile"},
	} {
		if sp := newShellPath(home, tc.shell, tc.goos, ""); sp.file != filepath.Join(home, tc.file) {
			t.Errorf("%s on %s: %s", tc.shell, tc.goos, sp.file)
		}
	}
	t.Setenv("ZDOTDIR", filepath.Join(home, "zdot"))
	if sp := newShellPath(home, "zsh", "darwin", ""); sp.file != filepath.Join(home, "zdot", ".zshrc") {
		t.Errorf("ZDOTDIR ignored: %s", sp.file)
	}
}

// Adding and removing the line leaves the user's file exactly as it was.
func TestShellPathAddRemove(t *testing.T) {
	t.Setenv("ZDOTDIR", "")
	for _, before := range []string{"", "alias ll='ls -l'\n", "export FOO=1"} {
		home := t.TempDir()
		dir := filepath.Join(home, ".local", "bin")
		sp := newShellPath(home, "/bin/zsh", "darwin", "/usr/bin:/bin")
		if before != "" {
			os.WriteFile(sp.file, []byte(before), 0o644)
		}
		if has, _ := sp.Has(dir); has {
			t.Fatal("has it before adding")
		}
		where, err := sp.Add(dir)
		if err != nil || where != filepath.Join("~", ".zshrc") {
			t.Fatalf("Add: %q, %v", where, err)
		}
		b, _ := os.ReadFile(sp.file)
		if !strings.Contains(string(b), `export PATH="$HOME/.local/bin:$PATH"`) || !strings.HasPrefix(string(b), before) {
			t.Fatalf("file after Add:\n%s", b)
		}
		if has, _ := sp.Has(dir); !has {
			t.Fatal("Has is false after Add")
		}
		if removed, err := sp.Remove(dir); !removed || err != nil {
			t.Fatalf("Remove: %v %v", removed, err)
		}
		// Exactly as before, except that a last line without a newline
		// gets one.
		want := before
		if want != "" && !strings.HasSuffix(want, "\n") {
			want += "\n"
		}
		b, _ = os.ReadFile(sp.file)
		if string(b) != want {
			t.Fatalf("file after Remove: %q, want %q", b, want)
		}
		if removed, _ := sp.Remove(dir); removed {
			t.Fatal("removed twice")
		}
	}
	// Already on PATH: nothing to add.
	home := t.TempDir()
	dir := filepath.Join(home, ".local", "bin")
	if has, _ := newShellPath(home, "bash", "linux", "/usr/bin"+string(os.PathListSeparator)+dir+"/").Has(dir); !has {
		t.Error("a folder on $PATH counts as missing")
	}
	// fish: a file of its own, removed whole.
	t.Setenv("XDG_CONFIG_HOME", "")
	fish := newShellPath(home, "fish", "linux", "")
	fish.Add(dir)
	if b, _ := os.ReadFile(fish.file); !strings.Contains(string(b), "set -gx PATH $HOME/.local/bin $PATH") {
		t.Fatalf("fish file:\n%s", b)
	}
	fish.Remove(dir)
	if _, err := os.Stat(fish.file); !errors.Is(err, os.ErrNotExist) {
		t.Error("fish file not removed")
	}
}

func TestWindowsPathList(t *testing.T) {
	t.Setenv("DENCO_TEST_LOCAL", `C:\Users\Me\AppData\Local`)
	dir := `C:\Users\Me\AppData\Local\Programs\Denco`
	list := `%SystemRoot%\system32;%DENCO_TEST_LOCAL%\Programs\Denco\;D:\tools`
	if !pathListHas(list, dir) || !pathListHas(`c:\users\me\appdata\local\programs\DENCO`, dir) {
		t.Error("case, %VARS% or a trailing \\ not handled")
	}
	if pathListHas(`C:\Denco2;`, dir) || pathListHas("", dir) {
		t.Error("false match")
	}
	if got := pathListAdd(`C:\a;C:\b;`, dir); got != `C:\a;C:\b;`+dir {
		t.Errorf("add: %q", got)
	}
	if got := pathListAdd("", dir); got != dir {
		t.Errorf("add to empty: %q", got)
	}
	rest, removed := pathListRemove(list, dir)
	if !removed || rest != `%SystemRoot%\system32;D:\tools` {
		t.Errorf("remove: %q %v (other entries must stay as written)", rest, removed)
	}
	if _, removed := pathListRemove(`C:\a`, dir); removed {
		t.Error("removed what wasn't there")
	}
	if got := expandWinVars(`%DENCO_TEST_LOCAL%\x;%NOT_SET_DENCO%\y;50%`); got != `C:\Users\Me\AppData\Local\x;%NOT_SET_DENCO%\y;50%` {
		t.Errorf("expand: %q", got)
	}
}

func TestSetupCommand(t *testing.T) {
	in, fp, out := testInstaller(t, runtime.GOOS)
	old := newInstaller
	defer func() { newInstaller = old }()
	newInstaller = func(e *env) (*installer, error) { in.out = e.stdout; return in, nil }
	var stdout, stderr bytes.Buffer
	e := &env{home: t.TempDir(), stdout: &stdout, stderr: &stderr}
	if code := runEnv(e, []string{"setup"}); code != 0 || len(fp.added) != 1 {
		t.Fatalf("setup: exit %d, %s", code, stderr.String())
	}
	if code := runEnv(e, []string{"setup", "-remove"}); code != 0 || len(fp.removed) != 1 {
		t.Fatalf("setup -remove: exit %d, %s", code, stderr.String())
	}
	if code := runEnv(e, []string{"setup", "now"}); code != 2 {
		t.Fatalf("setup now: exit %d", code)
	}
	_ = out
	// The real installer refuses inside tests (it would install the test
	// binary).
	if _, err := systemInstaller(e); err == nil {
		t.Fatal("systemInstaller works in a test binary")
	}
}

// Double-clicked from Downloads, Denco offers to install itself once.
func TestDoubleClickOffersSetup(t *testing.T) {
	t.Setenv("DENCO_HOME", t.TempDir())
	in, fp, _ := testInstaller(t, "windows")
	oldDC, oldIn, oldNI, oldRun := doubleClicked, consoleIn, newInstaller, runInstalled
	defer func() { doubleClicked, consoleIn, newInstaller, runInstalled = oldDC, oldIn, oldNI, oldRun }()
	doubleClicked = func() bool { return true }
	newInstaller = func(e *env) (*installer, error) { in.out = e.stdout; return in, nil }
	var ran []string
	runInstalled = func(path string, args []string) int { ran = append([]string{path}, args...); return 0 }

	// Yes (just Enter): installed, then the dashboard runs from the
	// installed copy.
	consoleIn = strings.NewReader("\n")
	var out, errOut bytes.Buffer
	if code := Main(nil, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "Install Denco for your user") || len(fp.added) != 1 ||
		len(ran) != 2 || ran[0] != in.dest() || ran[1] != "tui" {
		t.Fatalf("ran %v, PATH %v; output:\n%s", ran, fp.added, out.String())
	}

	// No: remembered, the dashboard opens from where it is (it fails here,
	// no terminal, so the window waits for Enter), and no second offer.
	in, fp, _ = testInstaller(t, "windows")
	ran = nil
	consoleIn = strings.NewReader("n\n\n")
	out.Reset()
	Main(nil, &out, &errOut)
	if len(fp.added) != 0 || ran != nil || !strings.Contains(out.String(), "not asking again") {
		t.Fatalf("after no: PATH %v, ran %v; output:\n%s", fp.added, ran, out.String())
	}
	consoleIn = strings.NewReader("\n")
	out.Reset()
	Main(nil, &out, &errOut)
	if strings.Contains(out.String(), "Install Denco") {
		t.Fatal("asked again after no")
	}
}

// Inside a test binary the real runInstalled refuses: running another
// executable (the installed test binary) would run the tests again.
func TestRunInstalledRefusesInTests(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses /bin/sh")
	}
	if code := runInstalled("/bin/sh", []string{"-c", "exit 0"}); code == 0 {
		t.Fatal("ran another executable from a test")
	}
}
