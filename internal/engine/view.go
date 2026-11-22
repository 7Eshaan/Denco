package engine

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/portmap"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/torrent"
)

// State is what a torrent is doing, as shown to the user.
type State string

const (
	StateMetadata    State = "metadata"    // magnet link: fetching the torrent's metadata from peers
	StateChecking    State = "checking"    // opening files / verifying saved progress
	StateDownloading State = "downloading" // incomplete, connected to peers
	StateStalled     State = "stalled"     // incomplete, no peers connected
	StateSeeding     State = "seeding"     // complete and uploading
	StateQueued      State = "queued"      // waiting for a download slot (MaxActive)
	StatePaused      State = "paused"
	StateError       State = "error"    // stopped by an error; Resume retries
	StateFinished    State = "finished" // complete and done seeding (limit reached, or seeding off)
)

// Torrent is a snapshot of one torrent for display. Byte counts are
// payload bytes; Downloaded/Uploaded are lifetime totals.
type Torrent struct {
	ID       int    `json:"id"`
	InfoHash string `json:"info_hash"`
	Name     string `json:"name"`
	Dir      string `json:"dir"` // download directory
	State    State  `json:"state"`
	Error    string `json:"error,omitempty"`
	// RetryAt: when a failed torrent is retried on its own (zero = it
	// isn't; it waits for the user); Retries: failures in a row (D47).
	RetryAt     time.Time `json:"retry_at,omitempty"`
	Retries     int       `json:"retries,omitempty"`
	Complete    bool      `json:"complete"`
	Private     bool      `json:"private"`
	Files       int       `json:"files"`
	PieceLength int64     `json:"piece_length"`
	// HasMetadata is false for a magnet link whose metadata hasn't arrived;
	// sizes, pieces and files are then unknown (zero). MetaHave/MetaTotal
	// count metadata pieces while fetching.
	HasMetadata bool `json:"has_metadata"`
	MetaHave    int  `json:"meta_have,omitempty"`
	MetaTotal   int  `json:"meta_total,omitempty"`

	TotalBytes   int64 `json:"total_bytes"`             // the whole torrent
	WantedBytes  int64 `json:"wanted_bytes"`            // the files not skipped (= TotalBytes if none are)
	DoneBytes    int64 `json:"done_bytes"`              // of WantedBytes, covered by verified pieces
	SkippedFiles int   `json:"skipped_files,omitempty"` // files the user chose not to download
	PiecesHave   int   `json:"pieces_have"`
	PiecesTotal  int   `json:"pieces_total"`

	DownloadRate int64         `json:"download_rate"` // bytes/s
	UploadRate   int64         `json:"upload_rate"`
	Downloaded   int64         `json:"downloaded"`
	Uploaded     int64         `json:"uploaded"`
	ETA          time.Duration `json:"eta"` // 0 when done, negative when unknown

	// Seeding (D45). Ratio is uploaded ÷ downloaded (-1 = nothing yet);
	// SeedingTime is lifetime. SeedPolicy says in words how long it seeds;
	// SeedNote why a finished torrent stopped.
	Ratio         float64       `json:"ratio"`
	SeedingTime   time.Duration `json:"seeding_time"`
	SeedMode      string        `json:"seed_mode"` // default, on, off
	SeedPolicy    string        `json:"seed_policy"`
	SeedNote      string        `json:"seed_note,omitempty"`
	UploadsPaused bool          `json:"uploads_paused,omitempty"`
	// Download queue (D58): Queue is the position among the incomplete
	// torrents, from 1 (0 = complete, not queued), of QueueLength; Forced
	// torrents start even when max_active_downloads are running.
	Queue       int  `json:"queue,omitempty"`
	QueueLength int  `json:"queue_length,omitempty"`
	Forced      bool `json:"forced,omitempty"`
	// The torrent's own speed limits in bytes/s (0 = none), on top of the
	// daemon's total limits (D59).
	DownLimit int64 `json:"down_limit,omitempty"`
	UpLimit   int64 `json:"up_limit,omitempty"`

	Peers           int    `json:"peers"`
	Seeds           int    `json:"seeds"`
	Leechers        int    `json:"leechers"`
	TrackerSeeders  int    `json:"tracker_seeders"` // -1 if unknown
	TrackerLeechers int    `json:"tracker_leechers"`
	TrackerError    string `json:"tracker_error,omitempty"`
	DHTPeers        int    `json:"dht_peers"` // found by the last DHT lookup

	AddedAt     time.Time `json:"added_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// File is one file of a torrent with its download progress.
type File struct {
	Index   int    `json:"index"`
	Path    string `json:"path"` // relative to the download directory, slash-separated
	Size    int64  `json:"size"`
	Done    int64  `json:"done"` // bytes covered by verified pieces
	Skipped bool   `json:"skipped,omitempty"`
	Renamed bool   `json:"renamed,omitempty"`
}

// Peer is one connected peer.
type Peer struct {
	Addr         netip.AddrPort `json:"addr"`
	Client       string         `json:"client"`
	Incoming     bool           `json:"incoming"`
	Progress     float64        `json:"progress"` // 0..1
	DownloadRate int64          `json:"download_rate"`
	UploadRate   int64          `json:"upload_rate"`
	Downloaded   int64          `json:"downloaded"`
	Uploaded     int64          `json:"uploaded"`
	PeerChoking  bool           `json:"peer_choking"`    // they choke us
	Interested   bool           `json:"interested"`      // we want their data
	AmChoking    bool           `json:"am_choking"`      // we choke them
	PeerWants    bool           `json:"peer_interested"` // they want our data
}

// Overview is the whole client's status.
type Overview struct {
	StartedAt   time.Time `json:"started_at"`
	ListenPort  uint16    `json:"listen_port"`
	DownloadDir string    `json:"download_dir"`
	StateDir    string    `json:"state_dir"`
	MaxActive   int       `json:"max_active"`
	MaxPeers    int       `json:"max_peers"`      // per torrent
	DownLimit   int64     `json:"download_limit"` // bytes/s, 0 = unlimited
	UpLimit     int64     `json:"upload_limit"`
	PEX         bool      `json:"pex"` // peer exchange on (never used for private torrents)
	DHT         bool      `json:"dht"` // DHT node running
	DHTPort     uint16    `json:"dht_port,omitempty"`
	DHTNodes    int       `json:"dht_nodes"`      // in its routing table
	DHTGood     int       `json:"dht_good_nodes"` // of which answered recently
	// IPv6 (D60): enable_ipv6, and the IPv6 DHT node (BEP 32) if it runs.
	IPv6      bool            `json:"ipv6"`
	DHT6      bool            `json:"dht6"`
	DHT6Port  uint16          `json:"dht6_port,omitempty"`
	DHT6Nodes int             `json:"dht6_nodes"`
	DHT6Good  int             `json:"dht6_good_nodes"`
	PortMap   *portmap.Status `json:"port_forwarding,omitempty"` // nil = off
	SeedRatio float64         `json:"seed_ratio"`                // global seeding limits; 0 = none
	SeedTime  time.Duration   `json:"seed_time"`
	Torrents  int             `json:"torrents"`
	ByState   map[State]int   `json:"by_state"`
	Complete  int             `json:"complete"`

	DownloadRate int64 `json:"download_rate"`
	UploadRate   int64 `json:"upload_rate"`
	Downloaded   int64 `json:"downloaded"` // payload since the daemon started
	Uploaded     int64 `json:"uploaded"`
	Peers        int   `json:"peers"`
}

// List returns every torrent, in queue order (D58).
func (e *Engine) List() []Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Torrent
	q := e.queueRanksLocked()
	for _, t := range e.queueOrderLocked() {
		if !t.removing {
			out = append(out, e.view(t, q))
		}
	}
	return out
}

// Get returns one torrent by ID or info hash prefix.
func (e *Engine) Get(ref string) (Torrent, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return Torrent{}, err
	}
	return e.viewLocked(t), nil
}

// Files lists a torrent's files with per-file progress.
func (e *Engine) Files(ref string) ([]File, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return nil, err
	}
	if t.mi == nil {
		return nil, fmt.Errorf("%s: %w", t.name(), ErrNoMetadata)
	}
	return e.filesLocked(t), nil
}

func (e *Engine) filesLocked(t *entry) []File {
	have := t.haveLocked()
	skip := t.skipLocked()
	out := make([]File, len(t.layout.Files))
	for i, f := range t.layout.Files {
		out[i] = File{Index: i, Path: f.Rel, Size: f.Length, Done: fileDone(&t.mi.Info, have, f),
			Skipped: i < len(skip) && skip[i], Renamed: f.Renamed}
	}
	return out
}

// Peers lists a running torrent's connected peers, fastest first.
func (e *Engine) Peers(ref string) ([]Peer, error) {
	e.mu.Lock()
	t, err := e.resolveLocked(ref)
	sess := (*torrent.Session)(nil)
	if err == nil {
		sess = t.sess
	}
	e.mu.Unlock()
	if err != nil || sess == nil {
		return nil, err
	}
	var out []Peer
	for _, p := range sess.Peers() {
		out = append(out, Peer{
			Addr: p.Addr, Client: p.Client, Incoming: !p.Outgoing, Progress: p.Progress,
			DownloadRate: p.DownloadRate, UploadRate: p.UploadRate,
			Downloaded: p.Downloaded, Uploaded: p.Uploaded,
			PeerChoking: p.Choking, Interested: p.Interested,
			AmChoking: p.AmChoking, PeerWants: p.PeerInterest,
		})
	}
	return out, nil
}

// Overview summarises the whole client.
func (e *Engine) Overview() Overview {
	e.mu.Lock()
	defer e.mu.Unlock()
	o := Overview{
		StartedAt: e.started, ListenPort: e.ln.Port(), MaxActive: e.cfg.MaxActive,
		MaxPeers: e.cfg.Session.MaxPeers, DownLimit: e.down.Rate(), UpLimit: e.up.Rate(), PEX: e.cfg.Session.PEX,
		DownloadDir: e.cfg.DownloadDir, StateDir: e.store.Dir(),
		SeedRatio: e.seedRatio, SeedTime: e.seedTime,
		ByState:    map[State]int{},
		Downloaded: e.finished.down, Uploaded: e.finished.up,
	}
	o.IPv6 = e.cfg.EnableIPv6
	if e.dht != nil {
		ds := e.dht.V4.Stats()
		o.DHT, o.DHTPort, o.DHTNodes, o.DHTGood = true, e.dht.Port(), ds.Nodes, ds.GoodNodes
		if v6 := e.dht.V6; v6 != nil {
			ds := v6.Stats()
			o.DHT6, o.DHT6Port, o.DHT6Nodes, o.DHT6Good = true, v6.Port(), ds.Nodes, ds.GoodNodes
		}
	}
	if e.portmap != nil {
		st := e.portmap.Status()
		o.PortMap = &st
	}
	q := e.queueRanksLocked()
	for _, t := range e.byID {
		if t.removing {
			continue
		}
		v := e.view(t, q)
		o.Torrents++
		o.ByState[v.State]++
		if v.Complete {
			o.Complete++
		}
		o.DownloadRate += v.DownloadRate
		o.UploadRate += v.UploadRate
		o.Peers += v.Peers
		if t.sess != nil {
			st := t.sess.Stats()
			o.Downloaded += st.Downloaded
			o.Uploaded += st.Uploaded
		}
	}
	return o
}

// haveLocked is the best current knowledge of verified pieces.
func (t *entry) haveLocked() *bitfield.Bitfield {
	if t.sess != nil {
		return t.sess.Have()
	}
	return t.have
}

func (e *Engine) viewLocked(t *entry) Torrent { return e.view(t, e.queueRanksLocked()) }

// view builds t's snapshot; q (nil = leave the queue fields empty) is
// queueRanksLocked, computed once when many torrents are shown.
func (e *Engine) view(t *entry, q *queueRanks) Torrent {
	v := Torrent{
		ID: t.id, InfoHash: t.hash.String(), Name: t.name(), Dir: t.state.DownloadDir,
		Downloaded: t.state.Downloaded, Uploaded: t.state.Uploaded,
		TrackerSeeders: -1, TrackerLeechers: -1, ETA: -1,
		AddedAt: t.state.AddedAt, CompletedAt: t.state.CompletedAt,
		Ratio: -1, SeedingTime: t.seedingTimeLocked(), SeedMode: modeName(t.seedMode),
		SeedPolicy: e.seedPolicyLocked(t), UploadsPaused: t.uploadsPaused, Forced: t.forced,
		DownLimit: t.downLim.Rate(), UpLimit: t.upLim.Rate(),
	}
	if q != nil && q.rank[t] > 0 {
		v.Queue, v.QueueLength = q.rank[t], q.total
	}
	if t.mi == nil { // magnet link without metadata
		if t.fetcher != nil {
			fs := t.fetcher.Status()
			v.Peers, v.MetaHave, v.MetaTotal, v.TrackerError = fs.Peers, fs.Have, fs.Total, fs.TrackerError
		}
		switch {
		case t.err != nil:
			v.State, v.Error, v.RetryAt, v.Retries = StateError, t.err.Error(), t.retryAt, t.retries
		case t.paused:
			v.State = StatePaused
		case t.running:
			v.State = StateMetadata
		default:
			v.State = StateQueued
		}
		return v
	}
	info := &t.mi.Info
	v.HasMetadata, v.Dir = true, t.layout.Root
	v.Private, v.Files, v.PieceLength = info.Private, len(info.Files), info.PieceLength
	v.TotalBytes, v.PiecesTotal = info.TotalLength, info.NumPieces()
	have := t.haveLocked()
	v.WantedBytes, v.DoneBytes, v.SkippedFiles = selection(info, t.layout, have, t.skipLocked())
	v.PiecesHave = have.Count()
	if t.sess != nil {
		st := t.sess.Stats()
		v.DownloadRate, v.UploadRate = st.DownloadRate, st.UploadRate
		v.Downloaded += st.Downloaded // lifetime = saved totals + this run
		v.Uploaded += st.Uploaded
		v.ETA = st.ETA()
		v.Peers, v.Seeds, v.Leechers = st.Peers, st.Seeds, st.Leechers
		v.TrackerSeeders, v.TrackerLeechers = st.TrackerSeeders, st.TrackerLeechers
		v.TrackerError = st.TrackerError
		v.DHTPeers = st.DHTPeers
	}
	v.Complete = have.Covers(t.wantLocked())
	if v.Complete {
		v.ETA = 0
	}
	v.Ratio = ratio(v.Uploaded, v.Downloaded, v.DoneBytes)

	switch {
	case t.err != nil:
		v.State, v.Error, v.RetryAt, v.Retries = StateError, t.err.Error(), t.retryAt, t.retries
	case t.paused:
		v.State = StatePaused // even while it is still shutting down
	case t.finished:
		v.State = StateFinished // also while it is still shutting down
		v.SeedNote = seedStopReason(t.seedMode, v.Ratio, v.SeedingTime, e.seedRatio, e.seedTime)
	case t.checking:
		v.State = StateChecking
	case t.sess != nil:
		v.State = sessionState(t.sess.Stats().State)
	case t.running:
		v.State = StateChecking // between check and session start
	default:
		v.State = StateQueued
	}
	return v
}

func sessionState(s torrent.State) State {
	switch s {
	case torrent.StateSeeding, torrent.StateComplete:
		return StateSeeding
	case torrent.StateStalled:
		return StateStalled
	default:
		return StateDownloading
	}
}

// selection sums the wanted files: their size, how much of it verified
// pieces cover, and how many files are skipped. Counting by file (not by
// piece) keeps a border piece's bytes in a skipped file out of the totals.
func selection(info *metainfo.Info, layout *storage.Layout, have *bitfield.Bitfield, skip []bool) (wanted, done int64, skipped int) {
	for i, f := range layout.Files {
		if i < len(skip) && skip[i] {
			skipped++
			continue
		}
		wanted += f.Length
		done += fileDone(info, have, f)
	}
	return wanted, done, skipped
}

// fileDone counts the bytes of f covered by verified pieces.
func fileDone(info *metainfo.Info, have *bitfield.Bitfield, f storage.File) int64 {
	if f.Length == 0 {
		return 0
	}
	end := f.Offset + f.Length
	var n int64
	for p := int(f.Offset / info.PieceLength); int64(p)*info.PieceLength < end; p++ {
		if !have.Has(p) {
			continue
		}
		start := int64(p) * info.PieceLength
		n += min(end, start+info.PieceSize(p)) - max(f.Offset, start)
	}
	return n
}

// String is a one-line description, handy in logs and errors.
func (t Torrent) String() string { return fmt.Sprintf("#%d %s", t.ID, t.Name) }
