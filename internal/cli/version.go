package cli

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
)

// Version is set by release builds:
//
//	go build -ldflags "-X github.com/7Eshaan/Denco/internal/cli.Version=v0.1.0"
//
// Without it, the version Go recorded at build time is used (e.g. v0.1.0 for
// `go install github.com/7Eshaan/Denco/cmd/denco@v0.1.0`), else "dev".
var Version = ""

func version() string {
	if Version != "" {
		return Version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

func cmdVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: denco version")
		return 2
	}
	fmt.Fprintf(stdout, "denco %s (%s/%s, %s)\n", version(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	return 0
}
