# Architecture

Status markers: ✅ implemented · 🚧 in progress · ⏳ planned.

## Process model

```
 denco add/list/pause/...  ──JSON/HTTP (127.0.0.1:<random> + token)──▶  denco daemon (foreground)
        (short-lived CLI)        address+token from $DENCO_HOME/daemon.json   │
                                                                               ├─ ipc server
                                                                               └─ engine
                                                                                  ├─ N torrents (run goroutine → Session + Saver)
                                                                                  ├─ TCP listener (incoming peers, shared)
                                                                                  ├─ resume store ($DENCO_HOME/resume)
                                                                                  └─ DHT node (UDP, same port number; state in resume/dht.json)
```

`$DENCO_HOME` defaults to `os.UserConfigDir()/denco`. Standalone commands
(`inspect`, `magnet`, `announce`, `probe`, `verify`, `download`) need no daemon.

## Package map

| Package | Status | Responsibility |
|---|---|---|
| `cmd/denco` | ✅ | `main.go` only: calls `cli.Main`. |
| `internal/bencode` | ✅ | Bencode decode/encode; raw byte spans of dict values (for info hash). |
| `internal/metainfo` | ✅ | Parse + validate `.torrent`; info hash; file list with offsets; tracker tiers. |
| `internal/format` | ✅ | Human-readable sizes, speeds, durations for CLI output. |
| `internal/magnet` | ✅ | Parse/print magnet URIs (xt hex+base32, dn, tr, x.pe). |
| `internal/peerid` | ✅ | Generate our 20-byte peer ID (`-DN0001-…`); `ClientName` decodes other clients' IDs. |
| `internal/tracker` | ✅ | `Client.Announce` (one announce, HTTP(S) or UDP) and `List` (BEP 12 tiers, concurrent, fallback + promote). `Client` dispatches by scheme: HTTP(S) in `http.go`, UDP (BEP 15 + BEP 41) in `udp.go`. |
| `internal/peer` | ✅ | Peer wire protocol: `Handshake`, `Message` codec + typed constructors/parsers, `Conn` (Dial/Accept, reader + writer goroutines, keep-alive, idle timeout, optional read limiter), BEP 10 `ExtHandshake` (`extension.go`), BEP 11 `PEX` message codec (`pex.go`). |
| `internal/metadata` | ✅ | BEP 9: `Message` encode/decode, `Piece`, `Assembler` (SHA-1 check, reset on mismatch), `Fetcher` (announce + up to 10 peers, each assembling its own full copy; bans a peer whose copy fails the hash; with `PEX`, also learns peers from one ut_pex message per peer — receive only; `Status`, `Peers`). |
| `internal/bitfield` | ✅ | Fixed-size piece set in wire format (high bit = piece 0); validated `FromBytes`. |
| `internal/storage` | ✅ | `Layout` (OS-safe paths, sanitising, collisions, containment) and `Storage` (ReadAt/WriteAt across files, handle LRU, no-overwrite, ReadOnly, safe DeleteFiles). |
| `internal/pieces` | ✅ | `Check`/`VerifyPiece`/`Recheck` (parallel hashing), `Manager` (rarest-first, random-first, endgame, memory budget, block ownership, wanted-pieces mask), `Wanted`/`SkipSet` (file selection → pieces, D44). |
| `internal/torrent` | ✅ | `Session`: event loop owning Manager + peers + swarm; dialing/backoff/IP bans, request pipelining, snub detection, hash+write workers, announce loop, uploading (validation, cancel, per-peer uploader), tit-for-tat choker + optimistic unchoke, seeding, pause uploads, peer exchange (`pex.go`, D41), file selection (`SetWanted`, partial seeds, D44), pacing under a download limit (`pace.go`: `Pacer`, scaled snub timeout and pipeline, D46), Stats/Peers. `Listener`: one TCP port routing incoming peers by info hash. |
| `internal/engine` | ✅ | `Engine`: torrent table (IDs, info-hash dedup), shared Listener + peer ID, resume store, download queue (`MaxActive`), per-torrent run goroutine (magnet: fetch metadata → open storage → restore/recheck → Session + Saver → final save), Add/AddMagnet/Pause/Resume/Remove(+delete files)/List/Get/Files/Peers/Overview, `SetLimits`, `SetFilesWanted` (D44), seeding controls in `seeding.go` (`SetUploadsPaused`, `SetSeedMode`, `SetSeedLimits`, ratio, per-run seeding watcher, finished state; D45), automatic retry of failed torrents (`retry.go`, D47), one `torrent.Pacer` for all sessions, an optional `portmap.Mapper` for the peer (TCP) and DHT (UDP) ports (D49). `commands.go`, `view.go`, `magnet.go`. |
| `internal/resume` | ✅ | `Store` (atomic save/load/list/delete of `<hash>.json` + `<hash>.torrent`), `Restore` (trust rules, partial recheck), `Saver` (periodic/no-op-skipping saves with lifetime totals). |
| `internal/ipc` | ✅ | Daemon file (`Info`, atomic 0600 write), `NewHandler`/`Serve` (loopback HTTP API over an `*engine.Engine`, bearer-token auth, error→status mapping), `Client` (+ `Connect`, stale-file detection). |
| `internal/config` | ✅ | `Config` + `Defaults(home)`, the settings table (`Settings`, `Lookup`, `Get`/`Set` with validation), `Load`/`Parse` (strict, line numbers), in-place `SetInFile`/`UnsetInFile`, `Init`, `KeysInFile`, rate syntax (`ParseRate`/`FormatRate`). |
| `internal/ratelimit` | ✅ | `Limiter`: token bucket shared by many goroutines; `WaitN(ctx, n)` as a first-come-first-served reservation (D46), live `SetRate`; nil/0 = unlimited. |
| `internal/dht` | ✅ | BEP 5 DHT node (D42): `Server` (UDP socket, reader goroutine answering ping/find_node/get_peers/announce_peer, queries matched by tid **and** source address, per-second query cap, verify-before-adding), iterative `lookup` (α=4, K=8), `GetPeers(ctx, hash, port)` (+ announce), routing table (160 k-buckets, replace bad/failing nodes), tokens (rotating secret), peer store (30-min TTL, caps), `dht.json` state (ID + ≤ 200 good nodes). IPv4 only. |
| `internal/portmap` | ✅ | Router port forwarding (D49): `Mapper` (`Start(Config)`, `Status`, `External`, `Close` removes the mappings) runs one goroutine: find a router, map, renew at half the lease, look again every 10 min if none answered or it refused. NAT-PMP client (`natpmp.go`, RFC 6886), UPnP IGD client (`upnp.go`: SSDP search, device description, SOAP Add/Delete/GetExternalIPAddress), default gateway lookup (`gateway*.go`: `/proc/net/route` on Linux, `route -n get default` on macOS, else first host of our subnet). |
| `internal/compact` | ✅ | Compact peer addresses (6/18 bytes) shared by `tracker`, `peer` (PEX) and `dht`. |
| `internal/cli` | ✅ | Every command. `cli.go` (dispatch, usage, `$DENCO_HOME`), `daemon.go` (daemon, stop), `client.go` (add/list/info/files/peers/pause/resume/remove/status), `render.go` (tables/detail views), `files.go` (`files` + skip/want, file-number lists), `logfile.go` (daemon log file with rotation, D48), plus standalone `inspect.go` (inspect, magnet), `announce.go`, `probe.go`, `verify.go`, `download.go`. |

