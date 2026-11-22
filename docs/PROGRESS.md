# Progress

**Current phase:** Phase 15 — qBittorrent parity, part 1 (user-selected
2026-11-21: queue control, per-torrent limits, full IPv6, web seeds,
MSE/PE, uTP; sub-phases 15a–15f in ROADMAP).

**2026-11-22:** CI failure fixed (D62): macOS CI failed
`TestLookupAsksRoutersForUnknownRegions` on the 15d commit. Two DHT tests
(both from Phase 11) expected node counts that random IDs sometimes make
impossible: ~1.1% and ~0.4% flakes, reproduced and explained by logging.
Test-only fix, verified with thousands of runs incl. under CPU load and
the race detector. Not committed yet. Found along the way: the
shallow-bucket test barely detects the rule it's named for (D62).

**Phase 15d — web seeds (D61) · ✅ done, committed as d0bd898.**
New `internal/webseed` (BEP 19 URLs, ranged GETs per file with exact
Content-Range checks, Retry-After, no-ranges detection, local/private
addresses refused at connect time, no proxies, timeouts). Sessions run
each url-list entry as a pseudo-peer fed by the piece Manager; data goes
through the normal hash/write path, paced by the download limits; errors
back off, bad data or hard errors give a web seed up for the run. Magnet
`ws=` kept in the .torrent made from metadata. Web seeds listed in
`denco peers` / TUI peers tab, counted in `info` / TUI details.

**Phase 15c — full IPv6 (D60) · ✅ done, committed as d97cff6.**
IPv6 DHT node next to the IPv4 one (BEP 32: `dht.Config.IPv6`, `nodes6`,
18-byte values, own table and `dht6.json`; `dht.Dual` merges lookups);
trackers announced to over IPv4 and IPv6 separately and merged (BEP 7,
`tracker.Client.DualStack`, 3 s grace for a slow family); bans per IPv6
/64; `enable_ipv6` (default true; off = IPv4 only, IPv6 addresses dropped
everywhere); `status` shows both DHT tables and an IPv6 row; the TUI's peer
ADDRESS column widens for IPv6 addresses.

**Phase 15b — per-torrent speed limits (D59) · ✅ done, committed as 4c4c8e8.**
Each torrent has its own up/down limiter on top of the global ones (peer
reader waits on both; the uploader too), kept on the engine entry so
changes apply live and survive pause/resume; saved as `down_limit` /
`up_limit`. Pacing uses the tighter of the global and the torrent's
per-peer share. `POST /v1/torrents/{ref}/limits`, `denco limit [-down R]
[-up R] <id>…|all`, "Speed limits" row in `info`; TUI `s` prompt, Limits
line in details, yellow DOWN/UP cells for limited torrents.

**Phase 15a — queue control (D58) · ✅ done, committed as 98d64c2.**
Queue order keys saved per torrent (`queue`), scheduler starts downloads in
queue order, `List` in queue order; `MoveInQueue` top/up/down/bottom
(steps over complete torrents, never stops a running download); force start
(`forced`, past `max_active_downloads`, ended by resume/pause). API routes
`/queue` and `/force`; `denco queue …`, `denco resume -force`; `Q` column
in `list` and the TUI, Queue row in `info` and TUI details; TUI keys
`K`/`J`/`T`/`B`/`f`; help overlay compacts on short terminals. README
screenshots regenerated (`go run ./scripts/tuishots`, demo got queue
positions).

**Phase 14** — terminal dashboard `denco tui` (D55) · ✅
done, committed as 8de87c2 (D54 version strings: 0308bc0), pushed, and
released as **v0.2.0** (tag → 8de87c2; GitHub release published
2026-11-17 with six archives + `SHA256SUMS`). New `internal/tui` (live table, details with
files/peers, add/pause/resume/remove, help; raw input via termios on
macOS/Linux and console modes on Windows, line-mode fallback; reconnects
after a daemon restart, never auto-starts on reconnect) and
`internal/cli/tui.go`. README has the command and keys.

**2026-11-18:** CI failure fixed (D57): the Go 1.23 job failed
`TestSessionRejectsBadMetadataRequests` — a session hung up on peers that
connected after it was registered with the listener but before `Run`
started (a real, harmless race in the daemon too). Now queued until Run.
Deterministic regression test; mutation-checked both ways. Committed as
a0ae613 and pushed — *after* the v0.2.0 tag, so no release contains it yet.

**2026-11-18:** README showcase (D56): color SVG screenshots of the
dashboard and the details view (`assets/`), drawn from the real TUI by
`go run ./scripts/tuishots`. Committed as 41180df (docs/.gitignore
tidy-up: fc60ec0), pushed.

