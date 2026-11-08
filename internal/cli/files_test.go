package cli

import (
	"slices"
	"strings"
	"testing"
)

func TestParseFileList(t *testing.T) {
	cases := []struct {
		args []string
		want []int
		err  string
	}{
		{[]string{"3"}, []int{2}, ""},
		{[]string{"2-4", "1"}, []int{0, 1, 2, 3}, ""},
		{[]string{"1,4,4", "2-2"}, []int{0, 1, 3}, ""},
		{[]string{"all"}, []int{0, 1, 2, 3, 4}, ""},
		{[]string{"0"}, nil, "doesn't exist"},
		{[]string{"6"}, nil, "doesn't exist"},
		{[]string{"4-2"}, nil, "not a file number"},
		{[]string{"x"}, nil, "not a file number"},
		{[]string{","}, nil, "no files"},
	}
	for _, c := range cases {
		got, err := parseFileList(c.args, 5)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("%v: err = %v, want %q", c.args, err, c.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("%v = %v, %v; want %v", c.args, got, err, c.want)
		}
	}
}

func TestSkipFromFlags(t *testing.T) {
	if got, _ := skipFromFlags("2", "", 3); !slices.Equal(got, []int{1}) {
		t.Errorf("-skip 2 = %v", got)
	}
	if got, _ := skipFromFlags("", "1,3", 4); !slices.Equal(got, []int{1, 3}) {
		t.Errorf("-only 1,3 of 4 = %v", got)
	}
	if _, err := skipFromFlags("1", "2", 3); err == nil {
		t.Error("-skip with -only must fail")
	}
	if got, err := skipFromFlags("", "", 3); got != nil || err != nil {
		t.Errorf("no flags = %v, %v", got, err)
	}
}

func TestFileSelectionCommands(t *testing.T) {
	home, tmp := t.TempDir(), t.TempDir()
	h := startDaemon(t, home, tmp)
	path, _ := writeTorrent(t, tmp, "trio", 3)

	if code, _, errOut := h.cmd("add", "-skip", "1", "-only", "2", path); code == 0 || !strings.Contains(errOut, "not both") {
		t.Errorf("-skip with -only: exit %d, %s", code, errOut)
	}
	if code, _, errOut := h.cmd("add", "-skip", "1", path, path); code != 2 || !strings.Contains(errOut, "one torrent") {
		t.Errorf("-skip with two torrents: exit %d, %s", code, errOut)
	}
	if code, _, errOut := h.cmd("add", "-skip", "1", "magnet:?xt=urn:btih:c12fe1c06bba254a9dc9f519b335aa7c1367a88a"); code == 0 || !strings.Contains(errOut, "denco files") {
		t.Errorf("-skip with a magnet: exit %d, %s", code, errOut)
	}
	if code, out, _ := h.cmd("add", "-skip", "all", path); code == 0 || !strings.Contains(out, "skip every file") {
		t.Errorf("-skip all: exit %d, %s", code, out)
	}

	out := h.mustCmd("add", "-paused", "-only", "2", path)
	if !strings.Contains(out, "2 files skipped") {
		t.Errorf("add output:\n%s", out)
	}
	out = h.mustCmd("files", "1")
	for _, want := range []string{"WANT", "1  skip", "2  yes", "3  skip", "2 files skipped"} {
		if !strings.Contains(out, want) {
			t.Errorf("files output lacks %q:\n%s", want, out)
		}
	}
	out = h.mustCmd("files", "1", "want", "3")
	if !strings.Contains(out, "3  yes") || !strings.Contains(out, "1 file skipped") {
		t.Errorf("after want 3:\n%s", out)
	}
	if out := h.mustCmd("info", "1"); !strings.Contains(out, "selected of") {
		t.Errorf("info lacks the selection:\n%s", out)
	}
	if code, _, errOut := h.cmd("files", "1", "skip", "9"); code != 1 || !strings.Contains(errOut, "files 1-3") {
		t.Errorf("skip 9: exit %d, %s", code, errOut)
	}
	if code, _, errOut := h.cmd("files", "1", "skip", "2-3"); code != 1 || !strings.Contains(errOut, "skip every file") {
		t.Errorf("skipping the rest: exit %d, %s", code, errOut)
	}
	if code, _, _ := h.cmd("files", "1", "toss", "1"); code != 2 {
		t.Errorf("unknown subcommand: exit %d", code)
	}
	out = h.mustCmd("files", "1", "want", "all")
	if strings.Contains(out, "  skip  ") || strings.Contains(out, "skipped") || strings.Count(out, "  yes  ") != 3 {
		t.Errorf("after want all:\n%s", out)
	}
}
