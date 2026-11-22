// Command fakedenco stands in for denco in the install script tests: it
// records its arguments in $DENCO_FAKE_LOG and exits with
// $DENCO_FAKE_EXIT (default 0). It installs nothing.
package main

import (
	"os"
	"strconv"
	"strings"
)

func main() {
	if log := os.Getenv("DENCO_FAKE_LOG"); log != "" {
		os.WriteFile(log, []byte(strings.Join(os.Args[1:], " ")), 0o644)
	}
	code, _ := strconv.Atoi(os.Getenv("DENCO_FAKE_EXIT"))
	os.Exit(code)
}
