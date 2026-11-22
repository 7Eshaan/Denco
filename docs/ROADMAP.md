# Roadmap

Stable phase plan. Each phase ends green (`go test -race ./...`, `go vet ./...`,
cross-compile) and with a user commit. Tick a box when its phase is
committed. Lettered phases (5b) were inserted later; numbering is kept stable
so older references stay valid.

- [x] **Phase 1 — Foundations.** Module scaffold, `bencode` (decode/encode,
      raw spans), `metainfo` (.torrent parse + validate, info hash, file layout,
      path safety), `denco inspect <file>` dev command.
- [x] **Phase 2 — Discovery.** `magnet` URI parser (xt/dn/tr, hex + base32),
      HTTP/HTTPS `tracker` client (announce, compact + dict peers, failure
      reasons, interval), tracker tests against `httptest`.
- [x] **Phase 3 — Peer wire protocol.** Handshake, message framing/codec,
      bitfield type, per-connection read/write loops, keep-alives, timeouts.
- [x] **Phase 4 — Storage & pieces.** Multi-file piece↔file mapping, safe path
      resolution (incl. Windows name sanitising), no-overwrite policy, piece
      hashing/verification, rarest-first picker, block requests.
- [x] **Phase 5 — Single-torrent engine.** Torrent session: tracker loop, peer
      pool, concurrent block downloads from many peers, endgame, reject bad
      pieces, reconnects. End-to-end test with an in-process tracker + seeder.
- [x] **Phase 5b — UDP trackers.** BEP 15 client (connect → announce, connection
      ID caching, retransmit with 15·2ⁿ s backoff, IPv4/IPv6), plugged into
      `tracker.List` behind the existing `Announcer` interface so `udp://`
      URLs stop being skipped. Tests against an in-process UDP server.
- [x] **Phase 6 — Uploading & seeding.** Serve requests, choking algorithm,
      incoming connections listener, seed after completion, upload stats.
- [x] **Phase 7 — Resume.** Persist per-torrent state (bitfield, options,
      stats), periodic + on-exit saves, crash-safe atomic writes, recheck on
      start when files changed.
- [x] **Phase 8 — Daemon, IPC, CLI.** Multi-torrent engine, daemon process,
      JSON-over-HTTP IPC on localhost, `add/list/info/pause/resume/remove/
      status/peers/files/stop`, progress tables, duplicate detection.
- [x] **Phase 9 — Configuration.** Config file (flat `key: value`), defaults,
      flag overrides, `denco config get/set`, rate limiting (up/down),
      connection limits, max active downloads queue.
- [x] **Phase 10 — Magnets end-to-end.** Extension protocol (BEP 10),
      `ut_metadata` (BEP 9), metadata validation against info hash, `.torrent`
      from HTTP(S) URL.
- [x] **Phase 10b — PEX.** Peer exchange (BEP 11, `ut_pex` over the BEP 10
      extension protocol from Phase 10): send/receive added/dropped peers,
      never for private torrents, behind `enable_pex`.
- [x] **Phase 11 — DHT.** Basic BEP 5 DHT (KRPC over UDP, routing table,
      get_peers / announce_peer, bootstrap, persisted node cache).
- [x] **Phase 12 — Daily-driver polish**, split into lettered sub-phases
      (user-approved 2026-11-09), each ending green with its own commit:
  - [x] **12a — File selection.** Skip / re-want files (`add -skip/-only`,
        `denco files <id> skip|want`), only wanted pieces downloaded,
        partial-seed completion, persisted in resume data.
  - [x] **12b — Seeding controls.** Per-torrent pause uploads / stop
        seeding, ratio display, optional `seed_ratio` / `seed_time` limits.
  - [x] **12c — Robustness.** Snub timeout scaled to speed limits (D36),
        automatic retry of failed torrents with backoff, optional daemon
        log file.
  - [x] **12d — NAT traversal.** UPnP / NAT-PMP port mapping (stdlib only),
        if the user confirms it is in scope.
  - [x] **12e — Docs & real-world testing.** README/usage docs; a full
        real download (user-approved, public network).
- [x] **Phase 13 — CI and releases.** GitHub Actions: tests on Linux,
      macOS and Windows; prebuilt binaries for a pushed version tag;
      `denco version` (D53). First release v0.1.0 published 2026-11-11.

- [x] **Phase 14 — Terminal dashboard.** `denco tui` (user-requested
      2026-11-17): live torrent table, details with files and peers,
      add/pause/resume/remove from the keyboard, over the existing daemon
      API; raw terminal input with the standard library on macOS, Linux
      and Windows, line-mode fallback elsewhere (D55).

- [ ] **Phase 15 — qBittorrent parity, part 1** (user-selected
      2026-11-21 from the qBittorrent comparison), lettered sub-phases,
      each ending green with its own commit:
  - [x] **15a — Queue control.** Queue order (top/up/down/bottom), force
        start past `max_active_downloads`, `Q` column, TUI keys (D58).
  - [x] **15b — Per-torrent speed limits.** Own up/down limiters under the
        global ones; pacing uses the tighter fair share.
  - [x] **15c — Full IPv6.** IPv6 DHT (BEP 32), announcing over both
        families (BEP 7), /64 bans, wider address columns.
  - [x] **15d — Web seeds (BEP 19).** HTTP range downloads as pseudo-peers;
        private/loopback URLs refused by default.
  - [x] **15e — Protocol encryption (MSE/PE).** `encryption:
        prefer|require|disable` (D63); checked on a real swarm: encrypted
        handshakes with qBittorrent, Transmission, rtorrent and Deluge.
  - [x] **15f — uTP (BEP 29).** LEDBAT over a UDP socket shared with the
        DHT; `transport: both|tcp|utp` (D64); checked on a real swarm: uTP
        handshakes with qBittorrent, Transmission and Deluge.

- [ ] **Phase 16 — Easy setup** (user-selected 2026-11-22: installing
      shouldn't need PATH knowledge on any platform).
  - [x] **16a — Double-click opens the dashboard (Windows).** No more
        flashing usage window (D65).
  - [x] **16b — `denco setup`.** (D66; double-click offers it on Windows.) The binary installs itself per user
        (`%LocalAppData%\Programs\Denco` + user PATH in the registry;
        `~/.local/bin` + a shell rc line), `-remove` undoes it. No admin.
  - [ ] **16c — Install scripts.** `install.sh` (macOS/Linux) and
        `install.ps1` (Windows): detect OS/arch, download, verify
        `SHA256SUMS`, then set up as 16b.
  - [ ] **16d — Package managers.** Homebrew tap (macOS + Linux), winget
        and Scoop (Windows), updated by the release workflow (needs a tap
        repo and tokens on the user's account).
  - [ ] **16e — Windows installer (optional).** Per-user Inno Setup
        installer next to the zip (needs approval for the build tool).

Out of scope for now (may revisit): BitTorrent v2, GUI (uTP, MSE and web
seeds moved into Phase 15 on 2026-11-21).