Dependency direction (lower never imports higher):
`bencode → metainfo/magnet → tracker/peer → pieces/storage → torrent → engine → ipc → cli → cmd`;
`ratelimit` (stdlib only) is used by `peer`, `torrent`, `engine`; `config` by `cli`;
`compact` by `tracker`, `peer`, `dht`; `dht` (uses `bencode`, `compact`, `metainfo`) by `engine` and
`config` (default routers) — sessions and fetchers see it only through small interfaces
(`torrent.PeerFinder`, `metadata.PeerFinder`);
`metadata` (uses `peer`, `tracker`) by `torrent` (serving) and `engine` (fetching);
`portmap` (stdlib only) by `engine` (and `cli` for its status type).

## Key data types (implemented)

- `bencode.Decode([]byte) (any, error)` — values are `int64`, `string`
  (raw bytes), `[]any`, `map[string]any`.
- `bencode.DecodePrefix([]byte) (any, int, error)` — decode one value, return
  bytes consumed (needed later for ut_metadata messages: dict + trailing data).
- `bencode.DictSpans([]byte) (map[string][]byte, error)` — raw encoded bytes of
  each top-level dict value. `InfoHash = sha1(spans["info"])`.
- `metainfo.MetaInfo{Announce, AnnounceList, Comment, CreatedBy, CreationDate,
  InfoHash [20]byte, Info, InfoBytes}`
