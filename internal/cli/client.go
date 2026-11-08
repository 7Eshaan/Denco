package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/ipc"
	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// Client commands: each connects to the daemon, makes one or a few API
// calls, prints the result and exits.

// connect finds the running daemon, or explains how to start one. On
// failure it returns a nil client and the exit code to use.
func connect(ctx context.Context, e *env) (*ipc.Client, int) {
	c, err := ipc.Connect(ctx, e.daemonFile())
	if errors.Is(err, ipc.ErrNoDaemon) {
		fmt.Fprintln(e.stderr, "denco: the daemon is not running. Start it with: denco daemon")
		return nil, 1
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return nil, 1
	}
	return c, 0
}

func interruptible() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt)
}

func cmdAdd(e *env, args []string) int {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	dir := fs.String("dir", "", "download directory (default: the daemon's)")
	existing := fs.Bool("existing", false, "reuse data already in the directory (hash-checked first)")
	paused := fs.Bool("paused", false, "add without starting")
	skip := fs.String("skip", "", "don't download these files (numbers as in 'denco inspect', e.g. 2,4-6)")
	only := fs.String("only", "", "download only these files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(e.stderr, "usage: denco add [-dir DIR] [-existing] [-paused] [-skip FILES | -only FILES] <file.torrent|magnet|url>...")
		return 2
	}
	selecting := *skip != "" || *only != ""
	if selecting && fs.NArg() != 1 {
		fmt.Fprintln(e.stderr, "denco: -skip and -only apply to one torrent at a time")
		return 2
	}
	absDir := ""
	if *dir != "" {
		var err error
		if absDir, err = filepath.Abs(*dir); err != nil { // the daemon has its own working dir
			fmt.Fprintf(e.stderr, "denco: %v\n", err)
			return 1
		}
	}
	ctx, stop := interruptible()
	defer stop()
	c, code := connect(ctx, e)
	if c == nil {
		return code
	}

	status := 0
	for i, arg := range fs.Args() {
		if i > 0 {
			fmt.Fprintln(e.stdout)
		}
		req, nfiles, err := addRequest(ctx, e, arg)
		if err == nil && selecting {
			if nfiles == 0 {
				err = fmt.Errorf("a magnet link has no file list yet; once its metadata arrives use: denco files <id> skip <files>")
			} else {
				req.Skip, err = skipFromFlags(*skip, *only, nfiles)
			}
		}
		if err != nil {
			fmt.Fprintf(e.stderr, "denco: %s: %v\n", short(arg, 60), err)
			status = 1
			continue
		}
		req.Dir, req.Existing, req.Paused = absDir, *existing, *paused
		t, err := c.Add(ctx, req)
		if err != nil {
			fmt.Fprintf(e.stdout, "  ✗ %v\n", err)
			status = 1
			continue
		}
		fmt.Fprintf(e.stdout, "  Added as #%d → %s (%s)\n", t.ID, t.Dir, stateLabel(t.State))
		if t.SkippedFiles > 0 {
			fmt.Fprintf(e.stdout, "  Downloading %s\n", selectedSize(t.WantedBytes, t.TotalBytes, t.SkippedFiles))
		}
	}
	return status
}

// addRequest turns one `denco add` argument — a .torrent path, an
// HTTP(S) URL of one, or a magnet link — into a request, printing what it
// is first (the spec: show metadata before starting). nfiles is the
// torrent's file count, 0 for a magnet link (not known yet).
func addRequest(ctx context.Context, e *env, arg string) (req ipc.AddRequest, nfiles int, err error) {
	var raw []byte
	switch {
	case magnet.IsMagnet(arg):
		req, err = magnetRequest(e, arg)
		return req, 0, err
	case isURL(arg):
		data, magnetURI, err := fetchTorrentURL(ctx, arg)
		if err != nil {
			return req, 0, err
		}
		if magnetURI != "" {
			fmt.Fprintln(e.stdout, "(the link points to a magnet link)")
			req, err = magnetRequest(e, magnetURI)
			return req, 0, err
		}
		raw = data
	default:
		data, err := os.ReadFile(arg)
		if err != nil {
			return req, 0, err
		}
		raw = data
	}
	mi, err := metainfo.Parse(raw) // validate and show it before sending
	if err != nil {
		return req, 0, err
	}
	printSummary(e.stdout, mi)
	return ipc.AddRequest{Torrent: raw}, len(mi.Info.Files), nil
}

