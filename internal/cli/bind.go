package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gambtho/projectmux/internal/bindpath"
	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

const bindHelp = `usage: projectmux bind [--clear] [--json] [--compact] <target> [<path>]

Record the directory a session opens in. <path> defaults to the current
directory, is interpreted relative to the repository root, must exist, and
must lie inside the repository; it is stored relative so it survives the
repository moving.

The bind is the session's base directory: every window's cwd composes on
top of it. Binding a session that does not exist yet creates it, so bind
is how a new named session is declared.

  --clear    remove the bind and keep the session
  --json     emit the versioned JSON envelope instead of human text
  --compact  emit the JSON on a single line (implies --json)
`

// bindEnvelope is the versioned JSON structure for projectmux bind. Bind
// is always emitted, and is null after --clear: a consumer cannot tell an
// absent field from a cleared one.
type bindEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Workspace     workspaceInfo `json:"workspace"`
	Bind          *string       `json:"bind"`
	Created       bool          `json:"created"`
}

func runBind(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("bind")
	clearBind := fs.Bool("clear", false, "remove the bind and keep the session")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, bindHelp)
			return nil
		}
		return usagef("bind: %s", err)
	}
	if *compact {
		*asJSON = true
	}
	switch {
	case fs.NArg() == 0:
		return usagef("bind: expected a target")
	case *clearBind && fs.NArg() > 1:
		return usagef("bind: --clear takes no path, got %q", fs.Arg(1))
	case fs.NArg() > 2:
		return usagef("bind: expected at most a target and a path, got %d arguments", fs.NArg())
	}

	// Identity only. Like stop, bind loads no workspace configuration, so
	// a broken workspace YAML can never block declaring where a session
	// opens.
	root, err := config.Root()
	if err != nil {
		return err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return err
	}
	ws, err := selectWorkspace(fs.Arg(0), defaults.Layer.RepositoryRoots)
	if err != nil {
		return err
	}
	bind, err := bindArgument(ws.RepoRoot, *clearBind, fs.Arg(1))
	if err != nil {
		return err
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	stateRoot, err := state.Root()
	if err != nil {
		return err
	}

	ctrl := controller.Controller{Store: st, Clock: systemClock{}}
	created, err := ctrl.SetBind(ctx, ws, bind,
		filepath.Join(stateRoot, "locks"), lockTimeout)
	if err != nil {
		return err
	}

	env := bindEnvelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			RepoRoot:    ws.RepoRoot,
			Session:     ws.Session,
			SessionName: ws.SessionName,
		},
		Bind:    bind,
		Created: created,
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	if bind == nil {
		fmt.Fprintf(stdout, "cleared the bind on %s\n", ws.SessionName)
	} else {
		fmt.Fprintf(stdout, "bound %s to %s\n", ws.SessionName, *bind)
	}
	if created {
		fmt.Fprintf(stdout,
			"created session %s; run `projectmux open` on it to start it\n", ws.SessionName)
	}
	return nil
}

// bindArgument turns the command line into the value stored on the
// record: nil for --clear, and otherwise the repository-relative form of
// the path, defaulting to the current directory.
//
// A path that does not exist, or that leaves the repository, is a caller
// mistake rather than a failure, so bindpath's error is re-typed here to
// land on exit 2 (spec §6). It already names the path.
func bindArgument(repoRoot string, clearBind bool, path string) (*string, error) {
	if clearBind {
		return nil, nil
	}
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determining the current directory: %w", err)
		}
		path = cwd
	}
	rel, err := bindpath.Rel(repoRoot, path)
	if err != nil {
		return nil, usagef("bind: %s", err)
	}
	return &rel, nil
}