- `metainfo.Info{Name, PieceLength, Pieces [][20]byte, Files []File,
  TotalLength, Private, MultiFile}` — single-file torrents are normalised to one
  `File` so downstream code never branches on single vs multi.
- `metainfo.File{Path []string, Length, Offset}` — `Path` excludes the torrent
  name; `Offset` is the byte offset within the concatenated torrent data.

- `magnet.Magnet{InfoHash, Name, Trackers []string, Peers []netip.AddrPort}`
- `tracker.Request{InfoHash, PeerID, Port, Uploaded, Downloaded, Left, Event,
  NumWant, Key, TrackerID}` → `tracker.Response{Interval, MinInterval,
  TrackerID, Seeders, Leechers (-1 = unknown), Peers []netip.AddrPort, Warning}`
- `tracker.Announcer` interface — one announce; `*Client` implements it, tests
  use fakes. `List.AnnounceAll(ctx, announcer, req) []Result` (one per tier).
- Peers everywhere are `netip.AddrPort` (D12).
- `peer.Conn{Remote, Peer Handshake, Outgoing}` with `Incoming() <-chan *Message`,
  `Send(*Message) error`, `Done()`, `Err()`, `Close()`, `BytesRead/Written()`.
  `peer.Dial(ctx, addr, infoHash, Options)`, `peer.Accept(netConn, known, Options)`.
- `peer.Message{ID, Payload}`; nil = keep-alive. `peer.Block{Index, Begin, Length}`.
- `bitfield.Bitfield` — `New(n)`, `FromBytes(b, n)`, `Has/Set/Clear/SetAll/Count/Full/Bytes/Clone`.
- `storage.NewLayout(dir, *Info) → *Layout{Root, Files []File{Path, Rel, Length, Offset, Renamed}}`;
  `storage.Open(layout, Options{AllowExisting, ReadOnly, MaxOpenFiles}) → *Storage`
  (`io.ReaderAt`/`io.WriterAt` over torrent offsets, `Sync`, `Close`, `DeleteFiles`).
- `pieces.Wanted(info, skip []bool) *Bitfield` (nil = all) — pieces overlapping a
  wanted file; `pieces.SkipSet(indexes, n)`. `Bitfield.Covers(other)` (nil = all).
- `pieces.Manager` (owned by one goroutine): `SetWanted(want) (abandoned)`,
  `Complete()` (all wanted verified) vs `HaveAll()`, `BytesLeft()` (wanted) /
  `MissingBytes()` (all, for trackers) / `WantedBytes()`, `PeerBitfield/PeerGoneBitfield/PeerHave`
  (availability), `Interesting`, `NextRequests(peer, peerHas, n) []peer.Block`,
  `BlockReceived(peer, index, begin, data) Received{Accepted, Cancel, Complete,
  Data, Contributors}`, `PieceVerified(i, ok)`, `PeerRequestsDropped(peer)`.
  Piece lifecycle: missing → active → pending (hashing) → have / back to missing.

- `torrent.New(mi, storage, have, Config) *Session`; `Run(ctx) error` (blocks
  until ctx is cancelled, a fatal `ErrStorage`, or — with `NoSeed` — completion);
  `Completed() <-chan struct{}`; `AddPeers([]netip.AddrPort)`;
  `SetUploadsPaused(bool)`; `InfoHash()`; `Stats() Stats` (state
  downloading/stalled/seeding/complete, sizes, up/down rates, ETA,
  peers/seeds/leechers, tracker counts/errors); `Peers() []PeerInfo` (incl.
  per-peer up/down, choke/interest both ways).
- `torrent.Listen(addr, peer.Options, log) *Listener`; `Add/Remove(*Session)`,
  `Port()`, `Addr()`, `Close()`.
- `Session.Have()` — copy of verified pieces (all already written to storage).
- `torrent.Config.Wanted`, `Session.SetWanted(want)` (any goroutine); `Completed()` closes
  when the selection is done; trackers get `completed` only for a full seed;
  `Stats.WantedBytes`.
