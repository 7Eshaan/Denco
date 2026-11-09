package torrent

import (
	"context"
	"errors"
	"math/rand/v2"
	"time"

	"github.com/7Eshaan/Denco/internal/tracker"
)

// trackerInfo is the latest tracker-reported swarm size (loop-owned).
type trackerInfo struct {
	seeders, leechers int // -1 if unknown
	lastErr           string
	working           bool
}

// announceLoop runs in its own goroutine for the life of the session:
// "started" first, then regular announces at the tracker's interval,
// "completed" as soon as the download finishes, and "stopped" on shutdown.
// Peers and swarm counts are sent to the loop as trackerEvents.
func (s *Session) announceLoop(ctx context.Context) {
	list := tracker.NewList(s.tiers)
	if list.Len() == 0 {
		return
	}
	key := rand.Uint32()
	event := tracker.EventStarted
	// "completed" means a full seed: a partial one (skipped files) never
	// sends it, and its "left" stays above zero (D44).
	completedPending := !s.startedFull // only report a completion we witnessed
	completed := s.full
	if s.startedFull {
		completed = nil // already closed: it would cut every first interval short
	}
	failures := 0
	anyStarted := false
	completedSent := false

	for {
		req := s.announceRequest(key, event)
		results := list.AnnounceAll(ctx, s.cfg.Announcer, req)
		if ctx.Err() != nil {
			break
		}
		ev, interval := summarize(results)
		s.send(ctx, ev)

		if ev.ok {
			anyStarted = true
			failures = 0
			if event == tracker.EventCompleted {
				completedSent = true
			}
			event = tracker.EventNone
		} else {
			failures++
			interval = retryDelay(s.cfg.AnnounceRetry, failures)
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		case <-completed:
			timer.Stop()
			completed = nil // fire once
			if completedPending {
				event = tracker.EventCompleted
			}
		}
		if ctx.Err() != nil {
			break
		}
	}

	if anyStarted {
		// ctx is already cancelled, so these get a short timeout of their own.
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// If we finished but were stopped before reporting it, report it now:
		// trackers count completions from this event.
		if completedPending && !completedSent && isClosed(s.full) {
			list.AnnounceAll(sctx, s.cfg.Announcer, s.announceRequest(key, tracker.EventCompleted))
		}
		list.AnnounceAll(sctx, s.cfg.Announcer, s.announceRequest(key, tracker.EventStopped))
	}
}

func isClosed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// port is the port we tell trackers, the DHT and peers to reach us on.
func (s *Session) port() uint16 {
	if s.cfg.PublicPort != nil {
		if p := s.cfg.PublicPort(); p != 0 {
			return p
		}
	}
	return s.cfg.Port
}

func (s *Session) announceRequest(key uint32, ev tracker.Event) tracker.Request {
	return tracker.Request{
		InfoHash:   s.mi.InfoHash,
		PeerID:     s.cfg.PeerID,
		Port:       s.port(),
		Uploaded:   s.uploaded.Load(),
		Downloaded: s.downloaded.Load(),
		Left:       s.left.Load(),
		Event:      ev,
		Key:        key,
	}
}

// summarize merges per-tier results: all peers, the largest swarm counts,
// and the shortest interval any working tracker asked for.
func summarize(results []tracker.Result) (trackerEvent, time.Duration) {
	ev := trackerEvent{seeders: -1, leechers: -1}
	var interval time.Duration
	var errs []error
	for _, r := range results {
		if r.Err != nil {
			errs = append(errs, r.Err)
			continue
		}
		ev.ok = true
		ev.peers = append(ev.peers, r.Response.Peers...)
		ev.seeders = max(ev.seeders, r.Response.Seeders)
		ev.leechers = max(ev.leechers, r.Response.Leechers)
		if interval == 0 || r.Response.Interval < interval {
			interval = r.Response.Interval
		}
	}
	if !ev.ok {
		ev.err = errors.Join(errs...)
	}
	return ev, interval
}

func retryDelay(base time.Duration, failures int) time.Duration {
	d := base
	for i := 1; i < failures && d < 30*time.Minute; i++ {
		d *= 2
	}
	return min(d, 30*time.Minute)
}

// onTracker is the loop side of a tracker result.
func (s *Session) onTracker(e trackerEvent) {
	s.tracker.working = e.ok
	if e.ok {
		s.tracker.seeders, s.tracker.leechers = e.seeders, e.leechers
		s.tracker.lastErr = ""
		s.swarm.add(e.peers)
	} else if e.err != nil {
		s.tracker.lastErr = e.err.Error()
		s.log.Warn("all trackers failed", "err", e.err)
	}
}
