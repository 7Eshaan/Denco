package cli

import (
	"bytes"
	"crypto/sha1"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/7Eshaan/Denco/internal/bencode"
)

func TestVerifyCommand(t *testing.T) {
	const pl = 16384
	content := bytes.Repeat([]byte("denco!"), pl) // 6 pieces' worth of data
	a, b := content[:pl*2+100], content[pl*2+100:]
	var hashes []byte
	for off := 0; off < len(content); off += pl {
		h := sha1.Sum(content[off:min(off+pl, len(content))])
		hashes = append(hashes, h[:]...)
	}
	torrent, _ := bencode.Encode(map[string]any{"info": map[string]any{
		"name": "pack", "piece length": pl, "pieces": string(hashes),
		"files": []any{
			map[string]any{"length": len(a), "path": []any{"a.bin"}},
			map[string]any{"length": len(b), "path": []any{"sub", "b.bin"}},
		},
	}})
	tmp := t.TempDir()
	tpath := filepath.Join(tmp, "pack.torrent")
	os.WriteFile(tpath, torrent, 0o644)

	dl := filepath.Join(tmp, "dl")
	os.MkdirAll(filepath.Join(dl, "pack", "sub"), 0o755)
	os.WriteFile(filepath.Join(dl, "pack", "a.bin"), a, 0o644)
	// b.bin missing entirely.

	var out, errOut bytes.Buffer
	if code := run([]string{"verify", "-dir", dl, tpath}, &out, &errOut); code != 0 {
		t.Fatalf("exit %d: %s", code, errOut.String())
	}
	s := out.String()
	// Pieces 0,1 are wholly in a.bin; piece 2 straddles into missing b.bin.
	for _, want := range []string{"pack/a.bin", "pack/sub/b.bin", "Total: 2 / 6 pieces good"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in:\n%s", want, s)
		}
	}
	// Read-only: verify must not have created b.bin.
	if _, err := os.Stat(filepath.Join(dl, "pack", "sub", "b.bin")); !os.IsNotExist(err) {
		t.Error("verify created a file")
	}

	// Now write b.bin too: everything verifies.
	os.WriteFile(filepath.Join(dl, "pack", "sub", "b.bin"), b, 0o644)
	out.Reset()
	run([]string{"verify", "-dir", dl, tpath}, &out, &errOut)
	if !strings.Contains(out.String(), "Total: 6 / 6 pieces good (100%)") {
		t.Errorf("full verify:\n%s", out.String())
	}
}

func TestVerifyUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"verify"}, &out, &errOut); code != 2 {
		t.Errorf("exit = %d", code)
	}
	if code := run([]string{"verify", "nope.torrent"}, &out, &errOut); code != 1 {
		t.Errorf("missing torrent exit = %d", code)
	}
}
