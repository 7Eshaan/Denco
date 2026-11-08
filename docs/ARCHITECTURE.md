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
                                                                                  └─ DHT node (UDP, Phase 11)
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
| `internal/peer` | ✅ | Peer wire protocol: `Handshake`, `Message` codec + typed constructors/parsers, `Conn` (Dial/Accept, reader + writer goroutines, keep-alive, idle timeout, optional read limiter), BEP 10 `ExtHandshake` (`extension.go`). |
| `internal/metadata` | ✅ | BEP 9: `Message` encode/decode, `Piece`, `Assembler` (SHA-1 check, reset on mismatch), `Fetcher` (announce + up to 10 peers, each assembling its own full copy; bans a peer whose copy fails the hash; `Status`, `Peers`). |
| `internal/bitfield` | ✅ | Fixed-size piece set in wire format (high bit = piece 0); validated `FromBytes`. |
| `internal/storage` | ✅ | `Layout` (OS-safe paths, sanitising, collisions, containment) and `Storage` (ReadAt/WriteAt across files, handle LRU, no-overwrite, ReadOnly, safe DeleteFiles). |
| `internal/pieces` | ✅ | `Check`/`VerifyPiece`/`Recheck` (parallel hashing) and `Manager` (rarest-first, random-first, endgame, memory budget, block ownership). |
| `internal/torrent` | ✅ | `Session`: event loop owning Manager + peers + swarm; dialing/backoff/IP bans, request pipelining, snub detection, hash+write workers, announce loop, uploading (validation, cancel, per-peer uploader), tit-for-tat choker + optimistic unchoke, seeding, pause uploads, Stats/Peers. `Listener`: one TCP port routing incoming peers by info hash. |
| `internal/engine` | ✅ | `Engine`: torrent table (IDs, info-hash dedup), shared Listener + peer ID, resume store, download queue (`MaxActive`), per-torrent run goroutine (magnet: fetch metadata → open storage → restore/recheck → Session + Saver → final save), Add/AddMagnet/Pause/Resume/Remove(+delete files)/List/Get/Files/Peers/Overview, `SetLimits`. `commands.go`, `view.go`, `magnet.go`. |
| `internal/resume` | ✅ | `Store` (atomic save/load/list/delete of `<hash>.json` + `<hash>.torrent`), `Restore` (trust rules, partial recheck), `Saver` (periodic/no-op-skipping saves with lifetime totals). |
| `internal/ipc` | ✅ | Daemon file (`Info`, atomic 0600 write), `NewHandler`/`Serve` (loopback HTTP API over an `*engine.Engine`, bearer-token auth, error→status mapping), `Client` (+ `Connect`, stale-file detection). |
| `internal/config` | ✅ | `Config` + `Defaults(home)`, the settings table (`Settings`, `Lookup`, `Get`/`Set` with validation), `Load`/`Parse` (strict, line numbers), in-place `SetInFile`/`UnsetInFile`, `Init`, `KeysInFile`, rate syntax (`ParseRate`/`FormatRate`). |
| `internal/ratelimit` | ✅ | `Limiter`: token bucket shared by many goroutines; `WaitN(ctx, n)`, live `SetRate`; nil/0 = unlimited. |
| `internal/dht` | ⏳ | BEP 5 DHT node. |
| `internal/cli` | ✅ | Every command. `cli.go` (dispatch, usage, `$DENCO_HOME`), `daemon.go` (daemon, stop), `client.go` (add/list/info/files/peers/pause/resume/remove/status), `render.go` (tables/detail views), plus standalone `inspect.go` (inspect, magnet), `announce.go`, `probe.go`, `verify.go`, `download.go`. |

Dependency direction (lower never imports higher):
`bencode → metainfo/magnet → tracker/peer → pieces/storage → torrent → engine → ipc → cli → cmd`;
`ratelimit` (stdlib only) is used by `peer`, `torrent`, `engine`; `config` by `cli`;
`metadata` (uses `peer`, `tracker`) by `torrent` (serving) and `engine` (fetching).

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
- `pieces.Manager` (owned by one goroutine): `PeerBitfield/PeerGoneBitfield/PeerHave`
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
  shuts down once; peer silent > 2 min → `ErrIdle`. Handshake bounded by
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
- **pieces.Manager**: not thread-safe; owned by the session goroutine.
  `pieces.Recheck` uses up to 4 worker goroutines internally.

## On-disk formats

- **Downloaded data:** `<download dir>/<name>[/<path…>]`, names sanitised per
  D20. Files grow sparsely as pieces arrive (no preallocation).
- **Resume data (D27):** `<state dir>/<infohash>.json` + `<infohash>.torrent`.
  Default state dir `os.UserConfigDir()/denco/resume`. JSON fields: `version`,
  `info_hash`, `name`, `download_dir`, `pieces`, `have` (base64 bitfield),
  `magnet` (only while a magnet has no metadata; then there is no `.torrent`),
  `uploaded`, `downloaded`, `added_at`, `completed_at`, `saved_at`, `clean`,
  `files` ([{`size`, `mtime_ns`}] in torrent order), and for the daemon
  (all optional): `id`, `paused`, `fresh` (never started: files at its paths
  aren't ours, D32), `existing` (reuse data, recheck on first start).
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
  bencoded `{"m": {"ut_metadata": 1}, "metadata_size": N, "v": "Denco 0.0.1",
  "p": port, "reqq": 256}`. Other IDs are the *receiver's* numbers from its `m`.
- **ut_metadata (BEP 9):** body = bencoded `{"msg_type": 0|1|2, "piece": i}`
  (+ `"total_size"` for data, followed directly by the piece bytes); pieces
  are 16 KiB, the last shorter.
- **Daemon API (D30):** HTTP/1.1 JSON on 127.0.0.1, header
  `Authorization: Bearer <token>`. `GET /v1/status`, `GET|POST /v1/torrents`
  (POST body `{"torrent":"<base64 .torrent>" | "magnet":"magnet:?…","dir","existing","paused"}`),
  `GET /v1/torrents/{ref}[/files|/peers]`, `POST /v1/torrents/{ref}/pause|resume`,
  `POST /v1/torrents/{ref}/remove {"delete_files":bool}` (→ torrent + `files_deleted`),
  `POST /v1/limits {"download":B/s,"upload":B/s}` (→ status), `POST /v1/shutdown`.
  Status now includes `max_peers`, `download_limit`, `upload_limit`.
  Errors: `{"error":"…","code":"not_found|ambiguous|duplicate|files_exist|invalid|busy|unauthorized|internal","id":N}`
  with 404/400/409/401/500. Full list in `internal/ipc/ipc.go`.
