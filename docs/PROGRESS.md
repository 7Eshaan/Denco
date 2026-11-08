# Progress

**Current phase:** 8 — Daemon, IPC, CLI · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 8, tick it in
`docs/ROADMAP.md` and start **Phase 9 — Configuration**: `internal/config`
(flat `key: value` file at `$DENCO_HOME/config`, D4; defaults when missing;
keys from the spec: `download_dir` (with `~` expansion), `max_active_downloads`,
`max_connections_per_torrent`, `download_limit`, `upload_limit`, `enable_dht`,
`enable_pex` (accepted, reported as not supported yet), `listen_port`,
`resume_enabled`/resume location), flags override the file
(`denco daemon -dir/-port/-max-active` already exist), `denco config
get/set/list`, and global up/down **rate limiting** (token bucket shared by
all sessions) plus per-torrent connection limit wiring (`torrent.Config.MaxPeers`).

## Known issues
- No rate limiting yet (Phase 9).
- No NAT traversal (UPnP/NAT-PMP): incoming peers need a forwarded port.
  Planned for Phase 12 (agreed with the user).
- Resume: a file edited externally *during* a run that then crashed isn't
  detected (D27); `denco verify` checks manually.
- Daemon runs in the foreground and logs to its terminal; no log file or
  auto-start yet (D30). Could revisit in Phase 12 (launchd/systemd notes).
- Removing the highest-numbered torrent lets the next add reuse its ID (D31).
- Linux/Windows: compiled + vetted, tests only *run* on macOS.

## Open questions for the user
- Daemon start: went with explicit `denco daemon` (my recommendation; not
  explicitly confirmed). Auto-start from `denco add` is possible later.
- `enable_pex`: accept-but-unsupported (my default) vs. adding PEX as a
  small phase after magnets — needed by Phase 9.
- UPnP/NAT-PMP in Phase 12, MSE encryption out of scope — OK?

## Last test run (2026-11-08, end of Phase 8)
- after review fixes (second Ctrl-C force-quits; `remove -delete` reports
  what it really deleted; restart test checks saved IDs via the API, with an
  ID gap — mutation "ignore saved IDs" now fails it): everything below re-run
- `go test -race -count=1 ./...` — all 15 packages pass
- `-race -count=5 -cpu=1,2,8` on `torrent`, `peer`, `engine`, `ipc`, `cli` — pass
- mutation checks on engine tests: 6 of 7 caught (queue limit, seeding not
  counted, fresh-file guard ×2, queued pause persisted, legacy ID). The
  survivor (dropping `Saver.SetPaused`) is behaviour-neutral: `finish()`
  re-persists the paused flag anyway.
- new regression test `TestStartedCompleteAnnouncesOnce` failed before the
  D34 fix, passes after
- manual smoke test of the built binary with a temp `DENCO_HOME`: daemon,
  add (incl. `-existing`), list, info, files, peers, pause, stop, restart
  (paused state and IDs restored)
- double Ctrl-C on the real binary, with a tracker that stalls "stopped":
  one SIGINT → clean exit after 5.0s; two → killed after 0.5s. (Caveat: a
  daemon started with `&` from a script inherits "ignore SIGINT", so there
  Ctrl-C does nothing at all; use `denco stop` or SIGTERM.)
- real config dir (`~/Library/Application Support/denco`) confirmed untouched
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: engine 87%, ipc 86%, cli 80%, resume 88%, torrent 92%, others 90–100%
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
- **Phase 8 (2026-11-08):** `engine` (torrent table with persistent IDs and
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

