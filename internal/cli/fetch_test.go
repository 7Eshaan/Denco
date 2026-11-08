package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddFromURLsAndMagnets(t *testing.T) {
	tmp := t.TempDir()
	tpath, _ := writeTorrent(t, tmp, "webfile", 1)
	torrentBytes, _ := os.ReadFile(tpath)
	const redirected = "magnet:?xt=urn:btih:1111111111111111111111111111111111111111&dn=via-redirect&tr=http%3A%2F%2F127.0.0.1%3A1%2Fa"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/file.torrent":
			w.Write(torrentBytes)
		case "/go":
			http.Redirect(w, r, "/file.torrent", http.StatusFound) // ordinary redirect: followed
		case "/magnet":
			w.Header().Set("Location", redirected)
			w.WriteHeader(http.StatusFound)
		case "/junk":
			w.Write([]byte("<html>not a torrent</html>"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	h := startDaemon(t, filepath.Join(tmp, "home"), filepath.Join(tmp, "dl"))
	out := h.mustCmd("add", srv.URL+"/go")
	if !strings.Contains(out, "webfile") || !strings.Contains(out, "Added as #1") {
		t.Errorf("add URL:\n%s", out)
	}
	out = h.mustCmd("add", srv.URL+"/magnet")
	if !strings.Contains(out, "points to a magnet link") || !strings.Contains(out, "via-redirect") || !strings.Contains(out, "Added as #2") {
		t.Errorf("add URL redirecting to a magnet:\n%s", out)
	}
	for path, want := range map[string]string{"/missing": "HTTP 404", "/junk": "not a usable .torrent"} {
		if code, _, errOut := h.cmd("add", srv.URL+path); code != 1 || !strings.Contains(errOut, want) {
			t.Errorf("add %s: exit %d, stderr %q", path, code, errOut)
		}
	}

	// A magnet with nothing to find peers with: added, with a warning.
	const lonely = "magnet:?xt=urn:btih:2222222222222222222222222222222222222222&dn=Lonely+One"
	out = h.mustCmd("add", lonely)
	for _, want := range []string{"Lonely One", "magnet link", "0 trackers", "warning: no trackers or peers", "Added as #3", "Fetching metadata"} {
		if !strings.Contains(out, want) {
			t.Errorf("add magnet missing %q:\n%s", want, out)
		}
	}
	if code, _, errOut := h.cmd("add", "magnet:?xt=urn:btih:nothex"); code != 1 || !strings.Contains(errOut, "invalid magnet") {
		t.Errorf("bad magnet: %d %q", code, errOut)
	}

	list := h.waitList("Fetching metadata")
	if !strings.Contains(list, "Lonely One") || !strings.Contains(list, "?") {
		t.Errorf("list:\n%s", list)
	}
	info := h.mustCmd("info", "3")
	for _, want := range []string{"Fetching metadata", "Metadata:", "unknown until the metadata arrives", "DHT"} {
		if !strings.Contains(info, want) {
			t.Errorf("info missing %q:\n%s", want, info)
		}
	}
	if code, _, errOut := h.cmd("files", "3"); code != 1 || !strings.Contains(errOut, "metadata not downloaded yet") {
		t.Errorf("files of a bare magnet: %d %q", code, errOut)
	}
	if code, out, _ := h.cmd("add", lonely); code != 1 || !strings.Contains(out, "already added as #3") {
		t.Errorf("duplicate magnet: %d\n%s", code, out)
	}
}
