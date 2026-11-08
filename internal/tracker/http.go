// Package tracker talks to BitTorrent trackers to find peers.
//
// Supported: HTTP/HTTPS trackers (BEP 3) with compact IPv4 peers (BEP 23),
// compact IPv6 peers (BEP 7) and the older dictionary peer format.
// UDP trackers (BEP 15) are not supported yet; Announce returns
// ErrUnsupportedScheme for them so callers can skip to the next tracker.
//
// This package only performs single announces and multi-tracker fallback
// (List). Deciding *when* to announce (intervals, events) belongs to the
// torrent session.
package tracker

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
)

// Event tells the tracker why we are announcing.
type Event string

const (
	EventNone      Event = ""
	EventStarted   Event = "started"
	EventCompleted Event = "completed"
	EventStopped   Event = "stopped"
)

const (
	maxResponseSize = 2 << 20 // tracker replies are small; cap to avoid abuse
	defaultInterval = 30 * time.Minute
	minInterval     = 1 * time.Minute // never announce faster than this
	defaultTimeout  = 30 * time.Second
	defaultNumWant  = 50
)

var (
	// ErrUnsupportedScheme is returned for tracker URLs we can't speak (udp://, wss://).
	ErrUnsupportedScheme = errors.New("tracker: unsupported URL scheme")
)

// FailureError is a tracker-reported failure ("failure reason" key).
type FailureError struct{ Reason string }

func (e *FailureError) Error() string { return "tracker failure: " + e.Reason }

// Request holds the per-announce values.
type Request struct {
	InfoHash   metainfo.Hash
	PeerID     peerid.ID
	Port       uint16
	Uploaded   int64
	Downloaded int64
	Left       int64
	Event      Event
	NumWant    int    // 0 means default (50)
	Key        uint32 // random per-session value; helps trackers identify us across IP changes
	TrackerID  string // echo of a previous response's "tracker id", if any
}

// Response is a parsed announce reply.
type Response struct {
	Interval    time.Duration // how long to wait before the next regular announce
	MinInterval time.Duration // 0 if the tracker didn't say
	TrackerID   string
	Seeders     int // "complete"; -1 if unknown
	Leechers    int // "incomplete"; -1 if unknown
	Peers       []netip.AddrPort
	Warning     string
}

// Client performs announces over HTTP(S). The zero value is usable.
type Client struct {
	HTTP      *http.Client // nil means a client with a 30s timeout
	UserAgent string       // sent as User-Agent; empty means "Denco/<version>"
}

// Announce sends one announce to trackerURL.
func (c *Client) Announce(ctx context.Context, trackerURL string, req Request) (*Response, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("tracker: bad URL %q: %w", trackerURL, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedScheme, u.Scheme)
	}
	u.RawQuery = buildQuery(u.RawQuery, req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = "Denco/0.0.1"
	}
	httpReq.Header.Set("User-Agent", ua)

	hc := c.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("tracker %s: %w", u.Host, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("tracker %s: reading response: %w", u.Host, err)
	}
	if len(body) > maxResponseSize {
		return nil, fmt.Errorf("tracker %s: response too large", u.Host)
	}
	// Some trackers send a bencoded failure with a non-200 status, so try to
	// parse first and only fall back to the HTTP status if that fails.
	parsed, perr := parseResponse(body)
	var failure *FailureError
	if errors.As(perr, &failure) {
		return nil, failure // the tracker's own reason beats a bare HTTP status
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("tracker %s: HTTP %s", u.Host, resp.Status)
	}
	if perr != nil {
		return nil, fmt.Errorf("tracker %s: %w", u.Host, perr)
	}
	return parsed, nil
}

