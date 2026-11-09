# Progress

**Current phase:** 12c — Robustness · ✅ done, waiting for the user's commit

**Next action:** once the user has committed 12c, tick it in
`docs/ROADMAP.md`, then ask the user whether **12d — NAT traversal**
(UPnP / NAT-PMP, stdlib only) is in scope; if not, go to **12e — Docs &
real-world testing**.

## Known issues
- A finished torrent (seed limit reached) stays finished when limits are
  raised; the user resumes it (D45). Per-torrent choices made while a
  torrent runs reach disk at its next periodic save (≤ 30 s) or when it stops.
- Skipped files can appear on disk holding only the bytes of a *border
  piece* shared with a wanted file (D44, Transmission-style). `add` still
  refuses if any target file exists, skipped ones included.
- The engine refreshes the file selection when a session starts (a change
  made while the torrent is being checked); only that tiny race window has
  no test that fails without the code (D44).
- Fetching metadata uses its own connections; the download then reconnects
  to the same peers (D38) — one extra handshake per peer, once per magnet.
- Upload side of D36 not changed: at the 1 KB/s minimum upload limit,
  remote clients might time out on us (D46). Download side fixed.
- Failed torrents' retry state isn't saved; after a daemon restart they
  simply start again (D47).
- No NAT traversal (UPnP/NAT-PMP): incoming peers need a forwarded port.
  12d, if the user confirms it is in scope.
- DHT: IPv4 only (no BEP 32); no BEP 42 secure node IDs. Behind NAT without
  a forwarded port the routing table fills slowly. Two DHT nodes alone
  (no tracker, no third node) take minutes to find each other.
- `denco download` (standalone) doesn't use extensions or DHT, and has no
  file-selection flags (it honours a selection saved by the daemon).
- Resume: a file edited externally *during* a run that then crashed isn't
  detected (D27); `denco verify` checks manually.
- Resume JSON writes `"completed_at": "0001-01-01T00:00:00Z"` for
  incomplete torrents (`omitempty` doesn't apply to time.Time). Harmless.
- Daemon runs in the foreground (no auto-start yet, D30); `log_file` /
  `-log` sends its log to a file (D48). Started with `&` from a script it
  ignores Ctrl-C.
- Config changes other than speed limits need a daemon restart.
- Removing the highest-numbered torrent lets the next add reuse its ID (D31).
- Linux/Windows: compiled + vetted, tests only *run* on macOS.

## Open questions for the user
- Daemon start: explicit `denco daemon` (my recommendation; not explicitly
  confirmed). Auto-start from `denco add` is possible later.
- UPnP/NAT-PMP (12d) in scope? MSE encryption stays out of scope?

## Last test run (2026-11-09, end of Phase 12c)
- `go test -race -count=1 ./...` — all 20 packages pass
- stress `-race -cpu=1,2,8`: `engine`, `torrent` ×10 (30 runs each);
  `peer`, `ratelimit`, `cli`, `config`, `pieces`, `metadata` ×5 (15 each)
  — all pass, no failures
- **D36 reproduced before fixing** (`TestLowDownloadLimitDoesNotSnubPeers`,
  8 peers on one 64 KB/s limit): peers dropped and redialed, 257 blocks sent
  for a 24-block torrent, 11/24 pieces after 30 s. Each fix was then shown
  necessary in turn: paced timeout alone → no snub drops at 500 ms but still
  churn; plus fair limiter → still ~10× duplicates; plus endgame off → done
  in ~6 s, every peer connected once
- **found while fixing:** the limiter starved readers (new
  `TestFairAcrossGoroutines`: old code 10/10 fail, new 60/60 pass with
  `-race -cpu=1,2,8`); `peer.Conn` dropped a throttled peer mid-message as
  idle (`TestThrottledReadIsNotIdle`, failed before the fix)
- new tests also: `TestPacer`; `TestRetryDelay`,
  `TestFailedTorrentRetriesByItself`, `TestPermanentErrorIsNotRetried`;
  `TestLogFileRotates`, `TestDaemonLogsToFile`, `TestRetryText`,
  `TestLogFileSetting`
- `Pause` now clears a failed torrent's shown error (found by the retry
  test: a paused torrent still showed "Error")
- mutation checks — 12 of 13 make a test fail: snub timeout / pipeline not
  scaled; endgame under a limit; peer count never reported; stopped session
  leaking its pacer share (no test covered it at first — added); no idle
  deadline; permanent errors retried; no retry; user resume not resetting
  failures; no backoff; log never rotated; log file ignored. **Survived
  (equivalent):** `pause` not cancelling the retry timer — the timer does
  nothing once `pause` has cleared the error
