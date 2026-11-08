# Progress

**Current phase:** 9 — Configuration · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 9, tick it in
`docs/ROADMAP.md` and start **Phase 10 — Magnets end-to-end**: BEP 10
extension handshake in `peer`/`torrent` (advertise `ut_metadata`, parse the
peer's `m` dict and `metadata_size`), BEP 9 `ut_metadata` (request/data/reject
pieces of 16 KiB, assemble, SHA-1 must equal the info hash, then
`metainfo.ParseInfo`), a "fetching metadata" state in the engine (torrent
known by hash + name only, peers from magnet `tr`/`x.pe`), then switch to a
normal session; `denco add <magnet>` and `denco add <http(s) URL>` (download
the .torrent, size cap, timeout). Design the extension layer so Phase 10b
(PEX, D37) plugs into it.

## Known issues
- Low download/upload limits shared by many peers can trip the 60s snub
  timeout and cause peer churn (D36). Phase 12 candidate.
- No NAT traversal (UPnP/NAT-PMP): incoming peers need a forwarded port.
  Planned for Phase 12.
- DHT and PEX are config keys only (Phases 11 and 10b); the daemon says so.
- Resume: a file edited externally *during* a run that then crashed isn't
  detected (D27); `denco verify` checks manually.
- Daemon runs in the foreground and logs to its terminal; no log file or
  auto-start yet (D30). Started with `&` from a script it ignores Ctrl-C.
- Config changes other than speed limits need a daemon restart.
- Removing the highest-numbered torrent lets the next add reuse its ID (D31).
- Linux/Windows: compiled + vetted, tests only *run* on macOS.

## Open questions for the user
- Daemon start: explicit `denco daemon` (my recommendation; not explicitly
  confirmed). Auto-start from `denco add` is possible later.
- UPnP/NAT-PMP in Phase 12, MSE encryption out of scope — OK?

## Last test run (2026-11-08, end of Phase 9)
- `go test -race -count=1 ./...` — all 17 packages pass
- `-race -count=5 -cpu=1,2,8` on `torrent`, `peer`, `engine`, `ipc`, `cli`,
  `ratelimit` — pass
- speed-limit test mutation-checked: removing the upload limiter, the
  download limiter, or the uploader's wait each makes it fail
- found while writing it: shared fake tracker let earlier unlimited engines
  join the swarm and hide the limit — test fixed (one tracker per measurement)
- manual smoke test of the built binary: `config` list/set/get/invalid
  value, file created with header, daemon picks up the file, `-down-limit`
  flag overrides it, live `config set upload_limit` applied, status shows it
- real config dir (`~/Library/Application Support/denco`) confirmed untouched
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: config 91%, ratelimit 100%, engine 87%, ipc 86%, cli 81%,
  others 88–100%
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
- **Phase 7 (2026-11-08, commit c4c40aa):** `resume` package (atomic Store, Restore with
  clean/crash trust rules + partial recheck, Saver); `MetaInfo.Raw`;
  `Session.Have()`; `denco download` auto-resumes (saved dir, `-state`,
  `-save-every`), saves on start/periodically/completion/clean exit. Incident:
  an old CLI test wrote into the real resume dir — artifacts removed, tests now
  isolated (`-state` + `TestMain` HOME redirect), rule added to CLAUDE.md.
  Mutation checks on the clean/crash rules and the resume CLI test. D27–D28.
- **Phase 8 (2026-11-08, commit 097a8da):** `engine` (torrent table with persistent IDs and
  info-hash dedup, shared listener, download queue, per-torrent run
  goroutine with restore/recheck + Saver, add/pause/resume/remove with safe
  file deletion, views); `ipc` (loopback JSON API, token auth, daemon file,
  client); `cli` package with all commands (`daemon`, `stop`, `add`, `list`,
  `info`, `files`, `peers`, `pause`, `resume`, `remove`, `status` + the
  standalone tools), `$DENCO_HOME`. `resume.State` gained `id/fresh/existing`;
  `Saver.SetPaused/State`. Fixed: double announce for complete-at-start
  torrents (D34); tracker errors no longer echo the announce URL. Review
  fixes: second Ctrl-C force-quits, `remove -delete` reports what it deleted,
  stronger restart test. D29–D34.
- **Phase 9 (2026-11-08):** `config` (strict flat file, defaults, in-place
  set/unset/init, rate syntax); `ratelimit` (shared token bucket, live
  SetRate); download limit in `peer.Conn` reader, upload limit in the
  session uploader; engine `SetLimits`, max peers per torrent; IPC
  `POST /v1/limits`; `denco config`; daemon reads the file, flags override
  (`-dir -port -max-active -max-peers -down-limit -up-limit`),
  `resume_enabled: false`, notes for DHT/PEX. PEX scheduled as Phase 10b
  (D37). D35–D37.

