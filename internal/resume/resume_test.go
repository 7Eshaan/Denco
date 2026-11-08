package resume

import (
	"context"
	"crypto/sha1"
	"errors"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/storage"
)

const pl = 1024

// fixture: a 3-file torrent fully written to disk under dir.
type fixture struct {
	mi     *metainfo.MetaInfo
	data   []byte
	layout *storage.Layout
	st     *storage.Storage
}

func newFixture(t *testing.T, sizes ...int) *fixture {
	t.Helper()
	total := 0
	for _, n := range sizes {
		total += n
	}
	data := make([]byte, total)
	r := rand.New(rand.NewPCG(9, 9))
	for i := range data {
		data[i] = byte(r.Uint32())
	}
	var hashes []byte
	for off := 0; off < total; off += pl {
		h := sha1.Sum(data[off:min(off+pl, total)])
		hashes = append(hashes, h[:]...)
	}
	var files []any
	for i, n := range sizes {
		files = append(files, map[string]any{"length": n, "path": []any{string(rune('a' + i))}})
	}
	raw, _ := bencode.Encode(map[string]any{"info": map[string]any{
		"name": "t", "piece length": pl, "pieces": string(hashes), "files": files,
	}})
	mi, err := metainfo.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	layout, err := storage.NewLayout(t.TempDir(), &mi.Info)
	if err != nil {
		t.Fatal(err)
	}
	st, err := storage.Open(layout, storage.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if _, err := st.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	st.Sync()
	return &fixture{mi: mi, data: data, layout: layout, st: st}
}

func (f *fixture) state(clean bool) *State {
	have := bitfield.New(f.mi.Info.NumPieces())
	have.SetAll()
	return &State{
		InfoHash:    f.mi.InfoHash.String(),
		Name:        f.mi.Info.Name,
		DownloadDir: f.layout.Root,
		Pieces:      f.mi.Info.NumPieces(),
		Have:        have.Bytes(),
		Clean:       clean,
		Files:       Stamp(f.layout),
		AddedAt:     time.Now(),
	}
}

func (f *fixture) restore(t *testing.T, st *State) (*bitfield.Bitfield, Report) {
	t.Helper()
	have, rep, err := Restore(context.Background(), st, f.mi, f.layout, f.st)
	if err != nil {
		t.Fatal(err)
	}
	return have, rep
}

// bump changes a file's mtime as if something else had written it.
func bump(t *testing.T, path string) {
	t.Helper()
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}

// --- Store ----------------------------------------------------------------------

func TestStoreRoundTrip(t *testing.T) {
	f := newFixture(t, 3000)
	s, err := OpenStore(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	st := f.state(true)
	st.Uploaded, st.Downloaded = 123, 456
	if err := s.Save(st, f.mi.Raw); err != nil {
		t.Fatal(err)
	}
	got, mi, err := s.Load(f.mi.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if mi.InfoHash != f.mi.InfoHash || got.Uploaded != 123 || got.Downloaded != 456 ||
		got.DownloadDir != f.layout.Root || string(got.Have) != string(st.Have) || !got.Clean {
		t.Fatalf("round trip lost data: %+v", got)
	}
	if got.Version != FormatVersion || got.SavedAt.IsZero() {
		t.Fatal("version/saved_at not set")
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(s.Dir())
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".tmp-") {
			t.Errorf("leftover temp file %s", e.Name())
		}
	}

	// A later save only needs the state (torrent already stored).
	st.Uploaded = 999
	if err := s.Save(st, nil); err != nil {
		t.Fatal(err)
	}
	got, _, _ = s.Load(f.mi.InfoHash)
	if got.Uploaded != 999 {
		t.Fatal("second save not visible")
	}

	if err := s.Delete(f.mi.InfoHash); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Load(f.mi.InfoHash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := s.Delete(f.mi.InfoHash); err != nil {
		t.Fatalf("deleting twice should be fine: %v", err)
	}
}

func TestStoreErrors(t *testing.T) {
	f := newFixture(t, 2000)
	s, _ := OpenStore(t.TempDir())
	if err := s.Save(&State{InfoHash: "short"}, f.mi.Raw); err == nil {
		t.Error("bad hash accepted")
	}
	if err := s.Save(f.state(false), nil); err == nil {
		t.Error("first save without torrent bytes accepted")
	}
	if _, _, err := s.Load(f.mi.InfoHash); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}

	s.Save(f.state(false), f.mi.Raw)
	jsonPath := filepath.Join(s.Dir(), f.mi.InfoHash.String()+".json")
	os.WriteFile(jsonPath, []byte("{not json"), 0o600)
	if _, _, err := s.Load(f.mi.InfoHash); err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("corrupt json: %v", err)
	}
	os.WriteFile(jsonPath, []byte(`{"version": 99}`), 0o600)
	if _, _, err := s.Load(f.mi.InfoHash); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("future version: %v", err)
	}
}