- smoke test, two real daemons + loopback tracker, scratch homes: B with
  `-log FILE -down-limit 100K` and its download folder blocked by a file →
  "retrying by itself in 1m 0s (attempt 2)"; block removed → retried
  automatically at the minute, 1 MB done in ~9 s (≈ the limit), 3 files
  byte-identical; log only in the file. Real config dir untouched
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
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
- **Phase 9 (2026-11-08, commit ef2f4f0):** `config` (strict flat file, defaults, in-place
  set/unset/init, rate syntax); `ratelimit` (shared token bucket, live
  SetRate); download limit in `peer.Conn` reader, upload limit in the
  session uploader; engine `SetLimits`, max peers per torrent; IPC
  `POST /v1/limits`; `denco config`; daemon reads the file, flags override
  (`-dir -port -max-active -max-peers -down-limit -up-limit`),
  `resume_enabled: false`, notes for DHT/PEX. PEX scheduled as Phase 10b
  (D37). D35–D37.
- **Phase 10 (2026-11-08):** BEP 10 `peer.ExtHandshake`; `metadata`
  package (BEP 9 messages, Assembler, Fetcher with per-peer assembly);
  sessions advertise ut_metadata and serve their info dict; engine
  `AddMagnet` + fetch-before-first-run, pending magnets persisted without a
  .torrent (`State.Magnet`) and restored, fetcher's peers handed to the
  session; IPC `magnet` in add, `no_metadata` error; `denco add` takes
  magnet links and HTTP(S) URLs (incl. redirects to magnets); "Fetching
  metadata" in list/info. Found and fixed: x.pe-only magnets stalled after
  metadata; honest peers banned on bad metadata. D38–D40. Commit dc97f4f;
  small CLI fixes in 44cd584.
- **Phase 10b (2026-11-08):** BEP 11 `peer.PEX` codec; sessions advertise
  `ut_pex` (not for private torrents), send added/dropped per peer (first
  right after its extension handshake, then ≤ 1/min, ≤ 50 each), accept
  ≤ 1 message per half interval and ≤ 50 addresses from a peer, PEX-grown
  swarm capped at 2000; metadata fetcher receives PEX (keeps PEX-only
  peers); `enable_pex` plumbed daemon → engine → session/fetcher;
  "Peer exchange" row in `denco status`; `enable_pex` defaults to true
  (user's choice). D41.
- **Phase 11 (2026-11-08):** `dht` package (KRPC, routing table with
  verify-before-add, tokens, peer store, iterative lookups, announce,
  maintenance, `dht.json`); `compact` package shared with tracker and PEX;
  sessions look up + announce (retry backoff, never for private torrents)
  and exchange PORT messages; the magnet fetcher looks up via DHT;
  engine runs one node on the peer port's number; `enable_dht` works,
  new `dht_bootstrap`; DHT rows in `denco status`/`info`; targeted bucket
  lookups fill the table (10 → 112 nodes on the public DHT). D42–D43.
- **Phase 11 commits:** a259f12 (DHT), da5999f (routing-table growth fix).
- **Phase 12a (2026-11-09):** file selection (D44): `pieces.Wanted`,
  Manager wanted mask (`Complete` vs `HaveAll`, `MissingBytes`),
  `Session.SetWanted` + partial seeds (trackers: real `left`, `completed`
  only when full), `resume.State.Skip` + `Saver.SetSkip`,
  `storage.Options.Skipped`, `engine.SetFilesWanted` (live, or restart
  when re-wanting a finished torrent), `AddOptions.Skip`, IPC
  `POST /v1/torrents/{ref}/files`, CLI `add -skip/-only`,
  `files <id> skip|want`, 1-based file numbers in `files`/`inspect`/`add`,
  selected size in `list`/`info`. Also: misplaced comment in
  `cli/download.go` fixed.
- **Phase 12a commit:** d399435.
- **Phase 12b (2026-11-09):** seeding controls (D45): ratio (uploaded ÷
  max(downloaded, have)), per-torrent seeding modes `on/off/default`
  (`denco seed`), global `seed_ratio`/`seed_time` (config, daemon flags,
  live via `POST /v1/seed-limits`), "finished" state (saved, never
  restarted on its own; `resume` refuses while a limit applies), lifetime
  seeding time (`seed_seconds`, paused uploads excluded), per-torrent
  pause uploads (`pause/resume -uploads`), RATIO in `list`, seeding rows
  in `info`/`status`. New IPC routes `/uploads`, `/seed`, `/v1/seed-limits`.

- **Phase 12b commit:** f2be1d2.
- **Phase 12c (2026-11-09):** pacing under a download limit (D46:
  `torrent.Pacer`, scaled snub timeout and pipeline, endgame off, fair
  `ratelimit` reservations, per-read idle deadline in `peer.Conn`);
  automatic retry of failed torrents with backoff (D47, `engine/retry.go`;
  `pause` clears the error); daemon log file with rotation (D48,
  `log_file`, `-log`).
