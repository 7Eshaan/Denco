package portmap

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func start(t *testing.T, cfg Config) *Mapper {
	t.Helper()
	m := Start(cfg)
	t.Cleanup(m.Close)
	return m
}

func TestNATPMPMapsAndRemoves(t *testing.T) {
	shorten(t)
	f := newFakePMP(t)
	m := start(t, f.config(Port{TCP, 6881}, Port{UDP, 6882}))
	st := waitState(t, m, Mapped)
	if st.Method != "NAT-PMP" || st.ExternalIP != "203.0.113.7" || st.Gateway != "127.0.0.1" {
		t.Errorf("status %+v", st)
	}
	if mp, ok := f.mapping("TCP/6881"); !ok || mp.external != 6881 || mp.lease != natpmpLifetime {
		t.Errorf("TCP mapping %+v %v", mp, ok)
	}
	if _, ok := f.mapping("UDP/6882"); !ok {
		t.Error("UDP port not mapped")
	}
	if ext, ok := m.External(TCP, 6881); !ok || ext != 6881 {
		t.Errorf("External(TCP, 6881) = %d, %v", ext, ok)
	}
	m.Close()
	if n, _ := f.count(); n != 0 {
		t.Errorf("%d mappings left after Close", n)
	}
	if _, ok := m.External(TCP, 6881); ok {
		t.Error("External still reports a mapping after Close")
	}
}

