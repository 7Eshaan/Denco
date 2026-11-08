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