**2026-11-21:** PROGRESS.md brought up to date (it still said D56/D57
were uncommitted and that v0.2.0 was the next step). Checked: `main` =
`origin/main` = a0ae613; releases v0.1.0, v0.1.1 and v0.2.0 all exist on
GitHub with 7 assets each.

**Phase 13** — CI and releases (D53) · ✅ done.
CI is green on Linux, macOS and Windows (the first run found 3
Windows-only test bugs, fixed). Release **v0.1.0** published 2026-11-11:
six archives + `SHA256SUMS`; the darwin/arm64 download was checked —
checksum OK, runs, reports `v0.1.0`.

**2026-11-17:** CI on macOS failed once in `TestDialAcceptExchange`
("byte counters not updated"): a test-only race — a counter is bumped just
after its socket write returns, so the peer can have the data first. The
test now waits up to 2 s; confirmed by forcing the delay (old test fails,
new passes). Committed as 2811412. Note: tag `v0.1.1` points at 3e596a0,
the commit *before* that fix.

**2026-11-17 (later):** version strings follow the release (D54): new
`internal/version` package; peer ID `-DN0100-` for v0.1.0 (was the fixed
`-DN0001-`), BEP 10 `v` "Denco 0.1.0" and User-Agent "Denco/0.1.0" (were
"0.0.1"). Release builds now stamp `internal/version.Tag` (was
`cli.Version`). Committed as 0308bc0.

**Next action:** the user commits the DHT test fix (D62) and checks CI is
green; then **15e — protocol encryption
(MSE/PE)** (plan: `internal/mse` — 768-bit DH, the VC scan with padding,
crypto_provide/select, RC4 with 1 KiB discarded, hashed info-hash lookup
for incoming; `encryption: prefer|require|disable` (default prefer:
encrypted first, plaintext fallback remembered per peer); PEX encryption
flag; VIA column with an E flag. Needs a short user-approved real-world
check against public peers).
Optional, any time: a release (v0.2.1 for the D57 fix, or v0.3.0 once
some of Phase 15 is in). Open items, if wanted: the plain
CLI should `clean` torrent names too (D55), the user's router check
(D50), one memory budget for all torrents (D51), `denco service install`
(D52), signing/Homebrew/Scoop (D53), and the out-of-scope list (uTP, MSE,
web seeds, v2, GUI).

## Known issues
- TUI on Windows (D55): raw mode, VT output and size polling are compiled
  and vetted, and the non-console path is tested in CI, but nothing has
  run in a real Windows console yet.
- CLI commands (`list`, `info`, `files`, `add`) print torrent names and
  paths as they are; a name with escape sequences could drive the terminal.
  The TUI filters them (`clean`); the CLI doesn't yet.
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
- Memory: partly downloaded pieces are capped at 128 MiB per active
  torrent; since D51 a 256 KiB-piece torrent uses a few MB of them
  (daemon ~100 MB RSS at ~4.8 MB/s), but big-piece torrents (4 MiB+) can
  still reach the cap, and each active torrent has its own.
- Upload side of D36 not changed: at the 1 KB/s minimum upload limit,
  remote clients might time out on us (D46). Download side fixed.
- Failed torrents' retry state isn't saved; after a daemon restart they
  simply start again (D47).
