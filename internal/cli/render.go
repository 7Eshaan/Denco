package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/format"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// Rendering of the daemon's data for the terminal. Pure functions of their
// input, so tests can check the output without a daemon.

const maxNameWidth = 40

func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0) }

// stateLabel is the user-facing status word.
func stateLabel(s engine.State) string {
	if s == "" {
		return "Unknown"
	}
	return strings.ToUpper(string(s[:1])) + string(s[1:])
}

// short cuts s to n runes, marking the cut with "…".
func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// running reports whether a torrent has live speeds/peers to show.
func running(t engine.Torrent) bool {
	switch t.State {
	case engine.StateDownloading, engine.StateStalled, engine.StateSeeding:
		return true
	}
	return false
}

// renderList prints incomplete torrents first, then completed ones, as
// two tables with the same columns.
func renderList(w io.Writer, list []engine.Torrent) {
	if len(list) == 0 {
		fmt.Fprintln(w, "No torrents. Add one with: denco add <file.torrent>")
		return
	}
	var incomplete, complete []engine.Torrent
	for _, t := range list {
		if t.Complete {
			complete = append(complete, t)
		} else {
			incomplete = append(incomplete, t)
		}
	}
	section := func(title string, ts []engine.Torrent) {
		fmt.Fprintf(w, "%s (%d)\n", title, len(ts))
		tw := newTable(w)
		fmt.Fprintln(tw, "ID\tNAME\tSIZE\tPROGRESS\tSTATUS\tDOWN\tUP\tETA\tPEERS")
		for _, t := range ts {
			down, up, eta, peers := "-", "-", "-", "-"
			if running(t) {
				up, peers = format.Speed(t.UploadRate), fmt.Sprint(t.Peers)
				if !t.Complete {
					down, eta = format.Speed(t.DownloadRate), format.Duration(t.ETA)
				}
			}
			fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, short(t.Name, maxNameWidth),
				format.Bytes(t.TotalBytes), format.Percent(t.DoneBytes, t.TotalBytes), stateLabel(t.State),
				down, up, eta, peers)
		}
		tw.Flush()
	}
	if len(incomplete) > 0 {
		section("Downloading", incomplete)
	}
	if len(complete) > 0 {
		if len(incomplete) > 0 {
			fmt.Fprintln(w)
		}
		section("Completed", complete)
	}
	for _, t := range list {
		if t.Error != "" {
			fmt.Fprintf(w, "\n#%d failed: %s\n  ('denco resume %d' retries)\n", t.ID, t.Error, t.ID)
		}
	}
}

