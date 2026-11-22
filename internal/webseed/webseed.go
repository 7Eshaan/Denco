// Package webseed downloads torrent data from HTTP(S) servers that hold the
// torrent's files ("web seeds", BEP 19 "GetRight style"): a .torrent's
// url-list or a magnet's ws= parameters. A web seed is a plain web server,
// so a piece is fetched with HTTP Range requests — one per file the piece
// spans — and then verified like data from any peer.
//
// Safety (DECISIONS D61): a torrent chooses these URLs, so by default they
// may not lead to this machine or its local network (loopback, private,
// link-local and similar addresses are refused at connect time, also after
// redirects); only http and https; no proxies from the environment;
// responses must be exactly the range asked for.
//
// The torrent session drives web seeds (torrent/webseed.go); this package
// only knows HTTP.
package webseed

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

	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/version"
)

var (
	// ErrLocalAddress: the web seed's host is on this machine or the local
	// network, which a torrent may not make us contact.
	ErrLocalAddress = errors.New("web seed points to a local or private address")
	// ErrNoRanges: the server ignored the Range header, so it can't serve
	// parts of files. Retrying won't help.
	ErrNoRanges = errors.New("web seed doesn't support range requests")
	// ErrBadURL: not an http(s) URL.
	ErrBadURL = errors.New("web seed URL must be http or https")
)

// RetryError is a server asking us to come back later (503 with
// Retry-After, or 429).
type RetryError struct {
	After time.Duration
	Err   error
}

func (e *RetryError) Error() string { return fmt.Sprintf("%v (retry in %s)", e.Err, e.After) }
func (e *RetryError) Unwrap() error { return e.Err }

// Timeouts; variables so tests can shrink them.
var (
	headerTimeout = 30 * time.Second // until the response headers arrive
	idleTimeout   = 30 * time.Second // between two reads of the body
)

const maxRedirects = 5

// Client fetches from web seeds. The zero value is not usable: use
// NewClient. Safe for concurrent use.
type Client struct {
	allowLocal bool
	http       *http.Client
}

// NewClient returns a client. allowLocal permits loopback and private
// addresses (tests, and nothing else by default).
func NewClient(allowLocal bool) *Client {
	c := &Client{allowLocal: allowLocal}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	tr := &http.Transport{
		Proxy:                 nil, // a torrent's URL never goes through the user's proxy settings
		DialContext:           c.dial(dialer),
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: headerTimeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
	}
	c.http = &http.Client{
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
				return fmt.Errorf("%w (redirected to %s)", ErrBadURL, req.URL.Scheme)
			}
			if len(via) >= maxRedirects {
				return errors.New("web seed: too many redirects")
			}
			return nil // the dialer checks the new host's addresses
		},
	}
	return c
}

// dial resolves the host and connects only to addresses a torrent may make
// us contact. Checking at connect time (not on the URL) also covers
// redirects and DNS names that resolve to local addresses.
func (c *Client) dial(d *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil {
			return nil, err
		}
		var errs []error
		for _, ip := range ips {
			if !c.allowLocal && !Public(ip) {
				errs = append(errs, fmt.Errorf("%w: %s is %s", ErrLocalAddress, host, ip))
				continue
			}
			conn, err := d.DialContext(ctx, network, net.JoinHostPort(ip.Unmap().String(), port))
			if err == nil {
				return conn, nil
			}
			errs = append(errs, err)
		}
		if len(errs) == 0 {
			return nil, fmt.Errorf("web seed: %s has no address", host)
		}
		return nil, errors.Join(errs...)
	}
}

// cgnat is the carrier-grade NAT range (RFC 6598): not the public internet.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

// Public reports whether ip is an ordinary internet address — not this
// machine, a private or link-local network, multicast or unspecified.
func Public(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() &&
		!ip.IsLinkLocalMulticast() && !ip.IsInterfaceLocalMulticast() && !ip.IsMulticast() &&
		!ip.IsUnspecified() && !cgnat.Contains(ip)
}

// Valid checks that base can be a web seed: an http(s) URL with a host.
func Valid(base string) error {
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%w: %q", ErrBadURL, base)
	}
	return nil
}

// URLFor is where file i of the torrent lives on the web seed base (BEP 19):
// a single-file torrent's file is base itself (or base + name when base
// ends in "/"); a multi-file torrent's is base/name/path…, every part
// escaped. The torrent's own names are used, not the ones sanitised for
// this OS's disk.
func URLFor(base string, info *metainfo.Info, i int) (string, error) {
	if err := Valid(base); err != nil {
		return "", err
	}
	if !info.MultiFile {
		if strings.HasSuffix(base, "/") {
			return base + url.PathEscape(info.Name), nil
		}
		return base, nil
	}
	parts := append([]string{info.Name}, info.Files[i].Path...)
	for k, p := range parts {
		parts[k] = url.PathEscape(p)
	}
	if !strings.HasSuffix(base, "/") {
		base += "/"
	}
	return base + strings.Join(parts, "/"), nil
}

