# Progress

**Current phase:** 4 — Storage & pieces · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 4, tick it in
`docs/ROADMAP.md` and start **Phase 5 — Single-torrent engine**
(`internal/torrent`): a session goroutine that runs the tracker loop, keeps a
pool of `peer.Conn`s, tracks per-peer state (choked/interested/bitfield),
feeds `pieces.Manager`, hashes completed pieces off-thread, writes via
`storage`, sends haves, handles reconnects/timeouts/bad peers. End-to-end test
with an in-process tracker + seeder; a dev `denco download` command.

## Known issues
- UDP trackers (`udp://`) are skipped until Phase 5b (D16).
- Linux/Windows: code compiles and vets for both, but tests have only been
  *run* on macOS. Windows path rules are unit-tested via an explicit flag;
  real NTFS behaviour (locks, attributes) is untested.
- `pieces.Manager` picking is O(pieces) per pick (D22) — fine for now.

## Open questions for the user
- none

## Last test run (2026-11-08, end of Phase 4)
- `go test -race -count=1 ./...` — all 11 packages pass
- `go test -race -count=20` on pieces, storage, peer — pass
- mutation check: inverting rarest-first comparison makes TestRarestFirst fail (test is effective)
- `go vet ./...` (darwin + windows) clean · `gofmt -l .` clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: pieces 98%, storage 92%, peer 93%, tracker 97%, cmd/denco 85%,
  others 90–100%
- external deps: none

## History
- **Phase 1 (2026-11-08, commit cbcccbb):** module scaffold; `bencode`;
  `metainfo`; `format`; `denco inspect`. Decisions D1–D11.
- **Phase 2 (2026-11-08, commit 2856b0e):** `magnet`; `peerid`; `tracker`
  HTTP client + BEP 12 `List`; `denco magnet`, `denco announce`. D12–D16.
- **Phase 3 (2026-11-08, commit d32d31d):** `bitfield`; `peer` (handshake,
  codec, `Conn`); `peerid.ClientName`; `denco probe`. Peer ID DE→DN. D17–D19.
- **Phase 4 (2026-11-08):** `storage` (Layout with OS-safe naming,
  case-insensitive collision handling, containment checks; Storage with
  cross-file I/O, handle LRU, no-overwrite, read-only mode, safe delete);
  `pieces` (Check/VerifyPiece/parallel Recheck; Manager with rarest-first,
  random-first, endgame, memory budget, contributor tracking); `denco verify`.
  Caught while writing: suffix-truncation infinite loop for 255-byte names.
  D20–D22.