// renderInfo prints one torrent in the layout of the spec's example.
func renderInfo(w io.Writer, t engine.Torrent) {
	tw := newTable(w)
	row := func(k, v string) { fmt.Fprintf(tw, "%s:\t%s\n", k, v) }
	blank := func() { fmt.Fprintln(tw) }

	row("ID", fmt.Sprint(t.ID))
	row("Name", t.Name)
	row("Status", stateLabel(t.State))
	if t.Error != "" {
		row("Error", t.Error)
	}
	row("Info hash", t.InfoHash)
	row("Location", t.Dir)
	blank()
	row("Size", format.Bytes(t.TotalBytes))
	row("Downloaded", format.Bytes(t.DoneBytes))
	row("Progress", format.Percent(t.DoneBytes, t.TotalBytes))
	row("Pieces", fmt.Sprintf("%d / %d  (%s each)", t.PiecesHave, t.PiecesTotal, format.Bytes(t.PieceLength)))
	row("Files", fmt.Sprint(t.Files))
	blank()
	if running(t) {
		row("Download Speed", format.Speed(t.DownloadRate))
		row("Upload Speed", format.Speed(t.UploadRate))
		if !t.Complete {
			row("ETA", format.Duration(t.ETA))
		}
		blank()
		row("Connected Peers", fmt.Sprint(t.Peers))
		row("Connected Seeds", fmt.Sprint(t.Seeds))
		row("Connected Leechers", fmt.Sprint(t.Leechers))
		if t.TrackerSeeders >= 0 {
			row("Swarm (trackers)", fmt.Sprintf("%d seeds, %d leechers", t.TrackerSeeders, max(t.TrackerLeechers, 0)))
		}
		if t.TrackerError != "" {
			row("Trackers", "failing: "+firstLine(t.TrackerError))
		}
		blank()
	}
	row("Total downloaded", format.Bytes(t.Downloaded))
	row("Total uploaded", format.Bytes(t.Uploaded))
	if t.Private {
		row("Private", "yes")
	}
	row("Added", formatTime(t.AddedAt))
	if !t.CompletedAt.IsZero() {
		row("Completed", formatTime(t.CompletedAt))
	}
	tw.Flush()
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func renderFiles(w io.Writer, t engine.Torrent, files []engine.File) {
	fmt.Fprintf(w, "#%d %s — %d file%s in %s\n\n", t.ID, t.Name, len(files), plural(len(files)), t.Dir)
	tw := newTable(w)
	fmt.Fprintln(tw, "#\tSIZE\tDONE\tPATH")
	for _, f := range files {
		done := format.Percent(f.Done, f.Size)
		if f.Size == 0 {
			done = "100%"
		}
		note := ""
		if f.Renamed {
			note = "  (renamed for this OS)"
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s%s\n", f.Index, format.Bytes(f.Size), done, f.Path, note)
	}
	tw.Flush()
}

func renderPeers(w io.Writer, t engine.Torrent, peers []engine.Peer) {
	if !running(t) {
		fmt.Fprintf(w, "#%d %s is %s: no peers.\n", t.ID, t.Name, strings.ToLower(stateLabel(t.State)))
		return
	}
	fmt.Fprintf(w, "#%d %s — %d peer%s\n\n", t.ID, t.Name, len(peers), plural(len(peers)))
	if len(peers) == 0 {
		return
	}
	tw := newTable(w)
	fmt.Fprintln(tw, "ADDRESS\tCLIENT\tDIR\tHAS\tDOWN\tUP\tSTATUS")
	for _, p := range peers {
		dir := "out"
		if p.Incoming {
			dir = "in"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.1f%%\t%s\t%s\t%s\n", p.Addr, short(p.Client, 24), dir, p.Progress*100,
			format.Speed(p.DownloadRate), format.Speed(p.UploadRate), peerStatus(p))
	}
	tw.Flush()
}

// peerStatus describes both directions of a connection in words.
func peerStatus(p engine.Peer) string {
	var parts []string
	switch {
	case p.Interested && !p.PeerChoking:
		parts = append(parts, "downloading")
	case p.Interested:
		parts = append(parts, "choked by peer")
	}
	switch {
	case p.PeerWants && !p.AmChoking:
		parts = append(parts, "uploading")
	case p.PeerWants:
		parts = append(parts, "peer waiting")
	}
	if len(parts) == 0 {
		return "idle"
	}
	return strings.Join(parts, ", ")
}

func renderStatus(w io.Writer, st ipc.Status) {
	tw := newTable(w)
	row := func(k, v string) { fmt.Fprintf(tw, "%s:\t%s\n", k, v) }
	row("Daemon", fmt.Sprintf("running (pid %d), up %s", st.PID, format.Duration(time.Since(st.StartedAt))))
	row("Peer port", fmt.Sprint(st.ListenPort))
	row("Download dir", st.DownloadDir)
	row("State dir", st.StateDir)
	var counts []string
	for _, s := range []engine.State{engine.StateDownloading, engine.StateStalled, engine.StateChecking,
		engine.StateSeeding, engine.StateQueued, engine.StatePaused, engine.StateError} {
		if n := st.ByState[s]; n > 0 {
			counts = append(counts, fmt.Sprintf("%d %s", n, s))
		}
	}
	summary := fmt.Sprint(st.Torrents)
	if len(counts) > 0 {
		summary += " (" + strings.Join(counts, ", ") + ")"
	}
	row("Torrents", summary)
	row("Completed", fmt.Sprintf("%d of %d", st.Complete, st.Torrents))
	row("Max downloading", fmt.Sprint(st.MaxActive))
	row("Speed", fmt.Sprintf("↓ %s  ↑ %s", format.Speed(st.DownloadRate), format.Speed(st.UploadRate)))
	row("This session", fmt.Sprintf("↓ %s  ↑ %s", format.Bytes(st.Downloaded), format.Bytes(st.Uploaded)))
	row("Connected peers", fmt.Sprint(st.Peers))
	tw.Flush()
}
