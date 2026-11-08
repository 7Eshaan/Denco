# Decisions

One short entry per decision. Newest at the bottom. Format:
`D<n> — title` · context · decision · alternatives · why.

---

### D1 — Context files for multi-chat continuity
- **Decision:** `CLAUDE.md` (stable rules + start/end-of-session protocol),
  `docs/ROADMAP.md` (stable phase plan, checkboxes), `docs/PROGRESS.md` (living
  state with a single explicit "Next action"), `docs/DECISIONS.md` (this file),
  `docs/ARCHITECTURE.md` (package map, data flow, formats). Plus git history: one
  commit per phase.
- **Alternatives:** single NOTES file; relying on chat memory.
- **Why:** splitting *stable plan* (ROADMAP) from *living state* (PROGRESS) keeps
  PROGRESS short, so a new chat can resume from one small file.

### D2 — Daemon + thin CLI architecture
- **Decision:** a long-running `denco daemon` owns all torrents; other commands
  (`add`, `list`, `pause`…) are short-lived clients talking to it over IPC.
- **Alternatives:** single foreground process per download (like `aria2c`).
- **Why:** required for `list/pause/resume` across invocations, seeding in the
  background and restoring on startup.

### D3 — IPC = JSON over HTTP on 127.0.0.1
- **Decision:** daemon serves a small JSON API via `net/http` bound to loopback,
  with a random auth token written to the state dir (file mode 0600).
- **Alternatives:** Unix domain sockets (awkward on Windows), gRPC (dependency).
- **Why:** stdlib-only, identical on all three OSes, easy to debug with curl.

### D4 — Config file format: flat `key: value` parsed in-house
- **Decision:** the config file uses the flat YAML-like syntax from the spec
  (`key: value`, `#` comments), parsed by a ~100-line stdlib parser.
- **Alternatives:** `gopkg.in/yaml.v3` (external dep), JSON (no comments), TOML.
- **Why:** the config is flat, so a full YAML library isn't needed; keeps
  `go.mod` at zero deps. Revisit only if nested config becomes necessary.

### D5 — Module layout: everything under `internal/`
- **Decision:** `cmd/denco` for the binary, one package per concern in `internal/`.
- **Why:** prevents accidental public API commitments; clear package boundaries.

### D6 — Bencode: generic values instead of reflection
- **Decision:** `bencode.Decode` returns `int64 | string | []any | map[string]any`;
  callers extract fields explicitly. `Encode` accepts the same shapes (plus
  `int`, `[]byte`, `[]string`, `map[string]string`). Byte strings are Go
  `string` (Go strings may hold arbitrary bytes, so binary is safe).
- **Alternatives:** struct tags + reflection (like `encoding/json`).
- **Why:** much less code, easier to read and fuzz; the number of bencoded
  structures in BitTorrent is small. Info hash is taken from the *raw* bytes of
  the `info` value (`bencode.DictSpans`), never from re-encoding, so torrents with
  non-canonical encoding still get the correct hash.

### D7 — Bencode strictness
- **Decision:** reject leading-zero / `-0` integers, trailing data, and nesting
  deeper than 256; *accept* unsorted dict keys and duplicate keys (last wins).
- **Why:** strict where malformed input signals corruption, lenient where real
  torrents in the wild are sloppy. Depth limit prevents stack exhaustion.

### D8 — Path safety split between metainfo and storage
- **Decision:** `metainfo` *rejects* torrents whose path components are empty,
  `.`, `..`, or contain `/`, `\`, NUL, and torrents with two identical file
  paths. Invalid UTF-8 in names is replaced with `_` (prefers `name.utf-8` /
  `path.utf-8` when present). Platform-specific issues — Windows reserved names
  (`CON`), illegal chars (`<>:"|?*`, which includes drive letters like `C:`),
  case-only collisions on case-insensitive filesystems, file-vs-directory
  clashes — are *handled in `storage`* (Phase 4), which also does a final
  "resolved path is inside the download dir" check.
- **Why:** traversal is malicious → reject. `1: Intro.mp4` is a perfectly valid
  name on macOS/Linux → keep the torrent working and only adapt where needed.

