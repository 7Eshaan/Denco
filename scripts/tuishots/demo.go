package main

import (
	"context"
	"errors"
	"net/netip"
	"strconv"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/ipc"
)

// demo is a pretend daemon with a believable mix of torrents, all freely
// shareable (Linux images, Blender's open movies).
type demo struct{}

const (
	mb = 1_000_000
	gb = 1_000_000_000
)

var started = time.Now().Add(-(3*time.Hour + 12*time.Minute))

func torrents() []engine.Torrent {
	base := func(id int, name string, st engine.State, size, done int64) engine.Torrent {
		return engine.Torrent{
			ID: id, Name: name, State: st, HasMetadata: true, Files: 1,
			TotalBytes: size, WantedBytes: size, DoneBytes: done, Complete: done == size,
			Ratio: -1, ETA: -1, TrackerSeeders: -1, TrackerLeechers: -1, PieceLength: 262144,
			Dir: "/Users/you/Downloads/Denco", AddedAt: started.Add(-24 * time.Hour),
		}
	}
	t1 := base(1, "debian-13.2.0-amd64-netinst.iso", engine.StateSeeding, 754*mb, 754*mb)
	t1.UploadRate, t1.Peers, t1.Leechers, t1.Ratio = 312_000, 6, 6, 1.84

	t2 := base(2, "ubuntu-24.04.3-desktop-amd64.iso", engine.StateDownloading, 6_200*mb, 4_018*mb)
	t2.DownloadRate, t2.UploadRate, t2.ETA, t2.Peers, t2.Seeds, t2.Leechers, t2.Ratio =
		8_400_000, 410_000, 4*time.Minute+20*time.Second, 47, 39, 8, 0.07

	t3 := sintel()

	t4 := base(4, "archlinux-2026.11.01-x86_64.iso", engine.StateMetadata, 0, 0)
	t4.HasMetadata, t4.Complete, t4.Peers, t4.MetaHave, t4.MetaTotal = false, false, 4, 2, 3

	t5 := base(5, "Fedora-Workstation-Live-43-x86_64.iso", engine.StatePaused, 2_300*mb, 506*mb)
	t5.Ratio = 0.11

	t6 := base(6, "linuxmint-22.2-cinnamon-64bit.iso", engine.StateQueued, 2_900*mb, 0)

	t7 := base(7, "Big Buck Bunny (4K, 60 fps)", engine.StateFinished, 1_100*mb, 1_100*mb)
	t7.Ratio = 2.00

	t8 := base(8, "openSUSE-Tumbleweed-DVD-x86_64-Current.iso", engine.StateStalled, 4_700*mb, 4_286*mb)
	t8.Ratio = 0.31
	all := []engine.Torrent{t1, t2, t3, t4, t5, t6, t7, t8}
	queued := 0
	for _, t := range all {
		if !t.Complete {
			queued++
		}
	}
	n := 0
	for i := range all { // queue positions in list order (D58)
		if !all[i].Complete {
			n++
			all[i].Queue, all[i].QueueLength = n, queued
		}
	}
	return all
}

// sintel is the multi-file torrent shown in the details screenshot.
func sintel() engine.Torrent {
	return engine.Torrent{
		ID: 3, Name: "Sintel (2010) [1080p]", InfoHash: "08ada5a7a6183aae1e09d831df6748d566095a10",
		Dir: "/Users/you/Downloads/Denco", State: engine.StateDownloading, HasMetadata: true,
		Files: 7, SkippedFiles: 1, PieceLength: 262144, PiecesHave: 1912, PiecesTotal: 5101,
		TotalBytes: 1_337 * mb, WantedBytes: 1_239 * mb, DoneBytes: 465 * mb,
		DownloadRate: 2_100_000, UploadRate: 96_000, ETA: 6*time.Minute + 8*time.Second,
		Downloaded: 497 * mb, Uploaded: 18 * mb, Ratio: 0.04,
		Peers: 18, Seeds: 9, Leechers: 9, TrackerSeeders: 34, TrackerLeechers: 12, DHTPeers: 21,
		SeedPolicy: "until stopped (no limits set)", AddedAt: started.Add(-20 * time.Minute),
	}
}

func (demo) Status(context.Context) (ipc.Status, error) {
	st := ipc.Status{PID: 48213}
	st.StartedAt = started
	st.Torrents, st.Peers, st.DHT, st.DHTNodes, st.PEX = 8, 75, true, 186, true
	st.DownloadRate, st.UploadRate = 10_500_000, 818_000
	st.ByState = map[engine.State]int{}
	for _, t := range torrents() {
		st.ByState[t.State]++
	}
	return st, nil
}

func (demo) List(context.Context) ([]engine.Torrent, error) { return torrents(), nil }

func (demo) Get(_ context.Context, ref string) (engine.Torrent, error) {
	for _, t := range torrents() {
		if strconv.Itoa(t.ID) == ref {
			return t, nil
		}
	}
	return engine.Torrent{}, &ipc.Error{Code: ipc.CodeNotFound, Message: "no such torrent"}
}

func (demo) Files(context.Context, string) ([]engine.File, error) {
	return []engine.File{
		{Index: 0, Path: "Sintel/Sintel.2010.1080p.mkv", Size: 1_228 * mb, Done: 454 * mb},
		{Index: 1, Path: "Sintel/Subtitles/sintel_en.srt", Size: 4_100, Done: 4_100},
		{Index: 2, Path: "Sintel/Subtitles/sintel_de.srt", Size: 4_300, Done: 4_300},
		{Index: 3, Path: "Sintel/Subtitles/sintel_es.srt", Size: 4_200, Done: 4_200},
		{Index: 4, Path: "Sintel/Subtitles/sintel_fr.srt", Size: 4_400, Done: 0},
		{Index: 5, Path: "Sintel/Extras/poster.jpg", Size: 11 * mb, Done: 11 * mb},
		{Index: 6, Path: "Sintel/Extras/making-of.mp4", Size: 98 * mb, Skipped: true},
	}, nil
}

func (demo) Peers(context.Context, string) ([]engine.Peer, error) {
	p := func(addr, client string, prog float64, down, up int64) engine.Peer {
		return engine.Peer{Addr: netip.MustParseAddrPort(addr), Client: client, Progress: prog,
			DownloadRate: down, UploadRate: up, Interested: true, PeerWants: up > 0}
	}
	return []engine.Peer{
		p("198.51.100.23:51413", "Transmission 4.0.6.0", 1, 640_000, 0),
		p("203.0.113.77:6881", "qBittorrent 5.0.2.0", 1, 512_000, 31_000),
	}, nil
}

var errDemo = errors.New("the demo daemon doesn't change anything")

func (demo) Pause(context.Context, string) (engine.Torrent, error)  { return engine.Torrent{}, errDemo }
func (demo) Resume(context.Context, string) (engine.Torrent, error) { return engine.Torrent{}, errDemo }
func (demo) ForceStart(context.Context, string) (engine.Torrent, error) {
	return engine.Torrent{}, errDemo
}
func (demo) MoveInQueue(context.Context, string, string) (engine.Torrent, error) {
	return engine.Torrent{}, errDemo
}
func (demo) SetTorrentLimits(context.Context, string, int64, int64) (engine.Torrent, error) {
	return engine.Torrent{}, errDemo
}
func (demo) Remove(context.Context, string, bool) (engine.Removed, error) {
	return engine.Removed{}, errDemo
}
func (demo) AddTorrent(context.Context, string) (engine.Torrent, error) {
	return engine.Torrent{}, errDemo
}
