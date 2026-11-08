// Package ipc connects the short-lived CLI to the long-running daemon:
// a small JSON API over HTTP, bound to 127.0.0.1 only, protected by a
// random token (DECISIONS D3, D30).
//
// The daemon writes its address and token to a "daemon file"
// (<denco home>/daemon.json, mode 0600). Clients read that file to find
// and authenticate to the daemon, so only the user who started it can
// control it.
//
// Files:
//
//	ipc.go    – the daemon file and shared wire types
//	server.go – HTTP handlers wrapping an *engine.Engine
//	client.go – Client used by the CLI
//
// API (all JSON; every request needs "Authorization: Bearer <token>"):
//
//	GET  /v1/status                  → Status
//	GET  /v1/torrents                → []engine.Torrent
//	POST /v1/torrents                AddRequest → engine.Torrent
//	GET  /v1/torrents/{ref}          → engine.Torrent
//	GET  /v1/torrents/{ref}/files    → []engine.File
//	POST /v1/torrents/{ref}/files    FilesRequest → []engine.File
//	GET  /v1/torrents/{ref}/peers    → []engine.Peer
//	POST /v1/torrents/{ref}/pause    → engine.Torrent
//	POST /v1/torrents/{ref}/resume   → engine.Torrent
//	POST /v1/torrents/{ref}/remove   RemoveRequest → engine.Removed
//	POST /v1/limits                  LimitsRequest → Status
//	POST /v1/shutdown                → {}
//
// {ref} is a torrent ID or an info hash prefix. Errors are
// {"error": "...", "code": "..."} with a matching HTTP status.
package ipc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/7Eshaan/Denco/internal/engine"
)

// DaemonFile is the daemon file's name inside the denco home directory.
const DaemonFile = "daemon.json"

// Info is the content of the daemon file.
type Info struct {
	Addr  string `json:"addr"`  // e.g. "127.0.0.1:53124"
	Token string `json:"token"` // 64 hex chars
	PID   int    `json:"pid"`
}

// NewToken returns a random 256-bit token as hex.
func NewToken() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("ipc: crypto/rand failed: %v", err))
	}
	return hex.EncodeToString(b)
}

// WriteInfo writes the daemon file atomically with owner-only permissions.
func WriteInfo(path string, info Info) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("ipc: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-daemon-*")
	if err != nil {
		return fmt.Errorf("ipc: %w", err)
	}
	defer os.Remove(tmp.Name()) // no-op after a successful rename
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return fmt.Errorf("ipc: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("ipc: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("ipc: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("ipc: %w", err)
	}
	return nil
}

// ErrNoDaemon means no daemon file exists (the daemon isn't running).
var ErrNoDaemon = errors.New("the denco daemon is not running")

// ReadInfo reads the daemon file.
func ReadInfo(path string) (Info, error) {
	var info Info
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return info, ErrNoDaemon
	}
	if err != nil {
		return info, fmt.Errorf("ipc: %w", err)
	}
	if err := json.Unmarshal(data, &info); err != nil || info.Addr == "" || info.Token == "" {
		return info, fmt.Errorf("ipc: %s is corrupt", path)
	}
	return info, nil
}

// AddRequest adds one torrent: either a .torrent file's bytes (Torrent,
// base64 in JSON — the daemon never reads the client's paths) or a magnet
// link (Magnet).
type AddRequest struct {
	Torrent  []byte `json:"torrent,omitempty"`
	Magnet   string `json:"magnet,omitempty"`
	Dir      string `json:"dir,omitempty"` // absolute; "" = daemon default
	Existing bool   `json:"existing,omitempty"`
	Paused   bool   `json:"paused,omitempty"`
	Skip     []int  `json:"skip,omitempty"` // file indexes (from 0) not to download; .torrent only
}

// FilesRequest marks files (indexes from 0) as wanted or skipped.
type FilesRequest struct {
	Files  []int `json:"files"`
	Wanted bool  `json:"wanted"`
}

// RemoveRequest removes a torrent.
type RemoveRequest struct {
	DeleteFiles bool `json:"delete_files"`
}

// LimitsRequest sets the total speed limits (bytes/s, 0 = unlimited).
type LimitsRequest struct {
	Download int64 `json:"download"`
	Upload   int64 `json:"upload"`
}

// Status is the daemon's overall status.
type Status struct {
	engine.Overview
	PID int `json:"pid"`
}

// Error codes, so clients can branch without parsing messages.
const (
	CodeNotFound  = "not_found"
	CodeAmbiguous = "ambiguous"
	CodeDuplicate = "duplicate"
	CodeExists    = "files_exist"
	CodeInvalid   = "invalid"
	CodeBusy      = "busy"
	CodeNoMeta    = "no_metadata"
	CodeAuth      = "unauthorized"
	CodeInternal  = "internal"
)

// Error is an error reported by the daemon.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"error"`
	ID      int    `json:"id,omitempty"` // for CodeDuplicate: the existing torrent
}

func (e *Error) Error() string { return e.Message }