func magnetRequest(e *env, uri string) (ipc.AddRequest, error) {
	m, err := magnet.Parse(uri)
	if err != nil {
		return ipc.AddRequest{}, err
	}
	name := m.Name
	if name == "" {
		name = "(no name in the link)"
	}
	fmt.Fprintf(e.stdout, "%s\n  magnet link · %s · %d tracker%s", name, m.InfoHash, len(m.Trackers), plural(len(m.Trackers)))
	if len(m.Peers) > 0 {
		fmt.Fprintf(e.stdout, " · %d peer%s", len(m.Peers), plural(len(m.Peers)))
	}
	fmt.Fprintln(e.stdout, "\n  files and size are known once the metadata arrives from peers")
	if len(m.Trackers) == 0 && len(m.Peers) == 0 {
		fmt.Fprintln(e.stdout, "  no trackers or peers in this link: peers must come from the DHT (enable_dht)")
	}
	return ipc.AddRequest{Magnet: m.String()}, nil
}

func isURL(s string) bool {
	s = strings.ToLower(s)
	return strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://")
}

func cmdList(e *env, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(e.stderr, "usage: denco list")
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	c, code := connect(ctx, e)
	if c == nil {
		return code
	}
	list, err := c.List(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	renderList(e.stdout, list)
	return 0
}

// oneRef parses "<command> <id>" for the single-torrent views.
func oneRef(e *env, name string, args []string) (string, bool) {
	if len(args) != 1 {
		fmt.Fprintf(e.stderr, "usage: denco %s <id>\n", name)
		return "", false
	}
	return args[0], true
}

func cmdInfo(e *env, args []string) int {
	ref, ok := oneRef(e, "info", args)
	if !ok {
		return 2
	}
	return withClient(e, func(ctx context.Context, c *ipc.Client) error {
		t, err := c.Get(ctx, ref)
		if err == nil {
			renderInfo(e.stdout, t)
		}
		return err
	})
}

func cmdPeers(e *env, args []string) int {
	ref, ok := oneRef(e, "peers", args)
	if !ok {
		return 2
	}
	return withClient(e, func(ctx context.Context, c *ipc.Client) error {
		t, err := c.Get(ctx, ref)
		if err != nil {
			return err
		}
		peers, err := c.Peers(ctx, ref)
		if err == nil {
			renderPeers(e.stdout, t, peers)
		}
		return err
	})
}

func cmdStatus(e *env, args []string) int {
	if len(args) != 0 {
		fmt.Fprintln(e.stderr, "usage: denco status")
		return 2
	}
	return withClient(e, func(ctx context.Context, c *ipc.Client) error {
		st, err := c.Status(ctx)
		if err == nil {
			renderStatus(e.stdout, st)
		}
		return err
	})
}

// withClient connects, runs f, and turns its error into an exit code.
func withClient(e *env, f func(context.Context, *ipc.Client) error) int {
	ctx, stop := interruptible()
	defer stop()
	c, code := connect(ctx, e)
	if c == nil {
		return code
	}
	if err := f(ctx, c); err != nil {
		fmt.Fprintf(e.stderr, "denco: %v\n", err)
		return 1
	}
	return 0
}

// pauseFlags parses pause/resume's flags; -uploads means "only uploads".
func pauseFlags(e *env, name string, args []string) (uploads bool, refs []string, ok bool) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	up := fs.Bool("uploads", false, "only uploads: keep downloading, stop sending data to peers")
	if err := fs.Parse(args); err != nil {
		return false, nil, false
	}
	return *up, fs.Args(), true
}

func cmdPause(e *env, args []string) int {
	uploads, refs, ok := pauseFlags(e, "pause", args)
	if !ok {
		return 2
	}
	if uploads {
		return forEach(e, "pause -uploads", refs, func(ctx context.Context, c *ipc.Client, ref string) (string, error) {
			t, err := c.SetUploadsPaused(ctx, ref, true)
			return fmt.Sprintf("Uploads paused for #%d %s (still downloading if incomplete)", t.ID, t.Name), err
		})
	}
	return forEach(e, "pause", refs, func(ctx context.Context, c *ipc.Client, ref string) (string, error) {
		t, err := c.Pause(ctx, ref)
		return fmt.Sprintf("Paused #%d %s", t.ID, t.Name), err
	})
}