func TestStoreList(t *testing.T) {
	s, _ := OpenStore(t.TempDir())
	a, b := newFixture(t, 1500), newFixture(t, 2500)
	sa, sb := a.state(false), b.state(false)
	sb.AddedAt = time.Now().Add(-time.Hour) // b added first
	s.Save(sa, a.mi.Raw)
	s.Save(sb, b.mi.Raw)
	// A broken entry must not hide the others.
	os.WriteFile(filepath.Join(s.Dir(), strings.Repeat("0", 40)+".json"), []byte("junk"), 0o600)
	os.WriteFile(filepath.Join(s.Dir(), "notes.txt"), []byte("ignored"), 0o600)

	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("got %d entries", len(list))
	}
	if list[0].Meta.InfoHash != b.mi.InfoHash || list[1].Meta.InfoHash != a.mi.InfoHash {
		t.Error("not sorted oldest first")
	}
	if list[2].Err == nil {
		t.Error("broken entry should carry an error")
	}
}

func TestDefaultDir(t *testing.T) {
	d, err := DefaultDir()
	if err != nil {
		t.Skip("no user config dir here:", err)
	}
	if !strings.HasSuffix(d, filepath.Join("denco", "resume")) {
		t.Errorf("DefaultDir = %s", d)
	}
}

// --- Restore -------------------------------------------------------------------

func TestRestoreTrustsUntouchedFiles(t *testing.T) {
	f := newFixture(t, 2500, 100, 3000)
	have, rep := f.restore(t, f.state(true))
	if !have.Full() || rep.Rechecked != 0 || rep.Dropped != 0 {
		t.Fatalf("have=%d/%d rep=%+v", have.Count(), have.Len(), rep)
	}
}

func TestRestoreAfterCrashDoesNotRecheck(t *testing.T) {
	f := newFixture(t, 2500, 3000)
	st := f.state(false) // last save was periodic: we crashed afterwards
	bump(t, f.layout.Files[0].Path)
	have, rep := f.restore(t, st)
	if !have.Full() || rep.Rechecked != 0 {
		t.Fatalf("crash case should trust the bitfield: have=%d rep=%+v", have.Count(), rep)
	}
}

func TestRestoreRechecksExternallyModifiedFile(t *testing.T) {
	f := newFixture(t, 2500, 3000) // pieces: file a = 0..2, file b = 2..5
	st := f.state(true)
	// Someone edits a byte of file b while Denco is stopped.
	pb := f.layout.Files[1].Path
	data, _ := os.ReadFile(pb)
	data[2000] ^= 0xff // torrent offset 2500+2000 = 4500 → piece 4
	os.WriteFile(pb, data, 0o644)
	bump(t, pb)

	have, rep := f.restore(t, st)
	if have.Has(4) {
		t.Error("modified piece 4 still trusted")
	}
	for _, p := range []int{0, 1, 2, 3, 5} {
		if !have.Has(p) {
			t.Errorf("good piece %d dropped", p)
		}
	}
	// Only file b's pieces (2..5) were rechecked, not file a's.
	if rep.Rechecked != 4 || rep.Failed != 1 || len(rep.Notes) != 1 {
		t.Fatalf("rep = %+v", rep)
	}
}

