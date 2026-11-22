package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/ipc"
)

// `denco setup` installs Denco for the current user so `denco` works in
// any new terminal, with no administrator rights and no PATH editing by
// hand (DECISIONS D66):
//
//   - Windows: %LocalAppData%\Programs\Denco\denco.exe, and that folder
//     on the user's PATH (the registry), announced to running programs.
//   - macOS, Linux: ~/.local/bin/denco, and one marked line in the
//     shell's startup file if that folder isn't on PATH yet.
//
// `denco setup -remove` undoes both; settings, resume data and downloads
// stay. Running setup from a newer version replaces the installed one.

// pathEditor makes a folder reachable through PATH, and undoes it.
type pathEditor interface {
	// Has reports whether dir is (or will be, in new terminals) on PATH.
	Has(dir string) (bool, error)
	// Add puts dir on PATH and says where ("your user PATH", "~/.zshrc").
	Add(dir string) (where string, err error)
	// Remove undoes Add; false if there was nothing to undo.
	Remove(dir string) (bool, error)
	// Reload tells the user how to make an open terminal see the change.
	Reload() string
}

// installer does the work of `denco setup`. newInstaller fills it from
// the system; tests fill it by hand.
type installer struct {
	goos     string
	exe      string // the running executable
	dir      string // the folder it is installed into
	name     string // denco or denco.exe
	path     pathEditor
	lookPath func(string) (string, error)
	// stopDaemon stops a running daemon (it may be running from the file
	// being replaced); it reports whether one was running.
	stopDaemon func() (bool, error)
	// deleteSelf removes the running executable after it exits (Windows,
	// where a running program can't be deleted). nil: just delete.
	deleteSelf func(path, dir string) error
	out        io.Writer
	// show shortens paths in messages (~ for the home folder); nil: as is.
	show func(string) string
}

func (in *installer) disp(p string) string {
	if in.show != nil {
		return in.show(p)
	}
	return p
}

func (in *installer) dest() string { return filepath.Join(in.dir, in.name) }

// samePath compares paths the way the OS does: case-insensitive on
// Windows. (macOS volumes can be case-sensitive, so only Windows folds.)
func (in *installer) samePath(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if in.goos == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// Installed reports whether the running executable is the installed copy.
func (in *installer) Installed() bool { return in.samePath(in.exe, in.dest()) }

// install copies the running executable into place and makes sure its
// folder is on PATH.
func (in *installer) install() error {
	dest := in.dest()
	if in.Installed() {
		fmt.Fprintf(in.out, "Denco is installed at %s.\n", in.disp(dest))
	} else {
		if other, err := in.lookPath(in.name); err == nil && !in.samePath(other, dest) {
			if p, err := filepath.EvalSymlinks(other); err != nil || !in.samePath(p, dest) {
				fmt.Fprintf(in.out, "Note: another denco is on your PATH at %s; the one installed now comes first only if its folder does.\n", other)
			}
		}
		running, err := in.stopDaemon()
		if err != nil {
			return fmt.Errorf("stopping the daemon first: %w", err)
		}
		if err := in.copyInto(dest); err != nil {
			return err
		}
		fmt.Fprintf(in.out, "Installed Denco at %s.\n", in.disp(dest))
		if running {
			fmt.Fprintln(in.out, "The daemon was stopped to replace it; your next denco command starts it again.")
		}
	}
	has, err := in.path.Has(in.dir)
	if err != nil {
		return err
	}
	if has {
		fmt.Fprintf(in.out, "%s is on your PATH (in new terminals).\n", in.disp(in.dir))
		return nil
	}
	where, err := in.path.Add(in.dir)
	if err != nil {
		return fmt.Errorf("adding %s to PATH: %w", in.dir, err)
	}
	fmt.Fprintf(in.out, "Added %s to %s.\n", in.disp(in.dir), where)
	fmt.Fprintf(in.out, "Open a new terminal and run: denco help%s\n", in.path.Reload())
	return nil
}

// copyInto writes the running executable to dest via a temporary file,
// so a failed copy never leaves a broken denco behind. On Windows a
// running dest (an old version's daemon was just stopped, but this may be
// the dashboard) can't be overwritten, only renamed: it is moved aside.
func (in *installer) copyInto(dest string) error {
	if err := os.MkdirAll(in.dir, 0o755); err != nil {
		return err
	}
	old := dest + ".old"
	os.Remove(old) // left by an earlier update
	src, err := os.Open(in.exe)
	if err != nil {
		return err
	}
	defer src.Close()
	tmp, err := os.CreateTemp(in.dir, ".denco-setup-*")
	if err != nil {
		return err
	}
	_, err = io.Copy(tmp, src)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o755)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("copying to %s: %w", in.dir, err)
	}
	if in.goos == "windows" {
		if _, err := os.Stat(dest); err == nil {
			if err := os.Rename(dest, old); err != nil {
				os.Remove(tmp.Name())
				return fmt.Errorf("moving the old %s aside: %w", in.name, err)
			}
		}
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("installing %s: %w", dest, err)
	}
	os.Remove(old) // fails while the old one still runs; the next setup cleans it
	return nil
}

