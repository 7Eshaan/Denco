# Progress

**Current phase:** 6 — Uploading & seeding · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 6, tick it in
`docs/ROADMAP.md` and start **Phase 7 — Resume** (`internal/resume`): a
per-torrent state file (info hash, .torrent bytes, download dir, have
bitfield, uploaded/downloaded totals, options) written atomically
(temp file + rename) on a timer, on completion and on clean shutdown; on
start, trust it only if file sizes/mtimes still match, otherwise recheck the
affected pieces. Wire into `denco download` (auto-resume without -existing).

## Known issues
- `denco download` has no resume file yet; rerun with `-existing` to reuse
  data (full recheck). Real resume: Phase 7.
- No rate limiting yet (Phase 9).
- No NAT traversal (UPnP/NAT-PMP): behind a home router, incoming peers can
  only reach us if the port is forwarded manually. Dialing out still works.
  Not on the roadmap — candidate for Phase 12.
- Linux/Windows: compiled + vetted, tests only *run* on macOS.

## Open questions for the user
- none

## Last test run (2026-11-08, end of Phase 6)
- `go test -race -count=1 ./...` — all 12 packages pass
- `internal/torrent`: `-race -count=20` pass; with `internal/peer`:
  `-race -count=5 -cpu=1,2,8` pass
- `cmd/denco`: `-race -count=10` pass
- mutation check: disabling cancel makes TestCancelledRequestNotSent fail (20/20 sent)
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: torrent 92%, tracker 96%, peer 93%, cmd/denco 80%, others 90–100%
- not verified: seeding to real internet peers (tests are loopback only)
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
- **Phase 5 (2026-11-08, commit a581894):** `torrent.Session` (event loop, peer state machine,
  request pipelining, endgame cancels, snub timeout, dialing with backoff +
  reconnect, bans for bad data, off-loop hash+write, announce loop with
  started/completed/stopped, Stats/Peers snapshots, rate meter);
  `peer.Conn.TrySend`; `denco download`. Bugs found by multi-CPU test runs and
  fixed: download stall after a late hash failure (regression test verified by
  mutation), `completed` lost on quick stop, stale stats at completion. D23–D24.
- **Phase 5b (2026-11-08, commit 76add54):** `tracker.UDPClient` (BEP 15 connect/announce,
  retransmit backoff, per-tracker connection-ID cache, stale-ID recovery, error
  messages, stray-packet filtering, IPv4/IPv6 peer formats, BEP 41 URL data);
  `Client.Announce` dispatches udp://. Tests use an in-process UDP tracker;
  mutation checks confirm the retry/retransmit tests. D25.
- **Phase 6 (2026-11-08):** `torrent.Listener`; uploading (request
  validation, cancel, per-peer uploader goroutine, upload stats); tit-for-tat
  choker with optimistic unchoke (pure `chooseUnchoked`); seeding after
  completion (drop seed↔seed, keep dialing), `NoSeed`, `SetUploadsPaused`;
  bans by IP; `denco download -seed`, listener with port fallback. Found and
  fixed: an edit script silently applied 1 of 8 changes (Python tuple
  expression) — caught by the seed-drop test; brittle cancel test rewritten
  and mutation-checked. D26.

