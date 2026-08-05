// Command projectmux orchestrates declarative tmux workspaces whose panes may
// run on the host or inside a Dev Container.
//
// This build is an alpha and implements the read-only configuration slice only.
package main

import (
	"os"

	"github.com/gambtho/projectmux/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