### D9 — v1 torrents only
- **Decision:** support BitTorrent v1 and the v1 half of hybrid torrents;
  v2-only torrents (no `pieces`) are rejected with a clear error.
- **Why:** v2 roughly doubles the hashing/storage complexity; v1 covers almost
  all public torrents.

### D10 — Human-readable sizes use decimal SI units
- **Decision:** `format.Bytes` uses 1 KB = 1000 B with labels B/KB/MB/GB/TB.
- **Alternatives:** binary units (KiB/MiB), or 1024 with "KB" labels (common
  but mislabelled).
- **Why:** labels stay accurate and match the spec's example output and macOS
  Finder. Side effect: power-of-two piece sizes display as e.g. `262.1 KB`.

### D11 — Minimum Go version 1.23
- **Decision:** `go.mod` declares `go 1.23` though development uses 1.27.
- **Why:** lets the project build on older distro toolchains; nothing newer is
  needed so far. Raise it only when a newer stdlib feature is actually used.

### D12 — Peer addresses use `net/netip.AddrPort`
- **Decision:** trackers, magnet `x.pe`, and (later) DHT/peer code all
  represent peers as `netip.AddrPort`; IPv4-mapped IPv6 is unmapped to IPv4.
- **Alternatives:** custom `Peer{IP net.IP; Port}` struct.
- **Why:** stdlib, comparable (usable as a map key for dedup), cheap, and
  handles IPv4 + IPv6 uniformly.

### D13 — Peer ID format `-DN0001-` + 12 random alphanumerics
- **Decision:** Azureus-style ID, regenerated on each daemon start.
- **Why:** the convention other clients and trackers recognise; random per
  session avoids long-term tracking. Some private trackers whitelist client
  IDs — if that matters later, this is the one place to change.
- **Amended in Phase 3:** originally `-DE…`, but `DE` is Deluge's code, so
  peers and trackers would have taken Denco for Deluge (misleading, and
  impersonating a client can get you banned from private trackers). Changed to
  `DN`, which isn't in the client-code lists I know of.

### D14 — Announce to all tiers concurrently
- **Decision:** `tracker.List.AnnounceAll` runs every tier in parallel; inside a
  tier, trackers are tried in (shuffled) order until one answers, and the one
  that answers is moved to the front (BEP 12). Each attempt has a 15s timeout.
  Magnet trackers each get their own tier.
- **Alternatives:** strict BEP 12 (stop at the first working tracker overall).
- **Why:** matches libtorrent/qBittorrent defaults; many public trackers are
  dead, and announcing to all of them finds far more peers.

### D15 — Tracker HTTP handling details
- **Decision:** build the query string by hand with byte-wise percent-encoding
  (url.Values would turn byte 0x20 into `+`); keep any existing query (private
  tracker passkeys); cap responses at 2 MiB; a bencoded `failure reason` wins
  over the HTTP status; any other non-200 is an error; clamp `interval` to at
  least 60s. UDP trackers return `ErrUnsupportedScheme` and are skipped.
- **Why:** each rule fixes a real-world interop or abuse case; UDP trackers
  (BEP 15) come in Phase 5b behind the same `Announcer` interface (D16).

### D16 — Add UDP trackers (BEP 15) as Phase 5b
- **Decision:** (user-approved 2026-11-08) implement UDP tracker support right
  after the single-torrent engine works, as a lettered phase so existing phase
  numbers stay valid.
- **Alternatives:** leave it out and rely on DHT (Phase 11).
- **Why:** most public torrents list mainly `udp://` trackers; without them
  peer discovery is poor until DHT exists. Stdlib-only (`net.UDPConn`), small.
  Placed after Phase 5 so it can be tested with real downloads end to end.

### D17 — One `peer.Conn` = two goroutines + channels
- **Decision:** after the handshake, each connection runs a reader goroutine
  (delivers messages on `Incoming()`, a buffered channel) and a writer
  goroutine (drains a buffered queue fed by `Send()`, batches writes into one
  flush, emits keep-alives after 90s idle). Any failure or `Close()` tears the
  connection down exactly once (`sync.Once`); `Done()` and `Err()` report it.
  Peers silent for 2 minutes are dropped (`ErrIdle`).
