// Command denco is a BitTorrent client.
//
// During early phases only developer commands exist; the full command set
// (add, list, pause, ...) arrives with the daemon in Phase 8.
//
// Each command lives in its own file (inspect.go, announce.go, probe.go, verify.go, download.go).
package main

import (
	"fmt"
	"io"
	"os"
)

const usage = `denco — a BitTorrent client

Usage:
  denco inspect <file.torrent>...          show torrent metadata and files
  denco magnet <magnet-uri>                parse and show a magnet link
  denco announce [-port N] <torrent|magnet> ask the trackers for peers
  denco probe [-n N] <torrent|magnet>       connect to peers and show what they have
  denco verify [-dir DIR] <file.torrent>    hash-check data already on disk (read-only)
  denco download [-dir DIR] <file.torrent>  download a torrent in the foreground
  denco help                               show this help
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run is main without os.Exit, so it can be tested.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "inspect":
		return cmdInspect(args[1:], stdout, stderr)
	case "magnet":
		return cmdMagnet(args[1:], stdout, stderr)
	case "announce":
		return cmdAnnounce(args[1:], stdout, stderr)
	case "probe":
		return cmdProbe(args[1:], stdout, stderr)
	case "verify":
		return cmdVerify(args[1:], stdout, stderr)
	case "download":
		return cmdDownload(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "denco: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}
