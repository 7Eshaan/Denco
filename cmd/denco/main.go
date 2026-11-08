// Command denco is a BitTorrent client: a background daemon plus a CLI to
// control it. All commands are implemented in internal/cli.
package main

import (
	"os"

	"github.com/7Eshaan/Denco/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
