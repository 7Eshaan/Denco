package torrent

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"time"

	"github.com/7Eshaan/Denco/internal/bitfield"
	"github.com/7Eshaan/Denco/internal/peer"
	"github.com/7Eshaan/Denco/internal/pieces"
	"github.com/7Eshaan/Denco/internal/webseed"
)

// Web seeds (BEP 19, DECISIONS D61). Each URL of the torrent's url-list is
// a pseudo-peer that has every piece. The loop gives an idle web seed a
// batch of blocks from the piece Manager (as it would a peer's request
// pipeline); a helper goroutine fetches them with ranged HTTP requests
// (webseed.Client) and hands the data back to the loop, which treats it
// exactly like blocks from a peer: BlockReceived, hash check, write, have
// messages. A web seed that fails is retried with backoff (or when the
// server's Retry-After says); one that can't work (no range support, a
// local address) or sent data failing the hash check is given up for the
// rest of the run.
//
// The Manager knows peers by address. A web seed's key is the unspecified
// IPv6 address with its index + 1 as the port: no real peer can have it
// (the swarm drops unspecified addresses, and nobody connects from one).

// webSeedBlocks is how many blocks one fetch asks for: at least a piece,
// at least 256 KiB, at most 4 MiB.
func webSeedBlocks(pieceLen int64) int {
	return min(256, max(16, int(pieceLen/peer.BlockSize)))
}

// webSeed is one web seed's state. Loop-owned, except url and key, which
// never change.
type webSeed struct {
	url string
	key pieces.PeerKey

	busy       bool      // a fetch is running
	nextTry    time.Time // backing off until then
	failures   int       // in a row
	strikes    int       // pieces it contributed to that failed the hash check
	disabled   string    // why it was given up for this run ("" = usable)
	lastErr    string
	downloaded int64
	rate       rate
}

func webSeedKey(i int) pieces.PeerKey {
	return netip.AddrPortFrom(netip.IPv6Unspecified(), uint16(i+1))
}

// newWebSeeds turns the torrent's url-list into web seeds: usable URLs
// (http/https), each once.
func newWebSeeds(urls []string) []*webSeed {
	var out []*webSeed
	seen := map[string]bool{}
	for _, u := range urls {
		if seen[u] || len(out) >= 64 {
			continue
		}
		seen[u] = true
		if webseed.Valid(u) != nil {
			continue
		}
		out = append(out, &webSeed{url: u, key: webSeedKey(len(out))})
	}
	return out
}

// Events from web seed fetches.
type (
	webData struct { // a contiguous run of blocks fetched
		ws     *webSeed
		blocks []peer.Block
		data   []byte
	}
	webDone struct { // the fetch ended (err nil: everything was sent)
		ws  *webSeed
		err error
	}
)

func (s *Session) webSeedFor(key pieces.PeerKey) *webSeed {
	for _, ws := range s.webSeeds {
		if ws.key == key {
			return ws
		}
	}
	return nil
}

// runWebSeeds gives every idle, usable web seed something to fetch, while
// the download isn't complete (loop goroutine).
func (s *Session) runWebSeeds(ctx context.Context, now time.Time) {
	if len(s.webSeeds) == 0 || ctx.Err() != nil || s.m.Complete() {
		return
	}
	if s.allPieces == nil {
		s.allPieces = bitfield.New(s.info.NumPieces())
		s.allPieces.SetAll()
	}
	for _, ws := range s.webSeeds {
		if ws.busy || ws.disabled != "" || now.Before(ws.nextTry) {
			continue
		}
		blocks := s.m.NextRequests(ws.key, s.allPieces, webSeedBlocks(s.info.PieceLength))
		if len(blocks) == 0 {
			return // nothing left to ask anyone for
		}
		ws.busy = true
		s.goHelper(func() { s.fetchWebSeed(ctx, ws, blocks) })
	}
}

