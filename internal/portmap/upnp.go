package portmap

import (
	"bufio"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// UPnP IGD. Discovery: an SSDP M-SEARCH (HTTP over UDP) to the multicast
// group; routers answer with the URL of their device description. That
// XML lists services; we want WANIPConnection (or WANPPPConnection, for
// PPPoE links) and its control URL, which takes SOAP calls.
//
// Anything on the LAN can answer an SSDP search, so a description or
// control URL must point at the device that answered: we never send
// requests to a host named in a reply that came from somewhere else.

var ssdpMulticast = netip.MustParseAddrPort("239.255.255.250:1900")

// ssdpWait is how long to collect answers to a search.
var ssdpWait = 2 * time.Second

const (
	upnpLease       = 3600 // seconds asked for; renewed at half
	maxUPnPResponse = 256 << 10
	upnpTimeout     = 5 * time.Second
)

// Search targets: IGD v1 and v2 routers, and those that only answer for
// the service itself.
var ssdpTargets = []string{
	"urn:schemas-upnp-org:device:InternetGatewayDevice:1",
	"urn:schemas-upnp-org:device:InternetGatewayDevice:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
}

// Services that can forward ports, best first.
var wanServices = []string{
	"urn:schemas-upnp-org:service:WANIPConnection:2",
	"urn:schemas-upnp-org:service:WANIPConnection:1",
	"urn:schemas-upnp-org:service:WANPPPConnection:1",
}

type upnp struct {
	host      string // the router's IP
	control   string // control URL
	service   string // service type, e.g. WANIPConnection:1
	local     netip.Addr
	client    *http.Client
	permanent bool // the router refused leases: map with lease 0
}

func (u *upnp) method() string { return "UPnP" }
func (u *upnp) addr() string   { return u.host }

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: upnpTimeout,
		// Redirects could lead away from the router that answered.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// discoverUPnP searches via ssdp and returns the first router whose
// description has a port-forwarding service.
func discoverUPnP(ctx context.Context, ssdp netip.AddrPort) (*upnp, error) {
	local, err := localAddrFor(ssdp.Addr())
	if err != nil {
		return nil, err
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: local.AsSlice()})
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	dst := net.UDPAddrFromAddrPort(ssdp)
	for _, st := range ssdpTargets {
		msg := "M-SEARCH * HTTP/1.1\r\n" +
			"HOST: 239.255.255.250:1900\r\n" +
			"MAN: \"ssdp:discover\"\r\n" +
			"MX: 2\r\n" +
			"ST: " + st + "\r\n\r\n"
		if _, err := conn.WriteToUDP([]byte(msg), dst); err != nil {
			if runtime.GOOS == "darwin" {
				// macOS blocks multicast for apps without Local Network access.
				return nil, fmt.Errorf("sending the search: %w (check System Settings › Privacy & Security › Local Network for your terminal app)", err)
			}
			return nil, fmt.Errorf("sending the search: %w", err)
		}
	}
	deadline := time.Now().Add(ssdpWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	conn.SetReadDeadline(deadline)
	client := newHTTPClient()
	seen := map[string]bool{}
	var lastErr error
	buf := make([]byte, 2048)
	for {
		k, from, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if lastErr != nil {
				return nil, lastErr
			}
			return nil, errors.New("no router answered")
		}
		loc, ok := ssdpLocation(buf[:k])
		if !ok || seen[loc] {
			continue
		}
		seen[loc] = true
		u, err := describeRouter(ctx, client, loc, from.Addr().Unmap())
		if err != nil {
			lastErr = err
			continue
		}
		u.local = local
		return u, nil
	}
}

// ssdpLocation returns the LOCATION of an SSDP answer.
func ssdpLocation(b []byte) (string, bool) {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(b)), nil)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	loc := resp.Header.Get("Location")
	return loc, resp.StatusCode == http.StatusOK && loc != ""
}

// sameHost checks that rawURL is plain HTTP to host (the device that
// answered our search).
func sameHost(rawURL string, host netip.Addr) (*url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	ip, err := netip.ParseAddr(u.Hostname())
	if u.Scheme != "http" || err != nil || ip.Unmap() != host {
		return nil, fmt.Errorf("router at %s pointed to %q, not itself", host, rawURL)
	}
	return u, nil
}

// Device description XML: devices nest, each with a service list.
type xmlService struct {
	ServiceType string `xml:"serviceType"`
	ControlURL  string `xml:"controlURL"`
}

type xmlDevice struct {
	Services []xmlService `xml:"serviceList>service"`
	Devices  []xmlDevice  `xml:"deviceList>device"`
}

type xmlRoot struct {
	URLBase string    `xml:"URLBase"`
	Device  xmlDevice `xml:"device"`
}

func (d *xmlDevice) services(out []xmlService) []xmlService {
	out = append(out, d.Services...)
	for i := range d.Devices {
		out = d.Devices[i].services(out)
	}
	return out
}

