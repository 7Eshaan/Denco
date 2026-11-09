package portmap

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"regexp"
	"strconv"
	"sync"
	"testing"
	"time"
)

// Fake routers on loopback. No test talks to a real router or sends
// multicast: every Config points Gateway / SSDPAddr at these.

type fakeMapping struct {
	proto              Proto
	internal, external uint16
	lease              int
	client             string // UPnP NewInternalClient
}

// fakePMP is a NAT-PMP server.
type fakePMP struct {
	conn     *net.UDPConn
	lifetime uint32 // granted lifetime; 0 = what was asked
	result   uint16 // non-zero: refuse every mapping with this code
	extShift uint16 // grant external = asked + extShift
	answer   int    // >0: answer only this many mapping requests (deletes always)

	silent bool // guarded by mu: ignore every request (no NAT-PMP here)

	mu       sync.Mutex
	maps     map[string]fakeMapping // "TCP/6881" → mapping
	requests int                    // mapping requests seen
}

// newFakePMP starts a server; setup runs before it serves.
func newFakePMP(t *testing.T, setup ...func(*fakePMP)) *fakePMP {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePMP{conn: conn, maps: map[string]fakeMapping{}}
	for _, fn := range setup {
		fn(f)
	}
	t.Cleanup(func() { conn.Close() })
	go f.serve()
	return f
}

func (f *fakePMP) port() uint16 { return uint16(f.conn.LocalAddr().(*net.UDPAddr).Port) }

func (f *fakePMP) config(ports ...Port) Config {
	return Config{Ports: ports, Gateway: netip.MustParseAddr("127.0.0.1"), NATPMPPort: f.port(), NoUPnP: true}
}

func (f *fakePMP) serve() {
	buf := make([]byte, 64)
	for {
		k, from, err := f.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		if k < 2 || buf[0] != 0 {
			continue
		}
		f.mu.Lock()
		silent := f.silent
		f.mu.Unlock()
		if silent {
			continue
		}
		op := buf[1]
		resp := make([]byte, 16)
		resp[1] = op + 128
		switch {
		case op == 0:
			copy(resp[8:12], []byte{203, 0, 113, 7})
			f.conn.WriteToUDPAddrPort(resp[:12], from)
		case (op == 1 || op == 2) && k >= 12:
			proto := TCP
			if op == 1 {
				proto = UDP
			}
			internal := binary.BigEndian.Uint16(buf[4:])
			external := binary.BigEndian.Uint16(buf[6:])
			life := binary.BigEndian.Uint32(buf[8:])
			key := fmt.Sprintf("%s/%d", proto, internal)
			copy(resp[8:10], buf[4:6])
			f.mu.Lock()
			if f.result != 0 {
				binary.BigEndian.PutUint16(resp[2:], f.result)
			} else if life == 0 {
				delete(f.maps, key)
			} else if f.requests++; f.answer > 0 && f.requests > f.answer {
				f.mu.Unlock()
				continue // a renewal the router never answers
			} else {
				if f.lifetime != 0 {
					life = f.lifetime
				}
				external += f.extShift
				f.maps[key] = fakeMapping{proto: proto, internal: internal, external: external, lease: int(life)}
				binary.BigEndian.PutUint16(resp[10:], external)
				binary.BigEndian.PutUint32(resp[12:], life)
			}
			f.mu.Unlock()
			f.conn.WriteToUDPAddrPort(resp, from)
		}
	}
}

func (f *fakePMP) setSilent(v bool) {
	f.mu.Lock()
	f.silent = v
	f.mu.Unlock()
}

func (f *fakePMP) mapping(key string) (fakeMapping, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.maps[key]
	return m, ok
}

func (f *fakePMP) count() (maps, requests int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.maps), f.requests
}

// fakeIGD is a UPnP router: an SSDP responder plus an HTTP server for the
// description and SOAP control.
type fakeIGD struct {
	ssdp *net.UDPConn
	srv  *httptest.Server

	location string // what SSDP answers; default: our description URL
	desc     string // description XML; %s = control path

	mu          sync.Mutex
	maps        map[string]fakeMapping // "TCP/6881" by external port
	onlyPerm    bool                   // refuse leases (725)
	lease501    bool                   // refuse leases with 501, like many ISP routers
	keptAlive   int                    // SOAP calls that didn't ask to close the connection
	taken       map[uint16]bool        // external ports owned by another device (718)
	descFetches int
}

const fakeDesc = `<?xml version="1.0"?>
<root xmlns="urn:schemas-upnp-org:device-1-0">
 <device>
  <deviceType>urn:schemas-upnp-org:device:InternetGatewayDevice:1</deviceType>
  <deviceList><device>
   <deviceType>urn:schemas-upnp-org:device:WANDevice:1</deviceType>
   <deviceList><device>
    <deviceType>urn:schemas-upnp-org:device:WANConnectionDevice:1</deviceType>
    <serviceList><service>
     <serviceType>urn:schemas-upnp-org:service:WANIPConnection:1</serviceType>
     <controlURL>%s</controlURL>
    </service></serviceList>
   </device></deviceList>
  </device></deviceList>
 </device>
</root>`

