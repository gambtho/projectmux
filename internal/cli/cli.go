// Package cli parses commands, chooses human or JSON presentation, and maps
// typed errors to exit codes. It contains no orchestration logic.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
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
	ExitRefused          = 6 // the plan refused: conflict or uncertainty, do not blindly retry
)

// version is overridden at release time with -ldflags "-X ...cli.version=v1.2.3".
var version = ""

const usage = `projectmux - declarative tmux workspaces, optionally backed by Dev Containers

usage: projectmux <command> [options]

commands:
  <workspace>
        shorthand for: open <workspace>
  open [--no-attach] [--json] [--compact] [<workspace>]
        observe, ensure, record, and attach the workspace session
  attach [--json] [--compact] [<workspace>]
        attach to the live workspace session; never creates one
  stop [--container] [--json] [--compact] [<workspace>]
        end the workspace session, and with --container its container
  autostart [--json] [--compact]
        start containers for registered primary worktrees with autostart: true
  config [--validate] [--json] [--compact] [<workspace>]
        print the normalized, merged configuration for a workspace, or with
        --validate report what is wrong in the configuration files and where
  list [--json] [--compact]
        list recorded workspaces and live identity-carrying tmux sessions
  status [--json] [--compact] [<workspace>]
        observe one workspace and explain drift and dependency failures
  doctor [--json] [--compact]
        diagnose dependencies, configuration, state, and drift; changes nothing
  version
        print the projectmux version
  help
        show this message

This is an alpha build. Only the commands listed above are implemented.
`

// reportedError marks a failure whose full detail already went to
// stdout as the command's structured report — the deliberate exception
// to the no-stdout-on-failure contract (stop/autostart spec §5). Main
// prints only its one-line summary to stderr.
//
// err is the underlying cause, and may be nil. It exists so that exit-code
// classification still sees what actually failed: the exit code is a
// property of the failure, not of which command reported it. Leaving it nil
// keeps the default failure code, which is what a partial stop wants.
type reportedError struct {
	msg string
	err error
}

func (e *reportedError) Error() string { return e.msg }
func (e *reportedError) Unwrap() error { return e.err }

// usageError marks a caller mistake, which exits 2 and prints usage guidance.
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, args ...any) error {
	return &usageError{msg: fmt.Sprintf(format, args...)}
}

// Main runs one command and returns the process exit code. A failing
// single-operation command writes nothing to stdout, so callers can pipe
// stdout without filtering diagnostics out of it. The deliberate
// exception (stop/autostart spec §5): commands whose structured report
// IS the output — autostart's batch and a partially succeeding stop —
// write that report to stdout and then return a reportedError carrying
// only a one-line summary for stderr; nothing further reaches stdout.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := dispatch(ctx, args, stdout)
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

// dispatch routes one command. Diagnostics are Main's responsibility, so
// nothing below writes to stderr.
func dispatch(ctx context.Context, args []string, stdout io.Writer) error {
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
	case "open":
		return runOpen(ctx, rest, stdout)
	case "attach":
		return runAttach(ctx, rest, stdout)
	case "stop":
		return runStop(ctx, rest, stdout)
	case "autostart":
		return runAutostart(ctx, rest, stdout)
	case "config":
		return runConfig(rest, stdout)
	case "list":
		return runList(ctx, rest, stdout)
	case "status":
		return runStatus(ctx, rest, stdout)
	case "doctor":
		return runDoctor(ctx, rest, stdout)
	default:
		if !strings.HasPrefix(command, "-") {
			// Design §8: `projectmux <workspace>` is shorthand for
			// open. A mistyped command therefore resolves as a
			// workspace name and exits 4, not 2 — the documented trade.
			return runOpen(ctx, append([]string{command}, rest...), stdout)
		}
		return usagef("unknown command %q", command)
	}
}

func exitCode(err error) int {
	var (
		usageErr   *usageError
		ambiguous  *resolve.AmbiguousError
		unknown    *resolve.UnknownWorkspaceError
		invalidCfg *config.InvalidConfigError
		refusal    *controller.RefusalError
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
	case errors.As(err, &refusal):
		return ExitRefused
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
// instead of writing to stderr and exiting the process itself. Usage is a no-op
// because flag calls it on parse errors as well as on -h; anything it wrote
// would land on stdout for a failing command, and the -h path prints help
// itself when Parse returns flag.ErrHelp.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}