// fetchWebSeed fetches blocks from ws in contiguous runs and sends them to
// the loop (helper goroutine: touches only ws.url).
func (s *Session) fetchWebSeed(ctx context.Context, ws *webSeed, blocks []peer.Block) {
	offset := func(b peer.Block) int64 { return int64(b.Index)*s.info.PieceLength + int64(b.Begin) }
	slices.SortFunc(blocks, func(a, b peer.Block) int { return int(offset(a) - offset(b)) })
	wait := func(ctx context.Context, n int) error {
		if err := s.cfg.TorrentDownLimiter.WaitN(ctx, n); err != nil {
			return err
		}
		return s.cfg.PeerOpts.ReadLimiter.WaitN(ctx, n)
	}
	for len(blocks) > 0 {
		run := 1
		for run < len(blocks) && offset(blocks[run]) == offset(blocks[run-1])+int64(blocks[run-1].Length) {
			run++
		}
		off := offset(blocks[0])
		end := offset(blocks[run-1]) + int64(blocks[run-1].Length)
		buf := make([]byte, end-off)
		if err := s.cfg.WebSeeds.Fetch(ctx, ws.url, s.info, off, buf, wait); err != nil {
			s.send(ctx, webDone{ws, err})
			return
		}
		s.send(ctx, webData{ws, blocks[:run], buf})
		blocks = blocks[run:]
	}
	s.send(ctx, webDone{ws, nil})
}

// onWebData takes fetched blocks in like piece messages from a peer.
func (s *Session) onWebData(e webData) {
	off := int64(0)
	for _, b := range e.blocks {
		data := e.data[off : off+int64(b.Length)]
		off += int64(b.Length)
		r := s.m.BlockReceived(e.ws.key, b.Index, b.Begin, data)
		if r.Accepted {
			n := int64(len(data))
			e.ws.downloaded += n
			e.ws.rate.add(n)
			s.downRate.add(n)
			s.downloaded.Add(n)
		}
		s.cancelElsewhere(r.Cancel, b)
		if r.Complete {
			s.verifyAndWrite(s.ctx(), int(b.Index), r.Data, r.Contributors)
		}
	}
}

// onWebDone frees a web seed for the next fetch, backing off after errors.
func (s *Session) onWebDone(ctx context.Context, e webDone) {
	ws := e.ws
	ws.busy = false
	s.m.PeerRequestsDropped(ws.key) // whatever it didn't deliver goes back to the pool
	now := time.Now()
	var retry *webseed.RetryError
	switch {
	case e.err == nil:
		ws.failures, ws.lastErr = 0, ""
	case ctx.Err() != nil:
		return
	case errors.Is(e.err, webseed.ErrNoRanges), errors.Is(e.err, webseed.ErrLocalAddress), errors.Is(e.err, webseed.ErrBadURL):
		ws.disabled = e.err.Error()
		s.log.Info("web seed given up", "url", ws.url, "err", e.err)
	case errors.As(e.err, &retry):
		ws.failures++
		ws.lastErr = e.err.Error()
		ws.nextTry = now.Add(min(max(retry.After, retryDelay(s.cfg.RetryBase, ws.failures)), maxBackoff))
	default:
		ws.failures++
		ws.lastErr = e.err.Error()
		ws.nextTry = now.Add(retryDelay(s.cfg.RetryBase, ws.failures))
		s.log.Debug("web seed failed", "url", ws.url, "err", e.err, "retry", ws.nextTry.Sub(now).Round(time.Second))
	}
	s.runWebSeeds(ctx, now)
}

// blameWebSeed counts a failed hash check against a web seed: alone it is
// certainly wrong and is given up at once, shared after 3 strikes — the
// rules peers get.
func (s *Session) blameWebSeed(ws *webSeed, alone bool) {
	ws.strikes++
	if alone || ws.strikes >= 3 {
		ws.disabled = "sent data that failed the hash check"
		s.log.Warn("web seed given up: bad data", "url", ws.url)
	}
}

// webSeedNote describes a web seed's state for the peer list.
func (ws *webSeed) note(now time.Time) string {
	switch {
	case ws.disabled != "":
		return "off: " + ws.disabled
	case ws.busy:
		return "downloading"
	case now.Before(ws.nextTry):
		return "retrying in " + ws.nextTry.Sub(now).Round(time.Second).String() + ": " + ws.lastErr
	}
	return "idle"
}
