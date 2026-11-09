// Package portmap asks the home router to forward Denco's ports, so peers
// on the internet can connect to us (DECISIONS D49). Two protocols are
// tried, both with the standard library only:
//
//   - NAT-PMP (RFC 6886): a few UDP packets to the default gateway, port
//     5351. Apple routers and many others; PCP routers answer it too.
//   - UPnP IGD: find the router by SSDP multicast, read its device
//     description over HTTP, then call AddPortMapping over SOAP.
//
// A Mapper keeps the mappings alive (renewing them before their lease
// ends), finds the router again if it stops answering, and removes the
// mappings when closed.
//
// Files:
//
//	portmap.go  – Mapper: discovery, renewal, status
//	natpmp.go   – NAT-PMP client
//	upnp.go     – UPnP IGD client (SSDP, device description, SOAP)
//	gateway*.go – finding the default gateway and our LAN address
package portmap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/netip"
	"sync"
	"time"
)

// Proto is a transport protocol, as the router protocols spell it.
type Proto string

const (
	TCP Proto = "TCP"
	UDP Proto = "UDP"
)

// Port is one local port to forward.
type Port struct {
	Proto    Proto
	Internal uint16
}

// Mapping is a port the router forwards to us.
type Mapping struct {
	Proto    Proto  `json:"proto"`
	Internal uint16 `json:"internal"`
	External uint16 `json:"external"` // usually equal to Internal
}

// State is where a Mapper stands.
type State string

const (
	Searching State = "searching" // looking for a router (or asking it)
	Mapped    State = "mapped"    // at least one port is forwarded
	NotFound  State = "not_found" // no NAT-PMP or UPnP router answered
	Failed    State = "failed"    // a router answered but refused
)

// Status is a snapshot for display.
type Status struct {
	State      State     `json:"state"`
	Method     string    `json:"method,omitempty"`  // "NAT-PMP" or "UPnP"
	Gateway    string    `json:"gateway,omitempty"` // the router's address
	ExternalIP string    `json:"external_ip,omitempty"`
	Mappings   []Mapping `json:"mappings,omitempty"`
	Err        string    `json:"error,omitempty"`
	NextTry    time.Time `json:"next_try,omitzero"` // NotFound/Failed: when we look again
}

// Config says what to forward. The address fields are for tests: left
// zero, the real gateway and the SSDP multicast group are used.
type Config struct {
	Ports []Port

	Gateway    netip.Addr     // router for NAT-PMP; zero = the default gateway
	NATPMPPort uint16         // 0 = 5351
	SSDPAddr   netip.AddrPort // where to send UPnP searches; zero = 239.255.255.250:1900
	NoNATPMP   bool           // skip NAT-PMP (tests of UPnP)
	NoUPnP     bool           // skip UPnP (tests of NAT-PMP)

	Logger *slog.Logger // default: discard
}

// Timings; variables so tests can shrink them.
var (
	retryEvery   = 10 * time.Minute // look for a router again after none answered
	refreshEvery = 30 * time.Minute // re-add permanent (lease-less) mappings
	removeWait   = 2 * time.Second  // how long Close waits for the router
)

const (
	description   = "Denco"
	conflictTries = 3 // other external ports to try when ours is taken
)

// errConflict: the router has the external port forwarded elsewhere.
var errConflict = errors.New("external port already forwarded to another device")

// router is a found NAT-PMP or UPnP gateway.
type router interface {
	method() string
	addr() string
	externalIP(ctx context.Context) (netip.Addr, error)
	// add forwards external → p.Internal on this machine; it returns the
	// external port granted and the lease (0 = until removed).
	add(ctx context.Context, p Port, external uint16) (uint16, time.Duration, error)
	remove(ctx context.Context, m Mapping) error
}

// Mapper forwards ports in the background. Safe for concurrent use.
type Mapper struct {
	cfg    Config
	log    *slog.Logger
	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	st Status
}

// Start begins looking for a router and forwarding cfg.Ports.
func Start(cfg Config) *Mapper {
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	m := &Mapper{cfg: cfg, log: log, cancel: cancel, done: make(chan struct{}), st: Status{State: Searching}}
	go m.run(ctx)
	return m
}

// Close stops renewing and asks the router to remove our mappings (waiting
// at most a couple of seconds for it).
func (m *Mapper) Close() {
	m.cancel()
	<-m.done
}

// Status returns the current state.
func (m *Mapper) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.st
	st.Mappings = append([]Mapping(nil), st.Mappings...)
	return st
}

// External is the port the router forwards to internal, if it does.
func (m *Mapper) External(proto Proto, internal uint16) (uint16, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mp := range m.st.Mappings {
		if mp.Proto == proto && mp.Internal == internal {
			return mp.External, true
		}
	}
	return 0, false
}

func (m *Mapper) set(st Status) {
	m.mu.Lock()
	m.st = st
	m.mu.Unlock()
}

