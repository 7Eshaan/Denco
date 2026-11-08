package engine

import (
	"fmt"
	"net/netip"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
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
	StateError       State = "error" // stopped by an error; Resume retries
)

// Torrent is a snapshot of one torrent for display. Byte counts are
// payload bytes; Downloaded/Uploaded are lifetime totals.
type Torrent struct {
	ID          int    `json:"id"`
	InfoHash    string `json:"info_hash"`
	Name        string `json:"name"`
	Dir         string `json:"dir"` // download directory
	State       State  `json:"state"`
	Error       string `json:"error,omitempty"`
	Complete    bool   `json:"complete"`
	Private     bool   `json:"private"`
	Files       int    `json:"files"`
	PieceLength int64  `json:"piece_length"`
	// HasMetadata is false for a magnet link whose metadata hasn't arrived;
	// sizes, pieces and files are then unknown (zero). MetaHave/MetaTotal
	// count metadata pieces while fetching.
	HasMetadata bool `json:"has_metadata"`
	MetaHave    int  `json:"meta_have,omitempty"`
	MetaTotal   int  `json:"meta_total,omitempty"`

	TotalBytes  int64 `json:"total_bytes"`
	DoneBytes   int64 `json:"done_bytes"` // verified
	PiecesHave  int   `json:"pieces_have"`
	PiecesTotal int   `json:"pieces_total"`

	DownloadRate int64         `json:"download_rate"` // bytes/s
	UploadRate   int64         `json:"upload_rate"`
	Downloaded   int64         `json:"downloaded"`
	Uploaded     int64         `json:"uploaded"`
	ETA          time.Duration `json:"eta"` // 0 when done, negative when unknown

	Peers           int    `json:"peers"`
	Seeds           int    `json:"seeds"`
	Leechers        int    `json:"leechers"`
	TrackerSeeders  int    `json:"tracker_seeders"` // -1 if unknown
	TrackerLeechers int    `json:"tracker_leechers"`
	TrackerError    string `json:"tracker_error,omitempty"`

	AddedAt     time.Time `json:"added_at"`
	CompletedAt time.Time `json:"completed_at,omitempty"`
}

// File is one file of a torrent with its download progress.
type File struct {
	Index   int    `json:"index"`
	Path    string `json:"path"` // relative to the download directory, slash-separated
	Size    int64  `json:"size"`
	Done    int64  `json:"done"` // bytes covered by verified pieces
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
	StartedAt   time.Time     `json:"started_at"`
	ListenPort  uint16        `json:"listen_port"`
	DownloadDir string        `json:"download_dir"`
	StateDir    string        `json:"state_dir"`
	MaxActive   int           `json:"max_active"`
	MaxPeers    int           `json:"max_peers"`      // per torrent
	DownLimit   int64         `json:"download_limit"` // bytes/s, 0 = unlimited
	UpLimit     int64         `json:"upload_limit"`
	PEX         bool          `json:"pex"` // peer exchange on (never used for private torrents)
	Torrents    int           `json:"torrents"`
	ByState     map[State]int `json:"by_state"`
	Complete    int           `json:"complete"`

	DownloadRate int64 `json:"download_rate"`
	UploadRate   int64 `json:"upload_rate"`
	Downloaded   int64 `json:"downloaded"` // payload since the daemon started
	Uploaded     int64 `json:"uploaded"`
	Peers        int   `json:"peers"`
}

// List returns every torrent, by ID.
func (e *Engine) List() []Torrent {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Torrent
	for _, t := range e.sortedLocked() {
		if !t.removing {
			out = append(out, e.viewLocked(t))
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
	have := t.haveLocked()
	out := make([]File, len(t.layout.Files))
	for i, f := range t.layout.Files {
		out[i] = File{Index: i, Path: f.Rel, Size: f.Length, Done: fileDone(&t.mi.Info, have, f), Renamed: f.Renamed}
	}
	return out, nil
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
		ByState:    map[State]int{},
		Downloaded: e.finished.down, Uploaded: e.finished.up,
	}
	for _, t := range e.byID {
		if t.removing {
			continue
		}
		v := e.viewLocked(t)
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

func (e *Engine) viewLocked(t *entry) Torrent {
	v := Torrent{
		ID: t.id, InfoHash: t.hash.String(), Name: t.name(), Dir: t.state.DownloadDir,
		Downloaded: t.state.Downloaded, Uploaded: t.state.Uploaded,
		TrackerSeeders: -1, TrackerLeechers: -1, ETA: -1,
		AddedAt: t.state.AddedAt, CompletedAt: t.state.CompletedAt,
	}
	if t.mi == nil { // magnet link without metadata
		if t.fetcher != nil {
			fs := t.fetcher.Status()
			v.Peers, v.MetaHave, v.MetaTotal, v.TrackerError = fs.Peers, fs.Have, fs.Total, fs.TrackerError
		}
		switch {
		case t.err != nil:
			v.State, v.Error = StateError, t.err.Error()
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
	if t.sess != nil {
		st := t.sess.Stats()
		v.DoneBytes = st.TotalBytes - st.BytesLeft
		v.PiecesHave = st.PiecesHave
		v.DownloadRate, v.UploadRate = st.DownloadRate, st.UploadRate
		v.Downloaded += st.Downloaded // lifetime = saved totals + this run
		v.Uploaded += st.Uploaded
		v.ETA = st.ETA()
		v.Peers, v.Seeds, v.Leechers = st.Peers, st.Seeds, st.Leechers
		v.TrackerSeeders, v.TrackerLeechers = st.TrackerSeeders, st.TrackerLeechers
		v.TrackerError = st.TrackerError
	} else {
		v.PiecesHave = t.have.Count()
		v.DoneBytes = doneBytes(info, t.have)
	}
	v.Complete = v.PiecesHave == v.PiecesTotal
	if v.Complete {
		v.ETA = 0
	}

	switch {
	case t.err != nil:
		v.State, v.Error = StateError, t.err.Error()
	case t.paused:
		v.State = StatePaused // even while it is still shutting down
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

func doneBytes(info *metainfo.Info, have *bitfield.Bitfield) int64 {
	var n int64
	for i := 0; i < info.NumPieces(); i++ {
		if have.Has(i) {
			n += info.PieceSize(i)
		}
	}
	return n
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