// remove undoes install. User data is left alone.
func (in *installer) remove(home string) error {
	if _, err := in.stopDaemon(); err != nil {
		return fmt.Errorf("stopping the daemon first: %w", err)
	}
	removed, err := in.path.Remove(in.dir)
	if err != nil {
		return err
	}
	if removed {
		fmt.Fprintf(in.out, "Removed %s from your PATH settings.\n", in.disp(in.dir))
	}
	dest := in.dest()
	switch _, err := os.Stat(dest); {
	case errors.Is(err, os.ErrNotExist):
		fmt.Fprintf(in.out, "Denco isn't installed at %s.\n", in.disp(dest))
	case in.Installed() && in.deleteSelf != nil:
		if err := in.deleteSelf(dest, in.dir); err != nil {
			return err
		}
		fmt.Fprintf(in.out, "Removed %s (it is deleted as this window closes).\n", in.disp(dest))
	default:
		if err := os.Remove(dest); err != nil {
			return err
		}
		os.Remove(dest + ".old")
		if in.goos == "windows" {
			os.Remove(in.dir) // our own folder; only if empty
		}
		fmt.Fprintf(in.out, "Removed %s.\n", in.disp(dest))
	}
	fmt.Fprintf(in.out, "Your settings, resume data and downloads are kept (settings: %s).\n", in.disp(home))
	return nil
}

// newInstaller describes this system. A variable for tests.
var newInstaller = systemInstaller