- **Alternatives:** a single goroutine with `select` on a net.Conn (not
  possible in Go — reads block); callbacks instead of channels.
- **Why:** the idiomatic Go shape; the session (Phase 5) can `select` over many
  peers' channels plus timers without ever blocking on a socket. Back-pressure
  is natural: if the session stops reading, the reader stops reading the socket.

### D18 — Peer wire codec is permissive about unknown messages
- **Decision:** `ReadMessage` decodes any ID (unknown ones like BEP 6 fast
  extension pass through for the session to ignore); typed parsers validate
  payload sizes strictly. Incoming messages capped at 2 MiB; blocks are 16 KiB.
- **Why:** dropping peers for messages we merely don't implement would cost
  us good peers; strict payload checks still catch malformed input.

### D19 — Accept reads the peer's handshake before replying
- **Decision:** for incoming connections we read the peer's handshake first,
  check the info hash against the torrents we serve (`known` callback), then
  reply. Self-connections (our own peer ID) are rejected in both directions.
- **Why:** one listening port serves all torrents, so we must learn which
  torrent the peer wants before we can answer; trackers often return our own
  address, so self-connection detection saves a wasted slot.

### D20 — On-disk layout rules (storage.Layout)
- **Decision:** files go to `<download dir>/<torrent name>/<path…>` (single-file:
  `<dir>/<name>`). Per component: on Windows replace `<>:"/\|?*` and control
  chars with `_`, strip trailing dots/spaces, prefix reserved device names
  (`CON`, `com1.txt` → `_CON`, `_com1.txt`); on every OS truncate to 255 bytes
  (keeping the extension, never splitting UTF-8). Collisions are detected
  **case-insensitively on every OS** and resolved with ` (1)`, ` (2)` suffixes;
  a path that needs an existing *file* to be a directory is rejected. Every
  final path must pass `filepath.IsLocal` and a `filepath.Rel` containment check.
  Files that were changed carry `Renamed: true` so the UI can say so.
- **Why:** same layout on all OSes (a torrent downloaded on Linux can be moved
  to a Mac and resumed); two independent containment checks for traversal.

### D21 — Storage I/O model
- **Decision:** one mutex per torrent serialises all reads/writes; up to 32
  file handles are cached (LRU). Files are created lazily on first write (sparse
  where the FS supports it), except zero-length files, created at open. No
  preallocation. `Open` refuses if any target file exists unless
  `AllowExisting` (resume / user said "use existing data") or `ReadOnly`
  (verify). `DeleteFiles` removes only the layout's files, then directories that
  became empty — never `RemoveAll`, never the download root.
- **Alternatives:** per-file locks; preallocating full size; mmap.
- **Why:** simplest correct design; disk is rarely the bottleneck for one
  torrent. Preallocation wastes space for skipped files (Phase 12) and is slow
  on some filesystems. Revisit if profiling shows lock contention.

### D22 — Piece selection & block bookkeeping (pieces.Manager)
- **Decision:** 16 KiB blocks. Order: (1) unrequested blocks of active pieces,
  (2) new piece — random until 4 pieces are complete, then rarest-first with
  random tie-break, (3) endgame duplicates when every missing block is already
  requested; duplicates produce cancel lists. Active pieces capped by a memory
  budget (128 MiB, min 4 pieces); an empty, un-requested active piece can be
  abandoned to free a slot. Completed pieces go to "pending" with their data
  and contributor list; the caller hashes, writes, then calls `PieceVerified`.
  Choke/disconnect → `PeerRequestsDropped` frees that peer's blocks.
- **Why:** the standard strategy (libtorrent/BEP 3 recommendations); hashing
  happens outside the Manager so it can run off the session goroutine; the
  contributor list is what Phase 5 needs to ban peers that send bad data.
- **Known cost:** picking scans all pieces (O(n)) — fine up to ~10⁵ pieces;
  revisit with a bucketed structure if profiling says so.
