# Progress

**Current phase:** 5b — UDP trackers · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 5b, tick it in
`docs/ROADMAP.md` and start **Phase 6 — Uploading & seeding** in
`internal/torrent` (+ a listener): accept incoming connections on the
configured port (one listener for all torrents, `peer.Accept` with a
known-hash callback), serve `request`s from storage (16 KiB max, honour
`cancel`), choking algorithm (4 regular slots by upload/download rate, 1
optimistic, rotate every 30s/10s), keep running after completion as a seeder,
upload stats, and controls to pause uploads / stop seeding.

## Known issues
- No uploading yet: we never unchoke peers and don't listen for incoming
  connections (Phase 6). Some peers may deprioritise us for not reciprocating.
- `denco download` has no resume file yet; rerun with `-existing` to reuse
  data (full recheck). Real resume: Phase 7.
- Linux/Windows: compiled + vetted, tests only *run* on macOS.

## Open questions for the user
- none

## Last test run (2026-11-08, end of Phase 5b)
- `go test -race -count=1 ./...` — all 12 packages pass
- `internal/tracker`: `-race -count=20 -cpu=1,2,8` pass
- `internal/torrent`, `internal/peer`: `-race -count=5 -cpu=1,2,8` pass
- mutation checks: disabling stale-ID retry fails TestUDPStaleConnectionIDRetried;
  disabling retransmits fails TestUDPRetransmitsLostPackets
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: tracker 96%, torrent 92%, others unchanged (79–100%)
- not verified: real public UDP trackers (tests are loopback only)
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
- **Phase 5b (2026-11-08):** `tracker.UDPClient` (BEP 15 connect/announce,
  retransmit backoff, per-tracker connection-ID cache, stale-ID recovery, error
  messages, stray-packet filtering, IPv4/IPv6 peer formats, BEP 41 URL data);
  `Client.Announce` dispatches udp://. Tests use an in-process UDP tracker;
  mutation checks confirm the retry/retransmit tests. D25.

