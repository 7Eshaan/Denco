// Package version is Denco's version, in one place. Everything that tells
// the outside world which version it is talks to — `denco version`, the
// peer ID prefix, the BEP 10 "v" field, the HTTP User-Agent — reads it
// here, so a release changes all of them at once (DECISIONS D54).
//
// Release builds stamp the tag:
//
//	go build -ldflags "-X github.com/7Eshaan/Denco/internal/version.Tag=v0.1.0"
//
// Without a stamp the module version Go recorded is used (e.g. v0.1.0 for
// `go install github.com/7Eshaan/Denco/cmd/denco@v0.1.0`), else "dev".
package version

import (
	"runtime/debug"
	"strconv"
	"strings"
)

// Tag is set by release builds with -ldflags (see the package comment).
var Tag = ""

// String is the full version, e.g. "v0.1.0", or "dev" for an unstamped
// local build.
func String() string {
	if Tag != "" {
		return Tag
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Short is the version without the leading "v" ("0.1.0", "0.2.0-rc1"),
// or "dev".
func Short() string { return strings.TrimPrefix(String(), "v") }

// Client is the name and version peers see in the extension handshake
// ("Denco 0.1.0").
func Client() string { return "Denco " + Short() }

// UserAgent is sent to trackers and when fetching .torrent files
// ("Denco/0.1.0").
func UserAgent() string { return "Denco/" + Short() }

// PeerIDDigits is the four-character version field of an Azureus-style
// peer ID: major, minor, patch, then 0 ("0100" for v0.1.0). Numbers above
// 9 use letters (10 = A … 35 = Z), as other clients do; anything that
// isn't a version (dev builds) is "0000".
func PeerIDDigits() string {
	return peerIDDigits(String())
}

func peerIDDigits(v string) string {
	nums, ok := parse(v)
	if !ok {
		return "0000"
	}
	b := make([]byte, 0, 4)
	for _, n := range append(nums, 0) {
		b = append(b, digit(n))
	}
	return string(b)
}

// parse reads "vMAJOR.MINOR.PATCH", ignoring any "-pre" or "+build" suffix.
func parse(v string) ([]int, bool) {
	core, ok := strings.CutPrefix(v, "v")
	if !ok {
		return nil, false
	}
	if i := strings.IndexAny(core, "-+"); i >= 0 {
		core = core[:i]
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return nil, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return nil, false
		}
		nums[i] = n
	}
	return nums, true
}

func digit(n int) byte {
	switch {
	case n < 10:
		return byte('0' + n)
	case n < 36:
		return byte('A' + n - 10)
	default:
		return 'Z'
	}
}
