// Command denco is a BitTorrent client.
//
// During early phases only developer commands exist; the full command set
// (add, list, pause, ...) arrives with the daemon in Phase 8.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

const usage = `denco — a BitTorrent client

Usage:
  denco inspect <file.torrent>...   show torrent metadata and files
  denco help                        show this help
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
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "denco: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

func cmdInspect(paths []string, stdout, stderr io.Writer) int {
	if len(paths) == 0 {
		fmt.Fprintln(stderr, "usage: denco inspect <file.torrent>...")
		return 2
	}
	status := 0
	for i, p := range paths {
		mi, err := metainfo.Load(p)
		if err != nil {
			fmt.Fprintf(stderr, "denco: %v\n", err)
			status = 1
			continue
		}
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		printMetaInfo(stdout, mi)
	}
	return status
}

func printMetaInfo(w io.Writer, mi *metainfo.MetaInfo) {
	info := &mi.Info
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	row := func(k, v string) { fmt.Fprintf(tw, "%s:\t%s\n", k, v) }

	row("Name", info.Name)
	row("Info hash", mi.InfoHash.String())
	row("Size", format.Bytes(info.TotalLength))
	row("Pieces", fmt.Sprintf("%d × %s", info.NumPieces(), format.Bytes(info.PieceLength)))
	row("Files", fmt.Sprint(len(info.Files)))
	if info.Private {
		row("Private", "yes (DHT disabled for this torrent)")
	}
	if mi.Comment != "" {
		row("Comment", mi.Comment)
	}
	if mi.CreatedBy != "" {
		row("Created by", mi.CreatedBy)
	}
	if !mi.CreationDate.IsZero() {
		row("Created", mi.CreationDate.Format(time.RFC1123))
	}
	if trackers := mi.Trackers(); len(trackers) > 0 {
		row("Trackers", strings.Join(trackers, "\n\t"))
	}
	tw.Flush()

	fmt.Fprintln(w, "\nFiles:")
	for _, f := range info.Files {
		fmt.Fprintf(w, "  %10s  %s\n", format.Bytes(f.Length), f.DisplayPath(info))
	}
}
