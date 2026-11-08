package storage

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openLayout(t *testing.T, root string, opts Options, files ...any) *Storage {
	t.Helper()
	l, err := NewLayout(root, mkInfo("t", files...))
	if err != nil {
		t.Fatal(err)
	}
	s, err := Open(l, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestWriteReadAcrossFiles(t *testing.T) {
	root := t.TempDir()
	// Files of 3, 0, 5 and 2 bytes: writes must split across boundaries and
	// skip the empty file.
	s := openLayout(t, root, Options{}, "a", 3, "empty", 0, "sub/b", 5, "c", 2)

	data := []byte("0123456789")
	if n, err := s.WriteAt(data, 0); err != nil || n != 10 {
		t.Fatalf("WriteAt = %d, %v", n, err)
	}
	for rel, want := range map[string]string{"a": "012", "empty": "", "sub/b": "34567", "c": "89"} {
		got, err := os.ReadFile(filepath.Join(root, "t", filepath.FromSlash(rel)))
		if err != nil || string(got) != want {
			t.Errorf("%s = %q (%v), want %q", rel, got, err, want)
		}
	}

	// Read a range straddling a, b and c.
	buf := make([]byte, 8)
	if _, err := s.ReadAt(buf, 1); err != nil || string(buf) != "12345678" {
		t.Fatalf("ReadAt = %q, %v", buf, err)
	}
	// Partial overwrite in the middle.
	if _, err := s.WriteAt([]byte("XY"), 4); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ReadAt(buf, 1); err != nil || string(buf) != "123XY678" {
		t.Fatalf("after overwrite ReadAt = %q, %v", buf, err)
	}
}

func TestOutOfOrderWritesMakeSparseFile(t *testing.T) {
	root := t.TempDir()
	s := openLayout(t, root, Options{}, "", 20) // single 20-byte file
	if _, err := s.WriteAt([]byte("end"), 17); err != nil {
		t.Fatal(err)
	}
	if _, err := s.WriteAt([]byte("start"), 0); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "t"))
	if len(got) != 20 || string(got[:5]) != "start" || string(got[17:]) != "end" {
		t.Fatalf("file = %q", got)
	}
}

func TestReadMissingFile(t *testing.T) {
	s := openLayout(t, t.TempDir(), Options{}, "a", 4, "b", 4)
	s.WriteAt([]byte("aaaa"), 0)
	buf := make([]byte, 4)
	if _, err := s.ReadAt(buf, 4); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file read err = %v", err)
	}
	// File exists but is short (only partly written).
	s.WriteAt([]byte("b"), 4)
	if _, err := s.ReadAt(buf, 4); err == nil {
		t.Fatal("short file read should fail")
	}
}

func TestBounds(t *testing.T) {
	s := openLayout(t, t.TempDir(), Options{}, "a", 4)
	if _, err := s.WriteAt([]byte("12345"), 0); err == nil {
		t.Error("write past end accepted")
	}
	if _, err := s.ReadAt(make([]byte, 1), -1); err == nil {
		t.Error("negative offset accepted")
	}
}

func TestOpenRefusesExistingFiles(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "t"), 0o755)
	userFile := filepath.Join(root, "t", "b")
	os.WriteFile(userFile, []byte("precious"), 0o644)

	l, _ := NewLayout(root, mkInfo("t", "a", 3, "b", 8))
	if _, err := Open(l, Options{}); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists", err)
	}
	// With AllowExisting, data is reused (and readable).
	s, err := Open(l, Options{AllowExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	buf := make([]byte, 8)
	if _, err := s.ReadAt(buf, 3); err != nil || string(buf) != "precious" {
		t.Fatalf("ReadAt = %q, %v", buf, err)
	}
}

func TestHandleCacheEviction(t *testing.T) {
	root := t.TempDir()
	var files []any
	for _, n := range []string{"f0", "f1", "f2", "f3", "f4", "f5"} {
		files = append(files, n, 2)
	}
	s := openLayout(t, root, Options{MaxOpenFiles: 2}, files...)
	data := []byte("aabbccddeeff")
	if _, err := s.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if len(s.handles) > 2 {
		t.Fatalf("%d handles open, cap is 2", len(s.handles))
	}
	buf := make([]byte, 12)
	if _, err := s.ReadAt(buf, 0); err != nil || !bytes.Equal(buf, data) {
		t.Fatalf("read after eviction = %q, %v", buf, err)
	}
}

func TestConcurrentWrites(t *testing.T) {
	s := openLayout(t, t.TempDir(), Options{MaxOpenFiles: 2}, "a", 1000, "b", 1000, "c", 1000)
	var wg sync.WaitGroup
	for g := 0; g < 30; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			chunk := bytes.Repeat([]byte{byte('A' + g%26)}, 100)
			if _, err := s.WriteAt(chunk, int64(g*100)); err != nil {
				t.Error(err)
			}
		}(g)
	}
	wg.Wait()
	buf := make([]byte, 3000)
	if _, err := s.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	for g := 0; g < 30; g++ {
		if buf[g*100] != byte('A'+g%26) || buf[g*100+99] != byte('A'+g%26) {
			t.Fatalf("chunk %d corrupted", g)
		}
	}
}

