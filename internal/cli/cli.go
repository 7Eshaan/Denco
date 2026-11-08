// Package cli implements every `denco` command.
//
// Two kinds of commands live here:
//
//   - daemon commands: `denco daemon` runs the engine in the foreground;
//     add/list/info/pause/resume/remove/status/peers/files/stop are
//     short-lived clients that talk to it over IPC (client.go, render.go);
//   - standalone commands that need no daemon: inspect, magnet, announce,
//     probe, verify, download.
//
// Files: cli.go (dispatch, usage, paths), daemon.go, client.go (client
// commands), configcmd.go (denco config), render.go (tables and detail
// views), plus one file per standalone command.
package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/7Eshaan/Denco/internal/config"
	"github.com/7Eshaan/Denco/internal/ipc"
)

const usage = `denco — a BitTorrent client

Daemon (keeps torrents running in the background):
  denco daemon [-v] [flags]                    run the daemon (Ctrl-C or 'denco stop' to quit);
                                               flags (-dir -port -max-active -max-peers
                                               -down-limit -up-limit) override the config file
  denco stop                                   stop the daemon (torrents resume on next start)

Settings ($DENCO_HOME/config.yaml; no daemon needed):
  denco config [-v]                            show all settings
  denco config set <key> <value>               change one (speed limits apply at once)
  denco config get|unset <key> · init · path

Managing torrents (needs a running daemon):
  denco add [-dir DIR] [-existing] [-paused] <torrent>...   add torrents: .torrent files,
                                               magnet links or http(s) URLs of .torrent files
  denco list                                   list torrents (downloading, then completed)
  denco info <id>                              details of one torrent
  denco files <id>                             files of a torrent with progress
  denco peers <id>                             connected peers
  denco pause <id>... | all                    pause torrents
  denco resume <id>... | all                   resume paused (or failed) torrents
  denco remove [-delete] <id>...               remove torrents (-delete also deletes their files)
  denco status                                 overall client status

  <id> is the number shown by 'denco list', or the start of the info hash.

Standalone tools (no daemon needed):
  denco inspect <file.torrent>...              show torrent metadata and files
  denco magnet <magnet-uri>                    parse and show a magnet link
  denco announce [-port N] <torrent|magnet>    ask the trackers for peers
  denco probe [-n N] <torrent|magnet>          connect to peers and show what they have
  denco verify [-dir DIR] <file.torrent>       hash-check data already on disk (read-only)
  denco download [-dir DIR] [-seed] <file.torrent>   download one torrent in the foreground

  denco help                                   show this help

Denco keeps its state in $DENCO_HOME (default: <user config dir>/denco).
`

// Main runs the command line and returns the process exit code.
func Main(args []string, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr)
}

// env is what every command needs to know about its surroundings.
// Tests build one with a temporary home.
type env struct {
	home           string // Denco's own files: resume data, daemon file
	stdout, stderr io.Writer
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
	case "remove", "rm":
		return cmdRemove(e, rest)
	case "status":
		return cmdStatus(e, rest)
	case "config":
		return cmdConfig(e, rest)

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
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "denco: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
