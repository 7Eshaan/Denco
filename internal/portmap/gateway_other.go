//go:build !linux && !darwin

package portmap

import (
	"errors"
	"net/netip"
)

// osGateway: no portable way here (Windows, BSDs); defaultGateway guesses.
func osGateway() (netip.Addr, error) {
	return netip.Addr{}, errors.New("default gateway lookup not supported on this OS")
}
