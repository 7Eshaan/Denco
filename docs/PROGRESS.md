# Progress

**Current phase:** 2 — Discovery · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 2, tick it in
`docs/ROADMAP.md` and start **Phase 3 — Peer wire protocol** (`internal/peer`,
`internal/bitfield`): handshake, message codec, connection read/write loops.

## Known issues
- UDP trackers (`udp://`) are skipped until Phase 5b (D16), so some public
  torrents/magnets find few peers for now.

## Open questions for the user
- none (UDP trackers: approved, scheduled as Phase 5b — D16)

## Last test run (2026-11-08, end of Phase 2)
- `go test -race -count=1 ./...` — all 7 packages pass
- `go test -race -count=20` on tracker + cmd (timing-sensitive) — pass
- `go vet ./...` clean · `gofmt -l .` clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: bencode 97%, format 100%, magnet 98%, metainfo 90%,
  peerid 89%, tracker 97%, cmd/denco 87%
- not verified: announcing to a real public tracker (tests use local
  `httptest` servers only)
- external deps: none

## History
- **Phase 1 (2026-11-08, commit cbcccbb):** module scaffold; `bencode`;
  `metainfo`; `format`; `denco inspect`. Decisions D1–D11.
- **Phase 2 (2026-11-08):** `magnet` (hex/base32 btih, dn, tr, x.pe,
  String round trip); `peerid`; `tracker` HTTP client (compact/dict/IPv6
  peers, failure reasons, passkey-safe query, size cap, interval clamp) and
  `List` (BEP 12 tiers, all tiers concurrently, fallback + promotion,
  per-attempt timeout); CLI split per command + `denco magnet`,
  `denco announce`. Fixed during testing: tracker failure reason was lost
  when sent with a non-200 status. Decisions D12–D16 (D16: UDP trackers
  added to roadmap as Phase 5b).
