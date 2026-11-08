package main

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

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

func cmdMagnet(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: denco magnet <magnet-uri>   (quote it: the URI contains '&')")
		return 2
	}
	m, err := magnet.Parse(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	name := m.Name
	if name == "" {
		name = "(not given — will be known after metadata download)"
	}
	fmt.Fprintf(tw, "Name:\t%s\n", name)
	fmt.Fprintf(tw, "Info hash:\t%s\n", m.InfoHash)
	if len(m.Trackers) > 0 {
		fmt.Fprintf(tw, "Trackers:\t%s\n", strings.Join(m.Trackers, "\n\t"))
	} else {
		fmt.Fprintf(tw, "Trackers:\tnone (peers must come from DHT)\n")
	}
	for _, p := range m.Peers {
		fmt.Fprintf(tw, "Peer:\t%s\n", p)
	}
	tw.Flush()
	return 0
}