- `engine.AddOptions.Skip []int`, `Engine.SetFilesWanted(ref, files, wanted) ([]File, error)`
  (`ErrBadFile`); `Torrent.WantedBytes/SkippedFiles`, `File.Skipped`;
  `resume.Saver.SetSkip`; `storage.Options.Skipped`.
- Robustness (D46–D48): `torrent.NewPacer(lim)`, `torrent.Config.Pacer`;
  `pieces.Manager.SetEndgame(bool)`; `engine.Config.RetryBase/RetryMax`,
  `Torrent.RetryAt/Retries`; `config.Config.LogFile` (`log_file`).
- Port forwarding (D49): `portmap.Start(Config{Ports, Gateway, NATPMPPort,
  SSDPAddr, NoNATPMP, NoUPnP, Logger}) *Mapper`; `Status()` → `Status{State
  (searching|mapped|not_found|failed), Method, Gateway, ExternalIP,
  Mappings, Err, NextTry}`; `External(proto, internal)`. `engine.Config.
  PortForward/PortMap`; `Overview.PortMap *portmap.Status` (nil = off);
  `torrent.Config.PublicPort func() uint16` (the router's external TCP port,
  told to trackers, the DHT and in the BEP 10 handshake);
  `config.Config.PortForwarding` (`port_forwarding`), `config.DefaultPortForwarding`.
- Seeding (D45): `engine.Config.SeedRatio/SeedTime/SeedCheckEvery`;
  `Engine.SetUploadsPaused(ref, bool)`, `SetSeedMode(ref, "on"|"off"|"default")`
  (`ErrBadSeedMode`), `SetSeedLimits(ratio, d)`; `Resume` → `ErrSeedLimit` for a
  finished torrent a limit would stop again; state `finished`;
  `Torrent.Ratio/SeedingTime/SeedMode/SeedPolicy/SeedNote/UploadsPaused`,
  `Overview.SeedRatio/SeedTime`; `resume.Snapshot.SeedingTime`,
  `resume.Saver.Update(func(*State))`; `config.ParseSeedTime/FormatSeedTime`.
- `resume.OpenStore(dir)`; `Store.Save(*State, torrentBytes)`, `Load(hash)
  (*State, *MetaInfo, error)`, `List() []Entry`, `Delete(hash)`;
  `resume.Restore(ctx, state, mi, layout, readerAt) (*Bitfield, Report, error)`;
  `resume.NewSaver(store, mi, layout, storage, prev)` → `Save(Snapshot, clean)`,
  `Run(ctx, interval, snapshotFn, errFn)`.
- `metainfo.MetaInfo.Raw` — original .torrent bytes (stored with resume data).
- `engine.Open(Config{StateDir, DownloadDir, ListenAddr, MaxActive, SaveEvery,
  Session torrent.Config, Logger}) *Engine`; `Add(raw, AddOptions{Dir,
  Existing, Paused}) (Torrent, error)` (`*DuplicateError`, `storage.ErrExists`,
  `metainfo.ErrInvalid`); `Pause/Resume/Get(ref)`, `Remove(ref, deleteFiles) (Removed{Torrent, FilesDeleted}, error)`,
  `List()`, `Files(ref) []File`, `Peers(ref) []Peer`, `Overview()`, `Close()`.
  `ref` = ID, `#ID` or ≥4-char info-hash prefix (`ErrNotFound`, `ErrAmbiguous`).
- `engine.Torrent` — display snapshot: ID, name, dir, `State` (checking,
  downloading, stalled, seeding, queued, paused, error), sizes, pieces,
  rates, ETA, lifetime totals, peer/seed/leecher counts, tracker info, times.
- `ipc.Client` methods mirror the engine; `ipc.Connect(ctx, daemonFile)`.
- `engine.AddMagnet(uri, AddOptions)`; `Torrent.HasMetadata`, `MetaHave/MetaTotal`;
  `engine.ErrNoMetadata`; state `metadata`.
- `peer.ExtHandshake{M, MetadataSize, Version, Port, ReqQ}`, `Encode`/`ParseExtHandshake`.
- `metadata.NewFetcher(hash, tiers, peers, Config) → Run(ctx) ([]byte, error)`, `Status()`, `Peers()`.
- `engine.Config.DownloadLimit/UploadLimit` (bytes/s) and `Engine.SetLimits(down, up)`;
  `peer.Options.ReadLimiter`, `torrent.Config.UploadLimiter` (`*ratelimit.Limiter`).