// describeRouter reads the description at loc and picks its best
// port-forwarding service.
func describeRouter(ctx context.Context, client *http.Client, loc string, from netip.Addr) (*upnp, error) {
	locURL, err := sameHost(loc, from)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, locURL.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device description: HTTP %d", resp.StatusCode)
	}
	var root xmlRoot
	if err := xml.NewDecoder(io.LimitReader(resp.Body, maxUPnPResponse)).Decode(&root); err != nil {
		return nil, fmt.Errorf("device description: %w", err)
	}
	base := locURL
	if root.URLBase != "" {
		if base, err = sameHost(root.URLBase, from); err != nil {
			return nil, err
		}
	}
	all := root.Device.services(nil)
	for _, want := range wanServices {
		for _, s := range all {
			if strings.TrimSpace(s.ServiceType) != want {
				continue
			}
			ref, err := url.Parse(strings.TrimSpace(s.ControlURL))
			if err != nil {
				return nil, err
			}
			control := base.ResolveReference(ref).String()
			if _, err := sameHost(control, from); err != nil {
				return nil, err
			}
			return &upnp{host: from.String(), control: control, service: want, client: client}, nil
		}
	}
	return nil, fmt.Errorf("device at %s has no port-forwarding service", from)
}

// upnpError is a SOAP fault from the router.
type upnpError struct {
	Code int
	Desc string
}

func (e *upnpError) Error() string { return fmt.Sprintf("router error %d (%s)", e.Code, e.Desc) }

// UPnP error codes we act on.
const (
	upnpActionFailed  = 501 // ActionFailed: some routers' answer to any lease but 0
	upnpConflict      = 718 // ConflictInMappingEntry
	upnpOnlyPermanent = 725 // OnlyPermanentLeasesSupported
)

// soap calls action with args (in the order the spec lists them) and
// decodes the response envelope into out, if non-nil.
func (u *upnp) soap(ctx context.Context, action string, args [][2]string, out any) error {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?>` +
		`<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" s:encodingStyle="http://schemas.xmlsoap.org/soap/encoding/">` +
		`<s:Body><u:` + action + ` xmlns:u="` + u.service + `">`)
	for _, a := range args {
		b.WriteString("<" + a[0] + ">")
		xml.EscapeText(&b, []byte(a[1]))
		b.WriteString("</" + a[0] + ">")
	}
	b.WriteString(`</u:` + action + `></s:Body></s:Envelope>`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.control, strings.NewReader(b.String()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", `text/xml; charset="utf-8"`)
	req.Header.Set("SOAPAction", `"`+u.service+"#"+action+`"`)
	// One connection per call: some routers close kept-alive connections
	// without saying so, and the next call fails with EOF.
	req.Close = true
	resp, err := u.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUPnPResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var fault struct {
			Code int    `xml:"Body>Fault>detail>UPnPError>errorCode"`
			Desc string `xml:"Body>Fault>detail>UPnPError>errorDescription"`
		}
		if xml.Unmarshal(body, &fault) == nil && fault.Code != 0 {
			return &upnpError{Code: fault.Code, Desc: fault.Desc}
		}
		return fmt.Errorf("%s: HTTP %d", action, resp.StatusCode)
	}
	if out != nil {
		if err := xml.Unmarshal(body, out); err != nil {
			return fmt.Errorf("%s: %w", action, err)
		}
	}
	return nil
}

func (u *upnp) externalIP(ctx context.Context) (netip.Addr, error) {
	var out struct {
		IP string `xml:"Body>GetExternalIPAddressResponse>NewExternalIPAddress"`
	}
	if err := u.soap(ctx, "GetExternalIPAddress", nil, &out); err != nil {
		return netip.Addr{}, err
	}
	return netip.ParseAddr(strings.TrimSpace(out.IP))
}

func (u *upnp) add(ctx context.Context, p Port, external uint16) (uint16, time.Duration, error) {
	lease := upnpLease
	if u.permanent {
		lease = 0
	}
	err := u.addMapping(ctx, p, external, lease)
	var ue *upnpError
	if errors.As(err, &ue) && (ue.Code == upnpOnlyPermanent || ue.Code == upnpActionFailed) && lease != 0 {
		// The standard answer is 725; many ISP routers say 501 instead.
		// Keep the original error if a permanent mapping fails too.
		if err2 := u.addMapping(ctx, p, external, 0); err2 == nil {
			u.permanent, lease, err = true, 0, nil
		} else if ue.Code == upnpOnlyPermanent {
			err = err2
		}
	}
	if errors.As(err, &ue) && ue.Code == upnpConflict {
		return 0, 0, errConflict
	}
	if err != nil {
		return 0, 0, err
	}
	return external, time.Duration(lease) * time.Second, nil
}

func (u *upnp) addMapping(ctx context.Context, p Port, external uint16, lease int) error {
	return u.soap(ctx, "AddPortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(int(external))},
		{"NewProtocol", string(p.Proto)},
		{"NewInternalPort", strconv.Itoa(int(p.Internal))},
		{"NewInternalClient", u.local.String()},
		{"NewEnabled", "1"},
		{"NewPortMappingDescription", description},
		{"NewLeaseDuration", strconv.Itoa(lease)},
	}, nil)
}

func (u *upnp) remove(ctx context.Context, m Mapping) error {
	return u.soap(ctx, "DeletePortMapping", [][2]string{
		{"NewRemoteHost", ""},
		{"NewExternalPort", strconv.Itoa(int(m.External))},
		{"NewProtocol", string(m.Proto)},
	}, nil)
}