func TestRestoreMissingFileDropsItsPieces(t *testing.T) {
	f := newFixture(t, 2500, 3000)
	st := f.state(false)
	os.Remove(f.layout.Files[1].Path)
	have, rep := f.restore(t, st)
	// File a covers pieces 0..2 but piece 2 straddles into missing b.
	if !have.Has(0) || !have.Has(1) || have.Has(2) || have.Has(3) || have.Has(5) {
		t.Fatalf("have = %08b", have.Bytes())
	}
	if rep.Dropped != 4 || rep.Kept != 2 {
		t.Fatalf("rep = %+v", rep)
	}
}

func TestRestoreTruncatedFile(t *testing.T) {
	f := newFixture(t, 5000)
	st := f.state(false)
	os.Truncate(f.layout.Files[0].Path, 2100) // pieces 0,1 complete; 2 partly
	have, _ := f.restore(t, st)
	if !have.Has(0) || !have.Has(1) || have.Has(2) || have.Has(4) {
		t.Fatalf("have = %08b", have.Bytes())
	}
}

func TestRestoreGrownFileIsRechecked(t *testing.T) {
	// Even after a crash, a file longer than the torrent says can't be ours alone.
	f := newFixture(t, 2048)
	st := f.state(false)
	fh, _ := os.OpenFile(f.layout.Files[0].Path, os.O_APPEND|os.O_WRONLY, 0)
	fh.Write([]byte("extra"))
	fh.Close()
	_, rep := f.restore(t, st)
	if rep.Rechecked != 2 || rep.Failed != 0 {
		t.Fatalf("rep = %+v", rep)
	}
}

func TestRestorePartialState(t *testing.T) {
	f := newFixture(t, 5000)
	st := f.state(true)
	partial := bitfield.New(f.mi.Info.NumPieces())
	partial.Set(1)
	partial.Set(3)
	st.Have = partial.Bytes()
	have, rep := f.restore(t, st)
	if have.Count() != 2 || !have.Has(1) || !have.Has(3) || rep.Saved != 2 {
		t.Fatalf("have=%08b rep=%+v", have.Bytes(), rep)
	}
}