func cmdResume(e *env, args []string) int {
	uploads, refs, ok := pauseFlags(e, "resume", args)
	if !ok {
		return 2
	}
	if uploads {
		return forEach(e, "resume -uploads", refs, func(ctx context.Context, c *ipc.Client, ref string) (string, error) {
			t, err := c.SetUploadsPaused(ctx, ref, false)
			return fmt.Sprintf("Uploads resumed for #%d %s", t.ID, t.Name), err
		})
	}
	return forEach(e, "resume", refs, func(ctx context.Context, c *ipc.Client, ref string) (string, error) {
		t, err := c.Resume(ctx, ref)
		return fmt.Sprintf("Resuming #%d %s (%s)", t.ID, t.Name, strings.ToLower(stateLabel(t.State))), err
	})
}

// cmdSeed sets torrents' seeding mode.
func cmdSeed(e *env, args []string) int {
	if len(args) < 2 || !slices.Contains([]string{"on", "off", "default"}, args[0]) {
		fmt.Fprintln(e.stderr, `usage: denco seed on|off|default <id>... | all
  on       seed until you stop it (ignores seed_ratio / seed_time)
  off      don't seed: stop now if complete, else as soon as it completes
  default  follow seed_ratio / seed_time (see 'denco config')`)
		return 2
	}
	mode := args[0]
	return forEach(e, "seed "+mode, args[1:], func(ctx context.Context, c *ipc.Client, ref string) (string, error) {
		t, err := c.SetSeedMode(ctx, ref, mode)
		return fmt.Sprintf("#%d %s: seeding %s — %s", t.ID, t.Name, t.SeedPolicy, strings.ToLower(stateLabel(t.State))), err
	})
}

func cmdRemove(e *env, args []string) int {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	del := fs.Bool("delete", false, "also delete the torrent's downloaded files")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 || slices.Contains(fs.Args(), "all") {
		fmt.Fprintln(e.stderr, "usage: denco remove [-delete] <id>...")
		return 2
	}
	return forEach(e, "remove", fs.Args(), func(ctx context.Context, c *ipc.Client, ref string) (string, error) {
		r, err := c.Remove(ctx, ref, *del)
		what := "files kept in " + r.Dir
		switch {
		case r.FilesDeleted:
			what = "files deleted"
		case *del:
			what = "nothing deleted: it never started, so no files there are its own"
		}
		return fmt.Sprintf("Removed #%d %s (%s)", r.ID, r.Name, what), err
	})
}

// forEach applies f to every torrent reference given ("all" = every
// torrent), reporting each result. The exit code is 1 if any failed.
func forEach(e *env, name string, refs []string, f func(context.Context, *ipc.Client, string) (string, error)) int {
	if len(refs) == 0 {
		fmt.Fprintf(e.stderr, "usage: denco %s <id>... | all\n", name)
		return 2
	}
	ctx, stop := interruptible()
	defer stop()
	c, code := connect(ctx, e)
	if c == nil {
		return code
	}
	if len(refs) == 1 && refs[0] == "all" {
		list, err := c.List(ctx)
		if err != nil {
			fmt.Fprintf(e.stderr, "denco: %v\n", err)
			return 1
		}
		refs = refs[:0]
		for _, t := range list {
			refs = append(refs, strconv.Itoa(t.ID))
		}
	}
	status := 0
	for _, ref := range refs {
		msg, err := f(ctx, c, ref)
		if err != nil {
			fmt.Fprintf(e.stderr, "denco: %s: %v\n", ref, err)
			status = 1
			continue
		}
		fmt.Fprintln(e.stdout, msg)
	}
	return status
}

// printSummary shows a torrent's metadata before it is added.
func printSummary(w io.Writer, mi *metainfo.MetaInfo) {
	info := &mi.Info
	fmt.Fprintf(w, "%s\n", info.Name)
	fmt.Fprintf(w, "  %s · %d file%s · %d piece%s × %s · %s\n", format.Bytes(info.TotalLength),
		len(info.Files), plural(len(info.Files)), info.NumPieces(), plural(info.NumPieces()),
		format.Bytes(info.PieceLength), mi.InfoHash)
	const maxShown = 5
	if info.MultiFile {
		for i, f := range info.Files {
			if i == maxShown {
				fmt.Fprintf(w, "    … %d more (see 'denco files')\n", len(info.Files)-maxShown)
				break
			}
			fmt.Fprintf(w, "    %3d  %10s  %s\n", i+1, format.Bytes(f.Length), f.DisplayPath(info))
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
