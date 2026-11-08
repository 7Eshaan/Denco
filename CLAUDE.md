# Denco — BitTorrent client in Go

Denco is a CLI BitTorrent client (daemon + thin CLI) written in Go, intended as a
daily-driver client and a portfolio project. Targets macOS, Linux and Windows.

## Start of every session (do this first)

1. Read `docs/PROGRESS.md` — current phase, the exact next action, open issues.
2. Skim `docs/ROADMAP.md` for the phase plan and `docs/ARCHITECTURE.md` for the
   package map. Check `docs/DECISIONS.md` before re-deciding anything.
3. Run `git status` and `git log --oneline -10` to see what the user committed.
4. Run the check commands below to confirm the tree is green before changing it.

## End of every phase / session

- Update `docs/PROGRESS.md` (state, next action, test results — truthfully).
- Add an entry to `docs/DECISIONS.md` for every non-obvious choice.
- Update `docs/ARCHITECTURE.md` if packages, data flow or formats changed.
- Tick the phase in `docs/ROADMAP.md`.
- Tell the user it is ready to commit and suggest a commit message.
  **Never run `git add`, `git commit` or `git push`** — the user does those.

## Hard constraints

- **Dependencies:** standard library only for the engine (bencode, metainfo,
  magnet, tracker, peer wire, pieces, storage, DHT) and the daemon IPC.
  Third-party packages only for config parsing or CLI ergonomics, and only after
  the user explicitly approves. Currently: **zero** external deps.
- **Cross-platform:** no cgo, no Unix-only syscalls outside `_unix.go`/`_windows.go`
  build-tagged files. Use `filepath` for OS paths. CI-style check: cross-compile
  for linux/windows/darwin.
- **Safety:** torrent-supplied paths must never escape the download directory;
  never overwrite unrelated existing files.
- **Honesty:** report test failures as they are. Do not mark a phase done unless
  every test passes.

## Commands

```sh
go build ./...                      # build everything
go test ./...                       # all tests
go test -race ./...                 # race detector (run before finishing a phase)
go vet ./...                        # static checks
gofmt -l .                          # must print nothing
GOOS=linux   go build ./...         # cross-compile checks
GOOS=windows go build ./...
go run ./cmd/denco <command>        # run the CLI
```

## Code conventions

- Module path: `github.com/7Eshaan/Denco`. Binary: `denco` (`cmd/denco`).
- All non-main code lives under `internal/`. One package = one responsibility;
  see `docs/ARCHITECTURE.md`.
- Each package has a `doc.go`-style package comment explaining its role.
- Errors: wrap with context (`fmt.Errorf("tracker announce: %w", err)`); export
  sentinel errors (`var ErrX = errors.New(...)`) only when callers branch on them.
- Concurrency: pass `context.Context` as the first argument to anything that
  blocks on the network; every goroutine must have a clear owner that stops it.
- Tests sit next to code (`foo_test.go`), table-driven where it helps, no network
  access to the public internet in tests (use local listeners / `httptest`).
- Keep functions short and names plain. Comment *why*, not *what*.