func TestNATPMPRenewsBeforeLeaseEnds(t *testing.T) {
	shorten(t)
	f := newFakePMP(t, func(f *fakePMP) { f.lifetime = 2 }) // seconds: renewed after 1
	m := start(t, f.config(Port{TCP, 6881}))
	waitState(t, m, Mapped)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, req := f.count(); req >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("mapping never renewed")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Stopping while a renewal waits for the router still removes the
// mappings in place.
func TestCloseDuringRenewalRemovesMappings(t *testing.T) {
	shorten(t)
	natpmpTries = []time.Duration{5 * time.Second} // the renewal hangs
	f := newFakePMP(t, func(f *fakePMP) { f.lifetime, f.answer = 2, 1 })
	m := start(t, f.config(Port{TCP, 6881}))
	waitState(t, m, Mapped)
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, req := f.count(); req >= 2 {
			break // the renewal was sent (after 1 s) and is waiting
		}
		if time.Now().After(deadline) {
			t.Fatal("no renewal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.Close()
	if n, _ := f.count(); n != 0 {
		t.Errorf("%d mappings left on the router", n)
	}
}

func TestNATPMPOtherExternalPort(t *testing.T) {
	shorten(t)
	f := newFakePMP(t, func(f *fakePMP) { f.extShift = 10 })
	m := start(t, f.config(Port{TCP, 6881}))
	waitState(t, m, Mapped)
	if ext, ok := m.External(TCP, 6881); !ok || ext != 6891 {
		t.Errorf("External = %d, %v; want the router's 6891", ext, ok)
	}
}

func TestNATPMPRefused(t *testing.T) {
	shorten(t)
	f := newFakePMP(t, func(f *fakePMP) { f.result = 2 })
	m := start(t, f.config(Port{TCP, 6881}))
	st := waitState(t, m, Failed)
	if !strings.Contains(st.Err, "not authorized") || st.NextTry.IsZero() {
		t.Errorf("status %+v", st)
	}
}

func TestNoRouterThenRouterAppears(t *testing.T) {
	shorten(t)
	f := newFakePMP(t)
	f.setSilent(true)
	m := start(t, f.config(Port{TCP, 6881}))
	st := waitState(t, m, NotFound)
	if st.Err == "" {
		t.Error("NotFound without a reason")
	}
	f.setSilent(false) // e.g. the laptop joined another network
	waitState(t, m, Mapped)
}

func TestUPnPMapsAndRemoves(t *testing.T) {
	shorten(t)
	f := newFakeIGD(t)
	m := start(t, f.config(Port{TCP, 6881}, Port{UDP, 6881}))
	st := waitState(t, m, Mapped)
	if st.Method != "UPnP" || st.ExternalIP != "198.51.100.4" || st.Gateway != "127.0.0.1" {
		t.Errorf("status %+v", st)
	}
	maps := f.mappings()
	tcp, ok := maps["TCP/6881"]
	if !ok || tcp.internal != 6881 || tcp.lease != upnpLease || tcp.client != "127.0.0.1" {
		t.Errorf("TCP mapping %+v %v", tcp, ok)
	}
	if _, ok := maps["UDP/6881"]; !ok {
		t.Error("UDP port not mapped")
	}
	m.Close()
	if left := f.mappings(); len(left) != 0 {
		t.Errorf("mappings left after Close: %v", left)
	}
}

func TestUPnPPermanentLeasesOnly(t *testing.T) {
	shorten(t)
	f := newFakeIGD(t, func(f *fakeIGD) { f.onlyPerm = true })
	m := start(t, f.config(Port{TCP, 6881}))
	waitState(t, m, Mapped)
	if mp := f.mappings()["TCP/6881"]; mp.lease != 0 {
		t.Errorf("lease %d; want 0 (the router only takes permanent ones)", mp.lease)
	}
}

func TestUPnPConflictTriesAnotherPort(t *testing.T) {
	shorten(t)
	f := newFakeIGD(t, func(f *fakeIGD) { f.taken[6881] = true }) // another computer on the LAN has it
	m := start(t, f.config(Port{TCP, 6881}))
	waitState(t, m, Mapped)
	ext, ok := m.External(TCP, 6881)
	if !ok || ext < 49152 {
		t.Fatalf("External = %d, %v; want a high port", ext, ok)
	}
	if mp, ok := f.mappings()[fmt.Sprintf("TCP/%d", ext)]; !ok || mp.internal != 6881 {
		t.Errorf("router has %+v", f.mappings())
	}
}

// A device answering our search must not send us to another host — for
// the description or for SOAP calls.
func TestUPnPOnlyTalksToTheDeviceThatAnswered(t *testing.T) {
	cases := map[string]func(f *fakeIGD){
		"description elsewhere": func(f *fakeIGD) { f.location = "http://192.0.2.1:80/desc.xml" },
		"control elsewhere": func(f *fakeIGD) {
			f.desc = strings.Replace(fakeDesc, "%s", "http://192.0.2.1:80/ctl", 1)
		},
		"https": func(f *fakeIGD) { f.location = strings.Replace(f.location, "http:", "https:", 1) },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			shorten(t)
			f := newFakeIGD(t, setup)
			m := start(t, f.config(Port{TCP, 6881}))
			st := waitState(t, m, NotFound)
			if !strings.Contains(st.Err, "not itself") {
				t.Errorf("error %q", st.Err)
			}
			if len(f.mappings()) != 0 {
				t.Error("mapped anyway")
			}
		})
	}
}

func TestFallsBackToUPnP(t *testing.T) {
	shorten(t)
	pmp, igd := newFakePMP(t), newFakeIGD(t)
	pmp.setSilent(true)
	cfg := pmp.config(Port{TCP, 6881})
	cfg.NoUPnP, cfg.SSDPAddr = false, igd.config().SSDPAddr
	m := start(t, cfg)
	if st := waitState(t, m, Mapped); st.Method != "UPnP" {
		t.Errorf("method %q", st.Method)
	}
}

func TestParseProcNetRoute(t *testing.T) {
	const s = "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\n" +
		"eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\n" +
		"eth0\t00000000\t0101A8C0\t0003\t0\t0\t100\t00000000\n"
	gw, err := parseProcNetRoute(s)
	if err != nil || gw != netip.MustParseAddr("192.168.1.1") {
		t.Errorf("got %v, %v", gw, err)
	}
	if _, err := parseProcNetRoute("Iface\tDestination\n"); err == nil {
		t.Error("no default route: want an error")
	}
}

func TestParseRouteGet(t *testing.T) {
	const s = "   route to: default\ndestination: default\n       mask: default\n    gateway: 10.0.0.138\n  interface: en0\n"
	gw, err := parseRouteGet(s)
	if err != nil || gw != netip.MustParseAddr("10.0.0.138") {
		t.Errorf("got %v, %v", gw, err)
	}
	if _, err := parseRouteGet("    gateway: fe80::1%en0\n"); err == nil {
		t.Error("IPv6 gateway: want an error")
	}
}

func TestFirstHost(t *testing.T) {
	cases := map[string]string{
		"192.168.1.23/24": "192.168.1.1",
		"10.0.5.9/16":     "10.0.0.1",
		"192.168.1.1/24":  "", // we are the .1: no guess
		"10.0.0.2/31":     "",
	}
	for in, want := range cases {
		gw, err := firstHost(netip.MustParsePrefix(in))
		if want == "" {
			if err == nil {
				t.Errorf("%s: got %v, want an error", in, gw)
			}
			continue
		}
		if err != nil || gw.String() != want {
			t.Errorf("%s: got %v, %v; want %s", in, gw, err, want)
		}
	}
}
