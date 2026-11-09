package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/7Eshaan/Denco/internal/portmap"
)

func TestPortMapText(t *testing.T) {
	cases := []struct {
		st   *portmap.Status
		want []string
	}{
		{nil, []string{"off (port_forwarding)", "6881 forwarded by hand"}},
		{&portmap.Status{State: portmap.Searching}, []string{"looking for your router"}},
		{&portmap.Status{State: portmap.Mapped, Method: "UPnP", Gateway: "192.168.1.1", ExternalIP: "203.0.113.9",
			Mappings: []portmap.Mapping{{Proto: portmap.TCP, Internal: 6881, External: 6881}, {Proto: portmap.UDP, Internal: 6881, External: 51000}}},
			[]string{"UPnP via router 192.168.1.1", "TCP 6881,", "UDP 51000→6881", "public address 203.0.113.9"}},
		{&portmap.Status{State: portmap.NotFound, NextTry: time.Now().Add(9*time.Minute + time.Second)},
			[]string{"no UPnP or NAT-PMP router found", "forward port 6881 by hand", "looking again in 9m"}},
		{&portmap.Status{State: portmap.Failed, Method: "NAT-PMP", Gateway: "10.0.0.1", Err: "router says: not authorized"},
			[]string{"router 10.0.0.1 (NAT-PMP) refused: router says: not authorized"}},
	}
	for _, c := range cases {
		got := portMapText(c.st, 6881)
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%+v: %q lacks %q", c.st, got, w)
			}
		}
	}
}
