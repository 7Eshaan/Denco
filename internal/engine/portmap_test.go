package engine

import (
	"encoding/binary"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/bencode"
	"github.com/7Eshaan/Denco/internal/portmap"
)

// pmpRouter is a loopback NAT-PMP router that forwards external port
// 40000 (TCP) / 40001 (UDP), whatever was asked — like a router whose
// default port is taken by another computer.
type pmpRouter struct {
	conn *net.UDPConn
	mu   sync.Mutex
	maps map[byte]uint16 // opcode → internal port
}

func newPMPRouter(t *testing.T) *pmpRouter {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	r := &pmpRouter{conn: conn, maps: map[byte]uint16{}}
	go func() {
		buf := make([]byte, 64)
		for {
			k, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			op := buf[1]
			resp := make([]byte, 16)
			resp[1] = op + 128
			if op == 0 {
				conn.WriteToUDPAddrPort(resp[:12], from)
				continue
			}
			if k < 12 {
				continue
			}
			copy(resp[8:10], buf[4:6])
			r.mu.Lock()
			if binary.BigEndian.Uint32(buf[8:]) == 0 {
				delete(r.maps, op)
			} else {
				r.maps[op] = binary.BigEndian.Uint16(buf[4:])
				binary.BigEndian.PutUint16(resp[10:], 40000+uint16(2-op)) // TCP (2) → 40000, UDP (1) → 40001
				binary.BigEndian.PutUint32(resp[12:], 7200)
			}
			r.mu.Unlock()
			conn.WriteToUDPAddrPort(resp, from)
		}
	}()
	return r
}

func (r *pmpRouter) config() portmap.Config {
	return portmap.Config{
		Gateway:    netip.MustParseAddr("127.0.0.1"),
		NATPMPPort: uint16(r.conn.LocalAddr().(*net.UDPAddr).Port),
		NoUPnP:     true,
	}
}

func (r *pmpRouter) mapped() map[byte]uint16 {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[byte]uint16{}
	for k, v := range r.maps {
		out[k] = v
	}
	return out
}

// With port forwarding on, the engine maps its peer and DHT ports, tells
// trackers the port the router forwards (not its own), and removes the
// mappings when it closes.
func TestPortForwardingAnnouncesRouterPort(t *testing.T) {
	router := newPMPRouter(t)
	var mu sync.Mutex
	var ports []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, _ := url.ParseQuery(r.URL.RawQuery)
		p, _ := strconv.Atoi(q.Get("port"))
		mu.Lock()
		ports = append(ports, p)
		mu.Unlock()
		body, _ := bencode.Encode(map[string]any{"interval": 1, "peers": ""})
		w.Write(body)
	}))
	t.Cleanup(srv.Close)

	tmp := t.TempDir()
	cfg := testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl"))
	cfg.EnableDHT = true // no bootstrap routers: the node stays silent
	cfg.PortForward, cfg.PortMap = true, router.config()
	e, err := Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()

	// Wait for the mapping before adding, so the first announce has it.
	deadline := time.Now().Add(10 * time.Second)
	for {
		if st := e.Overview().PortMap; st != nil && st.State == portmap.Mapped {
			if len(st.Mappings) != 2 || st.Method != "NAT-PMP" {
				t.Fatalf("status %+v", st)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("never mapped: %+v", e.Overview().PortMap)
		}
		time.Sleep(10 * time.Millisecond)
	}
	ov := e.Overview()
	if m := router.mapped(); m[2] != ov.ListenPort || m[1] != ov.DHTPort {
		t.Errorf("router forwards %v; want TCP→%d, UDP→%d", m, ov.ListenPort, ov.DHTPort)
	}

	f := newFixture(t, "fwd", srv.URL+"/announce", 16384, 20000)
	if _, err := e.Add(f.raw, AddOptions{}); err != nil {
		t.Fatal(err)
	}
	for {
		mu.Lock()
		got := append([]int(nil), ports...)
		mu.Unlock()
		if len(got) > 0 {
			if got[0] != 40000 {
				t.Errorf("tracker told port %d; want the router's 40000", got[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no announce")
		}
		time.Sleep(10 * time.Millisecond)
	}

	e.Close()
	if m := router.mapped(); len(m) != 0 {
		t.Errorf("mappings left after Close: %v", m)
	}
}

// Off (the default for engine.Config), nothing is shown or mapped.
func TestPortForwardingOff(t *testing.T) {
	tmp := t.TempDir()
	e := openEngine(t, testConfig(filepath.Join(tmp, "state"), filepath.Join(tmp, "dl")))
	if st := e.Overview().PortMap; st != nil {
		t.Errorf("PortMap = %+v; want nil", st)
	}
	if p := e.publicPort(); p != 0 {
		t.Errorf("publicPort = %d", p)
	}
}
