package engine

import (
	"errors"
	"fmt"
	"slices"
	"sort"
)

// The download queue (DECISIONS D58). Every torrent has a queue key; the
// scheduler starts incomplete torrents in key order, at most MaxActive at
// once. Users see a torrent's position among the incomplete ones ("2 of
// 5") and move it up, down, to the top or to the bottom. A forced torrent
// starts regardless of MaxActive and doesn't take a slot. Complete
// torrents keep their key (they seed without a slot) and get their place
// back if they become incomplete again (more files wanted).

// Queue moves (MoveInQueue).
const (
	QueueTop    = "top"
	QueueUp     = "up"
	QueueDown   = "down"
	QueueBottom = "bottom"
)

var (
	// ErrBadQueueMove: unknown move.
	ErrBadQueueMove = errors.New("queue move must be top, up, down or bottom")
	// ErrNotQueued: the torrent is complete, and the queue only orders downloads.
	ErrNotQueued = errors.New("not in the download queue")
)

// queueOrderLocked is every torrent in queue order (key, then ID).
func (e *Engine) queueOrderLocked() []*entry {
	out := make([]*entry, 0, len(e.byID))
	for _, t := range e.byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].queue != out[j].queue {
			return out[i].queue < out[j].queue
		}
		return out[i].id < out[j].id
	})
	return out
}

// nextQueueLocked is the key for a new torrent: after every other one.
func (e *Engine) nextQueueLocked() int {
	n := 0
	for _, t := range e.byID {
		n = max(n, t.queue)
	}
	return n + 1
}

// assignQueueLocked gives torrents saved without a queue key (older
// versions, `denco download`) one after the others, in ID order, and
// saves it.
func (e *Engine) assignQueueLocked() {
	var missing []*entry
	for _, t := range e.byID {
		if t.queue == 0 {
			missing = append(missing, t)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i].id < missing[j].id })
	for _, t := range missing {
		t.queue = e.nextQueueLocked()
		e.persistLocked(t)
	}
}

// queueRanks is each incomplete torrent's position in the queue (from 1)
// and how many there are. Computed once per listing: deciding whether a
// torrent is complete isn't free.
type queueRanks struct {
	rank  map[*entry]int
	total int
}

func (e *Engine) queueRanksLocked() *queueRanks {
	q := &queueRanks{rank: map[*entry]int{}}
	for _, t := range e.queueOrderLocked() {
		if !t.removing && !t.completeLocked() {
			q.total++
			q.rank[t] = q.total
		}
	}
	return q
}

// MoveInQueue moves an incomplete torrent in the download queue: QueueTop,
// QueueUp, QueueDown or QueueBottom. Up and down step past one other
// incomplete torrent (complete ones don't count: they aren't queued). The
// queue is re-evaluated at once, but a torrent already downloading isn't
// stopped to make room.
func (e *Engine) MoveInQueue(ref, move string) (Torrent, error) {
	switch move {
	case QueueTop, QueueUp, QueueDown, QueueBottom:
	default:
		return Torrent{}, fmt.Errorf("%w (got %q)", ErrBadQueueMove, move)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	t, err := e.resolveLocked(ref)
	if err != nil {
		return Torrent{}, err
	}
	if t.completeLocked() {
		return Torrent{}, fmt.Errorf("#%d %s: %w (it's complete; the queue only orders downloads)", t.id, t.name(), ErrNotQueued)
	}
	var order []*entry
	for _, o := range e.queueOrderLocked() {
		if !o.removing {
			order = append(order, o)
		}
	}
	i := slices.Index(order, t)
	order = slices.Delete(order, i, i+1)
	queued := func(o *entry) bool { return !o.completeLocked() }
	at := i
	switch move {
	case QueueTop:
		at = 0
	case QueueBottom:
		at = len(order)
	case QueueUp:
		for j := i - 1; j >= 0; j-- {
			if queued(order[j]) {
				at = j // just before the previous download
				break
			}
		}
	case QueueDown:
		for j := i; j < len(order); j++ {
			if queued(order[j]) {
				at = j + 1 // just after the next download
				break
			}
		}
	}
	order = slices.Insert(order, at, t)
	for k, o := range order {
		if o.queue != k+1 {
			o.queue = k + 1
			e.flagsChangedLocked(o)
		}
	}
	if at != i {
		e.log.Info("queue changed", "id", t.id, "torrent", t.name(), "move", move)
	}
	e.scheduleLocked()
	return e.viewLocked(t), nil
}