func TestRestoreMismatch(t *testing.T) {
	f := newFixture(t, 3000)
	other := newFixture(t, 4000)
	for name, mod := range map[string]func(*State){
		"other torrent": func(s *State) { s.InfoHash = other.mi.InfoHash.String() },
		"piece count":   func(s *State) { s.Pieces++ },
		"file count":    func(s *State) { s.Files = s.Files[:0] },
		"bitfield size": func(s *State) { s.Have = append(s.Have, 0) },
	} {
		st := f.state(true)
		mod(st)
		if _, _, err := Restore(context.Background(), st, f.mi, f.layout, f.st); !errors.Is(err, ErrMismatch) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

func TestStampMissingFile(t *testing.T) {
	f := newFixture(t, 3000)
	os.Remove(f.layout.Files[0].Path)
	if s := Stamp(f.layout); s[0].Size != -1 {
		t.Fatalf("stamp = %+v", s[0])
	}
}

// --- Saver -------------------------------------------------------------------------

func TestSaverAccumulatesTotalsAndSkipsNoops(t *testing.T) {
	f := newFixture(t, 3000)
	store, _ := OpenStore(t.TempDir())
	prev := f.state(true)
	prev.Uploaded, prev.Downloaded = 1000, 2000
	prev.AddedAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	sv := NewSaver(store, f.mi, f.layout, f.st, prev)
	have := bitfield.New(f.mi.Info.NumPieces())
	have.Set(0)
	if err := sv.Save(Snapshot{Have: have, Uploaded: 5, Downloaded: 7}, false); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Load(f.mi.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if got.Uploaded != 1005 || got.Downloaded != 2007 || got.Clean || !got.AddedAt.Equal(prev.AddedAt) {
		t.Fatalf("state = %+v", got)
	}
	if !got.CompletedAt.IsZero() {
		t.Fatal("CompletedAt set before completion")
	}

	// Identical snapshot: no write (SavedAt unchanged).
	before := got.SavedAt
	time.Sleep(5 * time.Millisecond)
	sv.Save(Snapshot{Have: have, Uploaded: 5, Downloaded: 7}, false)
	got, _, _ = store.Load(f.mi.InfoHash)
	if !got.SavedAt.Equal(before) {
		t.Error("no-op save rewrote the file")
	}

	// Completion and a clean final save.
	have.SetAll()
	sv.Save(Snapshot{Have: have, Uploaded: 5, Downloaded: 7}, true)
	got, _, _ = store.Load(f.mi.InfoHash)
	if got.CompletedAt.IsZero() || !got.Clean {
		t.Fatalf("final state = %+v", got)
	}
	// Stamps reflect the files at save time.
	if got.Files[0].Size != 3000 {
		t.Fatalf("stamp = %+v", got.Files[0])
	}
}

func TestSaverPausedAndState(t *testing.T) {
	f := newFixture(t, 3000)
	store, _ := OpenStore(t.TempDir())
	prev := f.state(true)
	prev.ID = 7
	sv := NewSaver(store, f.mi, f.layout, f.st, prev)
	if sv.State().InfoHash != "" {
		t.Fatal("State before the first save should be zero")
	}
	have := bitfield.New(f.mi.Info.NumPieces())
	snap := Snapshot{Have: have}
	if err := sv.Save(snap, false); err != nil {
		t.Fatal(err)
	}
	// Same snapshot, but the paused flag changed: must be written.
	sv.SetPaused(true)
	if err := sv.Save(snap, false); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.Load(f.mi.InfoHash)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Paused || got.ID != 7 {
		t.Fatalf("saved state = %+v, want paused with ID 7", got)
	}
	if st := sv.State(); !st.Paused || st.ID != 7 || !st.SavedAt.Equal(got.SavedAt) {
		t.Fatalf("State() = %+v, want what was written", st)
	}
}

func TestSaverRun(t *testing.T) {
	f := newFixture(t, 2000)
	store, _ := OpenStore(t.TempDir())
	sv := NewSaver(store, f.mi, f.layout, f.st, nil)
	have := bitfield.New(f.mi.Info.NumPieces())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var n int64
	go func() {
		sv.Run(ctx, 10*time.Millisecond, func() Snapshot {
			n++ // changes every call, so every tick writes
			return Snapshot{Have: have, Uploaded: n}
		}, nil)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if st, _, err := store.Load(f.mi.InfoHash); err == nil && st.Uploaded >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("periodic saves not happening")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
}

func TestStoreMagnetWithoutMetadata(t *testing.T) {
	store, _ := OpenStore(t.TempDir())
	f := newFixture(t, 3000)
	hash := f.mi.InfoHash.String()
	st := &State{InfoHash: hash, Name: "pending", Magnet: "magnet:?xt=urn:btih:" + hash}
	if err := store.Save(st, nil); err != nil {
		t.Fatalf("magnet state without .torrent: %v", err)
	}
	got, mi, err := store.Load(f.mi.InfoHash)
	if err != nil || mi != nil || got.Magnet == "" || got.Name != "pending" {
		t.Fatalf("load = %+v, %v, %v", got, mi, err)
	}
	entries, _ := store.List()
	if len(entries) != 1 || entries[0].Err != nil || entries[0].Meta != nil {
		t.Fatalf("list = %+v", entries)
	}
	// Metadata arrives: the .torrent is written and Magnet cleared.
	st.Magnet = ""
	st.Pieces = f.mi.Info.NumPieces()
	if err := store.Save(st, f.mi.Raw); err != nil {
		t.Fatal(err)
	}
	if _, mi, err := store.Load(f.mi.InfoHash); err != nil || mi == nil || mi.InfoHash != f.mi.InfoHash {
		t.Fatalf("after metadata: %v, %v", mi, err)
	}
	// A non-magnet state still needs the .torrent bytes.
	if err := store.Save(&State{InfoHash: strings.Repeat("ab", 20)}, nil); err == nil {
		t.Fatal("state without torrent or magnet accepted")
	}
}
