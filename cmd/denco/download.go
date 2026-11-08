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
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/torrent"
)

// cmdDownload is a foreground, single-torrent client: it listens for peers,
// downloads, and with -seed keeps uploading afterwards. The daemon (Phase 8)
// will replace it as the normal way to download, but it stays useful for
// testing.
func cmdDownload(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", ".", "download directory")
	port := fs.Uint("port", 6881, "TCP port to accept peers on (0 = random)")
	existing := fs.Bool("existing", false, "reuse data already in -dir (hash-checked first)")
	seed := fs.Bool("seed", false, "keep seeding after the download completes (until Ctrl-C)")
	verbose := fs.Bool("v", false, "log peer and tracker events")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *port > 65535 {
		fmt.Fprintln(stderr, "usage: denco download [-dir DIR] [-port N] [-seed] [-existing] [-v] <file.torrent>")
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
	id := peerid.New()
	ln, err := listen(uint16(*port), id, logger, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	defer ln.Close()

	s := torrent.New(mi, st, haveBits, torrent.Config{
		PeerID: id,
		Port:   ln.Port(),
		NoSeed: !*seed,
		Logger: logger,
	})
	ln.Add(s)
	defer ln.Remove(s)

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	completed := s.Completed()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-completed:
			completed = nil // only once
			if err := st.Sync(); err != nil {
				fmt.Fprintf(stderr, "\ndenco: flushing to disk: %v\n", err)
				return 1
			}
			fmt.Fprintf(stdout, "\nDone: %s downloaded to %s\n", mi.Info.Name, layout.Root)
			if *seed {
				fmt.Fprintf(stdout, "Seeding on port %d — press Ctrl-C to stop.\n", ln.Port())
			}
			// Without -seed, Run returns by itself (NoSeed) after announcing.
		case err := <-runErr:
			if err != nil {
				fmt.Fprintf(stderr, "\ndenco: %v\n", err)
				return 1
			}
			st := s.Stats()
			if st.BytesLeft > 0 {
				fmt.Fprintln(stdout, "\nStopped. (Resume support arrives in Phase 7; for now, rerun with -existing.)")
			} else if *seed {
				fmt.Fprintf(stdout, "\nStopped seeding. Uploaded %s this session.\n", format.Bytes(st.Uploaded))
			}
			return 0
		case <-ticker.C:
			fmt.Fprintln(stdout, progressLine(s.Stats()))
		}
	}
}

// listen opens the peer port, falling back to a random free port if the
// requested one is taken (e.g. another client is running).
func listen(port uint16, id peerid.ID, log *slog.Logger, stderr io.Writer) (*torrent.Listener, error) {
	opts := peer.Options{PeerID: id}
	ln, err := torrent.Listen(fmt.Sprintf(":%d", port), opts, log)
	if err == nil || port == 0 {
		return ln, err
	}
	fmt.Fprintf(stderr, "denco: port %d unavailable (%v); using a random port\n", port, err)
	return torrent.Listen(":0", opts, log)
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
	eta := "ETA " + format.Duration(st.ETA())
	if st.BytesLeft == 0 {
		eta = "up " + format.Bytes(st.Uploaded)
	}
	line := fmt.Sprintf("%-11s %6s  %9s / %-9s  ↓ %-11s ↑ %-11s %-12s peers %d (%d seeds)%s",
		st.State, format.Percent(done, st.TotalBytes), format.Bytes(done), format.Bytes(st.TotalBytes),
		format.Speed(st.DownloadRate), format.Speed(st.UploadRate), eta, st.Peers, st.Seeds, seeds)
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