func TestClose(t *testing.T) {
	s := openLayout(t, t.TempDir(), Options{}, "a", 4)
	s.WriteAt([]byte("x"), 0)
	if err := s.Sync(); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s.Close() // idempotent
	if _, err := s.WriteAt([]byte("x"), 0); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close err = %v", err)
	}
}

func TestDeleteFilesKeepsUserFiles(t *testing.T) {
	root := t.TempDir()
	s := openLayout(t, root, Options{}, "a", 2, "sub/deep/b", 2, "other/c", 2)
	s.WriteAt([]byte("aabbcc"), 0)

	// The user dropped their own file into one of the torrent's folders,
	// and has an unrelated file next to the torrent folder.
	userInTorrent := filepath.Join(root, "t", "other", "notes.txt")
	os.WriteFile(userInTorrent, []byte("mine"), 0o644)
	userBeside := filepath.Join(root, "unrelated.txt")
	os.WriteFile(userBeside, []byte("mine"), 0o644)

	if err := s.DeleteFiles(); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"t/a", "t/sub/deep/b", "t/sub/deep", "t/sub", "t/other/c"} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(gone))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be gone (err=%v)", gone, err)
		}
	}
	for _, kept := range []string{userInTorrent, userBeside} {
		if _, err := os.Stat(kept); err != nil {
			t.Errorf("user file %s was deleted", kept)
		}
	}
	if _, err := os.Stat(root); err != nil {
		t.Error("download root must never be removed")
	}
}

func TestDeleteFilesSingleFileAndMissing(t *testing.T) {
	root := t.TempDir()
	s := openLayout(t, root, Options{}, "", 4) // never written: file missing
	if err := s.DeleteFiles(); err != nil {
		t.Fatalf("missing files should be ignored: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("root removed")
	}
}

func TestZeroLengthFilesCreatedOnOpen(t *testing.T) {
	root := t.TempDir()
	openLayout(t, root, Options{}, "dir/empty.txt", 0, "x", 1)
	if fi, err := os.Stat(filepath.Join(root, "t", "dir", "empty.txt")); err != nil || fi.Size() != 0 {
		t.Fatalf("empty file not created: %v", err)
	}
}

func TestReadOnly(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "t"), 0o755)
	roFile := filepath.Join(root, "t", "a")
	os.WriteFile(roFile, []byte("abcd"), 0o444)   // read-only on disk: we must not need write access
	t.Cleanup(func() { os.Chmod(roFile, 0o644) }) // Windows can't delete read-only files
	l, _ := NewLayout(root, mkInfo("t", "a", 4, "empty", 0, "missing", 2))
	s, err := Open(l, Options{ReadOnly: true})
	if err != nil {
		t.Fatalf("read-only open must accept existing files: %v", err)
	}
	defer s.Close()
	buf := make([]byte, 4)
	if _, err := s.ReadAt(buf, 0); err != nil || string(buf) != "abcd" {
		t.Fatalf("ReadAt = %q, %v", buf, err)
	}
	if _, err := s.WriteAt([]byte("x"), 0); !errors.Is(err, ErrReadOnly) {
		t.Errorf("WriteAt err = %v", err)
	}
	if err := s.DeleteFiles(); !errors.Is(err, ErrReadOnly) {
		t.Errorf("DeleteFiles err = %v", err)
	}
	// Nothing was created.
	for _, name := range []string{"empty", "missing"} {
		if _, err := os.Stat(filepath.Join(root, "t", name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("read-only open created %s", name)
		}
	}
}

func TestSkippedEmptyFileNotCreated(t *testing.T) {
	root := t.TempDir()
	openLayout(t, root, Options{Skipped: []bool{true, false}}, "skipped.txt", 0, "kept.txt", 0, "x", 1)
	if _, err := os.Stat(filepath.Join(root, "t", "skipped.txt")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("skipped empty file was created (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(root, "t", "kept.txt")); err != nil {
		t.Errorf("wanted empty file missing: %v", err)
	}
}
