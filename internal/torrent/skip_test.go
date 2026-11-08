package torrent

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/pieces"
)

func TestSkippedFilesAreNotDownloaded(t *testing.T) {
	// A = [0, 5pl+100): pieces 0-5, piece 5 shared with B.
	// B = pieces 5-8, C = pieces 8-12.
	f := makeTorrent(t, pl, 5*pl+100, 3*pl, 4*pl)
	sd := newSeeder(t, f, seederOpts{})
	tr := newTracker(t, sd.addr)
	cfg := testConfig()
	cfg.Wanted = pieces.Wanted(&f.mi.Info, []bool{true}) // skip A

	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, cfg, false)
	r.waitComplete(t, 20*time.Second)

	for i := 0; i <= 4; i++ {
		if sd.wasRequested(i) {
			t.Errorf("piece %d belongs only to the skipped file but was requested", i)
		}
	}
	if !sd.wasRequested(5) {
		t.Error("border piece 5 is needed for file B")
	}
	have := r.s.Have()
	for i := 0; i < have.Len(); i++ {
		if have.Has(i) != (i >= 5) {
			t.Errorf("have piece %d = %v", i, have.Has(i))
		}
	}
	// B and C are complete; A holds only the border piece's bytes.
	info := &f.mi.Info
	for i, file := range info.Files {
		p := filepath.Join(append([]string{r.dir, info.Name}, file.Path...)...)
		got, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("file %d: %v", i, err)
		}
		from := file.Offset
		if i == 0 {
			from = 5 * pl // only the border piece was written
		}
		if int64(len(got)) != file.Length || !bytes.Equal(got[from-file.Offset:], f.data[from:file.Offset+file.Length]) {
			t.Errorf("file %d: wrong content", i)
		}
	}

	st := r.s.Stats()
	if st.State != StateSeeding || st.BytesLeft != 0 || st.WantedBytes != int64(len(f.data))-5*pl {
		t.Errorf("stats = %+v", st)
	}
	if err := r.stopAndWait(t); err != nil {
		t.Fatal(err)
	}
	// A partial seed is not a seed: trackers must not hear "completed".
	// Its "left" stays the skipped file's missing pieces (0-4).
	ev, lefts := tr.eventLog(), tr.leftLog()
	if slices.Contains(ev, "completed") || ev[len(ev)-1] != "stopped" {
		t.Errorf("tracker events = %v", ev)
	}
	if want := strconv.Itoa(5 * pl); lefts[len(lefts)-1] != want {
		t.Errorf("left in the final announce = %s, want %s (all lefts %v)", lefts[len(lefts)-1], want, lefts)
	}
}

func TestSetWantedCompletesAndReopens(t *testing.T) {
	f := makeTorrent(t, pl, 4*pl, 4*pl, 4*pl) // files = pieces 0-3, 4-7, 8-11
	sd := newSeeder(t, f, seederOpts{pieces: []int{0, 1, 2, 3, 4, 5, 6, 7}})
	tr := newTracker(t, sd.addr)
	r := start(t, f.withTracker(t, tr.srv.URL), t.TempDir(), nil, testConfig(), false)

	// Nobody has file C, so the download can't finish.
	deadline := time.Now().Add(10 * time.Second)
	for r.s.Stats().PiecesHave < 8 {
		if time.Now().After(deadline) {
			t.Fatalf("stats = %+v", r.s.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case <-r.s.Completed():
		t.Fatal("completed without file C")
	case <-time.After(100 * time.Millisecond):
	}

	// Skipping C finishes it.
	r.s.SetWanted(pieces.Wanted(&f.mi.Info, []bool{false, false, true}))
	r.waitComplete(t, 5*time.Second)
	if st := r.s.Stats(); st.State != StateSeeding || st.BytesLeft != 0 {
		t.Errorf("after skipping C: %+v", r.s.Stats())
	}

	// Wanting C again makes it a download again (Completed stays closed).
	r.s.SetWanted(nil)
	deadline = time.Now().Add(5 * time.Second)
	for st := r.s.Stats(); st.State == StateSeeding || st.BytesLeft != 4*pl; st = r.s.Stats() {
		if time.Now().After(deadline) {
			t.Fatalf("after wanting C again: %+v", st)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
