package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync/atomic"
	"text/tabwriter"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/storage"
)

// cmdVerify hash-checks data already on disk against a .torrent, without
// modifying anything. It exercises storage (layout + read-only I/O) and
// pieces.Recheck on real files.
func cmdVerify(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "download directory that contains the torrent's data")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: denco verify [-dir DIR] <file.torrent>")
		return 2
	}
	mi, err := metainfo.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	layout, err := storage.NewLayout(*dir, &mi.Info)
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	st, err := storage.Open(layout, storage.Options{ReadOnly: true})
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	total := mi.Info.NumPieces()
	var checked atomic.Int64
	start := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	done := make(chan struct{})
	go func() { // progress line every second
		for {
			select {
			case <-ticker.C:
				fmt.Fprintf(stderr, "checking… %s\n", format.Percent(checked.Load(), int64(total)))
			case <-done:
				return
			}
		}
	}()
	have, err := pieces.Recheck(ctx, st, &mi.Info, func(n int) { checked.Store(int64(n)) })
	close(done)
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "Checked %d pieces in %s\n\n", total, time.Since(start).Round(time.Millisecond))
	tw := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "FILE\tSIZE\tVERIFIED")
	for _, f := range layout.Files {
		note := ""
		if f.Renamed {
			note = "  (renamed for this OS)"
		}
		fmt.Fprintf(tw, "%s%s\t%s\t%s\n", f.Rel, note, format.Bytes(f.Length), fileProgress(&mi.Info, have, f))
	}
	tw.Flush()
	fmt.Fprintf(stdout, "\nTotal: %d / %d pieces good (%s)\n", have.Count(), total, format.Percent(int64(have.Count()), int64(total)))
	return 0
}

// fileProgress is the share of the pieces overlapping f that verified.
func fileProgress(info *metainfo.Info, have *bitfield.Bitfield, f storage.File) string {
	if f.Length == 0 {
		return "100%"
	}
	first := int(f.Offset / info.PieceLength)
	last := int((f.Offset + f.Length - 1) / info.PieceLength)
	good := 0
	for i := first; i <= last; i++ {
		if have.Has(i) {
			good++
		}
	}
	return format.Percent(int64(good), int64(last-first+1))
}
