package ipc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
)

// Client talks to a running daemon.
type Client struct {
	base  string // "http://127.0.0.1:port"
	token string
	http  *http.Client
}

// NewClient returns a client for the daemon described by info.
func NewClient(info Info) *Client {
	return &Client{
		base:  "http://" + info.Addr,
		token: info.Token,
		// Remove waits for a torrent to stop (it tells trackers first),
		// which can take a few seconds; nothing should take a minute.
		http: &http.Client{Timeout: time.Minute},
	}
}

// Connect reads the daemon file at path and checks the daemon answers.
// It returns ErrNoDaemon if there is no daemon file, or if the file is
// stale (left behind by a daemon that crashed).
func Connect(ctx context.Context, path string) (*Client, error) {
	info, err := ReadInfo(path)
	if err != nil {
		return nil, err
	}
	c := NewClient(info)
	if _, err := c.Status(ctx); err != nil {
		var apiErr *Error
		if errors.As(err, &apiErr) {
			return nil, err // something answered, but refused us
		}
		return nil, fmt.Errorf("%w (stale %s: %v)", ErrNoDaemon, path, err)
	}
	return c, nil
}

// Status returns the daemon's overall status.
func (c *Client) Status(ctx context.Context) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodGet, "/v1/status", nil, &s)
}

// List returns all torrents.
func (c *Client) List(ctx context.Context) ([]engine.Torrent, error) {
	var out []engine.Torrent
	return out, c.do(ctx, http.MethodGet, "/v1/torrents", nil, &out)
}

// Add adds a .torrent file's contents.
func (c *Client) Add(ctx context.Context, req AddRequest) (engine.Torrent, error) {
	var t engine.Torrent
	return t, c.do(ctx, http.MethodPost, "/v1/torrents", req, &t)
}

// Get returns one torrent.
func (c *Client) Get(ctx context.Context, ref string) (engine.Torrent, error) {
	var t engine.Torrent
	return t, c.do(ctx, http.MethodGet, torrentPath(ref, ""), nil, &t)
}

// Files lists a torrent's files.
func (c *Client) Files(ctx context.Context, ref string) ([]engine.File, error) {
	var out []engine.File
	return out, c.do(ctx, http.MethodGet, torrentPath(ref, "/files"), nil, &out)
}

// SetFiles marks files (indexes from 0) as wanted or skipped and returns
// the updated file list.
func (c *Client) SetFiles(ctx context.Context, ref string, files []int, wanted bool) ([]engine.File, error) {
	var out []engine.File
	return out, c.do(ctx, http.MethodPost, torrentPath(ref, "/files"), FilesRequest{Files: files, Wanted: wanted}, &out)
}

// Peers lists a torrent's connected peers.
func (c *Client) Peers(ctx context.Context, ref string) ([]engine.Peer, error) {
	var out []engine.Peer
	return out, c.do(ctx, http.MethodGet, torrentPath(ref, "/peers"), nil, &out)
}

// Pause pauses a torrent.
func (c *Client) Pause(ctx context.Context, ref string) (engine.Torrent, error) {
	var t engine.Torrent
	return t, c.do(ctx, http.MethodPost, torrentPath(ref, "/pause"), struct{}{}, &t)
}

// Resume resumes a torrent.
func (c *Client) Resume(ctx context.Context, ref string) (engine.Torrent, error) {
	var t engine.Torrent
	return t, c.do(ctx, http.MethodPost, torrentPath(ref, "/resume"), struct{}{}, &t)
}

// Remove removes a torrent, optionally deleting its files.
func (c *Client) Remove(ctx context.Context, ref string, deleteFiles bool) (engine.Removed, error) {
	var t engine.Removed
	return t, c.do(ctx, http.MethodPost, torrentPath(ref, "/remove"), RemoveRequest{DeleteFiles: deleteFiles}, &t)
}

// SetUploadsPaused pauses or resumes one torrent's uploads.
func (c *Client) SetUploadsPaused(ctx context.Context, ref string, paused bool) (engine.Torrent, error) {
	var t engine.Torrent
	return t, c.do(ctx, http.MethodPost, torrentPath(ref, "/uploads"), UploadsRequest{Paused: paused}, &t)
}

// SetSeedMode sets one torrent's seeding mode ("on", "off", "default").
func (c *Client) SetSeedMode(ctx context.Context, ref, mode string) (engine.Torrent, error) {
	var t engine.Torrent
	return t, c.do(ctx, http.MethodPost, torrentPath(ref, "/seed"), SeedRequest{Mode: mode}, &t)
}

// SetSeedLimits changes the global seeding limits (0 = no limit).
func (c *Client) SetSeedLimits(ctx context.Context, ratio float64, d time.Duration) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodPost, "/v1/seed-limits", SeedLimitsRequest{Ratio: ratio, Time: d}, &s)
}

// SetLimits changes the daemon's total speed limits (bytes/s, 0 = unlimited).
func (c *Client) SetLimits(ctx context.Context, down, up int64) (Status, error) {
	var s Status
	return s, c.do(ctx, http.MethodPost, "/v1/limits", LimitsRequest{Download: down, Upload: up}, &s)
}

// Shutdown asks the daemon to stop. It returns once the request is
// accepted; the daemon then stops its torrents and exits.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.do(ctx, http.MethodPost, "/v1/shutdown", struct{}{}, nil)
}

func torrentPath(ref, suffix string) string {
	return "/v1/torrents/" + url.PathEscape(ref) + suffix
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("talking to the daemon: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		e := &Error{Status: resp.StatusCode}
		if json.NewDecoder(resp.Body).Decode(e) != nil || e.Message == "" {
			e.Message = "daemon replied " + resp.Status
		}
		return e
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("bad reply from the daemon: %w", err)
	}
	return nil
}
