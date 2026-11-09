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
- **Superseded in part:** since Phase 5b, `udp://` is supported (D25); only
  other schemes (e.g. `wss://`) return `ErrUnsupportedScheme`.

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

### D23 — Torrent session = one event loop + helper goroutines
- **Decision:** `torrent.Session` has one loop goroutine that owns *all* mutable
  state (pieces.Manager, peers, swarm, tracker info). Helpers only send events
  on one channel: a forwarder per peer conn (messages, close), dial attempts,
  the announcer, and hash+write workers (bounded by a process-wide semaphore of
  NumCPU). The loop never blocks on I/O: it uses `peer.Conn.TrySend` and drops
  peers whose 256-message queue is full. Stats are published as an immutable
  snapshot (every tick and on completion) behind a mutex for other goroutines.
  `Run` returns only after every helper has exited (WaitGroup + draining).
- **Alternatives:** mutex around shared state touched by per-peer goroutines.
- **Why:** no lock ordering to get wrong, easy to reason about and to test;
  one slow peer can't stall the torrent.

### D24 — Session policies (Phase 5)
- **Decision:**
  - requests: 32 outstanding blocks per peer (16 KiB → 512 KiB in flight);
  - snubbing: a peer with outstanding requests that delivers nothing for 60s
    is disconnected (its blocks go back to the pool);
  - reconnects: failed dials back off 30s·2ⁿ (cap 30 min); a peer that gave
    us data is retried after 30s with its failure count reset;
  - bad data: a peer that alone supplied a piece failing its hash is banned
    immediately; shared blame bans after 3 strikes; banned peers are never
    redialed;
  - tracker: one interval for all tiers (the shortest any working tracker
    asked for); all-fail retries back off 1m·2ⁿ; `completed` is sent even if
    the user stops right after finishing; `stopped` on shutdown (5s timeout);
  - after completion: stop dialing (seeding behaviour is Phase 6).
- **Why:** conventional values from mainstream clients; each rule is covered by
  an end-to-end test in `internal/torrent/session_test.go`.

### D25 — UDP tracker client design (BEP 15 + BEP 41)
- **Decision:** `tracker.UDPClient`, reached through `tracker.Client.Announce`
  dispatching on URL scheme (so `List` and the session are unchanged). One
  connected UDP socket per announce; connection IDs cached per `host:port` for
  60s and shared process-wide (`defaultUDP`), so regular announces skip the
  connect round trip. Retransmit after 15·2ⁿ s (n ≤ 8); in practice the
  per-attempt context (15s in `List`) ends it sooner. A tracker error for a
  *cached* ID triggers one fresh connect + retry (tracker restarts). Packets
  with the wrong transaction ID are ignored. Peer entries are 6 bytes for an
  IPv4 tracker socket, 18 for IPv6. URL path+query is sent as BEP 41 URLData.
  ICMP "port unreachable" read errors fail the attempt immediately.
- **Alternatives:** one shared UDP socket demultiplexing all trackers (fewer
  sockets but more code: routing by transaction ID, lifetime management).
- **Why:** simplest correct implementation; sockets are cheap and short-lived.