- Port forwarding on the user's real router: UPnP found (no NAT-PMP). After
  the D50 fixes the `EOF` is gone, but the router answers 501 to every
  AddPortMapping, permanent leases included (seen 2026-11-09 during the
  user's Linux Mint download) — UPnP mapping is most likely disabled in
  the router's settings; Denco reports it correctly. Earlier notes:
- Port forwarding (D49) is tested only against fake routers on loopback.
  A read-only probe of the user's router from the dev environment found the
  gateway (192.168.1.1, both via `route` and by the Windows-path guess);
  NAT-PMP got no answer (the router most likely doesn't speak it) and the
  UPnP multicast search could not be sent there ("no route to host" — the
  same from Python, so a macOS Local Network permission / sandbox limit of
  that environment, not Denco). A real mapping hasn't been made yet.
- No IPv6 firewall pinholes (D60); the BEP 5 PORT message carries the
  local DHT port even if the router forwards another one (D49).
- DHT: IPv4 and IPv6 nodes (D60); no BEP 42 secure node IDs. Behind NAT without
  a forwarded port the routing table fills slowly. Two DHT nodes alone
  (no tracker, no third node) take minutes to find each other.
- `denco download` (standalone) doesn't use extensions or DHT, and has no
  file-selection flags (it honours a selection saved by the daemon).
- Resume: a file edited externally *during* a run that then crashed isn't
  detected (D27); `denco verify` checks manually.
- Resume JSON writes `"completed_at": "0001-01-01T00:00:00Z"` for
  incomplete torrents (`omitempty` doesn't apply to time.Time). Harmless.
- Commands start the daemon in the background (D52); nothing starts it at
  login yet (README shows launchd/systemd/Task Scheduler). Windows
  detaching is compiled and vetted, not run.
- Config changes other than speed limits need a daemon restart.
- Removing the highest-numbered torrent lets the next add reuse its ID (D31).
- Windows daemon detaching (D52) is compiled, vetted and covered by CI's
  Windows tests, but CI never spawns a real background process (tests run
  the daemon in-process), so it hasn't actually been run on Windows.

## Open questions for the user
- MSE encryption stays out of scope? (UPnP/NAT-PMP: confirmed, done in 12d.)

## Last test run (2026-11-22, Phase 15d, D61)
- `go test -race -count=1 ./...` — all 25 packages pass (new: `webseed`)
- stress `-race -count=5 -cpu=1,2,8` webseed, magnet, engine, cli, tui,
  torrent, peer (15 runs each) — all pass
- `go vet` (darwin, linux, windows) clean · gofmt clean · cross-compile
  linux/windows/darwin × amd64/arm64 — all build · external deps: none
- new tests: webseed `TestURLFor`, `TestFetchAcrossFiles`,
  `TestFetchRefusesLocalAddresses`, `TestFetchErrors`; torrent
  `TestDownloadFromWebSeed`, `TestBadWebSeedIsGivenUp`,
  `TestWebSeedRetriesWhenBusy`, `TestWebSeedHonoursDownloadLimit`,
  `TestLocalWebSeedRefusedByDefault`; engine `TestWebSeedThroughEngine`;
  magnet `TestWebSeedsInMagnet`; cli `TestRenderWebSeeds`; tui
  `TestWebSeedInPeersTab`. Mutation-checked: 8 mutations, each fails a
  test (the Content-Range check survived the first fixture; fixed)
- found by a test: the TUI details' Peers line was too long with web seeds
  added to it; they got their own "Web seeds" line
- not checked against a real public web seed (would need a public
  download; none was made)

## Previous test run (2026-11-22, Phase 15c, D60)
- `go test -race -count=1 ./...` — all 24 packages pass, the IPv6 tests
  really ran (this machine has ::1 and localhost → 127.0.0.1 + ::1)
- stress `-race -count=5 -cpu=1,2,8` dht, tracker, metadata, engine,
  cli, tui, torrent, peer (15 runs each) — all pass
- `go vet` (darwin, linux, windows) clean · gofmt clean · cross-compile
  linux/windows/darwin × amd64/arm64 — all build · external deps: none
- new tests: dht `TestIPv6AnnounceAndFindPeers`, `TestIPv6NodeSpeaksNodes6`,
  `TestDualSearchesBothFamilies`, nodes6/18-byte values in
  `TestCompactNodes`; tracker `TestDualStackHTTPAnnounce`,
  `TestDualStackToleratesOneFamily`, `TestDualStackUDPAnnounce`; torrent
  `TestDownloadFromIPv6Peer`, `TestBanKeys`; engine
  `TestIPv6DHTNodeAndDualStackTrackers`, `TestMagnetFromIPv6Peer`; cli
  `TestRenderStatusIPv6`; tui `TestIPv6PeerAddressesFit`; config
  `enable_ipv6`. Mutation-checked: 8 mutations (nodes6 key, Dual using one
  family, 18-byte values, DualStack ignored, no grace cancel, /64 bans,
  IPv4-only swarm and fetcher filters) — each fails a test
- found by a test: the ADDRESS column cap (41) cut full-length IPv6
  addresses; raised to their true maximum, 47

## Previous test run (2026-11-21, Phase 15b, D59)
- `go test -race -count=1 ./...` — all 24 packages pass
- stress `-race -count=5 -cpu=1,2,8` engine, cli, tui, ipc, torrent,
  peer (15 runs each) — all pass
- `go vet` (darwin, linux, windows) clean · gofmt clean · cross-compile
  linux/windows/darwin × amd64/arm64 — all build · external deps: none
- new tests: `TestTorrentLimiterStacksWithGlobal` (peer),
  `TestTorrentDownloadLimitDoesNotSnubPeers` + `TestFairShareTakesTheTighterLimit`
  (torrent), `TestTorrentLimits` (real transfer under each direction's
  own limit) + `TestTorrentLimitsAreSavedAndShown` (engine),
  `TestAPITorrentLimits`, `TestLimitCommand` (cli, end to end),
  `TestLimitsPrompt` + `TestLimitsShownAndHelp` (tui). Mutation-checked:
  not attaching the torrent's read limiter, the uploader skipping it,
  pacing ignoring it (the download then stalls — the D46 bug again), not
  saving the limits — each fails a test
- screens checked by eye at 80 columns: limits prompt, details, help

## Previous test run (2026-11-21, Phase 15a, D58)
- `go test -race -count=1 ./...` — all 24 packages pass
- stress `-race -count=5 -cpu=1,2,8` engine, cli, tui, ipc, torrent,
  peer (15 runs each) — all pass
- `go vet` (darwin, linux, windows) clean · gofmt clean · cross-compile
  linux/windows/darwin × amd64/arm64 — all build · external deps: none
- new tests: `engine/queue_test.go` (moves skip complete torrents, next
  slot goes to the queue head, order + force survive restart, forced holds
  no slot, pause/resume end force), legacy state gets a queue key,
  `TestAPIQueue`, `cli/queue_test.go` (end to end), `tui/queue_test.go`
  (keys, Q cells, details, help fits 80×24). Mutation-checked: ID-order
  scheduling, forced counting against the limit, ignoring force — all fail
  tests (the first two survived the first draft of the tests; fixed)
- screens checked by eye: dashboard 120×14, details, help at 80×24

## Earlier test run (2026-11-21, tree at a0ae613, no code changes)
- `go build`, `go vet`, gofmt clean; `go test -race -count=1 ./...` — all
  24 packages with tests pass (incl. `scripts/tuishots`); cross-compile
  linux and windows builds; external deps: none

## Previous test run (2026-11-17, Phase 14 TUI, D55)
- `go test -race -count=1 ./...` — all 23 packages pass (new: `tui`);
  stress `-race -count=5 -cpu=1,2,8` `tui`, `cli` (15 runs each) — pass;
  `torrent`, `peer` stress passed earlier today (untouched since)
- `go vet` (darwin, linux, windows; `tui` also freebsd) clean · gofmt
  clean · cross-compile linux/windows/darwin × amd64/arm64 — all build
- new tests: keys, text widths/`clean`, screen diff, model, rendering
  (9 screens × 13 widths × 8 heights fit exactly), run loop with fake
  terminal + client (actions, no overlapping refreshes while a refresh
  hangs, reconnect, resize, EOF, cancel, line mode), termios flags,
  Windows non-console test (CI only), and `TestTUIAgainstRealDaemon`
  (add file + magnet + failing add, pause, resume, details, remove, quit
  → daemon still has the other torrent). Mutation-checked: name cleaning,
  dropping a dead client. Found by tests: a raw 0x9b byte slipped through
  `clean`; details at 7 rows was one line too tall
- real pty (`script`, scratch DENCO_HOME, DHT/port forwarding off): TUI
  auto-started the daemon once, help/add magnet/details worked, exit 0,
  `stty -g` identical before and after, daemon still running afterwards
- external deps: none (`go list -deps`: standard library only)

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
- **Phase 12d (2026-11-09):** `portmap` (NAT-PMP, UPnP IGD, gateway lookup),
  engine `PortForward` + public port to trackers/DHT/BEP 10, `port_forwarding`
  setting, status row. D49.
- **Phase 12e (2026-11-09):** README (features, install, commands,
  settings, port forwarding, running at login); real-world download test;
  UPnP fixes from the real router (Connection: close, 501 → lease 0,
  one-line errors). D50.
- **After Phase 12 (2026-11-09):** lower memory per torrent — peers finish
  their own pieces, orphaned pieces adopted (D51); swarm simulation test.
- **After Phase 12 (2026-11-09):** daemon auto-start — `denco start`,
  commands start a detached background daemon, `auto_start` (D52).
- **After Phase 13 (2026-11-17):** test fix for byte counters (2811412);
  version strings follow the release via `internal/version` (D54).
- **Phase 14 (2026-11-17, commit 8de87c2):** `denco tui` terminal
  dashboard (D55). Released as v0.2.0 (2026-11-17).
- **After Phase 14 (2026-11-18):** README screenshots from the real TUI
  (D56, 41180df); fix for peers dropped just before `Run` (D57, a0ae613).
- **Phase 15a (2026-11-21, commit 98d64c2):** download queue control and
  force start (D58).
- **Phase 15b (2026-11-21, commit 4c4c8e8):** per-torrent speed limits (D59).
- **Phase 15c (2026-11-22, commit d97cff6):** full IPv6 (D60).
- **Phase 15d (2026-11-22, commit d0bd898):** web seeds (D61).
- **After Phase 15d (2026-11-22):** two flaky DHT tests made
  deterministic (D62, CI failure on macOS).