// buildQuery appends announce parameters to any existing query (private
// trackers often embed a passkey there). info_hash and peer_id are raw bytes
// and must be percent-encoded byte-by-byte, which url.Values can't do
// correctly (it would turn 0x20 into '+'), so we build the string by hand.
func buildQuery(existing string, r Request) string {
	numWant := r.NumWant
	if numWant <= 0 {
		numWant = defaultNumWant
	}
	params := []string{
		"info_hash=" + escapeBytes(r.InfoHash[:]),
		"peer_id=" + escapeBytes(r.PeerID[:]),
		"port=" + strconv.Itoa(int(r.Port)),
		"uploaded=" + strconv.FormatInt(r.Uploaded, 10),
		"downloaded=" + strconv.FormatInt(r.Downloaded, 10),
		"left=" + strconv.FormatInt(r.Left, 10),
		"compact=1",
		"numwant=" + strconv.Itoa(numWant),
		"key=" + strconv.FormatUint(uint64(r.Key), 16),
	}
	if r.Event != EventNone {
		params = append(params, "event="+string(r.Event))
	}
	if r.TrackerID != "" {
		params = append(params, "trackerid="+escapeBytes([]byte(r.TrackerID)))
	}
	q := strings.Join(params, "&")
	if existing != "" {
		q = existing + "&" + q
	}
	return q
}

// escapeBytes percent-encodes everything except RFC 3986 unreserved characters.
func escapeBytes(b []byte) string {
	const hex = "0123456789ABCDEF"
	var sb strings.Builder
	for _, c := range b {
		if ('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') ||
			c == '-' || c == '_' || c == '.' || c == '~' {
			sb.WriteByte(c)
		} else {
			sb.WriteByte('%')
			sb.WriteByte(hex[c>>4])
			sb.WriteByte(hex[c&15])
		}
	}
	return sb.String()
}

func parseResponse(body []byte) (*Response, error) {
	v, err := bencode.Decode(body)
	if err != nil {
		return nil, fmt.Errorf("bad response: %w", err)
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("bad response: not a dictionary")
	}
	if reason, ok := bencode.GetString(d, "failure reason"); ok {
		return nil, &FailureError{Reason: reason}
	}

	r := &Response{Interval: defaultInterval, Seeders: -1, Leechers: -1}
	if n, ok := bencode.GetInt(d, "interval"); ok && n > 0 {
		r.Interval = time.Duration(n) * time.Second
	}
	if n, ok := bencode.GetInt(d, "min interval"); ok && n > 0 {
		r.MinInterval = time.Duration(n) * time.Second
	}
	// Protect trackers (and ourselves) from absurdly short intervals.
	r.Interval = max(r.Interval, minInterval)
	if n, ok := bencode.GetInt(d, "complete"); ok && n >= 0 {
		r.Seeders = int(n)
	}
	if n, ok := bencode.GetInt(d, "incomplete"); ok && n >= 0 {
		r.Leechers = int(n)
	}
	r.TrackerID, _ = bencode.GetString(d, "tracker id")
	r.Warning, _ = bencode.GetString(d, "warning message")

	switch p := d["peers"].(type) {
	case string:
		r.Peers = append(r.Peers, parseCompact(p, 4)...)
	case []any:
		r.Peers = append(r.Peers, parseDictPeers(p)...)
	}
	if p, ok := bencode.GetString(d, "peers6"); ok {
		r.Peers = append(r.Peers, parseCompact(p, 16)...)
	}
	return r, nil
}

// parseCompact decodes packed (ip, port) entries: 6 bytes for IPv4, 18 for IPv6.
// A trailing partial entry is ignored.
func parseCompact(s string, ipLen int) []netip.AddrPort {
	entry := ipLen + 2
	out := make([]netip.AddrPort, 0, len(s)/entry)
	for i := 0; i+entry <= len(s); i += entry {
		ip, ok := netip.AddrFromSlice([]byte(s[i : i+ipLen]))
		if !ok {
			continue
		}
		port := binary.BigEndian.Uint16([]byte(s[i+ipLen : i+entry]))
		if port == 0 {
			continue
		}
		out = append(out, netip.AddrPortFrom(ip.Unmap(), port))
	}
	return out
}

// parseDictPeers decodes the original format: a list of {ip, port, peer id}.
// Entries with hostnames instead of IPs are skipped (we don't do DNS per peer).
func parseDictPeers(list []any) []netip.AddrPort {
	var out []netip.AddrPort
	for _, item := range list {
		d, ok := item.(map[string]any)
		if !ok {
			continue
		}
		ipStr, _ := bencode.GetString(d, "ip")
		port, _ := bencode.GetInt(d, "port")
		ip, err := netip.ParseAddr(ipStr)
		if err != nil || port <= 0 || port > 65535 {
			continue
		}
		out = append(out, netip.AddrPortFrom(ip.Unmap(), uint16(port)))
	}
	return out
}
