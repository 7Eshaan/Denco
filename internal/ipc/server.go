package ipc

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/7Eshaan/Denco/internal/engine"
	"github.com/7Eshaan/Denco/internal/magnet"
	"github.com/7Eshaan/Denco/internal/metainfo"
	"github.com/7Eshaan/Denco/internal/storage"
)

// maxBody caps request bodies. A .torrent for a huge torrent is a few MB
// (20 bytes per piece); 64 MiB is generous.
const maxBody = 64 << 20

// Serve serves h on a random port of 127.0.0.1 (never on other
// interfaces). Shut the returned server down to stop it.
func Serve(h http.Handler, log *slog.Logger) (*http.Server, net.Addr, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, nil, fmt.Errorf("ipc: %w", err)
	}
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
	go srv.Serve(ln)
	return srv, ln.Addr(), nil
}

// NewHandler returns the API handler for e. shutdown is called (once per
// request; the caller makes it idempotent) when a client asks the daemon
// to stop.
func NewHandler(e *engine.Engine, token string, shutdown func(), log *slog.Logger) http.Handler {
	h := &handler{e: e, shutdown: shutdown, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", h.status)
	mux.HandleFunc("GET /v1/torrents", h.list)
	mux.HandleFunc("POST /v1/torrents", h.add)
	mux.HandleFunc("GET /v1/torrents/{ref}", h.get)
	mux.HandleFunc("GET /v1/torrents/{ref}/files", h.files)
	mux.HandleFunc("POST /v1/torrents/{ref}/files", h.setFiles)
	mux.HandleFunc("GET /v1/torrents/{ref}/peers", h.peers)
	mux.HandleFunc("POST /v1/torrents/{ref}/pause", h.pause)
	mux.HandleFunc("POST /v1/torrents/{ref}/resume", h.resume)
	mux.HandleFunc("POST /v1/torrents/{ref}/remove", h.remove)
	mux.HandleFunc("POST /v1/torrents/{ref}/uploads", h.uploads)
	mux.HandleFunc("POST /v1/torrents/{ref}/seed", h.seed)
	mux.HandleFunc("POST /v1/limits", h.limits)
	mux.HandleFunc("POST /v1/seed-limits", h.seedLimits)
	mux.HandleFunc("POST /v1/shutdown", h.stop)
	return requireToken(token, mux)
}

// requireToken rejects requests without the right bearer token. The
// comparison is constant-time so the token can't be guessed byte by byte.
func requireToken(token string, next http.Handler) http.Handler {
	want := []byte("Bearer " + token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			writeErr(w, &Error{Status: http.StatusUnauthorized, Code: CodeAuth, Message: "bad or missing token"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

type handler struct {
	e        *engine.Engine
	shutdown func()
	log      *slog.Logger
}

func (h *handler) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, Status{Overview: h.e.Overview(), PID: os.Getpid()})
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	list := h.e.List()
	if list == nil {
		list = []engine.Torrent{} // "[]", not "null"
	}
	writeJSON(w, list)
}

func (h *handler) add(w http.ResponseWriter, r *http.Request) {
	var req AddRequest
	if !readJSON(w, r, &req) {
		return
	}
	opts := engine.AddOptions{Dir: req.Dir, Existing: req.Existing, Paused: req.Paused, Skip: req.Skip}
	var t engine.Torrent
	var err error
	switch {
	case req.Magnet != "" && len(req.Torrent) > 0:
		err = fmt.Errorf("%w: give a torrent or a magnet link, not both", metainfo.ErrInvalid)
	case req.Magnet != "":
		t, err = h.e.AddMagnet(req.Magnet, opts)
	default:
		t, err = h.e.Add(req.Torrent, opts)
	}
	reply(w, t, err)
}

func (h *handler) get(w http.ResponseWriter, r *http.Request) {
	t, err := h.e.Get(r.PathValue("ref"))
	reply(w, t, err)
}

func (h *handler) files(w http.ResponseWriter, r *http.Request) {
	f, err := h.e.Files(r.PathValue("ref"))
	reply(w, f, err)
}

func (h *handler) setFiles(w http.ResponseWriter, r *http.Request) {
	var req FilesRequest
	if !readJSON(w, r, &req) {
		return
	}
	f, err := h.e.SetFilesWanted(r.PathValue("ref"), req.Files, req.Wanted)
	reply(w, f, err)
}

func (h *handler) peers(w http.ResponseWriter, r *http.Request) {
	p, err := h.e.Peers(r.PathValue("ref"))
	if p == nil {
		p = []engine.Peer{}
	}
	reply(w, p, err)
}

func (h *handler) pause(w http.ResponseWriter, r *http.Request) {
	t, err := h.e.Pause(r.PathValue("ref"))
	reply(w, t, err)
}

func (h *handler) resume(w http.ResponseWriter, r *http.Request) {
	t, err := h.e.Resume(r.PathValue("ref"))
	reply(w, t, err)
}

func (h *handler) remove(w http.ResponseWriter, r *http.Request) {
	var req RemoveRequest
	if !readJSON(w, r, &req) {
		return
	}
	t, err := h.e.Remove(r.PathValue("ref"), req.DeleteFiles)
	reply(w, t, err)
}

func (h *handler) limits(w http.ResponseWriter, r *http.Request) {
	var req LimitsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Download < 0 || req.Upload < 0 {
		writeErr(w, &Error{Status: http.StatusBadRequest, Code: CodeInvalid, Message: "limits can't be negative"})
		return
	}
	h.e.SetLimits(req.Download, req.Upload)
	h.status(w, r)
}

func (h *handler) uploads(w http.ResponseWriter, r *http.Request) {
	var req UploadsRequest
	if !readJSON(w, r, &req) {
		return
	}
	t, err := h.e.SetUploadsPaused(r.PathValue("ref"), req.Paused)
	reply(w, t, err)
}

func (h *handler) seed(w http.ResponseWriter, r *http.Request) {
	var req SeedRequest
	if !readJSON(w, r, &req) {
		return
	}
	t, err := h.e.SetSeedMode(r.PathValue("ref"), req.Mode)
	reply(w, t, err)
}

func (h *handler) seedLimits(w http.ResponseWriter, r *http.Request) {
	var req SeedLimitsRequest
	if !readJSON(w, r, &req) {
		return
	}
	if req.Ratio < 0 || req.Time < 0 {
		writeErr(w, &Error{Status: http.StatusBadRequest, Code: CodeInvalid, Message: "seeding limits can't be negative"})
		return
	}
	h.e.SetSeedLimits(req.Ratio, req.Time)
	h.status(w, r)
}

func (h *handler) stop(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, struct{}{})
	h.shutdown()
}

// --- helpers -----------------------------------------------------------------------

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		writeErr(w, &Error{Status: http.StatusBadRequest, Code: CodeInvalid, Message: "bad request: " + err.Error()})
		return false
	}
	return true
}

