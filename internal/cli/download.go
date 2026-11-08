package cli

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
	"github.com/7Eshaan/Denco/internal/ipc"
	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/resume"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/torrent"
)

// cmdDownload is a foreground, single-torrent client: it listens for peers,
// downloads, and with -seed keeps uploading afterwards. Progress is saved to
// the resume store, so rerunning the same command continues where it left
// off. The daemon is the normal way to download; this stays useful for
// testing and one-off downloads.
func cmdDownload(e *env, args []string) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return runDownload(ctx, e, args)
}

type downloadOpts struct {
	dir       string
	dirSet    bool // -dir given explicitly
	port      uint
	existing  bool
	seed      bool
	verbose   bool
	stateDir  string
	stateSet  bool // -state given explicitly
	saveEvery time.Duration
}

// runDownload is cmdDownload with an explicit context, so tests can stop it.
func runDownload(ctx context.Context, e *env, args []string) int {
	stdout, stderr := e.stdout, e.stderr
	o, torrentPath, code := parseDownloadFlags(args, e.stateDir(), stderr)
	if code != 0 {
		return code
	}
	if !o.stateSet && daemonRunning(ctx, e) {
		// Both would write the same resume files.
		fmt.Fprintln(stderr, "denco: the daemon is running; use 'denco add' instead (or pass -state DIR)")
		return 1
	}
	fail := func(err error) int {
		fmt.Fprintf(stderr, "denco: %v\n", err)
		return 1
	}
	if magnet.IsMagnet(torrentPath) {
		fmt.Fprintln(stderr, "denco: magnet downloads need metadata exchange (Phase 10); use a .torrent file for now")
		return 1
	}
	mi, err := metainfo.Load(torrentPath)
	if err != nil {
		return fail(err)
	}
	store, err := resume.OpenStore(o.stateDir)
	if err != nil {
		return fail(err)
	}

	op, err := openForDownload(ctx, mi, store, &o, stdout, stderr)
	if err != nil {
		return fail(err)
	}
	defer op.st.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	if o.verbose {
		logger = slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	id := peerid.New()
	ln, err := listen(uint16(o.port), id, logger, stderr)
	if err != nil {
		return fail(err)
	}
	defer ln.Close()

	s := torrent.New(mi, op.st, op.have, torrent.Config{
		PeerID: id,
		Port:   ln.Port(),
		NoSeed: !o.seed,
		Logger: logger,
	})
	ln.Add(s)
	defer ln.Remove(s)

	// Save right away, so even a crash in the first seconds leaves an entry.
	saver := resume.NewSaver(store, mi, op.layout, op.st, op.prev)
	snapshot := func() resume.Snapshot {
		st := s.Stats()
		return resume.Snapshot{Have: s.Have(), Uploaded: st.Uploaded, Downloaded: st.Downloaded}
	}
	warn := func(err error) { fmt.Fprintf(stderr, "denco: saving resume data: %v\n", err) }
	if err := saver.Save(snapshot(), false); err != nil {
		warn(err)
	}
	saveCtx, stopSaving := context.WithCancel(context.Background())
	defer stopSaving()
	go saver.Run(saveCtx, o.saveEvery, snapshot, warn)

	runErr := make(chan error, 1)
	go func() { runErr <- s.Run(ctx) }()

	completed := s.Completed()
	if op.have != nil && op.have.Full() {
		completed = nil // nothing to announce: already complete at start
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-completed:
			completed = nil // only once
			if err := saver.Save(snapshot(), false); err != nil {
				warn(err)
			}
			fmt.Fprintf(stdout, "\nDone: %s downloaded to %s\n", mi.Info.Name, op.layout.Root)
			if o.seed {
				fmt.Fprintf(stdout, "Seeding on port %d — press Ctrl-C to stop.\n", ln.Port())
			}
			// Without -seed, Run returns by itself (NoSeed) after announcing.
		case err := <-runErr:
			stopSaving()
			// Run has returned: no more writes, so this save may be "clean".
			if serr := saver.Save(snapshot(), err == nil); serr != nil {
				warn(serr)
			}
			if err != nil {
				fmt.Fprintf(stderr, "\ndenco: %v\n", err)
				return 1
			}
			st := s.Stats()
			switch {
			case st.BytesLeft > 0:
				fmt.Fprintf(stdout, "\nStopped at %s. Run the same command again to resume.\n",
					format.Percent(st.TotalBytes-st.BytesLeft, st.TotalBytes))
			case o.seed:
				fmt.Fprintf(stdout, "\nStopped seeding. Uploaded %s this session.\n", format.Bytes(st.Uploaded))
			case op.have != nil && op.have.Full():
				fmt.Fprintf(stdout, "Already complete: %s\n", op.layout.Root)
			}
			return 0
		case <-ticker.C:
			fmt.Fprintln(stdout, progressLine(s.Stats()))
		}
	}
}

