package tracker

import (
	"context"
	"errors"
	"net/url"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
)

// fakeAnnouncer succeeds for URLs in ok, fails otherwise, and records calls.
type fakeAnnouncer struct {
	mu    sync.Mutex
	ok    map[string]bool
	slow  map[string]bool // blocks until ctx is done
	calls []string
}

func (f *fakeAnnouncer) Announce(ctx context.Context, u string, req Request) (*Response, error) {
	f.mu.Lock()
	f.calls = append(f.calls, u)
	ok, slow := f.ok[u], f.slow[u]
	f.mu.Unlock()
	if slow {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if ok {
		return &Response{Interval: time.Minute, Seeders: 1}, nil
	}
	return nil, errors.New("connection refused")
}

func TestNewListDedupAndEmpty(t *testing.T) {
	l := NewList([][]string{{"a", "", "b"}, {}, {"a", "c"}, {""}})
	tiers := l.Tiers()
	if len(tiers) != 2 {
		t.Fatalf("tiers = %v", tiers)
	}
	if len(tiers[0]) != 2 || !reflect.DeepEqual(tiers[1], []string{"c"}) {
		t.Fatalf("tiers = %v", tiers)
	}
}

func TestAnnounceAllFallbackAndPromote(t *testing.T) {
	l := NewList(nil)
	l.tiers = [][]string{{"dead1", "dead2", "good"}, {"good2"}, {"deadonly"}} // fixed order (no shuffle)
	f := &fakeAnnouncer{ok: map[string]bool{"good": true, "good2": true}}

	res := l.AnnounceAll(context.Background(), f, Request{})
	if len(res) != 3 {
		t.Fatalf("results = %d", len(res))
	}
	if res[0].URL != "good" || res[0].Err != nil || res[0].Response == nil {
		t.Errorf("tier 0 = %+v", res[0])
	}
	if res[1].URL != "good2" {
		t.Errorf("tier 1 = %+v", res[1])
	}
	if res[2].Err == nil || res[2].Response != nil {
		t.Errorf("tier 2 should fail: %+v", res[2])
	}

	// "good" must now be first in tier 0, others keep relative order.
	if got := l.Tiers()[0]; !reflect.DeepEqual(got, []string{"good", "dead1", "dead2"}) {
		t.Errorf("after promote tier 0 = %v", got)
	}

	// Second round: tier 0 should hit "good" immediately.
	f.calls = nil
	l.AnnounceAll(context.Background(), f, Request{})
	count := 0
	for _, c := range f.calls {
		if c == "dead1" || c == "dead2" {
			count++
		}
	}
	if count != 0 {
		t.Errorf("dead trackers retried before promoted one: %v", f.calls)
	}
}

func TestAnnounceAttemptTimeout(t *testing.T) {
	l := NewList(nil)
	l.tiers = [][]string{{"hang", "good"}}
	l.AttemptTimeout = 30 * time.Millisecond
	f := &fakeAnnouncer{ok: map[string]bool{"good": true}, slow: map[string]bool{"hang": true}}

	start := time.Now()
	res := l.AnnounceAll(context.Background(), f, Request{})
	if res[0].URL != "good" {
		t.Fatalf("hanging tracker blocked fallback: %+v", res[0])
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("attempt timeout not applied")
	}
}

func TestAnnounceAllCancelled(t *testing.T) {
	l := NewList([][]string{{"a", "b"}})
	f := &fakeAnnouncer{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := l.AnnounceAll(ctx, f, Request{})
	if !errors.Is(res[0].Err, context.Canceled) {
		t.Fatalf("err = %v", res[0].Err)
	}
	if len(f.calls) != 0 {
		t.Errorf("should not call trackers after cancel: %v", f.calls)
	}
}

func TestAnnounceAllUnsupportedSkipped(t *testing.T) {
	// Real Client: udp:// is rejected instantly and the http one is tried next.
	srv := serve(t, 200, func(url.Values) any { return map[string]any{"interval": 60} })
	l := NewList(nil)
	l.tiers = [][]string{{"udp://tracker.example:80", srv.URL}}
	res := l.AnnounceAll(context.Background(), &Client{}, baseRequest())
	if res[0].URL != srv.URL {
		t.Fatalf("result = %+v", res[0])
	}
}

func TestTiersHelpers(t *testing.T) {
	mi := &metainfo.MetaInfo{Announce: "a"}
	if got := TiersFromMetaInfo(mi); !reflect.DeepEqual(got, [][]string{{"a"}}) {
		t.Errorf("announce only = %v", got)
	}
	mi.AnnounceList = [][]string{{"b", "c"}, {"d"}}
	if got := TiersFromMetaInfo(mi); !reflect.DeepEqual(got, mi.AnnounceList) {
		t.Errorf("announce-list = %v", got)
	}
	if got := TiersFromMetaInfo(&metainfo.MetaInfo{}); got != nil {
		t.Errorf("trackerless = %v", got)
	}
	m := &magnet.Magnet{Trackers: []string{"x", "y"}}
	if got := TiersFromMagnet(m); !reflect.DeepEqual(got, [][]string{{"x"}, {"y"}}) {
		t.Errorf("magnet = %v", got)
	}
}
