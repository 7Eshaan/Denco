package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/metainfo"
)

// mkInfo builds an Info from "path/with/slashes" → length pairs, in order.
// A single entry with no slash and name == path builds a single-file torrent.
func mkInfo(name string, files ...any) *metainfo.Info {
	info := &metainfo.Info{Name: name, PieceLength: 16384}
	var off int64
	for i := 0; i < len(files); i += 2 {
		p := files[i].(string)
		n := int64(files[i+1].(int))
		var parts []string
		if p != "" {
			parts = strings.Split(p, "/")
		}
		info.Files = append(info.Files, metainfo.File{Path: parts, Length: n, Offset: off})
		off += n
	}
	info.TotalLength = off
	info.MultiFile = len(info.Files) > 1 || (len(info.Files) == 1 && len(info.Files[0].Path) > 0)
	return info
}

func rels(l *Layout) []string {
	var out []string
	for _, f := range l.Files {
		out = append(out, f.Rel)
	}
	return out
}

func TestLayoutBasic(t *testing.T) {
	root := t.TempDir()
	l, err := NewLayout(root, mkInfo("album", "cd1/01.flac", 10, "cover.jpg", 5))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(rels(l), ","); got != "album/cd1/01.flac,album/cover.jpg" {
		t.Fatalf("rels = %s", got)
	}
	want := filepath.Join(root, "album", "cd1", "01.flac")
	if l.Files[0].Path != want || l.Files[0].Renamed {
		t.Fatalf("file 0 = %+v, want path %s", l.Files[0], want)
	}
	if l.Files[1].Offset != 10 || l.TotalLength() != 15 {
		t.Fatal("offsets/total wrong")
	}

	single, err := NewLayout(root, mkInfo("ubuntu.iso", "", 100))
	if err != nil {
		t.Fatal(err)
	}
	if single.Files[0].Rel != "ubuntu.iso" {
		t.Fatalf("single-file rel = %q", single.Files[0].Rel)
	}
}

func TestLayoutRelativeRootMadeAbsolute(t *testing.T) {
	l, err := NewLayout("downloads", mkInfo("x", "", 1))
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(l.Root) || !filepath.IsAbs(l.Files[0].Path) {
		t.Fatalf("not absolute: %+v", l)
	}
}

func TestSanitizeWindows(t *testing.T) {
	tests := map[string]string{
		"1: Intro.mp4":   "1_ Intro.mp4",
		`a<b>c"d|e?f*g`:  "a_b_c_d_e_f_g",
		"trailing dot.":  "trailing dot",
		"trailing sp  ":  "trailing sp",
		"CON":            "_CON",
		"con.txt":        "_con.txt",
		"LPT9.tar.gz":    "_LPT9.tar.gz",
		"CONSOLE.txt":    "CONSOLE.txt", // only exact device names
		"tab\there":      "tab_here",
		"...":            "_",
		"normal name.rs": "normal name.rs",
	}
	for in, want := range tests {
		if got := sanitizeComponent(in, true); got != want {
			t.Errorf("windows %q → %q, want %q", in, got, want)
		}
	}
	// Elsewhere these are all legal and must be left alone.
	for _, in := range []string{"1: Intro.mp4", "CON", "what?.txt", "trailing dot."} {
		if got := sanitizeComponent(in, false); got != in {
			t.Errorf("unix %q changed to %q", in, got)
		}
	}
}

func TestLayoutWindowsRenamesFlagged(t *testing.T) {
	l, err := newLayout(t.TempDir(), mkInfo("show", "1: Pilot.mkv", 1, "ok.txt", 1), true)
	if err != nil {
		t.Fatal(err)
	}
	if l.Files[0].Rel != "show/1_ Pilot.mkv" || !l.Files[0].Renamed || l.Files[1].Renamed {
		t.Fatalf("files = %+v", l.Files)
	}
}

func TestTruncateName(t *testing.T) {
	long := strings.Repeat("a", 300) + ".mkv"
	got := truncateName(long, 255)
	if len(got) != 255 || !strings.HasSuffix(got, ".mkv") {
		t.Fatalf("len=%d suffix ok=%v", len(got), strings.HasSuffix(got, ".mkv"))
	}
	// Multi-byte characters are never split.
	jp := strings.Repeat("日", 100) // 300 bytes
	got = truncateName(jp, 255)
	if len(got) > 255 || !strings.HasPrefix(jp, got) || len(got)%3 != 0 {
		t.Fatalf("split rune: len=%d", len(got))
	}
}

func TestCollisionsCaseInsensitive(t *testing.T) {
	l, err := NewLayout(t.TempDir(), mkInfo("d", "Readme.txt", 1, "README.txt", 1, "readme.txt", 1, "x", 1))
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(rels(l), ",")
	if got != "d/Readme.txt,d/README (1).txt,d/readme (2).txt,d/x" {
		t.Fatalf("rels = %s", got)
	}
	if l.Files[0].Renamed || !l.Files[1].Renamed || !l.Files[2].Renamed {
		t.Fatal("Renamed flags wrong")
	}
}

func TestCollisionFileNamedLikeEarlierDir(t *testing.T) {
	// "sub" is a directory (from sub/a); a later *file* "SUB" gets renamed.
	l, err := NewLayout(t.TempDir(), mkInfo("d", "sub/a", 1, "SUB", 1))
	if err != nil {
		t.Fatal(err)
	}
	if l.Files[1].Rel != "d/SUB (1)" {
		t.Fatalf("rel = %q", l.Files[1].Rel)
	}
}

func TestCollisionDirNamedLikeEarlierFile(t *testing.T) {
	// File "x" first, then "x/y" needs "x" to be a directory: unfixable.
	_, err := NewLayout(t.TempDir(), mkInfo("d", "x", 1, "x/y", 1))
	if !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("err = %v", err)
	}
}

func TestCollisionLongNameTerminates(t *testing.T) {
	long := strings.Repeat("n", 255)
	l, err := NewLayout(t.TempDir(), mkInfo("d", long, 1, strings.ToUpper(long), 1))
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Base(l.Files[1].Path)
	if len(name) > 255 || !strings.HasSuffix(name, " (1)") {
		t.Fatalf("got %q (len %d)", name, len(name))
	}
}

func TestExisting(t *testing.T) {
	root := t.TempDir()
	l, err := NewLayout(root, mkInfo("d", "a", 1, "b", 1))
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Existing()) != 0 {
		t.Fatal("nothing should exist yet")
	}
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	os.WriteFile(filepath.Join(root, "d", "b"), []byte("user data"), 0o644)
	if ex := l.Existing(); len(ex) != 1 || ex[0] != l.Files[1].Path {
		t.Fatalf("Existing = %v", ex)
	}
}

func TestWithin(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "dl")
	if !within(root, filepath.Join(root, "a", "b")) {
		t.Error("child should be within")
	}
	if within(root, filepath.Join(string(filepath.Separator), "dlx", "a")) {
		t.Error("sibling with shared prefix must not be within")
	}
	if within(root, filepath.Join(root, "..", "etc")) {
		t.Error("parent escape must not be within")
	}
}
