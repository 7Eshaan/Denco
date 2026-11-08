# Progress

**Current phase:** 7 — Resume · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 7, tick it in
`docs/ROADMAP.md` and start **Phase 8 — Daemon, IPC, CLI**:
`internal/engine` (owns the Listener, one Session + Saver per torrent,
restores everything from `resume.Store.List()` on start, duplicate detection
by info hash, add/pause/resume/remove(+delete files), max active downloads
queue), `internal/ipc` (JSON over HTTP on 127.0.0.1 with a 0600 token file,
D3), `internal/cli` (`daemon`, `add`, `list`, `info`, `pause`, `resume`,
`remove`, `status`, `peers`, `files`, `stop`; tables like the spec's examples).
Keep `download`/`inspect`/`verify`/`probe` as standalone commands.

## Known issues
- No rate limiting yet (Phase 9).
- No NAT traversal (UPnP/NAT-PMP): incoming peers need a forwarded port.
  Candidate for Phase 12.
- Resume: a file edited externally *during* a run that then crashed isn't
  detected (D27); `denco verify` checks manually.
- Linux/Windows: compiled + vetted, tests only *run* on macOS.

## Open questions for the user
- none

## Last test run (2026-11-08, end of Phase 7)
- `go test -race -count=1 ./...` — all 13 packages pass
- `internal/resume`, `cmd/denco`: `-race -count=20 -cpu=1,2,8` pass
- `internal/torrent`, `internal/peer`: `-race -count=5 -cpu=1,2,8` pass
- mutation checks: ignoring the clean flag fails the external-modification
  test; always comparing mtimes fails the crash test; ignoring restored
  pieces fails the interrupt-and-resume CLI test
- real config dir (`~/Library/Application Support/denco`) confirmed untouched by tests
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: resume 88%, torrent 92%, cmd/denco 80%, others 90–100%
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
- **Phase 6 (2026-11-08, commit 011612f):** `torrent.Listener`; uploading (request
  validation, cancel, per-peer uploader goroutine, upload stats); tit-for-tat
  choker with optimistic unchoke (pure `chooseUnchoked`); seeding after
  completion (drop seed↔seed, keep dialing), `NoSeed`, `SetUploadsPaused`;
  bans by IP; `denco download -seed`, listener with port fallback. Found and
  fixed: an edit script silently applied 1 of 8 changes (Python tuple
  expression) — caught by the seed-drop test; brittle cancel test rewritten
  and mutation-checked. D26.
- **Phase 7 (2026-11-08):** `resume` package (atomic Store, Restore with
  clean/crash trust rules + partial recheck, Saver); `MetaInfo.Raw`;
  `Session.Have()`; `denco download` auto-resumes (saved dir, `-state`,
  `-save-every`), saves on start/periodically/completion/clean exit. Incident:
  an old CLI test wrote into the real resume dir — artifacts removed, tests now
  isolated (`-state` + `TestMain` HOME redirect), rule added to CLAUDE.md.
  Mutation checks on the clean/crash rules and the resume CLI test. D27–D28.

