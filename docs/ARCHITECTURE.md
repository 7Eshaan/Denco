# Architecture

Status markers: ✅ implemented · 🚧 in progress · ⏳ planned.

## Process model

```
 denco add/list/pause/...  ──JSON/HTTP (127.0.0.1 + token)──▶  denco daemon
        (short-lived CLI)                                       │
                                                                ├─ engine: N torrent sessions
                                                                ├─ TCP listener (incoming peers)
                                                                ├─ DHT node (UDP)
                                                                └─ resume store (state dir)
```

`denco inspect <file>` works without a daemon (pure parsing).

## Package map

| Package | Status | Responsibility |
|---|---|---|
| `cmd/denco` | ✅ (dev) | Entry point. One file per command: `inspect.go` (inspect, magnet), `announce.go` (+ shared `loadTarget`/`announceStarted`), `probe.go`, `verify.go`. Replaced by `internal/cli` in Phase 8. |
| `internal/bencode` | ✅ | Bencode decode/encode; raw byte spans of dict values (for info hash). |
| `internal/metainfo` | ✅ | Parse + validate `.torrent`; info hash; file list with offsets; tracker tiers. |
| `internal/format` | ✅ | Human-readable sizes, speeds, durations for CLI output. |
| `internal/magnet` | ✅ | Parse/print magnet URIs (xt hex+base32, dn, tr, x.pe). |
| `internal/peerid` | ✅ | Generate our 20-byte peer ID (`-DN0001-…`); `ClientName` decodes other clients' IDs. |
| `internal/tracker` | ✅ | `Client.Announce` (one HTTP/HTTPS announce) and `List` (BEP 12 tiers, concurrent, fallback + promote). UDP client (BEP 15) ⏳ Phase 5b. |
| `internal/peer` | ✅ | Peer wire protocol: `Handshake`, `Message` codec + typed constructors/parsers, `Conn` (Dial/Accept, reader + writer goroutines, keep-alive, idle timeout). |
| `internal/bitfield` | ✅ | Fixed-size piece set in wire format (high bit = piece 0); validated `FromBytes`. |
| `internal/storage` | ✅ | `Layout` (OS-safe paths, sanitising, collisions, containment) and `Storage` (ReadAt/WriteAt across files, handle LRU, no-overwrite, ReadOnly, safe DeleteFiles). |
| `internal/pieces` | ✅ | `Check`/`VerifyPiece`/`Recheck` (parallel hashing) and `Manager` (rarest-first, random-first, endgame, memory budget, block ownership). |
| `internal/torrent` | ⏳ | One torrent session: ties tracker + peers + pieces + storage. |
| `internal/engine` | ⏳ | Many sessions, limits, queueing, duplicate detection. |
| `internal/resume` | ⏳ | Persist/restore per-torrent state atomically. |
| `internal/ipc` | ⏳ | Daemon HTTP API server + client. |
| `internal/config` | ⏳ | Config file parse, defaults, overrides. |
| `internal/dht` | ⏳ | BEP 5 DHT node. |
| `internal/cli` | ⏳ | Command parsing and table rendering. |

Dependency direction (lower never imports higher):
`bencode → metainfo/magnet → tracker/peer → pieces/storage → torrent → engine → ipc → cli → cmd`.

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

## Data flow for a downloaded block (wired up in Phase 5)

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
- **pieces.Manager**: not thread-safe; owned by the session goroutine.
  `pieces.Recheck` uses up to 4 worker goroutines internally.

## On-disk formats

- **Downloaded data:** `<download dir>/<name>[/<path…>]`, names sanitised per
  D20. Files grow sparsely as pieces arrive (no preallocation).
- Resume data, config, IPC token: Phases 7–9.

## Wire / IPC formats

- **Tracker announce (outgoing):** `GET <announce>?[existing&]info_hash=…&peer_id=…
  &port=&uploaded=&downloaded=&left=&compact=1&numwant=50&key=<hex>[&event=]
  [&trackerid=]`. Raw-byte params percent-encoded byte-wise (D15).
- **Tracker response (incoming):** bencoded dict; `peers` as compact string
  (6 bytes/peer) or list of dicts; optional `peers6` (18 bytes/peer).
- **Peer handshake:** `<19>"BitTorrent protocol"<8 reserved><20 info hash><20 peer id>`
  (68 bytes). Reserved bits we use: `[5]&0x10` extensions (BEP 10), `[7]&0x01` DHT.
- **Peer messages:** `<uint32 BE length><uint8 id><payload>`; length 0 =
  keep-alive. IDs 0–9 (BEP 3), 20 extended (BEP 10). Max 2 MiB incoming.
