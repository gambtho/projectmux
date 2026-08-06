// Command projectmux orchestrates declarative tmux workspaces whose windows
// may run on the host or inside a Dev Container.
//
// It observes what tmux and Docker actually hold, plans the difference against
// the configured desired state, and reconciles it: open, attach, stop,
// autostart, list, status, config, and doctor.
//
// This build is an alpha. The configuration schema and the exit codes may
// still change below 1.0; the JSON envelopes carry a schema_version, and
// human-readable output is deliberately not a compatibility contract.
package main

import (
	"os"

	"github.com/gambtho/projectmux/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
