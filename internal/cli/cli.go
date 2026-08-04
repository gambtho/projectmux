// Package cli parses commands, chooses human or JSON presentation, and maps
// typed errors to exit codes. It contains no orchestration logic.
package cli

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"strings"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
)

// Exit codes. They are part of the command contract: automation branches on
// them, so they must stay stable as commands are added.
const (
	ExitOK               = 0
	ExitError            = 1 // unexpected or I/O failure
	ExitUsage            = 2
	ExitAmbiguous        = 3 // a workspace name matched more than one worktree
	ExitUnknownWorkspace = 4
	ExitInvalidConfig    = 5
)

// version is overridden at release time with -ldflags "-X ...cli.version=v1.2.3".
var version = ""

const usage = `projectmux - declarative tmux workspaces, optionally backed by Dev Containers

usage: projectmux <command> [options]

commands:
  config [--json] [--compact] [<workspace>]
        print the normalized, merged configuration for a workspace
  version
        print the projectmux version
  help
        show this message

This is an alpha build. Only the commands listed above are implemented.
`

// usageError marks a caller mistake, which exits 2 and prints usage guidance.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// Main runs one command and returns the process exit code. It writes nothing
// to stdout for a failing command, so callers can pipe stdout without having to
// filter diagnostics out of it.
func Main(args []string, stdout, stderr io.Writer) int {
	err := dispatch(args, stdout, stderr)
	if err == nil {
		return ExitOK
	}
	fmt.Fprintf(stderr, "projectmux: %s\n", err)

	var usageErr *usageError
	if errors.As(err, &usageErr) {
		fmt.Fprint(stderr, "\n", usage)
	}
	return exitCode(err)
}

func dispatch(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return nil
	}

	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintln(stdout, versionString())
		return nil
	case "config":
		return runConfig(rest, stdout, stderr)
	default:
		return usagef("unknown command %q", command)
	}
}

func exitCode(err error) int {
	var (
		usageErr   *usageError
		ambiguous  *resolve.AmbiguousError
		unknown    *resolve.UnknownWorkspaceError
		invalidCfg *config.InvalidConfigError
	)
	switch {
	case errors.As(err, &usageErr):
		return ExitUsage
	case errors.As(err, &ambiguous):
		return ExitAmbiguous
	case errors.As(err, &unknown):
		return ExitUnknownWorkspace
	case errors.As(err, &invalidCfg):
		return ExitInvalidConfig
	default:
		return ExitError
	}
}

// versionString prefers a release-time value, then the version the module was
// built from, and finally reports that neither is known.
func versionString() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := strings.TrimSpace(info.Main.Version); v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}

// newFlagSet builds a flag set that reports errors through the usage-error path
// instead of writing to stderr and exiting the process itself.
func newFlagSet(name string, out io.Writer, help string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() { fmt.Fprint(out, help) }
	return fs
}