// newFakeIGD starts a router; setup runs before it serves.
func newFakeIGD(t *testing.T, setup ...func(*fakeIGD)) *fakeIGD {
	t.Helper()
	f := &fakeIGD{desc: fakeDesc, maps: map[string]fakeMapping{}, taken: map[uint16]bool{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/desc.xml", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.descFetches++
		desc := f.desc
		f.mu.Unlock()
		fmt.Fprintf(w, desc, "/ctl/IPConn")
	})
	mux.HandleFunc("/ctl/IPConn", f.control)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.location = f.srv.URL + "/desc.xml"
	for _, fn := range setup {
		fn(f)
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f.ssdp = conn
	t.Cleanup(func() { conn.Close() })
	go f.serveSSDP()
	return f
}

func (f *fakeIGD) config(ports ...Port) Config {
	return Config{Ports: ports, SSDPAddr: f.ssdp.LocalAddr().(*net.UDPAddr).AddrPort(), NoNATPMP: true}
}

func (f *fakeIGD) serveSSDP() {
	buf := make([]byte, 2048)
	for {
		_, from, err := f.ssdp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		f.mu.Lock()
		loc := f.location
		f.mu.Unlock()
		f.ssdp.WriteToUDPAddrPort([]byte("HTTP/1.1 200 OK\r\nCACHE-CONTROL: max-age=120\r\n"+
			"ST: urn:schemas-upnp-org:device:InternetGatewayDevice:1\r\n"+
			"LOCATION: "+loc+"\r\n\r\n"), from)
	}
}

var soapArg = regexp.MustCompile(`<(New\w+)>([^<]*)</New\w+>`)

func (f *fakeIGD) control(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	args := map[string]string{}
	for _, m := range soapArg.FindAllStringSubmatch(string(body), -1) {
		args[m[1]] = m[2]
	}
	action := r.Header.Get("SOAPAction")
	f.mu.Lock()
	defer f.mu.Unlock()
	if !r.Close {
		f.keptAlive++
	}
	fail := func(code int, desc string) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><s:Fault>`+
			`<faultcode>s:Client</faultcode><faultstring>UPnPError</faultstring><detail>`+
			`<UPnPError xmlns="urn:schemas-upnp-org:control-1-0"><errorCode>%d</errorCode><errorDescription>%s</errorDescription></UPnPError>`+
			`</detail></s:Fault></s:Body></s:Envelope>`, code, desc)
	}
	ok := func(inner string) {
		fmt.Fprintf(w, `<?xml version="1.0"?><s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body>%s</s:Body></s:Envelope>`, inner)
	}
	switch action {
	case `"urn:schemas-upnp-org:service:WANIPConnection:1#GetExternalIPAddress"`:
		ok(`<u:GetExternalIPAddressResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"><NewExternalIPAddress>198.51.100.4</NewExternalIPAddress></u:GetExternalIPAddressResponse>`)
	case `"urn:schemas-upnp-org:service:WANIPConnection:1#AddPortMapping"`:
		ext, _ := strconv.Atoi(args["NewExternalPort"])
		in, _ := strconv.Atoi(args["NewInternalPort"])
		lease, _ := strconv.Atoi(args["NewLeaseDuration"])
		if f.onlyPerm && lease != 0 {
			fail(725, "OnlyPermanentLeasesSupported")
			return
		}
		if f.lease501 && lease != 0 {
			fail(501, "Action Failed")
			return
		}
		if f.taken[uint16(ext)] {
			fail(718, "ConflictInMappingEntry")
			return
		}
		key := fmt.Sprintf("%s/%d", args["NewProtocol"], ext)
		f.maps[key] = fakeMapping{proto: Proto(args["NewProtocol"]), internal: uint16(in), external: uint16(ext), lease: lease, client: args["NewInternalClient"]}
		ok(`<u:AddPortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"/>`)
	case `"urn:schemas-upnp-org:service:WANIPConnection:1#DeletePortMapping"`:
		key := fmt.Sprintf("%s/%s", args["NewProtocol"], args["NewExternalPort"])
		if _, found := f.maps[key]; !found {
			fail(714, "NoSuchEntryInArray")
			return
		}
		delete(f.maps, key)
		ok(`<u:DeletePortMappingResponse xmlns:u="urn:schemas-upnp-org:service:WANIPConnection:1"/>`)
	default:
		fail(401, "Invalid Action")
	}
}

func (f *fakeIGD) mappings() map[string]fakeMapping {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := map[string]fakeMapping{}
	for k, v := range f.maps {
		out[k] = v
	}
	return out
}

// waitState polls m until it reaches want.
func waitState(t *testing.T, m *Mapper, want State) Status {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if st := m.Status(); st.State == want {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("state never became %s; status %+v", want, m.Status())
	return Status{}
}

// shorten makes timings test-sized and restores them afterwards.
func shorten(t *testing.T) {
	t.Helper()
	oldTries, oldWait, oldRetry, oldRefresh := natpmpTries, ssdpWait, retryEvery, refreshEvery
	natpmpTries = []time.Duration{50 * time.Millisecond, 100 * time.Millisecond}
	ssdpWait = 300 * time.Millisecond
	retryEvery, refreshEvery = 200*time.Millisecond, time.Hour
	t.Cleanup(func() {
		natpmpTries, ssdpWait, retryEvery, refreshEvery = oldTries, oldWait, oldRetry, oldRefresh
	})
}
