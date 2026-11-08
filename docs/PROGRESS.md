# Progress

**Current phase:** 11 — DHT · ✅ done, waiting for the user's commit

**Next action:** once the user has committed Phase 11, tick it in
`docs/ROADMAP.md`, then plan **Phase 12 — Daily-driver polish** with the
user before coding: file selection (skip files), stop-seeding / pause-
uploads controls, ratio display, UPnP/NAT-PMP, snub timeout vs low limits
(D36), real-world interoperability testing (incl. the public DHT — so far
the DHT is only tested against Denco nodes on loopback), README/usage docs.

## Known issues
- Fetching metadata uses its own connections; the download then reconnects
  to the same peers (D38) — one extra handshake per peer, once per magnet.
- Low download/upload limits shared by many peers can trip the 60s snub
  timeout and cause peer churn (D36). Phase 12 candidate.
- No NAT traversal (UPnP/NAT-PMP): incoming peers need a forwarded port.
  Planned for Phase 12.
- DHT: tested only against Denco nodes on loopback, not yet the public
  network (no test may use it; a manual check is a Phase 12 item). IPv4
  only (no BEP 32); no BEP 42 secure node IDs.
- Behind NAT without a forwarded port, other DHT nodes can't reach us, so
  we answer few queries and the table fills slowly (UPnP: Phase 12).
- `denco download` (standalone tool) doesn't use extensions or DHT: no
  PEX, metadata serving or DHT there; the daemon does all three.
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

## Last test run (2026-11-08, end of Phase 11)
- `go test -race -count=1 ./...` — all 20 packages pass (new: `dht`, `compact`)
- `-race -count=10 -cpu=1,2,8` on `dht`, `torrent`, `metadata`, `engine`
  (30 runs each) and `-count=5 -cpu=1,2,8` on `peer`, `cli`, `ipc`,
  `config`, `compact`, `tracker` — pass
- **found by the stress run:** `TestMagnetThroughDHT` failed 3 of 30 — the
  test assumed the session's first DHT lookup returns before the download
  finishes, but the download can finish on the peers the metadata fetcher
  handed over. Test bug, not a product bug: it now waits for the lookup;
  then 60/60 and the full stress run above pass
- mutation checks — each makes a test fail: tokens always valid, responses
  accepted from any address, querying nodes added unverified, no query cap,
  lookups ignoring returned nodes, no announce, state not saved, routers
  unreachable; session: no quick retry, DHT for private torrents, PORT
  ignored, no announce port; engine: sessions/fetcher without DHT, DHT not
  closed (state not saved)
- smoke test, three real daemons, loopback only (A `dht_bootstrap: none`,
  B and C bootstrap from A): C added a magnet with only the info hash, found
  A through B's stored announce, had the metadata and all data in ~5 s,
  byte-identical to A's; every node saved `dht.json`; all stopped cleanly
- real config dir (`~/Library/Application Support/denco`) untouched; no
  test contacts the public DHT (`config.DefaultDHTBootstrap` cleared in the
  CLI's TestMain; other packages use loopback nodes or fakes)
- `go vet` (darwin, linux, windows) clean · gofmt clean
- cross-compile linux/windows/darwin × amd64/arm64 — all build
- coverage: dht 91%, compact 100%, torrent 92%, metadata 91%, engine 87%,
  config 92%, cli 82%
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
  new `dht_bootstrap`; DHT rows in `denco status`/`info`. D42–D43.
