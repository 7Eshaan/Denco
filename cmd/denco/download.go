package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/torrent"
)

// cmdDownload is a foreground, single-torrent downloader: the first time all
// the engine pieces run together. The daemon (Phase 8) will replace it as the
// normal way to download, but it stays useful for testing.
func cmdDownload(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "download directory")
	port := fs.Uint("port", 6881, "port reported to trackers")
	existing := fs.Bool("existing", false, "reuse data already in -dir (hash-checked first)")
	verbose := fs.Bool("v", false, "log peer and tracker events")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *port > 65535 {
		fmt.Fprintln(stderr, "usage: denco download [-dir DIR] [-existing] [-v] <file.torrent>")
		return 2
	}
	if magnet.IsMagnet(fs.Arg(0)) {
		fmt.Fprintln(stderr, "denco: magnet downloads need metadata exchange (Phase 10); use a .torrent file for now")
		return 1
	}
	mi, err := metainfo.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	layout, err := storage.NewLayout(*dir, &mi.Info)
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	st, err := storage.Open(layout, storage.Options{AllowExisting: *existing})
	if errors.Is(err, storage.ErrExists) {
		fmt.Fprintf(stderr, "denco: %v\n  (pass -existing to hash-check and reuse that data)\n", err)
		return 1
	}
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	fmt.Fprintf(stdout, "%s  (%s, %d files) → %s\n", mi.Info.Name, format.Bytes(mi.Info.TotalLength), len(mi.Info.Files), layout.Root)
	for _, f := range layout.Files {
		if f.Renamed {
			fmt.Fprintf(stdout, "  note: saved as %q (renamed for this OS)\n", f.Rel)
		}
	}

	haveBits, err := initialHave(ctx, st, mi, *existing, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if *verbose {
		logger = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	s := torrent.New(mi, st, haveBits, torrent.Config{PeerID: peerid.New(), Port: uint16(*port), Logger: logger})

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-s.Completed():
			stop() // ends Run; it announces "completed"/"stopped" on the way out
			if err := <-runErr; err != nil {
				fmt.Fprintf(stderr, "\ndenco: %v\n", err)
				return 1
			}
			if err := st.Sync(); err != nil {
				fmt.Fprintf(stderr, "\ndenco: flushing to disk: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "\nDone: %s downloaded to %s\n", mi.Info.Name, layout.Root)
			return 0
		case err := <-runErr:
			if err != nil {
				fmt.Fprintf(stderr, "\ndenco: %v\n", err)
				return 1
			}
			fmt.Fprintln(stdout, "\nStopped. (Resume support arrives in Phase 7; for now, rerun with -existing.)")
			return 0
		case <-ticker.C:
			fmt.Fprintln(stdout, progressLine(s.Stats()))
		}
	}
}

// initialHave hash-checks existing data when asked to reuse it.
func initialHave(ctx context.Context, st *storage.Storage, mi *metainfo.MetaInfo, existing bool, w io.Writer) (*bitfield.Bitfield, error) {
	if !existing {
		return nil, nil
	}
	fmt.Fprintln(w, "Checking existing data…")
	have, err := pieces.Recheck(ctx, st, &mi.Info, nil)
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(w, "  %d / %d pieces already present\n", have.Count(), mi.Info.NumPieces())
	return have, nil
}

func progressLine(st torrent.Stats) string {
	done := st.TotalBytes - st.BytesLeft
	seeds := ""
	if st.TrackerSeeders >= 0 {
		seeds = fmt.Sprintf(" (swarm: %d seeds, %d leechers)", st.TrackerSeeders, max(st.TrackerLeechers, 0))
	}
	line := fmt.Sprintf("%-11s %6s  %9s / %-9s  ↓ %-11s ETA %-7s  peers %d (%d seeds)%s",
		st.State, format.Percent(done, st.TotalBytes), format.Bytes(done), format.Bytes(st.TotalBytes),
		format.Speed(st.DownloadRate), format.Duration(st.ETA()), st.Peers, st.Seeds, seeds)
	if st.TrackerError != "" && st.Peers == 0 {
		line += "\n  trackers failing: " + firstLine(st.TrackerError)
	}
	return line
}

func firstLine(s string) string {
	for i, c := range s {
		if c == '\n' {
			return s[:i]
		}
	}
	return s
}
