package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/bencode"
)

func TestInspect(t *testing.T) {
	data, err := bencode.Encode(map[string]any{
		"announce": "http://tracker.example/announce",
		"info": map[string]any{
			"name":         "demo",
			"piece length": 16384,
			"files": []any{
				map[string]any{"length": 1500, "path": []any{"a", "one.txt"}},
				map[string]any{"length": 2500, "path": []any{"two.txt"}},
			},
			"pieces": strings.Repeat("x", 20),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "demo.torrent")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"inspect", path}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	for _, want := range []string{"demo", "4.0 KB", "demo/a/one.txt", "demo/two.txt", "http://tracker.example/announce"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestInspectErrors(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"inspect", filepath.Join(t.TempDir(), "nope.torrent")}, &out, &errOut); code != 1 {
		t.Errorf("missing file: exit %d, want 1", code)
	}
	if code := run([]string{"inspect"}, &out, &errOut); code != 2 {
		t.Errorf("no args: exit %d, want 2", code)
	}
	if code := run([]string{"bogus"}, &out, &errOut); code != 2 {
		t.Errorf("unknown cmd: exit %d, want 2", code)
	}
	if code := run(nil, &out, &errOut); code != 2 {
		t.Errorf("no cmd: exit %d, want 2", code)
	}
}