### D26 — Uploading, choking and seeding
- **Decision:**
  - **Listener:** one TCP port for all torrents (`torrent.Listener`); reads the
    peer's handshake, routes by info hash to the registered `Session`
    (`Add/Remove`), at most 64 handshakes in flight. A session refuses incoming
    conns while `Run` isn't active.
  - **Serving:** requests are validated (≤ 32 KiB, inside the piece, piece
    verified); bad ranges / nonexistent pieces disconnect the peer; requests
    while choked are ignored (BEP 3). Up to 256 queued per peer. A per-peer
    uploader goroutine does disk reads; the loop then sends only if the block
    wasn't cancelled and the peer wasn't choked meanwhile.
  - **Choker:** every 10s unchoke the 4 interested peers with the best rate —
    their upload to us while downloading, our upload to them while seeding —
    plus 1 optimistic unchoke rotated every 30s. Newly interested peers get a
    free slot immediately. `SetUploadsPaused(true)` chokes everyone.
  - **Seeding:** `Run` keeps going after completion (state `seeding`) unless
    `NoSeed`; seeds keep dialing (NATed leechers can't reach us) but drop
    connections to other seeds. `have` is not sent to peers that already have
    the piece.
  - **Bans by IP** (port ignored), since incoming peers use random ports.
    Tests use an unexported `banExactAddr` because every test peer is 127.0.0.1.
  - `denco download` seeds only with `-seed` (CLI default: exit when done; the
    engine default is to seed — the daemon will make this a config option).
- **Why:** the standard BitTorrent choking strategy (BEP 3, libtorrent's
  defaults); all choke decisions are a pure function (`chooseUnchoked`) so they
  can be unit-tested.

### D27 — Resume data format and trust rules
- **Decision:** per torrent, `<state dir>/<infohash>.torrent` (original bytes,
  written once) + `<infohash>.json` (State: version, download dir, verified
  bitfield, lifetime up/down totals, added/completed/saved times, per-file
  size+mtime stamps, `clean` flag). Default state dir:
  `os.UserConfigDir()/denco/resume` (dirs 0700, files 0600). Every write is
  temp file → fsync → rename → fsync dir.
  - **Save order:** take the session's `Have()` snapshot *first*, then
    `Storage.Sync()`, then stamp files, then write. Pieces in Have were written
    before being marked verified, so after the fsync they are durable.
  - **When:** immediately on start, every 30s (skipped if nothing changed),
    on completion, and a final `clean=true` save after `Run` returns.
  - **Restore:** missing/too-short file → drop its pieces; file longer than
    the torrent says → re-hash its pieces; `clean` state + changed size/mtime →
    re-hash that file's pieces (external modification); `!clean` (crash) →
    trust the bitfield without comparing mtimes, since our own writes after the
    last save changed them. Mismatched torrent/piece count/file list →
    `ErrMismatch`, caller falls back to a full recheck.
- **Alternatives:** bencoded state (libtorrent "fastresume"); always full
  recheck on start; per-piece checksums in the state.
- **Why:** JSON is stdlib and human-inspectable for debugging. The clean flag
  gives fast restarts after crashes (no multi-GB re-hash) while still catching
  files edited while Denco was stopped. **Known limitation:** a file modified
  externally *while Denco was running and then crashed* isn't detected; such
  pieces would be served wrongly until a manual recheck (`denco verify`).

### D28 — `denco download` resumes automatically
- **Decision:** rerunning `denco download <file.torrent>` finds the saved state
  and continues; without `-dir` it uses the saved download dir. `-existing`
  is only for data Denco has no state for. A saved state for a *different* dir
  than an explicit `-dir` is ignored (fresh start there).
- **Why:** "run the same command again" is the least surprising UX.

### D29 — Engine: one run goroutine per started torrent; pause = stop
- **Decision:** `engine.Engine` keeps a table of torrents under one mutex.
  Starting a torrent spawns a goroutine that owns *everything* for that run:
  opens storage, trusts/re-checks saved pieces (`resume.Restore`, full
  recheck on mismatch or for `-existing`), runs a `torrent.Session` with a
  `resume.Saver`, and on exit makes the final clean save and closes storage.
  **Pause** cancels that run (state `paused=true` saved); **resume** starts
  a fresh run from the saved state. A run that fails (e.g. disk error) leaves
  the torrent in `error` until the user resumes it (= retry).
  **Queue:** at most `MaxActive` (default 5) *incomplete* torrents run;
  complete ones always run (seeding) and don't take a slot. The queue is
  re-evaluated on every add/pause/resume/remove/completion/run exit, in ID order.
- **Alternatives:** make `Session` restartable/pausable in place; keep
  storage open for paused torrents.
- **Why:** `Session.Run` is single-use by design (D23); stopping and
  restarting reuses the already-tested resume path, holds no file handles
  for paused/queued torrents, and keeps the engine small. The cost — a
  paused torrent re-announces "started" on resume — is normal client behaviour.

### D30 — Daemon IPC details
- **Decision:** the daemon serves the JSON API (see ARCHITECTURE) on a
  *random* port of 127.0.0.1 and writes `{addr, token, pid}` to
  `<denco home>/daemon.json` (0600). Clients send `Authorization: Bearer
  <token>`, compared in constant time. `.torrent` files are sent as bytes,
  so the daemon never reads client paths (relative `-dir` is made absolute by
  the CLI). One daemon per home: `denco daemon` refuses if one answers, and
  `denco download` refuses (unless `-state` is given) because both would
  write the same resume files. A daemon file nobody answers at is "stale" and
  treated as no daemon. `denco stop` waits until the daemon file is gone
  (the daemon removes it after all torrents have saved and announced
  "stopped"). The daemon runs in the foreground; the user starts it. No
  auto-spawn for now: detaching a process portably needs OS-specific code.
  (Proposed to the user 2026-11-08 as the default; they said to start
  Phase 8 without choosing otherwise — revisit if they prefer auto-start.)
  The first Ctrl-C/SIGTERM starts a clean shutdown and then gives the
  signals back to the OS, so a second Ctrl-C quits immediately — safe,
  because resume data survives a hard stop (D27).
- **Alternatives:** fixed API port (collides, needs config); Unix sockets
  (D3); auto-starting the daemon from `denco add`.
- **Why:** stdlib only, identical on all OSes; the token file's permissions
  limit control to the user who started the daemon, and the token also stops
  web pages from driving the API via the browser.

### D31 — Torrent IDs
- **Decision:** each torrent gets a small number when added, stored in its
  resume state (`id`), so `denco pause 3` always means the same torrent,
  across restarts and after others are removed. New ID = highest + 1.
  States without an ID (made by `denco download`) get IDs on daemon start,
  in the order they were added. Commands also accept `#3` and an info-hash
  prefix of ≥ 4 hex chars (ambiguous prefixes are an error).
- **Known cost:** removing the highest-numbered torrent lets the next add
  reuse its number. Accepted; a persisted counter file isn't worth it yet.
- **Why:** list positions shift when torrents are removed, which makes
  scripted or quickly typed commands hit the wrong torrent.

### D32 — Which files a torrent owns
- **Decision:** a newly added torrent is `fresh` (saved in its state). While
  fresh, its storage is opened *without* `AllowExisting`, so a file that
  appeared at its path after `add` makes it fail with an error instead of
  being overwritten. `add` itself refuses if target files exist, unless
  `-existing` (then the first start hash-checks them). After the first
  successful open the files are the torrent's own (`fresh=false`).
  `remove -delete` deletes files only for torrents that own them (not fresh,
  or fresh with `-existing`), via `storage.DeleteFiles` (D21). `Remove`
  returns `FilesDeleted`, so the CLI says "nothing deleted" instead of
  claiming a deletion that didn't happen (found in review).
- **Why:** the "never overwrite unrelated files" rule must hold even for
  torrents that sit in the queue for days before they start.

### D33 — CLI layout and defaults
- **Decision:** all commands moved from `cmd/denco` to `internal/cli`
  (`cmd/denco/main.go` is 10 lines). Denco's own files live in `$DENCO_HOME`,
  default `os.UserConfigDir()/denco` (resume data in `resume/`, daemon file;
  config file in Phase 9). Default download dir `~/Downloads/Denco` (a
  dedicated folder, so nothing collides with other clients' data;
  configurable in Phase 9). `denco list` shows two tables — incomplete, then
  completed — with the same columns (spec: "show completed and incomplete
  torrents separately"). `pause`/`resume` accept `all`; `remove` doesn't, on
  purpose. Magnets/URLs in `add` are rejected with a pointer to Phase 10.
- **Why:** one package for all commands keeps them consistent;
  `DENCO_HOME` lets tests (and users running two instances) isolate state.

### D34 — Fix: complete-at-start torrents announced twice
- **Context:** found while smoke-testing the daemon. `announceLoop` waited on
  the session's `Completed()` channel to send `completed` early; for a
  torrent that *started* complete that channel is already closed, so the
  first wait ended at once and a second announce followed "started"
  immediately. Trackers may penalise announcing too often.
- **Decision:** don't watch `Completed()` when the session started complete.
  Regression test: `TestStartedCompleteAnnouncesOnce` (fails without the fix).

### D35 — Config file
- **Decision:** `$DENCO_HOME/config.yaml`, flat `key: value` (D4). Keys:
  `download_dir`, `max_active_downloads`, `max_connections_per_torrent`,
  `download_limit`, `upload_limit`, `enable_dht`, `enable_pex`,
  `listen_port`, `resume_enabled`, `resume_dir`. Missing file = defaults.
  **Strict:** unknown keys, duplicate keys and bad values are errors with
  the line number (the daemon refuses to start). `#` starts a comment
  (whole line, or after a space); values may be quoted to contain `#`.
  Paths: `~` expanded, must then be absolute (no dependence on the daemon's
  working dir). Booleans: true/false/yes/no/on/off. Rates: see D36.
  `denco config set` validates, then edits the file *in place* (only that
  line; comments and order kept; appends new keys); `unset` removes the line;
  `init` writes a commented file with every key. Daemon flags override the
  file and are validated by the same code (`cfg.Set`). `resume_enabled:
  false` gives the engine a throwaway state dir deleted at exit, so nothing
  persists (the engine itself stays unaware). `enable_dht` (default true,
  as in the spec) and `enable_pex` (default false) are accepted now; the
  daemon prints a note that they do nothing yet. *Amended in Phase 10b:*
  `enable_pex` now works and defaults to **true** (D41).
- **Alternatives:** lenient parsing (warn on unknown keys); rewriting the
  whole file on `set` (loses comments); JSON.
- **Why:** a typo like `max_actve_downloads` silently doing nothing is the
  worst config bug; preserving comments keeps hand-edited files intact.

### D36 — Global rate limiting
- **Decision:** `internal/ratelimit.Limiter`, a token bucket (burst = ¼ s
  of the rate, ≥ 32 KiB) with "take after use, go into debt, wait until
  repaid"; one for downloads and one for uploads, shared by every
  connection of the engine, so limits are totals. **Download:** applied to
  the socket reader in `peer.Conn` (reading slower makes TCP slow the peer;
  counts wire bytes). **Upload:** applied in the per-peer uploader *before*
  the disk read (counts payload), not in the socket writer — a throttled
  writer would fill the send queue and the session would drop the peer.
  Limits change live (`SetRate` wakes waiters; debt is forgiven) via
  `POST /v1/limits`; `denco config set download_limit/upload_limit` applies
  them to a running daemon. Rate syntax: `0` unlimited, bare number = KB/s,
  `K/M/G` (decimal, D10) or `KiB/MiB/GiB`, optional `/s`; minimum 1 KB/s.
- **Alternatives:** `golang.org/x/time/rate` (external dep); per-torrent
  limits; throttling in the writer.
- **Known cost:** with a low limit shared by many peers each peer may get
  < ~270 B/s, slower than one 16 KiB block per 60 s, so the snub timeout
  (D24) drops peers and they get redialed. E.g. 10 KB/s over 50 peers.
  Candidate fix for Phase 12: scale the request pipeline / timeout with
  the limit.

### D37 — PEX as Phase 10b
- **Decision:** (user left it to my judgement, 2026-11-08) implement peer
  exchange (BEP 11) right after magnets, as Phase 10b; until then
  `enable_pex` is accepted and reported as unavailable.
- **Why:** PEX matters for a daily driver — swarms fill up fast even when
  trackers are dead, and it's the main source of peers for many magnets —
  but it is carried by the BEP 10 extension handshake that Phase 10 builds
  for `ut_metadata`. Doing it before Phase 10 would mean building that
  layer twice; after, it's a small addition. Never used for private torrents.

### D38 — Magnet links: fetch metadata first, then a normal session
- **Decision:** a magnet torrent's run goroutine first runs a
  `metadata.Fetcher` (own announce loop with `left=1`, ≤ 10 peers at a time,
  BEP 10 handshake, BEP 9 requests of 16 KiB pieces, ≤ 4 outstanding per
  peer), verifies SHA-1 = info hash, validates with `metainfo.Parse`, wraps
  the info dict in a `.torrent` (magnet trackers as tiers) and saves it.
  Then it continues as an ordinary torrent: same fresh/existing-file rules
  (D32), same session. Every peer the fetcher heard of (incl. `x.pe`) is
  handed to that first session — without this, an `x.pe`-only magnet
  fetched its metadata and then stalled (found by a test). Until metadata
  arrives the resume state has `magnet` set and no `.torrent` file, so a
  pending magnet survives restarts. State `metadata` ("Fetching metadata")
  counts as an active download for the queue.
  **Each peer's worker assembles the whole metadata from that peer alone**;
  the first copy that hashes right wins, and a peer whose full copy fails
  is certainly lying and is banned for this fetch. Metadata > 16 MiB is
  refused. *First version* mixed pieces from several peers and, on a bad
  hash, banned every contributor — which banned honest peers alongside a
  liar and could hang the fetch forever (caught by the multi-CPU stress
  run: 3 of 31 runs hung). Metadata is small, so the duplicated download
  is cheap; correctness wins.
- **Alternatives:** teach `torrent.Session` to run without metadata (it is
  built around the piece count everywhere); keep the fetch connections and
  hand them to the session (saves a reconnect, much more coupling).
- **Why:** keeps the session unchanged and the magnet logic in two small
  places (`metadata`, `engine/magnet.go`); the cost is one extra connection
  per peer, once per magnet.

### D39 — Extension protocol and serving metadata
- **Decision:** BEP 10 handshake types live in `peer` (`ExtHandshake`;
  lenient: odd values are skipped, only a non-dict fails). Denco asks peers
  to send `ut_metadata` on ID 1 (`peer.ExtMetadataID`); Phase 10b adds
  `ut_pex` the same way. Sessions (and the listener, so incoming peers see
  it) set the extension bit; a session sends its extended handshake
  (`m`, `metadata_size`, `v`, `p`, `reqq`) right after its bitfield to peers
  that set the bit, and answers ut_metadata requests from its info dict
  (reject for out-of-range pieces), also for private torrents (BEP 9
  doesn't restrict it and the peer already knows the hash).
- **Why:** without serving, magnet users can't get metadata from Denco
  seeds — and Denco-to-Denco magnet tests would be impossible.

### D40 — `.torrent` from HTTP(S) URLs is fetched by the CLI
- **Decision:** `denco add <url>` downloads the file in the CLI (30s
  timeout, 16 MiB cap, ≤ 10 redirects), validates it, and sends the bytes
  like a local file. A redirect to a `magnet:` URL (common on torrent
  sites) is caught and the magnet is added instead.
- **Alternatives:** the daemon downloads URLs.
- **Why:** the daemon API stays "bytes or magnet"; errors appear right in
  the user's terminal; uses the user's own network environment (proxies set
  in their shell apply).

### D41 — Peer exchange (BEP 11)
- **Decision:** sessions advertise `ut_pex` (ID 2) when `enable_pex` is on
  and the torrent isn't private (BEP 27: private swarms get peers only from
  their tracker). Rules:
  - **What we advertise:** connected peers that accept connections at a
    known address — ones we dialed (flag `reachable`), and incoming ones
    whose extension handshake gave a listening port (`p`); never the
    recipient itself. Seeds carry the `seed` flag.
  - **When:** per peer. The first message goes out right after that peer's
    extension handshake, later ones at most once per `PEXInterval` (60s),
    each listing what was added/dropped since the last one *sent to that
    peer* (≤ 50 each; the rest follow later). If there is nothing new the
    peer's timer isn't advanced, so news isn't held back for a minute.
  - **What we accept:** from one peer, one message per half interval (more
    frequent ones are ignored, not punished) and ≤ 50 addresses per
    message; PEX can grow a torrent's swarm only to 2000 known addresses
    (tracker peers are unlimited: we chose those sources). A seed skips
    addresses flagged `seed`. A malformed message (not a dict) drops the peer.
  - **Magnets:** the metadata fetcher also asks for `ut_pex`, but only
    *receives* (privacy is unknown before the metadata arrives; receiving
    leaks nothing). A peer with no metadata but with PEX — typically another
    magnet user — is kept until its PEX message arrives (≤ RequestTimeout).
    PEX peers join the fetcher's list, which seeds the download (D38).
  - **Default on** (user's choice, 2026-11-08), like qBittorrent,
    Transmission and libtorrent; the spec's example showed `false`, but PEX
    is safe to leave on because private torrents never use it. Turn off
    with `enable_pex: false`.
  - `denco status` shows "Peer exchange: on/off". The standalone
    `denco download` tool doesn't speak extensions, so no PEX there.
- **Alternatives:** one global round every minute (first version: a magnet
  user who stays a few seconds never heard anything — found while writing
  the fetcher test); advertising incoming peers at their source address
  (ephemeral port, useless to others); sending PEX from the fetcher too.
- **Why:** follows BEP 11 and libtorrent's limits (50 per message, one per
  minute) while making the first message useful; caps keep a hostile peer
  from flooding the swarm with fake addresses.

### D42 — DHT (BEP 5)
- **Decision:** a new `dht` package; the engine runs one node for all
  torrents when `enable_dht` is on (default), on UDP with the same port
  number as the peer port (random if taken).
  - **Node:** random 160-bit ID, kept across restarts in
    `<state dir>/dht.json` with up to 200 good nodes (so restarts rarely need
    the routers). IPv4 only; BEP 32 (IPv6), BEP 42 (secure IDs), BEP 43
    (read-only nodes) and BEP 44 (mutable items) are out of scope.
  - **Routing table:** 160 fixed buckets by shared-prefix length, K = 8 —
    a simplification of Kademlia's split buckets that keeps at most K nodes
    per distance band. Good = answered in the last 15 min; bad = 2 failures
    in a row (removed at 5). A full bucket only takes a newcomer in place of
    a bad node or a stale one that has failed; questionable nodes are
    pinged every minute, so this happens naturally. Old nodes are favoured
    (they're the most reliable, per Kademlia).
  - **Trust:** a response counts only if its transaction ID *and* source
    address match the query (no forged answers). A node that queries us
    joins the table only after answering our ping (≤ 16 such pings at once),
    so NATed or spoofed nodes don't pollute it. Bootstrap routers are asked
    for nodes but never put in the table (libtorrent does the same).
    Incoming queries capped at 500/s; announced peers stored 30 min,
    ≤ 500 per hash, ≤ 5000 hashes; ≤ 50 values per answer.
  - **Lookups:** iterative, α = 4 queries in flight, stop when the K closest
    known nodes have all answered; ≤ 200 queries and ≤ 1000 candidates per
    lookup. The routers are asked too when the table has < K nodes *or*
    knows nobody sharing more leading bits with the target than we do (no
    node in the target's bucket) — comparing whole distances isn't enough,
    a node from the wrong half can win on trailing bits.
  - **Filling the table** (added after the public-DHT check, which showed
    ~10 nodes after a minute): bootstrap looks up our own ID, then a random
    ID *inside* each bucket that has room, shallowest first (8 lookups per
    round, 4 at a time); repeated every maintenance round until 100 good
    nodes. Each bucket is tried at most once per refresh period (15 min),
    except that an attempt that reached nobody doesn't count. Random IDs
    alone don't work: half of them land in bucket 0. Result on the public
    DHT: 78 nodes after 1 min, 112 after ~2.5 min.
  - **Sessions:** look up (and announce our TCP port to the K closest that
    gave tokens) at start and every 15 min; a lookup that finds no one is
    retried after 1 min, doubling up to 15 (at daemon start the table is
    often empty). PORT messages both ways: we tell DHT-capable peers our
    port; a peer's PORT is pinged and added if it answers. **Private
    torrents never use the DHT** (BEP 27).
  - **Magnets:** the metadata fetcher looks up peers every minute but
    doesn't announce (nothing to offer yet). It is the only way a magnet
    with nothing but the hash can start.
  - **Config:** `enable_dht` (default true), new `dht_bootstrap`
    (comma-separated host:port, `none` = only remembered nodes; default the
    four well-known routers). `config.DefaultDHTBootstrap` is a variable so
    the CLI's `TestMain` clears it — daemon tests run with DHT on but can
    never contact the public routers. Engine/torrent/metadata tests use
    loopback nodes or fakes.
- **Alternatives:** an external DHT library (dependency rule); a DHT per
  torrent (wasteful, and against how the network expects nodes to behave);
  adding querying nodes directly (pollution, the BEP 5 "good node" rule).
- **Why:** BEP 5 as mainstream clients implement it, kept small; every
  rule above has a test that fails without it.

### D43 — Shared `compact` package
- **Decision:** the 6/18-byte address format lives in `internal/compact`
  (`Append`, `Parse`, `Usable`), used by trackers, PEX and the DHT, instead
  of three copies. `Parse` returns every entry unfiltered (so PEX flags stay
  aligned); callers drop what `Usable` rejects. Trackers now also skip
  unspecified/multicast addresses, not only port 0.


### D44 — File selection (skip files), Phase 12a
- **Decision:** users skip or re-want files; the download works in pieces.
  - **Wanted pieces:** a piece is wanted if it overlaps at least one
    wanted file (`pieces.Wanted`). A *border piece* shared with a skipped
    file is downloaded whole (its hash covers both files) and written
    whole, so the skipped neighbour is created (sparse) holding just those
    bytes — as Transmission does. Empty skipped files aren't created.
  - **Manager:** `want` mask (nil = all). `wanted()` (what to pick/request,
    endgame, interest) honours it; `Complete()` = every *wanted* piece
    verified; `HaveAll()` = full seed. `SetWanted` abandons active pieces
    no longer wanted (their partial data is dropped; the session cancels
    the outstanding requests).
  - **Session:** `Config.Wanted` + thread-safe `SetWanted` (applied by the
    loop). `Completed()` closes when the selection is done (state
    `seeding`: a *partial seed* that uploads what it has). Trackers are
    told the truth: `left` = *all* missing bytes and `completed` is only
    sent for a full seed, so a partial seed is never counted as a seed.
  - **Engine:** selection stored as `resume.State.Skip` (file indexes, from
    0; omitted when empty, so old state files load unchanged).
    `SetFilesWanted(ref, files, wanted)` applies it at once: live via
    `Session.SetWanted` (skipping the rest of a download finishes it), but
    wanting more of a torrent that had *finished* restarts its run — it
    then re-enters the queue (`max_active_downloads`) and re-announces as a
    download, which a channel that's already closed couldn't express. The
    session and saver are created under the engine lock from the current
    selection, so a change made while the torrent is being checked isn't
    lost. Skipping every file with data is refused ("pause or remove it").
    Magnets can't be added with `-skip` (no file list yet); use
    `denco files <id> skip` once the metadata has arrived.
  - **Safety unchanged:** `add` still refuses if *any* target file exists,
    skipped ones included — a border piece would write into it.
  - **Display:** `Torrent.WantedBytes`/`DoneBytes` count *files* (a border
    piece's bytes in a skipped file don't count); `list` shows the
    selected size and its progress, `info` "X selected of Y (N files
    skipped)", `files` a WANT column. Files are numbered from 1 in the CLI
    (`files`, `inspect`, `add` summary); the API uses indexes from 0.
  - **CLI:** `denco add -skip LIST | -only LIST` (one torrent per command),
    `denco files <id> skip|want LIST`; LIST = `3`, `2-5`, `1,4,7-9`, `all`.
  - `denco download` (standalone) honours a selection saved by the daemon
    but has no flags for it.
- **Alternatives:** a separate "parts file" for border pieces (libtorrent /
  qBittorrent) — keeps skipped files from appearing, but is a second
  storage format to write, serve from and clean up; per-file priorities
  (high/normal) — not needed for "skip what I don't want"; keeping the
  session running when re-wanting a finished torrent — it would bypass the
  download queue and never re-announce.
- **Why:** smallest correct design on top of the existing piece machinery;
  every rule above has a test that fails without it (mutation-checked),
  except the refresh of the selection at session start, which only matters
  if it changes during the brief checking phase.

### D45 — Seeding controls, Phase 12b
- **Decision:**
  - **Ratio** = lifetime uploaded ÷ max(lifetime downloaded, bytes of the
    wanted files we have). The `max` gives a torrent added with
    `-existing` (downloaded ≈ 0) a finite ratio, and charges wasted
    downloads. -1 (shown "-") until there is anything to divide by.
  - **Seeding modes per torrent** (`resume.State.Seed`): default = follow
    the global limits; `on` = seed until the user stops it, ignoring the
    limits; `off` = don't seed (stop now if complete, else on completion).
    `denco seed on|off|default <id>…|all`.
  - **Global limits** `seed_ratio` (float) and `seed_time` (`24h`, `2d`,
    `1d12h`; ≥ 1 min), both 0 = no limit by default (the spec lists no
    defaults; mainstream clients seed forever by default). Config file,
    daemon flags `-seed-ratio`/`-seed-time`, applied live by
    `denco config set` (`POST /v1/seed-limits`).
  - **Finished:** a complete torrent that reached a limit (or has seeding
    off) is stopped like a paused one and marked `finished` (saved), state
    "Finished". It is *not* restarted on its own — not on daemon start,
    not when limits are raised; `scheduleLocked` also refuses to start a
    complete torrent whose limit is already reached, so it never starts
    just to stop. `resume` restarts it only if no limit would stop it at
    once; otherwise it fails with the reason and the hint
    `denco seed on <id>` (as qBittorrent, which re-pauses at once, but
    explained). Wanting more files (12a) clears it: it downloads again.
  - **Seeding time** (lifetime, `seed_seconds`) counts time running
    complete with uploads *not* paused, measured by a per-run watcher
    goroutine (every 5 s) that also enforces the limits. Saved with the
    other resume data, but a change of < 5 minutes alone doesn't force a
    write (seeding torrents would otherwise write every 30 s). Stored in
    whole seconds, each run's time *rounded up*, so the saved total never
    falls below a limit the run reached (found by the stress run:
    truncation turned 0.3 s into 0 and blanked the "why finished" note).
  - **Pause uploads per torrent:** `denco pause -uploads <id>|all` /
    `resume -uploads`; the session's existing `SetUploadsPaused` (chokes
    everyone; downloading continues). Saved (`uploads_paused`) and applied
    at the next start. `list` shows "paused" in UP.
  - Per-torrent choices live in the engine entry and are copied into every
    save (`flagsTo`, `resume.Saver.Update`), so a run's final save can't
    drop a choice made while it ran.
  - **Display:** RATIO column in `list`; `info` shows ratio, seeding time,
    the seeding policy in words (or why it finished), paused uploads;
    `status` shows the global limits.
- **Alternatives:** per-torrent numeric ratio/time limits (more flags and
  state for little gain; `on`/`off`/`default` covers the common cases);
  pausing instead of a separate "finished" state (the user couldn't tell a
  limit from their own pause); restarting finished torrents when limits
  are raised (surprising uploads after a config edit).
- **Why:** covers the spec's "stop seeding", "pause uploads", "display
  seeding status / uploaded data" with a small state machine; every rule
  has a test that fails without it (mutation-checked).

### D46 — Pacing downloads under a limit (fixes D36), Phase 12c
- **Context:** reproduced first: 8 peers sharing a 64 KB/s limit (snub
  timeout scaled to 500 ms in the test) — peers were dropped and redialed
  over and over, the seeders sent **257 blocks for a 24-block torrent**,
  and after 30 s only 11 of 24 pieces had arrived. Four causes, each fixed:
  1. **Snub timeout:** a `torrent.Pacer` (one per engine, shared by all
     sessions) knows the download limit and how many peers we are
     downloading from (each session reports its unchoked+interested peers
     every tick, and removes them when it stops). The timeout becomes
     max(`RequestTimeout`, 3 × one block's time at the fair share), capped
     at 10 min.
  2. **Pipeline:** requests per peer = 5 s of its fair share (≥ 2, ≤ 32):
     deep queues under a low limit only make blocks arrive minutes later.
  3. **Endgame off under a limit** (`pieces.Manager.SetEndgame`): duplicate
     requests compete for *our own* bandwidth, and the cancels arrive after
     the data is already on the wire. A peer that really died is still
     dropped by the paced timeout and its blocks re-requested.
  4. **Fair limiter:** `ratelimit.WaitN` was unfair — a waiter re-checked
     the *global* balance after waking, so a reader that had just been let
     through could read again at once and starve the others (one test run:
     a goroutine got 0 bytes). It now works as a reservation: the wait is
     fixed at call time from the debt queued ahead of it, first come first
     served. Long-run rate and `SetRate` behaviour are unchanged.
  Also: **`peer.Conn`'s idle deadline is re-armed before every socket read**
  instead of once per message, so our own throttling between reads no
  longer counts as the peer being silent (a throttled 400 KB message was
  dropped as "idle timeout").
  After: same test completes in ~6 s (the limit's minimum), every peer
  connected once, ≤ 32 blocks sent.
- **Not changed:** the *upload* side. Remote clients time out on us only
  if a block takes them minutes; with 4+1 unchoke slots even 1 KB/s gives
  each ~200 B/s — borderline only at the 1 KB/s minimum. Revisit if seen.
- **Alternatives:** counting partial bytes as progress (needs reader-level
  piece accounting; peers sending only keep-alives/haves would never time
  out); per-torrent limits (not in the spec).

### D47 — Automatic retry of failed torrents, Phase 12c
- **Decision:** a run that ends with an error is retried on its own after
  1 min, doubling up to 1 h (`engine.Config.RetryBase/RetryMax`), until it
  works or the user acts. Not for errors a retry can't fix:
  `storage.ErrExists` (someone else's files — D32), `ErrUnsafePath`,
  `metainfo.ErrInvalid`; those wait for the user as before. A run that
  lasted ≥ 10 min before failing resets the delay. `resume` = the user's
  retry (now, failures forgotten); `pause` cancels the retry and clears the
  shown error (paused is the user's latest word); `remove`/`Close` cancel
  it. Shown as "retrying by itself in 2m (attempt 3); 'denco resume 3'
  retries now", or "needs you: fix the problem, then 'denco resume 3'".
  Not saved: after a daemon restart a failed torrent simply starts again,
  which is the same as a retry.
- **Why:** spec 3 "handle failed downloads and retry"; a full disk or an
  unplugged drive shouldn't need the user to notice and type `resume`.

### D48 — Daemon log file, Phase 12c
- **Decision:** `log_file` setting / `denco daemon -log FILE` (`none` =
  terminal, the default). The log then goes *only* to the file (appended);
  the startup lines still print. At 10 MB the file is renamed to
  `<name>.1` (replacing the previous one) and a new one started, so at
  most ~20 MB. A writer with its own mutex, stdlib only; on Windows the
  old `.1` is removed and the file closed before renaming.
- **Why:** running the daemon under launchd/systemd/a login item needs its
  log somewhere; one backup keeps the latest history without unbounded
  growth. Auto-start of the daemon itself is still an open question.

### D49 — Port forwarding (NAT-PMP, UPnP IGD), Phase 12d
- **Decision:** a new `internal/portmap` package, standard library only,
  driven by the engine when `port_forwarding` is on (**default on**, like
  mainstream clients). It forwards the peer port (TCP) and the DHT port
  (UDP).
  - **Order:** NAT-PMP first (one round trip to the gateway, ~2 s at most
    when nothing answers), then UPnP (SSDP search, 2 s of answers). PCP
    (RFC 6887) isn't spoken; PCP routers answer NAT-PMP requests.
  - **Gateway:** the standard library can't find it. Linux reads
    `/proc/net/route`, macOS runs `/sbin/route -n get default`
    (build-tagged files); elsewhere (Windows), or if that fails, we guess
    the first host of our subnet (`192.168.1.1` for `192.168.1.23/24`). A
    wrong guess only costs the NAT-PMP timeout; UPnP doesn't need the
    gateway address.
  - **Leases:** NAT-PMP asks 7200 s, UPnP 3600 s, both renewed at half the
    lease granted; a router that only takes permanent UPnP mappings
    (error 725) gets lease 0, re-added every 30 min. Mappings are removed
    when the daemon stops (≤ 2 s wait).
  - **Port differs:** we ask for external = internal. If the router
    grants another port (NAT-PMP may), or UPnP says the port is forwarded
    to another device (718, then up to 3 random ports ≥ 49152), sessions
    announce the router's port: `torrent.Config.PublicPort` feeds the
    tracker `port`, DHT `announce_peer` and the BEP 10 `p` field. The BEP 5
    PORT message still carries our local DHT port.
  - **Failure:** nothing found or refused → shown in `denco status` with
    the reason, looked for again every 10 min (a laptop changes networks);
    renewal failure → the same.
  - **Safety:** anything on the LAN can answer an SSDP search, so we only
    send HTTP to the IP that answered (description, `URLBase` and control
    URL must all point at it), plain HTTP, no redirects, responses capped
    at 256 KiB. The mapping's internal client is our own LAN address.
  - **Tests:** fake NAT-PMP and UPnP routers on loopback; every test
    points `Gateway`/`SSDPAddr` at them. `config.DefaultPortForwarding` is
    turned off in the CLI tests so no daemon there talks to the real router.
- **Why:** behind a home NAT, nobody can connect to us without a forwarded
  port, which halves the peers we can reach and hurts seeding and the DHT.
  The two protocols cover nearly all home routers; both are small enough to
  write with `net`, `net/http` and `encoding/xml`.
  - **Found while testing:** a stop during a renewal lost track of the
    mappings in place (they stayed until their lease ran out) — fixed,
    `TestCloseDuringRenewalRemovesMappings` fails without it; and a stop
    didn't wake a pending NAT-PMP read (up to the full retry wait) — the
    read is now interrupted. On macOS a failed multicast send hints at the
    Local Network privacy setting.
- **Not done:** IPv6 (no NAT there; a firewall pinhole would need PCP or
  UPnP IGD:2's `AddPinhole`); watching for a network change between
  renewals (the next renewal or the 10-min retry notices).

### D50 — Real-router fixes and the first real download, Phase 12e
- **Context:** the first daemon run against the user's real router (UPnP,
  WANIPConnection:1 at 192.168.1.1:5555, no NAT-PMP) failed: the TCP
  mapping got `501 Action Failed`, the UDP one `EOF`, and `denco status`
  printed the combined error over two lines.
- **Decision:**
  - SOAP calls set `Connection: close` (one connection per call). The
    `EOF` came right after an error response on a kept-alive connection —
    the router had closed it without saying so.
  - A `501` to a mapping with a lease is retried once with lease 0, like
    `725`; if that fails too, the original `501` is shown. Many ISP
    routers answer 501 instead of the standard 725. **Not confirmed on the
    user's router:** trying it there (a real mapping) was denied in the
    dev environment; the user checks with `denco status`.
  - Several errors are joined with `; ` (one line), not `errors.Join`.
- **Real download (Debian 13.7 netinst, 792.7 MB, public swarm):** added by
  https URL; done in 3 min 14 s (≈ 4.1 MB/s, up to 47 peers); SHA-256
  matches Debian's; total downloaded = exactly the size (no waste). Stop
  0.6 s; restart → straight to seeding, no full recheck. A trackerless
  magnet (DHT only) got its metadata in ~15 s and reached 4.8 MB/s.
- **Observed, left as is:** with many fast seeds, partly downloaded pieces
  fill most of the 128 MB per-torrent memory budget (D22); daemon RSS was
  ~290 MB during the download. Within the design limit.

### D51 — Peers finish their own pieces (less memory), after Phase 12
- **Problem (D50):** in a real download with ~46 fast seeds, ~112 MB of
  received data sat in partly downloaded pieces and the daemon used
  ~290 MB. The picker's first rule was "continue any active piece", so
  every peer took blocks of every piece; each piece then waited for its
  slowest peer's block, and hundreds were half done at once, up to the
  128 MiB per-torrent budget (D22).
- **Decision:** `pieces.Manager.NextRequests` prefers, in order:
  1. pieces this peer already works on (requested or delivered blocks),
     and *orphaned* pieces — data received but nothing requested from
     anyone (their peer left or choked us), which would otherwise wait;
  2. a new piece (rarest first, within the budget);
  3. other peers' pieces — when the budget allows nothing new, or this
     peer has nothing new to offer;
  4. endgame, unchanged.
  The 128 MiB budget stays: it is now a worst-case cap, reached only with
  big pieces (one 4 MiB piece per peer doesn't fit).
- **Measured:** a swarm simulation through the real Manager (40 peers,
  mixed speeds, 32-block pipelines; `sim_test.go`, 20 tie-breaking seeds,
  `SIM_SPREAD=1 go test -v -run SimSpread ./internal/pieces`):
  256 KiB pieces — peak 105 → 30 MiB, same time (median 199 → 198 ticks);
  1 MiB — 128 → 59 MiB and faster (263 → 197); 4 MiB — still the 128 MiB
  cap, faster (598 → 423, worst 603 → 544), because pieces no longer wait
  on slow peers. The new rule was never slower on any seed. (A first
  single-run limit in the test was too tight and failed under stress —
  the Manager's tie-breaking is random; the test now fixes the seed.) Lower budgets were tried and rejected: 32 MiB made 1 MiB
  pieces 6× slower, 64 MiB made 4 MiB pieces 3× slower. Real download
  (same Debian ISO): unverified data 112 MB → 1–8 MB, daemon RSS ~290 →
  ~102 MB, same time (3 m 15 s), checksum matches.
- **Not done:** one budget shared by all torrents (each active torrent
  still has its own 128 MiB cap); writing blocks straight to disk instead
  of buffering whole pieces.

### D52 — The daemon starts by itself (auto-start)
- **Decision:** any command that needs the daemon (`add`, `list`, `info`,
  `files`, `peers`, `pause`, `resume`, `seed`, `remove`, `status`) starts
  it in the background when none is running; `denco start [daemon flags]`
  does it explicitly; `denco daemon` still runs in the foreground. Setting
  `auto_start` (default `true`); `false` restores the old "start it
  yourself" message. `denco stop` never starts one ("isn't running").
  Resolves the open question from D30.
- **How:** the CLI re-runs its own executable (`os.Executable()`) as
  `denco daemon` with `DENCO_HOME` set, detached — a new session on
  Unix (`Setsid`), `DETACHED_PROCESS` + new process group on Windows
  (`detach_unix.go` / `detach_windows.go`) — so closing the terminal or
  Ctrl-C doesn't reach it. Its stdout/stderr go to `<home>/daemon.out`
  (a file, not a pipe: a pipe would break when the CLI exits); its log
  goes to `log_file`, or `<home>/daemon.log` if that is `none`. The CLI
  waits up to 30 s for the daemon to answer; if it exits first, the
  error shows the end of `daemon.out` (e.g. a bad config line).
  The start notice goes to stderr so stdout stays the command's output.
- **One daemon only:** `<home>/autostart.lock` (created with `O_EXCL`)
  lets one `denco` start the daemon while others wait for it; a lock older
  than 35 s (its `denco` was killed) is ignored.
- **Tests:** `env.spawn` runs the daemon in-process; the real spawner
  refuses inside a test binary (`testing.Testing()`), which would
  otherwise start a copy of itself.
- **Not done:** starting at login (launchd/systemd/Task Scheduler) — the
  README shows how; a `denco service install` could automate it later.

