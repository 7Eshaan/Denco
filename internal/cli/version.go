package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/7Eshaan/Denco/internal/version"
)

// cmdVersion prints the version (internal/version: stamped by release
// builds, else the module version, else "dev").
func cmdVersion(args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: denco version")
		return 2
	}
	fmt.Fprintf(stdout, "denco %s (%s/%s, %s)\n", version.String(), runtime.GOOS, runtime.GOARCH, runtime.Version())
	return 0
}
