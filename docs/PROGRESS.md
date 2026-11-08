# Progress

**Current phase:** 3 — Peer wire protocol · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 3, tick it in
`docs/ROADMAP.md` and start **Phase 4 — Storage & pieces** (`internal/storage`,
`internal/pieces`): piece↔file mapping, safe path resolution incl. Windows
sanitising (see D8), no-overwrite policy, piece verification, rarest-first
picker, block bookkeeping.

## Known issues
- UDP trackers (`udp://`) are skipped until Phase 5b (D16).
- Peer ID prefix changed from `-DE` to `-DN` this phase (DE is Deluge's code,
  D13 amended). Nothing persisted the old one, so no migration is needed.

## Open questions for the user
- none

## Last test run (2026-11-08, end of Phase 3)
- `go test -race -count=1 ./...` — all 9 packages pass
- `go test -race -count=20` on peer, tracker, cmd (timing-sensitive) — pass
- goroutine-leak test (50 connect/close cycles) — pass
- fuzz: `FuzzReadMessage` 10s, no crashers (manual, not in CI)
- `go vet ./...` clean · `gofmt -l .` clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: bencode 97%, bitfield 100%, format 100%, magnet 98%,
  metainfo 90%, peer 93%, peerid 96%, tracker 97%, cmd/denco 85%
- not verified: real public trackers/peers (tests use loopback only);
  `denco probe <torrent>` is the tool for that
- external deps: none

## History
- **Phase 1 (2026-11-08, commit cbcccbb):** module scaffold; `bencode`;
  `metainfo`; `format`; `denco inspect`. Decisions D1–D11.
- **Phase 2 (2026-11-08, commit 2856b0e):** `magnet`; `peerid`; `tracker`
  HTTP client + BEP 12 `List`; `denco magnet`, `denco announce`. Fixed: failure
  reason lost on non-200. D12–D16 (D16: UDP trackers → Phase 5b).
- **Phase 3 (2026-11-08):** `bitfield`; `peer` (handshake, message codec +
  fuzz test, `Conn` with reader/writer goroutines, keep-alive, idle timeout,
  self-connection + info-hash checks, ctx-cancellable dial); `peerid.ClientName`;
  `denco probe`. Changed peer ID prefix DE→DN. D17–D19.