func parseDownloadFlags(args []string, defState string, stderr io.Writer) (downloadOpts, string, int) {
	var o downloadOpts
	fs := flag.NewFlagSet("download", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.dir, "dir", ".", "download directory (default: the saved one when resuming, else .)")
	fs.UintVar(&o.port, "port", 6881, "TCP port to accept peers on (0 = random)")
	fs.BoolVar(&o.existing, "existing", false, "no saved state: hash-check data already in -dir and reuse it")
	fs.BoolVar(&o.seed, "seed", false, "keep seeding after the download completes (until Ctrl-C)")
	fs.BoolVar(&o.verbose, "v", false, "log peer and tracker events")
	fs.StringVar(&o.stateDir, "state", defState, "where resume data is kept")
	fs.DurationVar(&o.saveEvery, "save-every", 30*time.Second, "how often to save resume data")
	if err := fs.Parse(args); err != nil {
		return o, "", 2
	}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "dir":
			o.dirSet = true
		case "state":
			o.stateSet = true
		}
	})
	if fs.NArg() != 1 || o.port > 65535 || o.saveEvery <= 0 {
		fmt.Fprintln(stderr, "usage: denco download [-dir DIR] [-port N] [-seed] [-existing] [-state DIR] [-v] <file.torrent>")
		return o, "", 2
	}
	return o, fs.Arg(0), 0
}

// opened is the outcome of openForDownload.
type opened struct {
	layout *storage.Layout
	st     *storage.Storage
	have   *bitfield.Bitfield // nil = nothing yet
	prev   *resume.State      // nil = new torrent
}

// openForDownload picks how to start: resume from saved state, recheck
// existing data (-existing), or start fresh (refusing to overwrite files).
func openForDownload(ctx context.Context, mi *metainfo.MetaInfo, store *resume.Store, o *downloadOpts, stdout, stderr io.Writer) (*opened, error) {
	prev, _, err := store.Load(mi.InfoHash)
	switch {
	case errors.Is(err, resume.ErrNotFound):
		prev = nil
	case err != nil:
		fmt.Fprintf(stderr, "denco: ignoring unreadable resume data: %v\n", err)
		prev = nil
	}
	if prev != nil && !o.dirSet {
		o.dir = prev.DownloadDir // resume where it was downloading before
	}
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return nil, err
	}
	layout, err := storage.NewLayout(o.dir, &mi.Info)
	if err != nil {
		return nil, err
	}
	if prev != nil && prev.DownloadDir != layout.Root {
		fmt.Fprintf(stderr, "denco: saved progress is for %s; starting over in %s\n", prev.DownloadDir, layout.Root)
		prev = nil
	}

	fmt.Fprintf(stdout, "%s  (%s, %d files) → %s\n", mi.Info.Name, format.Bytes(mi.Info.TotalLength), len(mi.Info.Files), layout.Root)
	for _, f := range layout.Files {
		if f.Renamed {
			fmt.Fprintf(stdout, "  note: saved as %q (renamed for this OS)\n", f.Rel)
		}
	}

	// Our own files from an earlier run are expected to exist when resuming.
	st, err := storage.Open(layout, storage.Options{AllowExisting: prev != nil || o.existing})
	if errors.Is(err, storage.ErrExists) {
		return nil, fmt.Errorf("%v\n  (pass -existing to hash-check and reuse that data)", err)
	}
	if err != nil {
		return nil, err
	}
	op := &opened{layout: layout, st: st, prev: prev}

	if prev != nil {
		have, rep, err := resume.Restore(ctx, prev, mi, layout, st)
		if err == nil {
			for _, n := range rep.Notes {
				fmt.Fprintf(stdout, "  %s\n", n)
			}
			fmt.Fprintf(stdout, "Resuming: %d / %d pieces verified", have.Count(), mi.Info.NumPieces())
			if rep.Rechecked > 0 {
				fmt.Fprintf(stdout, " (re-checked %d, %d failed)", rep.Rechecked, rep.Failed)
			}
			fmt.Fprintln(stdout)
			op.have = have
			return op, nil
		}
		if !errors.Is(err, resume.ErrMismatch) {
			st.Close()
			return nil, err
		}
		fmt.Fprintln(stderr, "denco: saved progress doesn't match; checking files instead")
		o.existing = true
	}
	if o.existing {
		fmt.Fprintln(stdout, "Checking existing data…")
		have, err := pieces.Recheck(ctx, st, &mi.Info, nil)
		if err != nil {
			st.Close()
			return nil, err
		}
		fmt.Fprintf(stdout, "  %d / %d pieces already present\n", have.Count(), mi.Info.NumPieces())
		op.have = have
	}
	return op, nil
}

// listen opens the peer port, falling back to a random free port if the
// requested one is taken (e.g. another client is running).
// daemonRunning reports whether a daemon answers for this home directory.
func daemonRunning(ctx context.Context, e *env) bool {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_, err := ipc.Connect(ctx, e.daemonFile())
	return err == nil
}

func listen(port uint16, id peerid.ID, log *slog.Logger, stderr io.Writer) (*torrent.Listener, error) {
	opts := peer.Options{PeerID: id}
	ln, err := torrent.Listen(fmt.Sprintf(":%d", port), opts, log)
	if err == nil || port == 0 {
		return ln, err
	}
	fmt.Fprintf(stderr, "denco: port %d unavailable (%v); using a random port\n", port, err)
	return torrent.Listen(":0", opts, log)
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
