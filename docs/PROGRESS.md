# Progress

**Current phase:** 10b — PEX · not started (Phase 10 committed as dc97f4f)

**Next action:** start **Phase 10b — PEX** (D37): `ut_pex` (BEP 11) in
`torrent/extensions.go` — advertise `"ut_pex": 2` (new `peer.ExtPEXID`),
send `added`/`added.f`/`dropped` (+ `added6`/`dropped6`) about once a minute
to peers that support it (≤ 50 added per message), feed received peers into
the swarm (`swarm.add`), never for private torrents, only when
`enable_pex` is on (plumb the config flag: daemon → engine.Config →
torrent.Config). Rate-limit what we accept from one peer. Then drop the
"not available" note for PEX.

## Known issues
- A magnet link with no trackers and no `x.pe` peers can't find anyone until
  DHT (Phase 11); it waits in "Fetching metadata" and says why.
- Fetching metadata uses its own connections; the download then reconnects
  to the same peers (D38) — one extra handshake per peer, once per magnet.
- Low download/upload limits shared by many peers can trip the 60s snub
  timeout and cause peer churn (D36). Phase 12 candidate.
- No NAT traversal (UPnP/NAT-PMP): incoming peers need a forwarded port.
  Planned for Phase 12.
- DHT and PEX are config keys only so far (Phases 11 and 10b).
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

## Last test run (2026-11-08, small CLI fixes after Phase 10)
- `go test -race -count=1 ./...` — all 18 packages pass; vet, gofmt and
  linux/windows cross-compile clean
- new `TestRenderStatusCountsEveryState` fails without its fix (checked)

## Previous test run (2026-11-08, end of Phase 10)
- first full run: **`metadata` TestFetchFromPeers hung in 3 of 31 runs** —
  a real design bug (mixed-peer assembly banned honest peers alongside a
  liar). Fetcher redesigned to per-peer assembly (D38); regression assert
  added (honest peer never banned)
- after the fix: `go test -race -count=1 ./...` — all 18 packages pass
- `-race -count=10 -cpu=1,2,8` on `metadata`, `engine`, `torrent` (30 runs
  each) and `-count=5 -cpu=1,2,8` on `peer`, `ipc`, `cli` — pass
- mutation check: removing the fetcher→session peer handoff makes the
  x.pe-only magnet test stall (that bug was found by the test first)
- smoke test, two real daemons with separate homes: B added a magnet with
  only the hash and A's address, got the metadata in ~17 ms, downloaded
  3 files byte-identical to A's (re-run on the final build)
- real config dir (`~/Library/Application Support/denco`) untouched
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: metadata 89%, engine 88%, cli 82%, torrent 91%, peer 94%,
  others 85–100%
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
  metadata; honest peers banned on bad metadata. D38–D40.

