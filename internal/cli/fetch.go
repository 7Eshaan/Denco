package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// maxTorrentFile caps a downloaded .torrent (20 bytes per piece: 16 MiB
// is ~800,000 pieces, far beyond real torrents).
const maxTorrentFile = 16 << 20

// fetchTorrentURL downloads a .torrent from an HTTP(S) URL. Some sites
// answer with a redirect to a magnet link instead; then magnetURI is set
// and data is nil. The result is validated before it is returned.
func fetchTorrentURL(ctx context.Context, url string) (data []byte, magnetURI string, err error) {
	client := &http.Client{
		Timeout: 30 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if strings.EqualFold(req.URL.Scheme, "magnet") {
				return http.ErrUseLastResponse // hand the magnet back to us
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", "Denco/0.0.1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); resp.StatusCode/100 == 3 && magnet.IsMagnet(loc) {
		return nil, loc, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download failed: HTTP %s", resp.Status)
	}
	data, err = io.ReadAll(io.LimitReader(resp.Body, maxTorrentFile+1))
	if err != nil {
		return nil, "", fmt.Errorf("download failed: %w", err)
	}
	if len(data) > maxTorrentFile {
		return nil, "", fmt.Errorf("file is larger than %d MB; not a .torrent", maxTorrentFile>>20)
	}
	if _, err := metainfo.Parse(data); err != nil {
		return nil, "", fmt.Errorf("not a usable .torrent file: %w", err)
	}
	return data, "", nil
}
