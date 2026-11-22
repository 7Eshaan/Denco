// Package cli implements every `denco` command.
//
// Two kinds of commands live here:
//
//   - daemon commands: `denco daemon` runs the engine in the foreground;
//     add/list/info/pause/resume/remove/status/peers/files/stop are
//     short-lived clients that talk to it over IPC (client.go, render.go),
//     and tui runs the terminal dashboard (tui.go, internal/tui);
//   - standalone commands that need no daemon: inspect, magnet, announce,
//     probe, verify, download.
//
// Files: cli.go (dispatch, usage, paths), daemon.go, client.go (client
// commands), configcmd.go (denco config), render.go (tables and detail
// views), plus one file per standalone command.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/config"
	"github.com/7Eshaan/Denco/internal/ipc"
)

const usage = `denco — A daemon-based BitTorrent client for the command line, written in Go with zero dependencies.

Daemon (keeps torrents running in the background):
  denco daemon [-v] [flags]                    run the daemon (Ctrl-C or 'denco stop' to quit);
                                               flags (-dir -port -max-active -max-peers -down-limit
                                               -up-limit -seed-ratio -seed-time -log) override the config
  denco start [flags]                          start the daemon in the background (same flags);
                                               other commands do this by themselves (auto_start)
  denco stop                                   stop the daemon (torrents resume on next start)

Settings ($DENCO_HOME/config.yaml; no daemon needed):
  denco config [-v]                            show all settings
  denco config set <key> <value>               change one (speed and seed limits apply at once)
  denco config get|unset <key> · init · path

Managing torrents (starts the daemon in the background if it isn't running):
  denco add [-dir DIR] [-existing] [-paused] <torrent>...   add torrents: .torrent files,
                                               magnet links or http(s) URLs of .torrent files
            [-skip FILES | -only FILES]        choose files by number (see 'denco inspect'),
                                               e.g. -skip 2,4-6
  denco list                                   list torrents (downloading, then completed)
  denco info <id>                              details of one torrent
  denco files <id>                             files of a torrent with progress
  denco files <id> skip|want <files>           stop / start downloading some files
  denco peers <id>                             connected peers
  denco pause [-uploads] <id>... | all         pause torrents (-uploads: only uploading)
  denco resume [-uploads] <id>... | all        resume paused, failed or finished torrents
  denco resume -force <id>...                  start now, even if max_active_downloads are running
  denco queue top|up|down|bottom <id>...       move downloads in the queue (see Q in 'denco list')
  denco limit [-down R] [-up R] <id>... | all  a torrent's own speed limits, e.g. -down 500K
                                               (0 = none; without flags: show them)
  denco seed on|off|default <id>... | all      seed with no limits / don't seed / use the
                                               seed_ratio and seed_time limits
  denco remove [-delete] <id>...               remove torrents (-delete also deletes their files)
  denco status                                 overall client status
  denco tui                                    live dashboard: watch, add, pause, resume and
                                               remove torrents with the keyboard (? for keys)

  <id> is the number shown by 'denco list', or the start of the info hash.

Standalone tools (no daemon needed):
  denco inspect <file.torrent>...              show torrent metadata and files
  denco magnet <magnet-uri>                    parse and show a magnet link
  denco announce [-port N] <torrent|magnet>    ask the trackers for peers
  denco probe [-n N] <torrent|magnet>          connect to peers and show what they have
  denco verify [-dir DIR] <file.torrent>       hash-check data already on disk (read-only)
  denco download [-dir DIR] [-seed] <file.torrent>   download one torrent in the foreground

  denco setup [-remove]                        install Denco for your user so 'denco' works in
                                               any new terminal (no admin rights); -remove undoes it
  denco version                                show the version
  denco help                                   show this help

Denco keeps its state in $DENCO_HOME (default: <user config dir>/denco).
`

// Main runs the command line and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 && doubleClicked() {
		return runDoubleClicked(stdout, stderr)
	}
	return run(args, stdout, stderr)
}

// doubleClicked reports whether Denco was started by double-clicking it
// (Windows: it has a console window to itself). A variable for tests.
var doubleClicked = ownConsole

// consoleIn is where "Press Enter" is read from. A variable for tests.
var consoleIn io.Reader = os.Stdin

// runDoubleClicked opens the dashboard (starting the daemon if needed)
// instead of printing the usage into a window that closes at once. The
// first time, Denco offers to install itself (D66). If the dashboard
// fails, the window waits for Enter so the message can be read. Quitting
// the dashboard closes the window; the daemon keeps running.
func runDoubleClicked(stdout, stderr io.Writer) int {
	in := bufio.NewReader(consoleIn)
	code, done := offerSetup(in, stdout, stderr)
	if !done {
		code = run([]string{"tui"}, stdout, stderr)
	}
	if code != 0 {
		fmt.Fprint(stderr, "\nDenco is a command-line program: open a terminal and run \"denco help\" to see what it can do.\nPress Enter to close this window.")
		in.ReadString('\n')
	}
	return code
}

