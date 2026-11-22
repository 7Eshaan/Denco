// Package tracker talks to BitTorrent trackers to find peers.
//
// Supported: HTTP/HTTPS trackers (BEP 3) with compact IPv4 peers (BEP 23),
// compact IPv6 peers (BEP 7) and the older dictionary peer format, and UDP
// trackers (BEP 15, udp.go). Other schemes (e.g. wss://) return
// ErrUnsupportedScheme so callers can skip to the next tracker.
//
// This package only performs single announces and multi-tracker fallback
// (List). Deciding *when* to announce (intervals, events) belongs to the
// torrent session.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/compact"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/peerid"
	"github.com/7Eshaan/Denco/internal/version"
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
	// ErrUnsupportedScheme is returned for tracker URLs we can't speak (e.g. wss://).
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

// Client announces to HTTP(S) and UDP trackers. The zero value is usable
// and safe for concurrent use.
type Client struct {
	HTTP      *http.Client // nil means a client with a 30s timeout
	UserAgent string       // sent as User-Agent; empty means version.UserAgent()
	UDP       *UDPClient   // nil means a shared default UDPClient
	// DualStack announces over IPv4 and over IPv6 separately (BEP 7, D60),
	// so a tracker with both kinds of address lists us under both and
	// returns peers of both. Families the tracker (or we) lack fail at once
	// and are ignored. Off: one announce over whichever family dials first.
	DualStack bool

	famOnce sync.Once
	famHTTP [2]*http.Client // DualStack: HTTP clients dialing tcp4 / tcp6
}

// defaultUDP is shared so UDP connection IDs are reused across torrents.
var defaultUDP = &UDPClient{}

// Announce sends one announce to trackerURL, choosing the protocol by scheme.
func (c *Client) Announce(ctx context.Context, trackerURL string, req Request) (*Response, error) {
	u, err := url.Parse(trackerURL)
	if err != nil {
		return nil, fmt.Errorf("tracker: bad URL %q: %w", trackerURL, err)
	}
	switch u.Scheme {
	case "http", "https", "udp":
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedScheme, u.Scheme)
	}
	if !c.DualStack {
		return c.announceFamily(ctx, u, req, anyFamily)
	}
	return c.announceDual(ctx, u, req)
}

// Address families an announce may be forced to.
const (
	anyFamily = iota
	ipv4
	ipv6
)

// announceFamily sends one announce over the given family.
func (c *Client) announceFamily(ctx context.Context, u *url.URL, req Request, fam int) (*Response, error) {
	if u.Scheme == "udp" {
		udp := c.UDP
		if udp == nil {
			udp = defaultUDP
		}
		return udp.announceNet(ctx, u.String(), req, [...]string{"udp", "udp4", "udp6"}[fam])
	}
	hc := c.HTTP
	if fam != anyFamily {
		hc = c.familyHTTP(fam)
	}
	return c.announceHTTP(ctx, hc, cloneURL(u), req)
}

func cloneURL(u *url.URL) *url.URL {
	v := *u
	return &v
}

// dualGrace is how long, after one family's announce succeeded, the other
// may still take: a broken IPv6 route mustn't delay working IPv4 answers
// by a whole attempt timeout.
var dualGrace = 3 * time.Second

// announceDual announces over IPv4 and IPv6 at once and merges the answers.
func (c *Client) announceDual(ctx context.Context, u *url.URL, req Request) (*Response, error) {
	type result struct {
		resp *Response
		err  error
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	results := make(chan result, 2)
	for _, fam := range []int{ipv4, ipv6} {
		go func() {
			resp, err := c.announceFamily(ctx, u, req, fam)
			results <- result{resp, err}
		}()
	}
	var oks []*Response
	var errs []error
	var grace <-chan time.Time
	for n := 0; n < 2; n++ {
		var r result
		select {
		case r = <-results:
		case <-grace:
			cancel() // the other family had its chance
			r = <-results
		}
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		oks = append(oks, r.resp)
		if grace == nil {
			t := time.NewTimer(dualGrace)
			defer t.Stop()
			grace = t.C
		}
	}
	if len(oks) == 0 {
		return nil, dualError(errs)
	}
	return mergeResponses(oks), nil
}

// dualError picks the error worth showing when both families failed: not
// "no address of this family" when the other has a real reason.
func dualError(errs []error) error {
	for _, err := range errs {
		if !noAddress(err) {
			return err
		}
	}
	return errs[0]
}

// noAddress reports a dial that failed because the host (or we) has no
// address of the family asked for.
func noAddress(err error) bool {
	var ae *net.AddrError
	return errors.As(err, &ae)
}

// mergeResponses combines the answers of both families: every peer once,
// the shorter interval, the larger swarm counts.
func mergeResponses(rs []*Response) *Response {
	out := *rs[0]
	seen := map[netip.AddrPort]bool{}
	out.Peers = nil
	for _, r := range rs {
		for _, p := range r.Peers {
			if !seen[p] {
				seen[p] = true
				out.Peers = append(out.Peers, p)
			}
		}
	}
	for _, r := range rs[1:] {
		out.Interval = min(out.Interval, r.Interval)
		if r.MinInterval > 0 && (out.MinInterval == 0 || r.MinInterval < out.MinInterval) {
			out.MinInterval = r.MinInterval
		}
		out.Seeders, out.Leechers = max(out.Seeders, r.Seeders), max(out.Leechers, r.Leechers)
		if out.TrackerID == "" {
			out.TrackerID = r.TrackerID
		}
		if out.Warning == "" {
			out.Warning = r.Warning
		}
	}
	return &out
}

// familyHTTP is an HTTP client that only dials tcp4 (ipv4) or tcp6 (ipv6),
// built from c.HTTP's settings.
func (c *Client) familyHTTP(fam int) *http.Client {
	c.famOnce.Do(func() {
		base := c.HTTP
		if base == nil {
			base = &http.Client{Timeout: defaultTimeout}
		}
		for i, network := range []string{"tcp4", "tcp6"} {
			tr := http.DefaultTransport.(*http.Transport).Clone()
			if bt, ok := base.Transport.(*http.Transport); ok {
				tr = bt.Clone()
			}
			dial := tr.DialContext
			if dial == nil {
				dial = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
			}
			tr.DialContext = func(ctx context.Context, _, addr string) (net.Conn, error) {
				return dial(ctx, network, addr)
			}
			hc := *base
			hc.Transport = tr
			c.famHTTP[i] = &hc
		}
	})
	return c.famHTTP[fam-ipv4]
}

func (c *Client) announceHTTP(ctx context.Context, hc *http.Client, u *url.URL, req Request) (*Response, error) {
	u.RawQuery = buildQuery(u.RawQuery, req)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("tracker: %w", err)
	}
	ua := c.UserAgent
	if ua == "" {
		ua = version.UserAgent()
	}
	httpReq.Header.Set("User-Agent", ua)

	if hc == nil {
		hc = &http.Client{Timeout: defaultTimeout}
	}
	resp, err := hc.Do(httpReq)
	if err != nil {
		// *url.Error repeats the whole announce URL (info hash, peer ID…);
		// the host prefix below is all a user needs.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
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

// parseCompact decodes packed peers (6 bytes each for IPv4, 18 for IPv6),
// skipping unusable entries such as port 0.
func parseCompact(s string, ipLen int) []netip.AddrPort {
	var out []netip.AddrPort
	for _, a := range compact.Parse(s, ipLen) {
		if compact.Usable(a) {
			out = append(out, a)
		}
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
