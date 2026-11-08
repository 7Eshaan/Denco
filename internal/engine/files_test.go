package engine

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// Three files across 16 KiB pieces: file 0 = pieces 0-2, file 1 = pieces
// 2-5 (2 and 5 are shared border pieces), file 2 = pieces 5-7.
func selectionFixture(t *testing.T, tracker string) *fixture {
	return newFixture(t, "album", tracker, 16384, 40000, 50000, 30000)
}

func TestAddWithSkippedFile(t *testing.T) {
	tracker := newTracker(t)
	f := selectionFixture(t, tracker)
	tmp := t.TempDir()
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	a := openEngine(t, testConfig(filepath.Join(tmp, "sa"), seedDir))
	a.Add(f.raw, AddOptions{Existing: true})
	waitState(t, a, "1", StateSeeding)

	dl := filepath.Join(tmp, "dl")
	b := openEngine(t, testConfig(filepath.Join(tmp, "sb"), dl))
	for _, bad := range [][]int{{3}, {-1}, {0, 1, 2}} {
		if _, err := b.Add(f.raw, AddOptions{Skip: bad}); !errors.Is(err, ErrBadFile) {
			t.Errorf("Add with skip %v: err = %v, want ErrBadFile", bad, err)
		}
	}
	if len(b.List()) != 0 {
		t.Fatal("a refused add must not leave a torrent behind")
	}
	if _, err := b.AddMagnet("magnet:?xt=urn:btih:"+f.mi.InfoHash.String(), AddOptions{Skip: []int{1}}); !errors.Is(err, ErrBadFile) {
		t.Errorf("magnet with skip: err = %v", err)
	}

	if _, err := b.Add(f.raw, AddOptions{Skip: []int{1, 1}}); err != nil {
		t.Fatal(err)
	}
	v := waitState(t, b, "1", StateSeeding)
	if !v.Complete || v.SkippedFiles != 1 || v.WantedBytes != 70000 || v.DoneBytes != 70000 || v.TotalBytes != 120000 {
		t.Fatalf("view = %+v", v)
	}
	files, _ := b.Files("1")
	for i, file := range files {
		if file.Skipped != (i == 1) {
			t.Errorf("file %d skipped = %v", i, file.Skipped)
		}
		if i != 1 && file.Done != file.Size {
			t.Errorf("file %d done %d of %d", i, file.Done, file.Size)
		}
	}
	for _, i := range []int{0, 2} {
		got, err := os.ReadFile(f.path(dl, i))
		file := f.mi.Info.Files[i]
		if err != nil || string(got) != string(f.data[file.Offset:file.Offset+file.Length]) {
			t.Fatalf("file %d wrong on disk (err=%v)", i, err)
		}
	}

	// The selection is saved: after a restart it is still a finished
	// partial download.
	b.Close()
	b = openEngine(t, testConfig(filepath.Join(tmp, "sb"), dl))
	if v := waitState(t, b, "1", StateSeeding); v.SkippedFiles != 1 || !v.Complete {
		t.Fatalf("after restart: %+v", v)
	}

	// Wanting the file again restarts the torrent (a new session, so the
	// queue and trackers see a download again), which then finishes it.
	b.mu.Lock()
	oldSess := b.byID[1].sess
	b.mu.Unlock()
	files, err := b.SetFilesWanted("1", []int{1}, true)
	if err != nil || slices.ContainsFunc(files, func(f File) bool { return f.Skipped }) {
		t.Fatalf("SetFilesWanted = %v, %v", files, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := b.Get("1")
		b.mu.Lock()
		sess := b.byID[1].sess
		b.mu.Unlock()
		if v.State == StateSeeding && v.Complete && v.SkippedFiles == 0 && sess != nil && sess != oldSess {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never finished the re-wanted file: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.checkData(t, dl)
}

func TestSkipWhileDownloadingAndPartialSeed(t *testing.T) {
	tracker := newTracker(t)
	f := selectionFixture(t, tracker)
	tmp := t.TempDir()

	// The only seeder has file 1 damaged in the middle (pieces 3-4), so it
	// is a partial seed that skips file 1.
	seedDir := filepath.Join(tmp, "seed")
	f.writeData(t, seedDir)
	p := f.path(seedDir, 1)
	data, _ := os.ReadFile(p)
	clear(data[10000:40000])
	os.WriteFile(p, data, 0o644)
	a := openEngine(t, testConfig(filepath.Join(tmp, "sa"), seedDir))
	if _, err := a.Add(f.raw, AddOptions{Existing: true, Skip: []int{1}}); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, a, "1", StateSeeding); !v.Complete || v.PiecesHave != 6 {
		t.Fatalf("partial seed: %+v", v)
	}

	// B wants everything, so it gets stuck without pieces 3-4.
	dl := filepath.Join(tmp, "dl")
	b := openEngine(t, testConfig(filepath.Join(tmp, "sb"), dl))
	b.Add(f.raw, AddOptions{})
	deadline := time.Now().Add(10 * time.Second)
	for {
		v, _ := b.Get("1")
		if v.PiecesHave == 6 {
			if v.Complete || v.State == StateSeeding {
				t.Fatalf("complete without file 1: %+v", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("B never got the 6 available pieces: %+v", v)
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Skipping file 1 while it runs finishes the download in place.
	if _, err := b.SetFilesWanted("1", []int{1}, false); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, b, "1", StateSeeding); !v.Complete || v.SkippedFiles != 1 {
		t.Fatalf("after skipping: %+v", v)
	}
	if _, err := b.SetFilesWanted("1", []int{0, 2}, false); !errors.Is(err, ErrBadFile) {
		t.Errorf("skipping every file: err = %v", err)
	}
	if _, err := b.SetFilesWanted("1", []int{5}, true); !errors.Is(err, ErrBadFile) {
		t.Errorf("bad index: err = %v", err)
	}

	// Wanting it again: a download that can't finish (nobody has 3-4).
	b.SetFilesWanted("1", []int{1}, true)
	v := waitState(t, b, "1", StateDownloading, StateStalled)
	if v.Complete || v.SkippedFiles != 0 {
		t.Fatalf("after wanting it again: %+v", v)
	}
}
