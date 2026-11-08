package dht

import (
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"time"
)

// Saved between runs (StateFile): our node ID — keeping it means the nodes
// near us still find us after a restart — and up to maxSavedNodes good
// nodes, so the next start rarely needs the bootstrap routers.

const maxSavedNodes = 200

type savedState struct {
	ID    string      `json:"id"`
	Nodes []savedNode `json:"nodes"`
}

type savedNode struct {
	ID   string `json:"id"`
	Addr string `json:"addr"`
}

type state struct {
	id    ID
	nodes []NodeInfo
}

// loadState reads the state file. Anything missing or broken just means
// a fresh ID and no nodes.
func loadState(path string, log *slog.Logger) state {
	st := state{id: NewID()}
	if path == "" {
		return st
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Warn("dht: reading state", "err", err)
		}
		return st
	}
	var s savedState
	if err := json.Unmarshal(data, &s); err != nil {
		log.Warn("dht: state file is corrupt; starting fresh", "file", path, "err", err)
		return st
	}
	if b, err := hex.DecodeString(s.ID); err == nil && len(b) == len(st.id) {
		copy(st.id[:], b)
	}
	for _, n := range s.Nodes {
		b, err1 := hex.DecodeString(n.ID)
		a, err2 := netip.ParseAddrPort(n.Addr)
		if err1 != nil || err2 != nil || len(b) != 20 {
			continue
		}
		var ni NodeInfo
		copy(ni.ID[:], b)
		ni.Addr = a
		st.nodes = append(st.nodes, ni)
	}
	return st
}

// saveState writes the ID and the good nodes closest to a spread of IDs
// (atomically: temp file + rename).
func (s *Server) saveState() {
	if s.cfg.StateFile == "" {
		return
	}
	out := savedState{ID: s.id.String()}
	now := time.Now()
	s.table.mu.Lock()
	for _, n := range s.table.byAddr {
		if len(out.Nodes) == maxSavedNodes {
			break
		}
		if !n.bad() && now.Sub(n.lastSeen) <= questionableIn {
			out.Nodes = append(out.Nodes, savedNode{ID: n.ID.String(), Addr: n.Addr.String()})
		}
	}
	s.table.mu.Unlock()
	data, _ := json.MarshalIndent(out, "", "  ")
	dir := filepath.Dir(s.cfg.StateFile)
	tmp, err := os.CreateTemp(dir, ".tmp-dht-*")
	if err != nil {
		s.log.Warn("dht: saving state", "err", err)
		return
	}
	defer os.Remove(tmp.Name()) // no-op after the rename
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), s.cfg.StateFile)
	}
	if err != nil {
		s.log.Warn("dht: saving state", "err", err)
	}
}
