# Progress

**Current phase:** 1 — Foundations · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 1, start **Phase 2 —
Discovery** (`internal/magnet`, `internal/tracker`) as laid out in
`docs/ROADMAP.md`.

## Known issues
- none

## Open questions for the user
- none

## Last test run (2026-11-08, end of Phase 1)
- `go test -race -count=1 ./...` — all 4 packages pass
- `go vet ./...` — clean · `gofmt -l .` — clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: bencode 97%, metainfo 90%, format 100%, cmd/denco 83%
- fuzz: `FuzzDecode` 20s / ~9M execs, no crashers (run manually, not in CI)
- external deps: none

## History
- **Phase 1 (2026-11-08):** module scaffold; `bencode` (decode, encode,
  DecodePrefix, DictSpans, typed getters, fuzz test); `metainfo` (parse +
  validate, raw-bytes info hash, normalised file list with offsets, tracker
  tiers, path safety); `format` (sizes/speeds/percent/durations);
  `denco inspect` dev command. Decisions D1–D11.