// setupDeclined, in Denco's home, records a "no" to the install offer.
const setupDeclined = "setup-declined"

// offerSetup asks a double-clicking user once whether to install Denco
// (D66). On yes it installs and runs the dashboard from the installed
// copy (so the daemon runs from there, not from Downloads); done reports
// that it did. A no is remembered.
func offerSetup(in *bufio.Reader, stdout, stderr io.Writer) (code int, done bool) {
	home, err := Home()
	if err != nil {
		return 0, false
	}
	inst, err := newInstaller(&env{home: home, stdout: stdout, stderr: stderr})
	if err != nil || inst.Installed() || inst.brew {
		return 0, false
	}
	if _, err := os.Stat(filepath.Join(home, setupDeclined)); err == nil {
		return 0, false
	}
	fmt.Fprint(stdout, "Install Denco for your user, so 'denco' works in any terminal? [Y/n] ")
	answer, _ := in.ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(answer)); a == "n" || a == "no" {
		os.MkdirAll(home, 0o700)
		os.WriteFile(filepath.Join(home, setupDeclined), []byte("Answered no to installing Denco; run 'denco setup' to install.\n"), 0o600)
		fmt.Fprintln(stdout, "OK, not asking again. Run 'denco setup' any time to install.")
		return 0, false
	}
	if err := inst.install(); err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1, true
	}
	fmt.Fprintln(stdout, "Opening the dashboard…")
	return runInstalled(inst.dest(), []string{"tui"}), true
}

// runInstalled runs the installed copy in this window. A variable for
// tests.
var runInstalled = func(path string, args []string) int {
	if testing.Testing() {
		fmt.Fprintln(os.Stderr, "denco: running another executable is disabled in tests")
		return 1
	}
	cmd := exec.Command(path, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		return exit.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "denco: %v\n", err)
	return 1
}

// env is what every command needs to know about its surroundings.
// Tests build one with a temporary home.
type env struct {
	home           string // Denco's own files: resume data, daemon file
	stdout, stderr io.Writer
	spawn          spawnFunc // starts a background daemon; nil = a real process (autostart.go)
}

func (e *env) daemonFile() string { return filepath.Join(e.home, ipc.DaemonFile) }
func (e *env) configFile() string { return filepath.Join(e.home, config.FileName) }

// loadConfig reads the config file (defaults if it doesn't exist).
func (e *env) loadConfig() (config.Config, error) {
	return config.Load(e.configFile(), config.Defaults(e.home))
}

// Home returns the directory for Denco's own files: $DENCO_HOME if set,
// else <user config dir>/denco (e.g. ~/Library/Application Support/denco).
func Home() (string, error) {
	if h := os.Getenv("DENCO_HOME"); h != "" {
		return filepath.Abs(h)
	}
	base, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding the config directory (set DENCO_HOME): %w", err)
	}
	return filepath.Join(base, "denco"), nil
}

// run resolves the home directory and dispatches.
func run(args []string, stdout, stderr io.Writer) int {
	home, err := Home()
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	return runEnv(&env{home: home, stdout: stdout, stderr: stderr}, args)
}

func runEnv(e *env, args []string) int {
	if len(args) == 0 {
		fmt.Fprint(e.stderr, usage)
		return 2
	}
	stdout, stderr, rest := e.stdout, e.stderr, args[1:]
	switch args[0] {
	case "daemon":
		return cmdDaemon(e, rest)
	case "start":
		return cmdStart(e, rest)
	case "stop":
		return cmdStop(e, rest)
	case "add":
		return cmdAdd(e, rest)
	case "list", "ls":
		return cmdList(e, rest)
	case "info":
		return cmdInfo(e, rest)
	case "files":
		return cmdFiles(e, rest)
	case "peers":
		return cmdPeers(e, rest)
	case "pause":
		return cmdPause(e, rest)
	case "resume":
		return cmdResume(e, rest)
	case "queue":
		return cmdQueue(e, rest)
	case "limit":
		return cmdLimit(e, rest)
	case "remove", "rm":
		return cmdRemove(e, rest)
	case "status":
		return cmdStatus(e, rest)
	case "seed":
		return cmdSeed(e, rest)
	case "config":
		return cmdConfig(e, rest)
	case "tui":
		return cmdTUI(e, rest)

	case "inspect":
		return cmdInspect(rest, stdout, stderr)
	case "magnet":
		return cmdMagnet(rest, stdout, stderr)
	case "announce":
		return cmdAnnounce(rest, stdout, stderr)
	case "probe":
		return cmdProbe(rest, stdout, stderr)
	case "verify":
		return cmdVerify(rest, stdout, stderr)
	case "download":
		return cmdDownload(e, rest)
	case "version", "--version":
		return cmdVersion(rest, stdout, stderr)
	case "setup":
		return cmdSetup(e, rest)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "denco: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
