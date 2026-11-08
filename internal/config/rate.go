package config

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/7Eshaan/Denco/internal/format"
)

// rateUnits maps unit suffixes to bytes. Decimal units match how Denco
// displays sizes (D10); the binary ones are accepted for people used to
// other clients.
var rateUnits = map[string]float64{
	"b": 1,
	"k": 1e3, "kb": 1e3, "kib": 1 << 10,
	"m": 1e6, "mb": 1e6, "mib": 1 << 20,
	"g": 1e9, "gb": 1e9, "gib": 1 << 30,
}

// ParseRate parses a speed: "0" (unlimited), "500" (KB/s), "500K",
// "2.5MB/s", "1 MiB/s", "800B". Case and spaces don't matter.
func ParseRate(v string) (int64, error) {
	s := strings.ToLower(strings.ReplaceAll(v, " ", ""))
	s = strings.TrimSuffix(s, "/s")
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	num, unit := s[:i], s[i:]
	n, err := strconv.ParseFloat(num, 64)
	if err != nil || math.IsInf(n, 0) {
		return 0, fmt.Errorf("%q is not a speed (try 500K or 2M)", v)
	}
	mult := 1e3 // a bare number means KB/s, like most clients' settings
	if unit != "" {
		var ok bool
		if mult, ok = rateUnits[unit]; !ok {
			return 0, fmt.Errorf("%q: unknown unit %q (use K, M or G)", v, unit)
		}
	}
	bytes := n * mult
	if bytes > 1e12 {
		return 0, fmt.Errorf("%q is too fast to be a real limit", v)
	}
	if bytes > 0 && bytes < 1000 {
		// Below ~1 KB/s peers time out between blocks; refuse instead of
		// silently breaking every download.
		return 0, fmt.Errorf("%q is below the minimum of 1 KB/s (use 0 for unlimited)", v)
	}
	return int64(bytes), nil
}

// FormatRate renders a limit the way ParseRate reads it ("0" = unlimited).
func FormatRate(n int64) string {
	if n <= 0 {
		return "0"
	}
	return format.Speed(n)
}

// DescribeRate is FormatRate for people: "unlimited" instead of "0".
func DescribeRate(n int64) string {
	if n <= 0 {
		return "unlimited"
	}
	return format.Speed(n)
}
