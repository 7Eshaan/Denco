package metainfo

import (
	"bytes"
	"crypto/sha1"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
)

const testPieceLen = 16 * 1024

// fakePieces returns n distinct 20-byte hashes concatenated.
func fakePieces(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		h := sha1.Sum([]byte{byte(i), byte(i >> 8)})
		b.Write(h[:])
	}
	return b.String()
}

func singleFileInfo(length int64) map[string]any {
	n := int((length + testPieceLen - 1) / testPieceLen)
	return map[string]any{
		"name":         "ubuntu.iso",
		"piece length": testPieceLen,
		"length":       length,
		"pieces":       fakePieces(n),
	}
}

func encodeTorrent(t *testing.T, root map[string]any) []byte {
	t.Helper()
	data, err := bencode.Encode(root)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseSingleFile(t *testing.T) {
	length := int64(testPieceLen*3 + 100) // 4 pieces, last one short
	info := singleFileInfo(length)
	data := encodeTorrent(t, map[string]any{
		"announce":      "http://tracker.example/announce",
		"comment":       "hello",
		"created by":    "test",
		"creation date": 1700000000,
		"info":          info,
	})

	mi, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	infoBytes, _ := bencode.Encode(info)
	if mi.InfoHash != sha1.Sum(infoBytes) {
		t.Errorf("info hash mismatch")
	}
	if mi.Info.Name != "ubuntu.iso" || mi.Info.MultiFile {
		t.Errorf("name/multifile wrong: %+v", mi.Info)
	}
	if mi.Info.TotalLength != length || len(mi.Info.Files) != 1 || mi.Info.Files[0].Length != length {
		t.Errorf("lengths wrong: %+v", mi.Info.Files)
	}
	if got := mi.Info.Files[0].DisplayPath(&mi.Info); got != "ubuntu.iso" {
		t.Errorf("DisplayPath = %q", got)
	}
	if mi.Info.NumPieces() != 4 {
		t.Errorf("NumPieces = %d", mi.Info.NumPieces())
	}
	if mi.Info.PieceSize(0) != testPieceLen || mi.Info.PieceSize(3) != 100 {
		t.Errorf("PieceSize: %d %d", mi.Info.PieceSize(0), mi.Info.PieceSize(3))
	}
	if mi.Comment != "hello" || mi.CreatedBy != "test" {
		t.Errorf("comment/created by wrong")
	}
	if !mi.CreationDate.Equal(time.Unix(1700000000, 0)) {
		t.Errorf("creation date = %v", mi.CreationDate)
	}
	if got := mi.Trackers(); len(got) != 1 || got[0] != "http://tracker.example/announce" {
		t.Errorf("Trackers = %v", got)
	}
	if !bytes.Equal(mi.Raw, data) {
		t.Error("Raw does not hold the original file bytes")
	}
	data[0] = 'X' // Raw must be a copy
	if mi.Raw[0] == 'X' {
		t.Error("Raw aliases the input buffer")
	}
}

func TestPieceSizeExactMultiple(t *testing.T) {
	mi, err := Parse(encodeTorrent(t, map[string]any{"info": singleFileInfo(testPieceLen * 2)}))
	if err != nil {
		t.Fatal(err)
	}
	if mi.Info.PieceSize(1) != testPieceLen {
		t.Errorf("last piece = %d, want full", mi.Info.PieceSize(1))
	}
}

func TestParseMultiFile(t *testing.T) {
	info := map[string]any{
		"name":         "album",
		"piece length": testPieceLen,
		"files": []any{
			map[string]any{"length": 10000, "path": []any{"cd1", "01.flac"}},
			map[string]any{"length": 0, "path": []any{"empty.txt"}},
			map[string]any{"length": 30000, "path": []any{"cd2", "02.flac"}},
		},
		"pieces": fakePieces(3), // 40000 bytes / 16384 = 3 pieces
	}
	mi, err := Parse(encodeTorrent(t, map[string]any{"info": info}))
	if err != nil {
		t.Fatal(err)
	}
	if !mi.Info.MultiFile || len(mi.Info.Files) != 3 || mi.Info.TotalLength != 40000 {
		t.Fatalf("unexpected info: %+v", mi.Info)
	}
	wantOffsets := []int64{0, 10000, 10000}
	for i, f := range mi.Info.Files {
		if f.Offset != wantOffsets[i] {
			t.Errorf("file %d offset = %d, want %d", i, f.Offset, wantOffsets[i])
		}
	}
	if got := mi.Info.Files[2].DisplayPath(&mi.Info); got != "album/cd2/02.flac" {
		t.Errorf("DisplayPath = %q", got)
	}
}

func TestPreferUTF8Fields(t *testing.T) {
	info := map[string]any{
		"name":         "bad\xff",
		"name.utf-8":   "Música",
		"piece length": testPieceLen,
		"files": []any{
			map[string]any{"length": 5, "path": []any{"x\xfe"}, "path.utf-8": []any{"canción.mp3"}},
			map[string]any{"length": 5, "path": []any{"legacy\xffname"}},
		},
		"pieces": fakePieces(1),
	}
	mi, err := Parse(encodeTorrent(t, map[string]any{"info": info}))
	if err != nil {
		t.Fatal(err)
	}
	if mi.Info.Name != "Música" {
		t.Errorf("name = %q", mi.Info.Name)
	}
	if mi.Info.Files[0].Path[0] != "canción.mp3" {
		t.Errorf("path = %q", mi.Info.Files[0].Path)
	}
	if mi.Info.Files[1].Path[0] != "legacy_name" {
		t.Errorf("invalid UTF-8 not replaced: %q", mi.Info.Files[1].Path)
	}
}

func TestTrackersDedupAndOrder(t *testing.T) {
	data := encodeTorrent(t, map[string]any{
		"announce": "http://a/announce",
		"announce-list": []any{
			[]any{"http://b/announce", "http://a/announce"},
			[]any{"http://c/announce"},
			"junk", // tolerated
		},
		"info": singleFileInfo(100),
	})
	mi, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(mi.Trackers(), ",")
	if got != "http://b/announce,http://a/announce,http://c/announce" {
		t.Errorf("Trackers = %s", got)
	}
}

func TestInfoHashUsesRawBytes(t *testing.T) {
	// Hand-written info dict with unsorted keys: re-encoding would sort them
	// and give a different hash. We must hash the original bytes.
	pieces := fakePieces(1)
	info := "d6:lengthi10e12:piece lengthi16384e4:name1:x6:pieces20:" + pieces + "e"
	data := []byte("d4:info" + info + "e")
	mi, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if mi.InfoHash != sha1.Sum([]byte(info)) {
		t.Error("info hash not computed over raw bytes")
	}
	if len(mi.InfoHash.String()) != 40 {
		t.Error("hex string length")
	}
}

func TestParseInvalid(t *testing.T) {
	base := func() map[string]any { return singleFileInfo(testPieceLen * 2) }
	with := func(mod func(m map[string]any)) map[string]any {
		m := base()
		mod(m)
		return m
	}
	multi := func(paths ...[]any) map[string]any {
		var files []any
		for _, p := range paths {
			files = append(files, map[string]any{"length": 10, "path": p})
		}
		return map[string]any{
			"name": "dir", "piece length": testPieceLen,
			"files": files, "pieces": fakePieces(1),
		}
	}

	tests := map[string]map[string]any{
		"no name":            with(func(m map[string]any) { delete(m, "name") }),
		"name traversal":     with(func(m map[string]any) { m["name"] = ".." }),
		"name with slash":    with(func(m map[string]any) { m["name"] = "a/b" }),
		"name with bslash":   with(func(m map[string]any) { m["name"] = `a\b` }),
		"no piece length":    with(func(m map[string]any) { delete(m, "piece length") }),
		"tiny piece length":  with(func(m map[string]any) { m["piece length"] = 1 }),
		"no pieces":          with(func(m map[string]any) { delete(m, "pieces") }),
		"v2 only":            with(func(m map[string]any) { delete(m, "pieces"); m["meta version"] = 2 }),
		"pieces not mult 20": with(func(m map[string]any) { m["pieces"] = "abc" }),
		"too few pieces":     with(func(m map[string]any) { m["pieces"] = fakePieces(1) }),
		"too many pieces":    with(func(m map[string]any) { m["pieces"] = fakePieces(5) }),
		"negative length":    with(func(m map[string]any) { m["length"] = -1 }),
		"zero length":        with(func(m map[string]any) { m["length"] = 0; m["pieces"] = "" }),
		"length and files":   with(func(m map[string]any) { m["files"] = []any{} }),
		"no length or files": with(func(m map[string]any) { delete(m, "length") }),
		"path dotdot":        multi([]any{"..", "etc", "passwd"}),
		"path dot":           multi([]any{".", "x"}),
		"path empty comp":    multi([]any{""}),
		"path empty list":    multi([]any{}),
		"path with slash":    multi([]any{"a/../../b"}),
		"path with nul":      multi([]any{"a\x00b"}),
		"path non-string":    multi([]any{int64(5)}),
		"duplicate paths":    multi([]any{"a"}, []any{"a"}),
	}
	for name, info := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Parse(encodeTorrent(t, map[string]any{"info": info}))
			if err == nil {
				t.Fatal("expected error")
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("error %v does not wrap ErrInvalid", err)
			}
		})
	}

	for name, data := range map[string]string{
		"garbage":       "not bencode",
		"not dict":      "li1ee",
		"missing info":  "d8:announce1:xe",
		"info not dict": "d4:infoi1ee",
	} {
		if _, err := Parse([]byte(data)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: got %v, want ErrInvalid", name, err)
		}
	}
}

func TestWindowsLegalButUnusualNamesAccepted(t *testing.T) {
	// These are fine on macOS/Linux; storage sanitises them on Windows (D8).
	for _, name := range []string{"1: Intro.mp4", "C:stuff", "what?.txt", "CON"} {
		info := singleFileInfo(10)
		info["name"] = name
		if _, err := Parse(encodeTorrent(t, map[string]any{"info": info})); err != nil {
			t.Errorf("%q rejected: %v", name, err)
		}
	}
}

func TestLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.torrent")
	if err := os.WriteFile(path, encodeTorrent(t, map[string]any{"info": singleFileInfo(10)}), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "missing.torrent")); err == nil {
		t.Fatal("expected error for missing file")
	}
}
