// Command projectmux orchestrates declarative tmux workspaces whose windows
// may run on the host or inside a Dev Container.
//
// It observes what tmux and Docker actually hold and plans the difference
// against the configured desired state. Only open, stop, and autostart act on
// that plan; attach joins a session it never creates, and config, list,
// status, doctor, and version observe without mutating anything.
//
// This build is an alpha. Nothing it emits is a compatibility contract below
// 1.0 — not the configuration schema, the command surface, the JSON
// envelopes, or the exit codes. The envelopes carry a schema_version so a
// break is expressible, and human-readable output remains the least stable of
// them; automation should parse --json.
package main

import (
	"os"

	"github.com/gambtho/projectmux/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
