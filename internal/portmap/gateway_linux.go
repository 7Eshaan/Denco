package portmap

import (
	"net/netip"
	"os"
)

func osGateway() (netip.Addr, error) {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return netip.Addr{}, err
	}
	return parseProcNetRoute(string(b))
}