- `config.Load(path, config.Defaults(home)) (Config, error)`; `cfg.Set(key, text)`.
- `dht.Open(Config{Addr, Bootstrap, StateFile, …}) *Server`; `GetPeers(ctx, hash, port)`,
  `AddNode(addr)`, `Port()`, `Stats()`, `Close()` (saves state). `engine.Config.EnableDHT/
  DHTBootstrap`; `torrent.Config.DHT/DHTInterval/DHTRetry`; `metadata.Config.DHT`;
  `engine.Overview.DHT/DHTPort/DHTNodes/DHTGood`, `Torrent.DHTPeers`;
  `config.Config.DHTBootstrap` (`dht_bootstrap`), `config.DefaultDHTBootstrap`.
- `peer.PEX{Added, AddedFlags, Dropped}`, `Encode`/`ParsePEX`, flags `PEXSeed`,
  `PEXReachable`…, `MaxPEXPeers` (50). `torrent.Config.PEX`/`PEXInterval`,
  `metadata.Config.PEX`, `engine.Overview.PEX` (`enable_pex` → daemon →
  `engine.Config.Session.PEX`).

## Data flow for a magnet link (D38)

```
AddMagnet → state {magnet, fresh} saved (no .torrent) → run goroutine:
  metadata.Fetcher: announce(left=1) + x.pe → dial ≤10 peers → BEP 10 handshake
    → peer has metadata_size → request its 16 KiB pieces → that peer's Assembler → SHA-1 == info hash?
       no  → ban that peer (others keep going)
       yes → .torrent = {info, announce-list from magnet} → metainfo.Parse → save
  → (same as a .torrent) prepare storage → Session (+ peers the fetcher found)
```

## Data flow for a downloaded block

```
peer.Conn.Incoming() ─piece msg─▶ session ─▶ Manager.BlockReceived
                                              │ Complete?
                                              ▼
                       hash worker: pieces.Check(data, info.Pieces[i])
                         ok ─▶ Storage.WriteAt ─▶ Manager.PieceVerified(i, true) ─▶ send Have to peers
                         bad ─▶ Manager.PieceVerified(i, false) ─▶ blame Contributors
```

## Concurrency model

- **peer.Conn** (D17): 2 goroutines per connection. Reader → `Incoming()`
  (buffered 64, closed on shutdown). `Send()` → buffered queue → writer
  (batches, flushes, keep-alive every 90s idle). First error or `Close()`
  shuts down once; peer silent > 2 min → `ErrIdle` (deadline re-armed per
  socket read, so our own throttling never counts as silence, D46). Handshake bounded by
  10s and by the caller's context.
