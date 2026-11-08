// Package format turns raw numbers into the human-readable strings shown by
// the CLI: sizes ("5.2 GB"), speeds ("4.2 MB/s"), durations ("7m 42s").
//
// Sizes use decimal SI units (1 KB = 1000 B) so the labels are honest; this
// matches macOS Finder and most Linux tools. See DECISIONS D10.
package format

import (
	"fmt"
	"time"
)

var units = []string{"B", "KB", "MB", "GB", "TB", "PB"}

// Bytes formats a byte count: 0 B, 999 B, 1.5 KB, 5.2 GB.
func Bytes(n int64) string {
	if n < 0 {
		return "-" + Bytes(-n)
	}
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	i := 0
	for v >= 999.95 && i < len(units)-1 {
		v /= 1000
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// Speed formats a rate in bytes per second.
func Speed(bytesPerSec int64) string {
	return Bytes(bytesPerSec) + "/s"
}

// Percent formats a ratio in [0,1] as "65.4%". Never shows 100% until done
// is exactly total, so a nearly finished download doesn't look complete.
func Percent(done, total int64) string {
	if total <= 0 {
		return "0.0%"
	}
	if done >= total {
		return "100%"
	}
	p := float64(done) / float64(total) * 100
	if p > 99.9 {
		p = 99.9
	}
	return fmt.Sprintf("%.1f%%", p)
}

// Duration formats d compactly with the two most significant units:
// "42s", "7m 42s", "3h 5m", "2d 4h". Negative means unknown and gives "∞".
func Duration(d time.Duration) string {
	if d < 0 {
		return "∞"
	}
	s := int64(d.Round(time.Second) / time.Second)
	days, s := s/86400, s%86400
	hours, s := s/3600, s%3600
	mins, secs := s/60, s%60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm %ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

// ETA estimates time remaining; returns -1 (rendered as ∞) when stalled.
func ETA(remaining, bytesPerSec int64) time.Duration {
	if remaining <= 0 {
		return 0
	}
	if bytesPerSec <= 0 {
		return -1
	}
	return time.Duration(float64(remaining) / float64(bytesPerSec) * float64(time.Second))
}
