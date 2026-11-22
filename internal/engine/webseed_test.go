package engine

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// A .torrent with a web seed downloads from it with no peer anywhere, and
// the web seed shows in the torrent's view and peer list. A magnet's ws=
// web seed survives into the .torrent made from the fetched metadata.
func TestWebSeedThroughEngine(t *testing.T) {
	f := newFixture(t, "from the web", "", 16384, 70000, 30000)
	tmp := t.TempDir()
	webDir := filepath.Join(tmp, "web")
	f.writeData(t, webDir)
	srv := httptest.NewServer(http.FileServer(http.Dir(webDir)))
	t.Cleanup(srv.Close)

	raw, _ := bencode.Encode(map[string]any{"url-list": srv.URL + "/", "info": bencode.Raw(f.mi.InfoBytes)})
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	cfg.WebSeedLocal = true
	e := openEngine(t, cfg)
	v, err := e.Add(raw, AddOptions{})
	if err != nil || v.WebSeeds != 1 {
		t.Fatalf("add: %d web seeds, %v", v.WebSeeds, err)
	}
	waitState(t, e, "1", StateSeeding)
	f.checkData(t, cfg.DownloadDir)
	peers, _ := e.Peers("1")
	found := false
	for _, p := range peers {
		found = found || (p.WebSeed == srv.URL+"/" && p.Downloaded == f.mi.Info.TotalLength)
	}
	if !found {
		t.Errorf("web seed not in the peer list: %+v", peers)
	}

	// Magnet: metadata from a peer (the seeding engine above), web seed
	// from ws=, which the saved .torrent keeps.
	peer := "127.0.0.1:" + strconv.Itoa(int(e.Overview().ListenPort))
	lcfg := testConfig(filepath.Join(tmp, "leechstate"), filepath.Join(tmp, "dl2"))
	lcfg.WebSeedLocal = true
	leech := openEngine(t, lcfg)
	if _, err := leech.AddMagnet(magnetURI(f, "", "", peer)+"&ws="+srv.URL+"/", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	if v := waitState(t, leech, "1", StateSeeding); v.WebSeeds != 1 {
		t.Errorf("magnet torrent has %d web seeds, want 1", v.WebSeeds)
	}
	leech.Close()
	mi, err := metainfo.Load(filepath.Join(lcfg.StateDir, f.mi.InfoHash.String()+".torrent"))
	if err != nil || len(mi.URLList) != 1 || mi.URLList[0] != srv.URL+"/" {
		t.Fatalf("saved .torrent url-list = %v, %v", mi.URLList, err)
	}
}