func reply(w http.ResponseWriter, v any, err error) {
	if err != nil {
		writeErr(w, toError(err))
		return
	}
	writeJSON(w, v)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(e.Status)
	json.NewEncoder(w).Encode(e)
}

// toError maps engine errors to HTTP statuses and codes.
func toError(err error) *Error {
	e := &Error{Status: http.StatusInternalServerError, Code: CodeInternal, Message: err.Error()}
	var dup *engine.DuplicateError
	switch {
	case errors.As(err, &dup):
		e.Status, e.Code, e.ID = http.StatusConflict, CodeDuplicate, dup.ID
	case errors.Is(err, engine.ErrNotFound):
		e.Status, e.Code = http.StatusNotFound, CodeNotFound
	case errors.Is(err, engine.ErrAmbiguous):
		e.Status, e.Code = http.StatusBadRequest, CodeAmbiguous
	case errors.Is(err, storage.ErrExists):
		e.Status, e.Code = http.StatusConflict, CodeExists
	case errors.Is(err, metainfo.ErrInvalid), errors.Is(err, magnet.ErrInvalid), errors.Is(err, engine.ErrBadFile),
		errors.Is(err, engine.ErrBadSeedMode):
		e.Status, e.Code = http.StatusBadRequest, CodeInvalid
	case errors.Is(err, engine.ErrBusy):
		e.Status, e.Code = http.StatusConflict, CodeBusy
	case errors.Is(err, engine.ErrNoMetadata):
		e.Status, e.Code = http.StatusConflict, CodeNoMeta
	case errors.Is(err, engine.ErrSeedLimit):
		e.Status, e.Code = http.StatusConflict, CodeSeedLimit
	}
	return e
}
