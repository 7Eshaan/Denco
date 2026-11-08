package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metadata"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/resume"
	"github.com/7Eshaan/Denco/internal/storage"
	"github.com/7Eshaan/Denco/internal/tracker"
)

// Torrents added by magnet link start without metadata (no file list, no
// piece hashes). Their run goroutine first fetches the metadata from peers
// (metadata.Fetcher), turns it into a .torrent, saves it, and then carries
// on exactly like a torrent added from a file. See DECISIONS D38.

// ErrNoMetadata: the torrent's metadata hasn't been fetched yet.
var ErrNoMetadata = errors.New("metadata not downloaded yet")

// AddMagnet adds a torrent by magnet link. Like Add it refuses duplicates;
// the files are checked against existing ones once the metadata arrives.
func (e *Engine) AddMagnet(uri string, opts AddOptions) (Torrent, error) {
	m, err := magnet.Parse(uri)
	if err != nil {
		return Torrent{}, err
	}
	if len(opts.Skip) > 0 {
		return Torrent{}, fmt.Errorf("%w: a magnet link has no file list yet; skip files once its metadata has arrived", ErrBadFile)
	}
	dir := opts.Dir
	if dir == "" {
		dir = e.cfg.DownloadDir
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return Torrent{}, ErrClosed
	}
	if old := e.byHash[m.InfoHash]; old != nil {
		if old.removing {
			return Torrent{}, ErrBusy
		}
		return Torrent{}, &DuplicateError{ID: old.id, Name: old.name()}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Torrent{}, fmt.Errorf("download directory: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Torrent{}, err
	}
	t := &entry{
		id: e.nextID, hash: m.InfoHash, paused: opts.Paused,
		state: resume.State{
			ID:          e.nextID,
			InfoHash:    m.InfoHash.String(),
			Name:        m.Name,
			DownloadDir: abs,
			AddedAt:     time.Now().UTC(),
			Paused:      opts.Paused,
			Fresh:       true,
			Existing:    opts.Existing,
			Magnet:      m.String(),
		},
	}
	if err := e.store.Save(&t.state, nil); err != nil {
		return Torrent{}, err
	}
	e.nextID++
	e.byID[t.id] = t
	e.byHash[t.hash] = t
	e.log.Info("magnet added", "id", t.id, "torrent", t.name())
	e.scheduleLocked()
	return e.viewLocked(t), nil
}

// fetchMetadata gets a magnet torrent's info dict from peers and makes the
// entry a normal torrent. It returns the updated state.
func (e *Engine) fetchMetadata(ctx context.Context, t *entry, state resume.State) (resume.State, error) {
	m, err := magnet.Parse(state.Magnet)
	if err != nil {
		return state, err
	}
	tiers := tracker.TiersFromMagnet(m)
	fcfg := metadata.Config{
		PeerOpts:  e.peerOpts(),
		Announcer: e.cfg.Session.Announcer,
		Port:      e.ln.Port(),
		PEX:       e.cfg.Session.PEX,
		Retry:     e.cfg.Session.AnnounceRetry, // 0 = the fetcher's default
		Logger:    e.log,
	}
	if e.dht != nil {
		fcfg.DHT = e.dht
	}
	f := metadata.NewFetcher(m.InfoHash, tiers, m.Peers, fcfg)
	e.mu.Lock()
	t.fetcher = f
	e.mu.Unlock()
	info, err := f.Run(ctx)
	e.mu.Lock()
	t.fetcher = nil
	t.bootstrap = f.Peers()
	e.mu.Unlock()
	if err != nil {
		return state, err
	}

	raw, err := torrentFile(info, tiers)
	if err != nil {
		return state, err
	}
	mi, err := metainfo.Parse(raw) // the hash matched; now check it's a usable torrent
	if err != nil {
		return state, fmt.Errorf("received metadata: %w", err)
	}
	layout, err := storage.NewLayout(state.DownloadDir, &mi.Info)
	if err != nil {
		return state, err
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	t.mi, t.layout = mi, layout
	t.have = bitfield.New(mi.Info.NumPieces())
	t.state.Name = mi.Info.Name
	t.state.DownloadDir = layout.Root
	t.state.Pieces = mi.Info.NumPieces()
	t.state.Have = t.have.Bytes()
	t.state.Magnet = ""
	if err := e.store.Save(&t.state, raw); err != nil {
		return state, err
	}
	e.log.Info("metadata received", "id", t.id, "torrent", mi.Info.Name, "files", len(mi.Info.Files))
	state.Name, state.DownloadDir, state.Pieces, state.Have, state.Magnet =
		t.state.Name, t.state.DownloadDir, t.state.Pieces, t.state.Have, ""
	return state, nil
}

// torrentFile wraps fetched metadata in a .torrent with the magnet's
// trackers, so the torrent can be saved and restored like any other.
func torrentFile(info []byte, tiers [][]string) ([]byte, error) {
	root := map[string]any{"info": bencode.Raw(info), "created by": "Denco (from magnet link)"}
	if len(tiers) > 0 {
		list := make([]any, len(tiers))
		for i, tier := range tiers {
			list[i] = tier
		}
		root["announce"] = tiers[0][0]
		root["announce-list"] = list
	}
	return bencode.Encode(root)
}

// name is what the user sees for a torrent: its name, or for a magnet
// without a name (no "dn") the start of its info hash.
func (t *entry) name() string {
	switch {
	case t.mi != nil:
		return t.mi.Info.Name
	case t.state.Name != "":
		return t.state.Name
	default:
		return "magnet " + t.hash.String()[:12] + "…"
	}
}

// raw is the .torrent to save with the state (nil before metadata).
func (t *entry) raw() []byte {
	if t.mi == nil {
		return nil
	}
	return t.mi.Raw
}

func hashFromHex(s string) (metainfo.Hash, error) {
	var h metainfo.Hash
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != len(h) {
		return h, fmt.Errorf("bad info hash %q", s)
	}
	copy(h[:], b)
	return h, nil
}