- **tracker.List.AnnounceAll**: one goroutine per tier, joined before return.
- **storage.Storage**: safe for concurrent use; one mutex per torrent (D21).
- **torrent.Session** (D23): one loop goroutine owns all state. Helpers →
  `events` channel: per-peer forwarder + per-peer uploader (disk reads) (+ the
  Conn's own 2 goroutines), dialers (≤ DialParallel), announcer (1), hash/write
  workers (process-wide limit NumCPU). Loop sends only via `TrySend`.
  `Stats()`/`Peers()` read a snapshot under a mutex. `Run` waits for every
  helper before returning.
- **torrent.Listener**: 1 accept goroutine + ≤ 64 handshake goroutines; hands
  finished handshakes to a session via a mutex-guarded queue + wake event.
- **engine.Engine** (D29): one mutex guards the torrent table and entry
  fields; methods are called concurrently by IPC handlers. Per started
  torrent: 1 run goroutine (owns storage, Session, Saver) + 2 helpers
  (periodic saver, completion watcher), joined before the run exits.
  `Close` cancels all runs and waits. Lock order: engine mutex → session
  snapshot mutex (sessions never call into the engine).
- **ipc**: `net/http` server goroutines call the engine; `Serve` binds
  127.0.0.1 only.
- **Rate limiting (D36):** two engine-wide `ratelimit.Limiter`s. Each
  `peer.Conn` reader calls `WaitN` after every socket read (download);
  each per-peer uploader calls `WaitN` before each block read (upload),
  with a per-peer context so a disconnect ends the wait. `Conn` gained a
  context cancelled on close for the same reason.
- **portmap.Mapper** (D49): 1 goroutine per engine (when on): discover →
  map → sleep until half the lease → renew; `Close` (from the engine's
  `closeShared`) cancels it and waits while it deletes the mappings
  (≤ 2 s). `Status`/`External` read a snapshot under a mutex; sessions call
  `External` through `torrent.Config.PublicPort` (no engine lock).
- **pieces.Manager**: not thread-safe; owned by the session goroutine.
  `pieces.Recheck` uses up to 4 worker goroutines internally.

## On-disk formats

- **Downloaded data:** `<download dir>/<name>[/<path…>]`, names sanitised per
  D20. Files grow sparsely as pieces arrive (no preallocation). A skipped
  file can still appear, holding only the bytes of a border piece shared
  with a wanted file (D44).
- **Resume data (D27):** `<state dir>/<infohash>.json` + `<infohash>.torrent`.
  Default state dir `os.UserConfigDir()/denco/resume`. JSON fields: `version`,
  `info_hash`, `name`, `download_dir`, `pieces`, `have` (base64 bitfield),
  `magnet` (only while a magnet has no metadata; then there is no `.torrent`),
  `uploaded`, `downloaded`, `added_at`, `completed_at`, `saved_at`, `clean`,
  `files` ([{`size`, `mtime_ns`}] in torrent order), and for the daemon
  (all optional): `id`, `paused`, `fresh` (never started: files at its paths
  aren't ours, D32), `existing` (reuse data, recheck on first start), `skip`
  (indexes of files not to download, D44), `uploads_paused`, `seed` ("on" /
  "off"; absent = global limits), `finished` (done seeding; not restarted
  on its own), `seed_seconds` (lifetime seeding time) (D45).
- **DHT state (D42):** `<state dir>/dht.json` = `{"id": "<40 hex>", "nodes":
  [{"id": "<40 hex>", "addr": "ip:port"}, …]}` (≤ 200 good nodes), written
  atomically every 15 min and on close. Missing/corrupt = fresh ID, no nodes.
- **Daemon file:** `$DENCO_HOME/daemon.json` (0600):
  `{"addr":"127.0.0.1:<port>","token":"<64 hex>","pid":N}`. Written after the
  API is listening, removed after a clean shutdown (only if still ours).
- **Config file (D35):** `$DENCO_HOME/config.yaml`:
  ```
  # comment
  download_dir: ~/Downloads/Denco
  upload_limit: 500K        # trailing comment
  resume_dir: "/path/with #hash"
  ```
  One `key: value` per line; keys from `config.Settings()`; strict.

## Wire / IPC formats

- **Tracker announce (outgoing):** `GET <announce>?[existing&]info_hash=…&peer_id=…
  &port=&uploaded=&downloaded=&left=&compact=1&numwant=50&key=<hex>[&event=]
  [&trackerid=]`. Raw-byte params percent-encoded byte-wise (D15).
- **Tracker response (incoming):** bencoded dict; `peers` as compact string
  (6 bytes/peer) or list of dicts; optional `peers6` (18 bytes/peer).
- **UDP tracker (BEP 15):** big-endian binary datagrams. connect `[0x41727101980
  u64][0 u32][tx u32]` → `[0][tx][conn_id u64]`; announce (98 bytes + BEP 41
  options `[0x2][len][data]…`) → `[1][tx][interval][leechers][seeders][peers…]`;
  error `[3][tx][message]`. Event codes: 0 none, 1 completed, 2 started, 3 stopped.
- **Peer handshake:** `<19>"BitTorrent protocol"<8 reserved><20 info hash><20 peer id>`
  (68 bytes). Reserved bits we use: `[5]&0x10` extensions (BEP 10), `[7]&0x01` DHT.
- **Peer messages:** `<uint32 BE length><uint8 id><payload>`; length 0 =
  keep-alive. IDs 0–9 (BEP 3), 20 extended (BEP 10). Max 2 MiB incoming.
- **Extended messages (BEP 10):** `<20><ext id><body>`. ID 0 = handshake,
  bencoded `{"m": {"ut_metadata": 1[, "ut_pex": 2]}, "metadata_size": N, "v": "Denco 0.0.1",
  "p": port, "reqq": 256}`. Other IDs are the *receiver's* numbers from its `m`.
- **ut_metadata (BEP 9):** body = bencoded `{"msg_type": 0|1|2, "piece": i}`
  (+ `"total_size"` for data, followed directly by the piece bytes); pieces
  are 16 KiB, the last shorter.
- **ut_pex (BEP 11):** body = bencoded `{"added": <6·n bytes>, "added.f":
  <n flag bytes>, "dropped": <6·m bytes>[, "added6", "added6.f",
  "dropped6"]}` (compact IPv4 / 18-byte IPv6 entries). Flags: 0x01
  encryption, 0x02 seed, 0x04 uTP, 0x08 holepunch, 0x10 reachable. Denco
  sends ≤ 50 added + ≤ 50 dropped per message, first message right after
  the peer's extension handshake, then ≤ 1 per minute.
- **DHT / KRPC (BEP 5):** one bencoded dict per UDP datagram (IPv4).
  Query `{"t": tid(2 bytes), "y": "q", "q": method, "a": {"id": 20 bytes, …}}`,
  response `{"t", "y": "r", "r": {"id", …}}`, error `{"t", "y": "e", "e": [code, msg]}`
  (201 generic, 202 server, 203 protocol, 204 unknown method). Methods:
  `ping`; `find_node{target}` → `nodes`; `get_peers{info_hash}` → `token`
  + `values` (list of 6-byte peers, ≤ 50) and/or `nodes`;
  `announce_peer{info_hash, port, token, implied_port}`. Compact node info =
  20-byte ID + 6-byte IPv4 address. Tokens: SHA-1(secret ‖ IP)[:8], secret
  rotated every 5 min, previous one accepted.
- **Peer wire PORT (BEP 5):** id 9, 2-byte DHT port; sent after the
  bitfield/extension handshake to peers that set the DHT reserved bit.
- **NAT-PMP (RFC 6886):** UDP to gateway:5351. External address `[0][0]` →
  `[0][128][result u16][epoch u32][ip 4]`; map `[0][1=UDP|2=TCP][0 u16]
  [internal u16][suggested external u16][lifetime u32]` → `[0][128+op]
  [result][epoch][internal][external][lifetime]`; lifetime 0 deletes. We ask
  7200 s; sent again after 250 ms, 500 ms, 1 s.
- **UPnP IGD:** SSDP `M-SEARCH * HTTP/1.1` to 239.255.255.250:1900 (ST
  InternetGatewayDevice:1/:2, WANIPConnection:1) → `LOCATION` of the device
  description XML → `WANIPConnection:2/1` or `WANPPPConnection:1`
  `controlURL` → SOAP POST (`SOAPAction: "<service>#<action>"`):
  `AddPortMapping` (lease 3600 s; 0 if the router answers 725, or 501 —
  D50), `Connection: close` on every call,
  `DeletePortMapping`, `GetExternalIPAddress`. Faults: `UPnPError/errorCode`
  (718 conflict → retry with a random port ≥ 49152). Only HTTP to the IP that
  answered the search; no redirects; bodies ≤ 256 KiB.
- **Daemon API (D30):** HTTP/1.1 JSON on 127.0.0.1, header
  `Authorization: Bearer <token>`. `GET /v1/status`, `GET|POST /v1/torrents`
  (POST body `{"torrent":"<base64 .torrent>" | "magnet":"magnet:?…","dir","existing","paused","skip":[i…]}`),
  `GET /v1/torrents/{ref}[/files|/peers]`, `POST /v1/torrents/{ref}/files {"files":[i…],"wanted":bool}`
  (→ files), `POST /v1/torrents/{ref}/uploads {"paused":bool}`,
  `POST /v1/torrents/{ref}/seed {"mode":"on|off|default"}` (→ torrent),
  `POST /v1/seed-limits {"ratio":float,"time":ns}` (→ status), `POST /v1/torrents/{ref}/pause|resume`,
  `POST /v1/torrents/{ref}/remove {"delete_files":bool}` (→ torrent + `files_deleted`),
  `POST /v1/limits {"download":B/s,"upload":B/s}` (→ status), `POST /v1/shutdown`.
  Status now includes `max_peers`, `download_limit`, `upload_limit`, and
  `port_forwarding` (`portmap.Status`, absent when off).
  Errors: `{"error":"…","code":"not_found|ambiguous|duplicate|files_exist|invalid|busy|no_metadata|seed_limit|unauthorized|internal","id":N}`
  with 404/400/409/401/500. Full list in `internal/ipc/ipc.go`.
