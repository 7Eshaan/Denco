# Progress

**Current phase:** 5 — Single-torrent engine · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 5, tick it in
`docs/ROADMAP.md` and start **Phase 5b — UDP trackers** (BEP 15) in
`internal/tracker` (new `udp.go`; make `Client.Announce` dispatch on scheme so
`tracker.List` needs no change). Then Phase 6 (uploading & seeding).

## Known issues
- UDP trackers (`udp://`) are skipped until Phase 5b (D16).
- No uploading yet: we never unchoke peers and don't listen for incoming
  connections (Phase 6). Some peers may deprioritise us for not reciprocating.
- `denco download` has no resume file yet; rerun with `-existing` to reuse
  data (full recheck). Real resume: Phase 7.
- Linux/Windows: compiled + vetted, tests only *run* on macOS.

## Open questions for the user
- none

## Last test run (2026-11-08, end of Phase 5)
- `go test -race -count=1 ./...` — all 12 packages pass
- `internal/torrent`: `-race -count=30` pass; `-race -count=5 -cpu=1,2,8` pass
- `internal/peer`, `cmd/denco`: `-race -count=10` pass
- goroutine-leak test (10 sessions stopped at different stages) — pass
- mutation check: removing the stall fix makes `TestHashFailureNearEndDoesNotStall` fail 5/8 runs
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: torrent 92%, pieces 98%, storage 92%, peer 93%, tracker 97%,
  cmd/denco 79%, others 90–100%
- not verified: a real download from the public internet (all tests use
  loopback seeders + httptest trackers). `denco download <file.torrent>` is
  ready for that.
- external deps: none

## History
- **Phase 1 (2026-11-08, commit cbcccbb):** module scaffold; `bencode`;
  `metainfo`; `format`; `denco inspect`. Decisions D1–D11.
- **Phase 2 (2026-11-08, commit 2856b0e):** `magnet`; `peerid`; `tracker`
  HTTP client + BEP 12 `List`; `denco magnet`, `denco announce`. D12–D16.
- **Phase 3 (2026-11-08, commit d32d31d):** `bitfield`; `peer` (handshake,
  codec, `Conn`); `peerid.ClientName`; `denco probe`. Peer ID DE→DN. D17–D19.
- **Phase 4 (2026-11-08, commit 9bbe2bd):** `storage` (Layout with OS-safe naming,
  case-insensitive collision handling, containment checks; Storage with
  cross-file I/O, handle LRU, no-overwrite, read-only mode, safe delete);
  `pieces` (Check/VerifyPiece/parallel Recheck; Manager with rarest-first,
  random-first, endgame, memory budget, contributor tracking); `denco verify`.
  Caught while writing: suffix-truncation infinite loop for 255-byte names.
  D20–D22.
- **Phase 5 (2026-11-08):** `torrent.Session` (event loop, peer state machine,
  request pipelining, endgame cancels, snub timeout, dialing with backoff +
  reconnect, bans for bad data, off-loop hash+write, announce loop with
  started/completed/stopped, Stats/Peers snapshots, rate meter);
  `peer.Conn.TrySend`; `denco download`. Bugs found by multi-CPU test runs and
  fixed: download stall after a late hash failure (regression test verified by
  mutation), `completed` lost on quick stop, stale stats at completion. D23–D24.

