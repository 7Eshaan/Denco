package portmap

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"net/netip"
	"strings"
)

// Finding the default gateway (the router NAT-PMP talks to). The standard
// library has no API for it: Linux and macOS ask the OS
// (gateway_linux.go, gateway_darwin.go); elsewhere, and if that fails, we
// guess the first address of our LAN's subnet (192.168.1.1 for
// 192.168.1.23/24), which is what nearly every home router uses. A wrong
// guess costs ~2 s of unanswered NAT-PMP packets; UPnP is then tried.

// defaultGateway returns the IPv4 default gateway.
func defaultGateway() (netip.Addr, error) {
	if gw, err := osGateway(); err == nil && gw.Is4() {
		return gw, nil
	}
	return guessGateway()
}

// localAddrFor returns the address of ours that packets to dst leave from.
// Connecting a UDP socket only picks a route; nothing is sent.
func localAddrFor(dst netip.Addr) (netip.Addr, error) {
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(netip.AddrPortFrom(dst, 9)))
	if err != nil {
		return netip.Addr{}, err
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).AddrPort().Addr().Unmap(), nil
}

// guessGateway: the first host address of the subnet our default route
// leaves from.
func guessGateway() (netip.Addr, error) {
	// A documentation address (RFC 5737): only used to pick the route.
	local, err := localAddrFor(netip.MustParseAddr("192.0.2.1"))
	if err != nil {
		return netip.Addr{}, err
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return netip.Addr{}, err
	}
	for _, ifc := range ifaces {
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			p, ok := netipPrefix(ipn)
			if !ok || p.Addr() != local {
				continue
			}
			return firstHost(p)
		}
	}
	return netip.Addr{}, errors.New("default gateway not found")
}

func netipPrefix(n *net.IPNet) (netip.Prefix, bool) {
	ip, ok := netip.AddrFromSlice(n.IP)
	if !ok {
		return netip.Prefix{}, false
	}
	ones, _ := n.Mask.Size()
	if ip.Is4In6() && ones > 32 {
		ones -= 96
	}
	return netip.PrefixFrom(ip.Unmap(), ones), true
}

// firstHost is the subnet's network address + 1.
func firstHost(p netip.Prefix) (netip.Addr, error) {
	if !p.Addr().Is4() || p.Bits() > 30 {
		return netip.Addr{}, errors.New("default gateway not found")
	}
	gw := p.Masked().Addr().Next()
	if gw == p.Addr() {
		return netip.Addr{}, errors.New("default gateway not found")
	}
	return gw, nil
}

// parseProcNetRoute reads Linux's /proc/net/route: the default route has
// destination 00000000; the gateway is hex in host (little-endian) order.
func parseProcNetRoute(s string) (netip.Addr, error) {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		b, err := hex.DecodeString(f[2])
		if err != nil || len(b) != 4 {
			continue
		}
		var ip [4]byte
		binary.BigEndian.PutUint32(ip[:], binary.LittleEndian.Uint32(b))
		if gw := netip.AddrFrom4(ip); !gw.IsUnspecified() {
			return gw, nil
		}
	}
	return netip.Addr{}, errors.New("no default route")
}

// parseRouteGet reads macOS's `route -n get default`: "gateway: 192.168.1.1".
func parseRouteGet(s string) (netip.Addr, error) {
	sc := bufio.NewScanner(strings.NewReader(s))
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), ":")
		if !ok || k != "gateway" {
			continue
		}
		if gw, err := netip.ParseAddr(strings.TrimSpace(v)); err == nil && gw.Is4() {
			return gw, nil
		}
	}
	return netip.Addr{}, errors.New("no IPv4 default gateway")
}