// Waiter is called after every read of n bytes of a response body; it
// paces the download (speed limits) and fails when the caller gives up.
type Waiter func(ctx context.Context, n int) error

// Fetch fills dst with the torrent's bytes starting at offset off, from the
// web seed base: one ranged GET per file the span covers.
func (c *Client) Fetch(ctx context.Context, base string, info *metainfo.Info, off int64, dst []byte, wait Waiter) error {
	end := off + int64(len(dst))
	if off < 0 || end > info.TotalLength {
		return fmt.Errorf("web seed: range [%d,%d) outside the torrent", off, end)
	}
	for i, f := range info.Files {
		lo, hi := max(off, f.Offset), min(end, f.Offset+f.Length)
		if lo >= hi {
			continue
		}
		u, err := URLFor(base, info, i)
		if err != nil {
			return err
		}
		if err := c.get(ctx, u, lo-f.Offset, f.Length, dst[lo-off:hi-off], wait); err != nil {
			return err
		}
	}
	return nil
}

// get reads len(dst) bytes of the file at u, from byte from, into dst.
// fileLen is the file's length, to accept a whole-file 200 answer.
func (c *Client) get(ctx context.Context, u string, from, fileLen int64, dst []byte, wait Waiter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	to := from + int64(len(dst)) - 1
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to))
	req.Header.Set("User-Agent", version.UserAgent())
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // the URL is shown by the caller
		}
		return fmt.Errorf("web seed %s: %w", hostOf(u), err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusPartialContent:
		s, e, ok := contentRange(resp.Header.Get("Content-Range"))
		if !ok || s != from || e != to {
			return fmt.Errorf("web seed %s: answered %q for bytes %d-%d", hostOf(u), resp.Header.Get("Content-Range"), from, to)
		}
	case http.StatusOK:
		// The whole file: fine only when that is what we asked for.
		if from != 0 || int64(len(dst)) != fileLen {
			return fmt.Errorf("web seed %s: %w", hostOf(u), ErrNoRanges)
		}
	case http.StatusServiceUnavailable, http.StatusTooManyRequests:
		return &RetryError{After: retryAfter(resp.Header.Get("Retry-After")),
			Err: fmt.Errorf("web seed %s: HTTP %s", hostOf(u), resp.Status)}
	default:
		return fmt.Errorf("web seed %s: HTTP %s", hostOf(u), resp.Status)
	}
	// Read exactly the range; a stalled server is given up after idleTimeout.
	idle := time.AfterFunc(idleTimeout, cancel)
	defer idle.Stop()
	for got := 0; got < len(dst); {
		n, err := resp.Body.Read(dst[got:])
		idle.Reset(idleTimeout)
		got += n
		if n > 0 && wait != nil {
			if werr := wait(ctx, n); werr != nil {
				return werr
			}
		}
		if err != nil {
			if got == len(dst) {
				break
			}
			if errors.Is(err, io.EOF) {
				err = io.ErrUnexpectedEOF
			}
			if ctx.Err() != nil && err != nil {
				err = fmt.Errorf("%w (nothing received for %s)", err, idleTimeout)
			}
			return fmt.Errorf("web seed %s: %w", hostOf(u), err)
		}
	}
	return nil
}

// contentRange parses "bytes START-END/TOTAL".
func contentRange(v string) (start, end int64, ok bool) {
	v, found := strings.CutPrefix(strings.TrimSpace(v), "bytes ")
	if !found {
		return 0, 0, false
	}
	span, _, _ := strings.Cut(v, "/")
	a, b, found := strings.Cut(span, "-")
	if !found {
		return 0, 0, false
	}
	s, err1 := strconv.ParseInt(a, 10, 64)
	e, err2 := strconv.ParseInt(b, 10, 64)
	return s, e, err1 == nil && err2 == nil
}

// retryAfter reads Retry-After (seconds, or an HTTP date); 0 when absent.
func retryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		return max(time.Until(t), 0)
	}
	return 0
}

// hostOf is the host of a URL, for short error messages.
func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil && p.Host != "" {
		return p.Host
	}
	return u
}

// Host is a web seed's host for display ("cdimage.debian.org").
func Host(base string) string { return hostOf(base) }

var defaultOnce sync.Once
var defaultClient *Client

// Default is a shared client that refuses local addresses.
func Default() *Client {
	defaultOnce.Do(func() { defaultClient = NewClient(false) })
	return defaultClient
}
