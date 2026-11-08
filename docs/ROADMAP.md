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
- [ ] **Phase 11 — DHT.** Basic BEP 5 DHT (KRPC over UDP, routing table,
      get_peers / announce_peer, bootstrap, persisted node cache).
- [ ] **Phase 12 — Daily-driver polish.** File selection (skip files),
      stop-seeding / pause-uploads controls, ratio display, error states &
      retry, real-world interoperability testing, README/usage docs.

Out of scope for now (may revisit): uTP, encryption (MSE),
web seeds, BitTorrent v2, TUI/GUI.