func systemInstaller(e *env) (*installer, error) {
	if testing.Testing() {
		// It would install the test binary (and the double-click path then
		// runs it, which runs the tests again). Tests build installers by
		// hand, with temporary folders.
		return nil, errors.New("setup with the real executable is disabled in tests")
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	in := &installer{
		goos: runtime.GOOS, exe: exe, name: "denco", lookPath: exec.LookPath, out: e.stdout,
		stopDaemon: func() (bool, error) { return stopDaemon(e) },
	}
	if in.goos == "windows" {
		local := os.Getenv("LOCALAPPDATA")
		if local == "" {
			return nil, errors.New("LOCALAPPDATA isn't set")
		}
		in.dir, in.name = filepath.Join(local, "Programs", "Denco"), "denco.exe"
		in.path, in.deleteSelf = registryPath{}, deleteAfterExit
		return in, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	in.dir = filepath.Join(home, ".local", "bin")
	sp := newShellPath(home, os.Getenv("SHELL"), runtime.GOOS, os.Getenv("PATH"))
	in.path, in.show = sp, sp.display
	return in, nil
}

func cmdSetup(e *env, args []string) int {
	fs := flag.NewFlagSet("setup", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	remove := fs.Bool("remove", false, "uninstall: remove the installed copy and the PATH change")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		fmt.Fprintln(e.stderr, "usage: denco setup [-remove]")
		return 2
	}
	in, err := newInstaller(e)
	if err == nil {
		if *remove {
			err = in.remove(e.home)
		} else {
			err = in.install()
		}
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	return 0
}

// stopDaemon stops a running daemon and waits for it to exit; false if
// none was running.
func stopDaemon(e *env) (bool, error) {
	ctx := context.Background()
	c, err := ipc.Connect(ctx, e.daemonFile())
	if errors.Is(err, ipc.ErrNoDaemon) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := c.Shutdown(ctx); err != nil {
		return true, err
	}
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if _, err := ipc.Connect(ctx, e.daemonFile()); errors.Is(err, ipc.ErrNoDaemon) {
			return true, nil
		}
	}
	return true, errors.New("the daemon hasn't stopped after a minute")
}

// --- macOS / Linux: the shell's startup file ---------------------------

const setupMarker = "# Added by denco setup; `denco setup -remove` takes it out."

// shellPath edits one shell startup file: the one the user's login shell
// reads ($SHELL), or ~/.profile for shells it doesn't know.
type shellPath struct {
	home    string
	file    string // where the line goes
	fish    bool   // a fish conf.d file of our own (removed whole)
	envPath string // $PATH now
}

func newShellPath(home, shell, goos, envPath string) *shellPath {
	sp := &shellPath{home: home, envPath: envPath}
	switch filepath.Base(shell) {
	case "zsh":
		dir := os.Getenv("ZDOTDIR")
		if dir == "" {
			dir = home
		}
		sp.file = filepath.Join(dir, ".zshrc")
	case "bash":
		// macOS Terminal starts login shells (~/.bash_profile); Linux
		// terminals start interactive ones (~/.bashrc).
		sp.file = filepath.Join(home, ".bashrc")
		if goos == "darwin" {
			sp.file = filepath.Join(home, ".bash_profile")
		}
	case "fish":
		cfg := os.Getenv("XDG_CONFIG_HOME")
		if cfg == "" {
			cfg = filepath.Join(home, ".config")
		}
		sp.file, sp.fish = filepath.Join(cfg, "fish", "conf.d", "denco.fish"), true
	default:
		sp.file = filepath.Join(home, ".profile")
	}
	return sp
}

// display shortens home to ~ in messages.
func (sp *shellPath) display(p string) string {
	if rel, err := filepath.Rel(sp.home, p); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.Join("~", rel)
	}
	return p
}

// shellDir writes dir for the shell: $HOME/… when under home.
func (sp *shellPath) shellDir(dir string) string {
	if rel, err := filepath.Rel(sp.home, dir); err == nil && !strings.HasPrefix(rel, "..") {
		return "$HOME/" + filepath.ToSlash(rel)
	}
	return dir
}

func (sp *shellPath) line(dir string) string {
	if sp.fish {
		d := sp.shellDir(dir)
		return fmt.Sprintf("contains -- %s $PATH; or set -gx PATH %s $PATH", d, d)
	}
	return fmt.Sprintf("export PATH=\"%s:$PATH\"", sp.shellDir(dir))
}

func (sp *shellPath) Has(dir string) (bool, error) {
	for _, p := range filepath.SplitList(sp.envPath) {
		if p != "" && filepath.Clean(p) == filepath.Clean(dir) {
			return true, nil
		}
	}
	b, err := os.ReadFile(sp.file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil && strings.Contains(string(b), setupMarker), err
}

func (sp *shellPath) Add(dir string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(sp.file), 0o755); err != nil {
		return "", err
	}
	b, err := os.ReadFile(sp.file)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	block := setupMarker + "\n" + sp.line(dir) + "\n"
	if len(b) > 0 {
		block = "\n" + block
		if !strings.HasSuffix(string(b), "\n") {
			block = "\n" + block
		}
	}
	f, err := os.OpenFile(sp.file, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(block)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return sp.display(sp.file), err
}

func (sp *shellPath) Remove(string) (bool, error) {
	b, err := os.ReadFile(sp.file)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if sp.fish {
		if !strings.Contains(string(b), setupMarker) {
			return false, nil
		}
		return true, os.Remove(sp.file)
	}
	lines := strings.SplitAfter(string(b), "\n")
	var out []string
	removed := false
	for i := 0; i < len(lines); i++ {
		if strings.TrimRight(lines[i], "\r\n") == setupMarker {
			removed = true
			i++ // and the line after it
			// The blank line Add put before the block goes too.
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == "" {
				out = out[:n-1]
			}
			continue
		}
		out = append(out, lines[i])
	}
	if !removed {
		return false, nil
	}
	return true, os.WriteFile(sp.file, []byte(strings.Join(out, "")), 0o644)
}

func (sp *shellPath) Reload() string {
	if sp.fish {
		return ""
	}
	return fmt.Sprintf(" (or in this one: source %s)", sp.display(sp.file))
}

// --- Windows: the user PATH as a list ----------------------------------

// pathListHas reports whether the ;-separated list holds dir (case-
// insensitive, %VARS% expanded, trailing \ ignored).
func pathListHas(list, dir string) bool {
	for _, p := range strings.Split(list, ";") {
		if sameWinDir(p, dir) {
			return true
		}
	}
	return false
}

// pathListAdd appends dir to the list, keeping every entry as it was.
func pathListAdd(list, dir string) string {
	list = strings.TrimRight(list, ";")
	if list == "" {
		return dir
	}
	return list + ";" + dir
}

// pathListRemove drops dir from the list; false if it wasn't there.
func pathListRemove(list, dir string) (string, bool) {
	var keep []string
	removed := false
	for _, p := range strings.Split(list, ";") {
		if sameWinDir(p, dir) {
			removed = true
			continue
		}
		if p != "" {
			keep = append(keep, p)
		}
	}
	return strings.Join(keep, ";"), removed
}

func sameWinDir(entry, dir string) bool {
	clean := func(s string) string { return strings.TrimRight(expandWinVars(strings.TrimSpace(s)), `\/`) }
	e := clean(entry)
	return e != "" && strings.EqualFold(e, clean(dir))
}

// expandWinVars expands %NAME% the way Windows does in REG_EXPAND_SZ
// values; unknown names are left as written.
func expandWinVars(s string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(s, '%')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i+1:], '%')
		if j < 0 {
			break
		}
		name := s[i+1 : i+1+j]
		b.WriteString(s[:i])
		if v, ok := os.LookupEnv(name); ok && name != "" {
			b.WriteString(v)
		} else {
			b.WriteString(s[i : i+j+2])
		}
		s = s[i+j+2:]
	}
	b.WriteString(s)
	return b.String()
}
