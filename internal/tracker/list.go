package tracker

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// Announcer is anything that can announce to one tracker URL. *Client
// implements it; tests substitute fakes.
type Announcer interface {
	Announce(ctx context.Context, trackerURL string, req Request) (*Response, error)
}

// Result is the outcome of announcing to one tier.
type Result struct {
	Tier     int
	URL      string    // the tracker that answered (empty on failure)
	Response *Response // nil on failure
	Err      error     // all attempt errors joined, nil on success
}

// defaultAttemptTimeout bounds each single-tracker attempt so one dead
// tracker can't stall its whole tier.
const defaultAttemptTimeout = 15 * time.Second

// List manages a torrent's trackers following BEP 12 tiers.
//
// Within a tier, trackers are tried in order until one answers, and the one
// that answered moves to the front so it is tried first next time. Unlike a
// strict reading of BEP 12, *all tiers are announced to concurrently* (the
// default in libtorrent/qBittorrent), which finds many more peers when lots
// of public trackers are dead. See DECISIONS D14.
type List struct {
	AttemptTimeout time.Duration // 0 means 15s

	mu    sync.Mutex
	tiers [][]string
}

// NewList builds a List from tiers, dropping empty and duplicate URLs and
// shuffling each tier as BEP 12 requires (spreads load across trackers).
func NewList(tiers [][]string) *List {
	seen := make(map[string]bool)
	l := &List{}
	for _, tier := range tiers {
		var t []string
		for _, u := range tier {
			if u != "" && !seen[u] {
				seen[u] = true
				t = append(t, u)
			}
		}
		if len(t) == 0 {
			continue
		}
		rand.Shuffle(len(t), func(i, j int) { t[i], t[j] = t[j], t[i] })
		l.tiers = append(l.tiers, t)
	}
	return l
}

// Tiers returns a copy of the current tier order (for display).
func (l *List) Tiers() [][]string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([][]string, len(l.tiers))
	for i, t := range l.tiers {
		out[i] = append([]string(nil), t...)
	}
	return out
}

// Len returns the number of tiers.
func (l *List) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.tiers)
}

// AnnounceAll announces to every tier concurrently and returns one Result
// per tier, in tier order. It returns when all tiers have finished.
func (l *List) AnnounceAll(ctx context.Context, a Announcer, req Request) []Result {
	n := l.Len()
	results := make([]Result, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = l.announceTier(ctx, a, i, req)
		}(i)
	}
	wg.Wait()
	return results
}

func (l *List) announceTier(ctx context.Context, a Announcer, tier int, req Request) Result {
	l.mu.Lock()
	urls := append([]string(nil), l.tiers[tier]...)
	l.mu.Unlock()

	timeout := l.AttemptTimeout
	if timeout <= 0 {
		timeout = defaultAttemptTimeout
	}

	var errs []error
	for _, u := range urls {
		if ctx.Err() != nil {
			errs = append(errs, ctx.Err())
			break
		}
		actx, cancel := context.WithTimeout(ctx, timeout)
		resp, err := a.Announce(actx, u, req)
		cancel()
		if err == nil {
			l.promote(tier, u)
			return Result{Tier: tier, URL: u, Response: resp}
		}
		errs = append(errs, fmt.Errorf("%s: %w", u, err))
	}
	return Result{Tier: tier, Err: errors.Join(errs...)}
}

// promote moves u to the front of its tier.
func (l *List) promote(tier int, u string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.tiers[tier]
	for i, v := range t {
		if v == u {
			copy(t[1:i+1], t[:i])
			t[0] = u
			return
		}
	}
}

// TiersFromMetaInfo returns the tracker tiers of a .torrent: announce-list if
// present (BEP 12 says it overrides announce), otherwise the single announce.
func TiersFromMetaInfo(mi *metainfo.MetaInfo) [][]string {
	if len(mi.AnnounceList) > 0 {
		return mi.AnnounceList
	}
	if mi.Announce != "" {
		return [][]string{{mi.Announce}}
	}
	return nil
}

// TiersFromMagnet puts each magnet tracker in its own tier, so all of them
// are announced to (magnet links carry no tier information).
func TiersFromMagnet(m *magnet.Magnet) [][]string {
	tiers := make([][]string, 0, len(m.Trackers))
	for _, tr := range m.Trackers {
		tiers = append(tiers, []string{tr})
	}
	return tiers
}
