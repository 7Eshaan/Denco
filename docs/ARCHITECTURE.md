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
| `cmd/denco` | ✅ (dev) | Entry point. One file per command: `inspect.go` (inspect, magnet), `announce.go`. Replaced by `internal/cli` in Phase 8. |
| `internal/bencode` | ✅ | Bencode decode/encode; raw byte spans of dict values (for info hash). |
| `internal/metainfo` | ✅ | Parse + validate `.torrent`; info hash; file list with offsets; tracker tiers. |
| `internal/format` | ✅ | Human-readable sizes, speeds, durations for CLI output. |
| `internal/magnet` | ✅ | Parse/print magnet URIs (xt hex+base32, dn, tr, x.pe). |
| `internal/peerid` | ✅ | Generate our 20-byte peer ID (`-DE0001-…`). |
| `internal/tracker` | ✅ | `Client.Announce` (one HTTP/HTTPS announce) and `List` (BEP 12 tiers, concurrent, fallback + promote). UDP client (BEP 15) ⏳ Phase 5b. |
| `internal/peer` | ⏳ | Peer wire protocol: handshake, messages, connection loops. |
| `internal/bitfield` | ⏳ | Compact piece-set type shared by peer/pieces/resume. |
| `internal/storage` | ⏳ | Map pieces onto files, safe paths, read/write blocks. |
| `internal/pieces` | ⏳ | Piece state, rarest-first picker, block bookkeeping, verification. |
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

## On-disk formats

- None yet. (Resume data, config, IPC token: Phases 7–9.)

## Wire / IPC formats

- **Tracker announce (outgoing):** `GET <announce>?[existing&]info_hash=…&peer_id=…
  &port=&uploaded=&downloaded=&left=&compact=1&numwant=50&key=<hex>[&event=]
  [&trackerid=]`. Raw-byte params percent-encoded byte-wise (D15).
- **Tracker response (incoming):** bencoded dict; `peers` as compact string
  (6 bytes/peer) or list of dicts; optional `peers6` (18 bytes/peer).