// run is the Mapper's goroutine: find a router, map, renew; on failure
// start over later. On exit it removes what it mapped.
func (m *Mapper) run(ctx context.Context) {
	defer close(m.done)
	var r router
	var maps []Mapping
	defer func() {
		if r != nil && len(maps) > 0 {
			m.removeAll(r, maps)
		}
	}()
	for ctx.Err() == nil {
		m.set(Status{State: Searching})
		var err error
		r, err = m.discover(ctx)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			maps = nil
			m.log.Info("port forwarding: no UPnP or NAT-PMP router found; peers can still reach you if you forward the port by hand", "err", err)
			m.set(Status{State: NotFound, Err: err.Error(), NextTry: time.Now().Add(retryEvery)})
			sleep(ctx, retryEvery)
			continue
		}
		// Keep the mappings alive until the router stops cooperating.
		for ctx.Err() == nil {
			got, renew, err := m.mapAll(ctx, r, maps)
			if ctx.Err() != nil {
				return // stopped mid-renewal: maps (still in place) are removed
			}
			if err != nil {
				m.log.Warn("port forwarding failed", "method", r.method(), "router", r.addr(), "err", err)
				m.set(Status{State: Failed, Method: r.method(), Gateway: r.addr(), Err: err.Error(), NextTry: time.Now().Add(retryEvery)})
				r, maps = nil, nil
				sleep(ctx, retryEvery)
				break
			}
			prev := maps
			maps = got
			if !sameMappings(prev, maps) {
				st := m.Status()
				m.log.Info("port forwarding on", "method", r.method(), "router", r.addr(), "external_ip", st.ExternalIP, "ports", describe(maps))
			}
			sleep(ctx, renew)
		}
	}
}

// discover finds a router: NAT-PMP first (one round trip to the gateway),
// then UPnP.
func (m *Mapper) discover(ctx context.Context) (router, error) {
	var errs []error
	gw := m.cfg.Gateway
	if !m.cfg.NoNATPMP {
		if !gw.IsValid() {
			var err error
			if gw, err = defaultGateway(); err != nil {
				errs = append(errs, err)
			}
		}
		if gw.IsValid() {
			port := m.cfg.NATPMPPort
			if port == 0 {
				port = natpmpPort
			}
			n := &natpmp{gw: netip.AddrPortFrom(gw, port)}
			if _, err := n.externalIP(ctx); err == nil {
				return n, nil
			} else {
				errs = append(errs, fmt.Errorf("NAT-PMP at %s: %w", gw, err))
			}
		}
	}
	if !m.cfg.NoUPnP {
		ssdp := m.cfg.SSDPAddr
		if !ssdp.IsValid() {
			ssdp = ssdpMulticast
		}
		u, err := discoverUPnP(ctx, ssdp)
		if err == nil {
			return u, nil
		}
		errs = append(errs, fmt.Errorf("UPnP: %w", err))
	}
	if len(errs) == 0 {
		return nil, errors.New("port forwarding: no method enabled")
	}
	return nil, errors.Join(errs...)
}

// mapAll (re)adds every port, asking for the external ports granted last
// time. It returns the mappings in place and when to renew them; an error
// only if no port could be mapped.
func (m *Mapper) mapAll(ctx context.Context, r router, prev []Mapping) ([]Mapping, time.Duration, error) {
	var maps []Mapping
	var errs []error
	renew := refreshEvery
	for _, p := range m.cfg.Ports {
		want := p.Internal
		for _, old := range prev {
			if old.Proto == p.Proto && old.Internal == p.Internal {
				want = old.External
			}
		}
		ext, lease, err := r.add(ctx, p, want)
		for i := 0; errors.Is(err, errConflict) && i < conflictTries; i++ {
			ext, lease, err = r.add(ctx, p, uint16(49152+rand.IntN(16384)))
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s port %d: %w", p.Proto, p.Internal, err))
			continue
		}
		maps = append(maps, Mapping{Proto: p.Proto, Internal: p.Internal, External: ext})
		if lease > 0 {
			renew = min(renew, lease/2)
		}
	}
	if len(maps) == 0 {
		return nil, 0, errors.Join(errs...)
	}
	st := Status{State: Mapped, Method: r.method(), Gateway: r.addr(), Mappings: maps}
	if len(errs) > 0 {
		st.Err = errors.Join(errs...).Error()
	}
	if ip, err := r.externalIP(ctx); err == nil {
		st.ExternalIP = ip.String()
	}
	m.set(st)
	return maps, max(renew, time.Second), nil
}

// removeAll is called on Close, after the run context is cancelled.
func (m *Mapper) removeAll(r router, maps []Mapping) {
	ctx, cancel := context.WithTimeout(context.Background(), removeWait)
	defer cancel()
	for _, mp := range maps {
		if err := r.remove(ctx, mp); err != nil {
			m.log.Debug("port forwarding: removing mapping failed", "port", mp.External, "proto", mp.Proto, "err", err)
		}
	}
	m.set(Status{State: Searching})
}

func sameMappings(a, b []Mapping) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func describe(maps []Mapping) string {
	s := ""
	for i, mp := range maps {
		if i > 0 {
			s += ", "
		}
		s += fmt.Sprintf("%s %d→%d", mp.Proto, mp.External, mp.Internal)
	}
	return s
}

// sleep waits d or until ctx is done.
func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
