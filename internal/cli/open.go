package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/gambtho/projectmux/internal/bindpath"
	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

const openHelp = `usage: projectmux open [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]

Observe, ensure, record, and attach the workspace session, resolved
either from <target> or from the current directory. <target> is <repo> or
<repo>/<session>. The bare form "projectmux <target>" is shorthand for
this command (no flags).

  --no-attach  ensure and record without attaching the terminal
  --cwd <path> record <path> as the session's base directory before
               opening, in the same locked operation
  --json       emit the versioned JSON envelope (implies --no-attach)
  --compact    emit the JSON on a single line (implies --json)
`

// lockTimeout bounds how long open waits for another operation on the
// same workspace before failing with a typed error.
const lockTimeout = 10 * time.Second

// openEnvelope is the versioned JSON structure for projectmux open.
type openEnvelope struct {
	SchemaVersion         int                `json:"schema_version"`
	Workspace             workspaceInfo      `json:"workspace"`
	Action                string             `json:"action"`
	Session               string             `json:"session"`
	Drifted               bool               `json:"drifted"`
	BindWarning           string             `json:"bind_warning,omitempty"`
	Container             *openContainerInfo `json:"container,omitempty"`
	ContainerWindowsStale bool               `json:"container_windows_stale,omitempty"`
	Bind                  *string            `json:"bind,omitempty"`
}

// openContainerInfo is the ensured container as reported by open.
type openContainerInfo struct {
	Kind        string `json:"kind"`
	ContainerID string `json:"container_id"`
	Health      string `json:"health"`
}

func runOpen(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("open")
	noAttach := fs.Bool("no-attach", false, "ensure without attaching the terminal")
	cwdFlag := fs.String("cwd", "", "record the session's base directory")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, openHelp)
			return nil
		}
		return usagef("open: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("open: expected at most one workspace, got %d", fs.NArg())
	}
	if *compact {
		*asJSON = true
	}
	if *asJSON {
		*noAttach = true
	}

	res, ws, err := ensureWorkspace(ctx, fs.Arg(0), *cwdFlag)
	if err != nil {
		return err
	}

	if *asJSON {
		env := openEnvelope{
			SchemaVersion: OutputSchemaVersion,
			Workspace: workspaceInfo{
				ID:          ws.ID,
				Slug:        ws.Slug,
				RepoRoot:    ws.RepoRoot,
				Session:     ws.Session,
				SessionName: ws.SessionName,
			},
			Action:                string(res.Action),
			Session:               res.Session,
			Drifted:               res.Drifted,
			BindWarning:           res.BindWarning,
			ContainerWindowsStale: res.ContainerWindowsStale,
			Bind:                  res.Bind,
		}
		if res.Container != nil {
			env.Container = &openContainerInfo{
				Kind:        res.Container.Kind,
				ContainerID: res.Container.ContainerID,
				Health:      string(res.Container.Health),
			}
		}
		return writeJSON(stdout, env, *compact)
	}

	fmt.Fprintf(stdout, "session %s (%s)\n", res.Session, res.Action)
	if res.BindWarning != "" {
		fmt.Fprintln(stdout, res.BindWarning)
	}
	if res.Container != nil {
		fmt.Fprintf(stdout, "container %s (%s)\n", res.Container.ContainerID, res.Container.Health)
	}
	if res.Bind != nil {
		fmt.Fprintf(stdout, "bind %s\n", *res.Bind)
	}
	if res.ContainerWindowsStale {
		fmt.Fprintln(stdout, "container replaced; existing session keeps its old windows — run `projectmux stop` (once available) or kill the session and reopen to rebuild them")
	}
	if res.Drifted {
		fmt.Fprintln(stdout, "configuration has drifted; run `projectmux status` for details")
	}
	if *noAttach {
		return nil
	}
	return attachTerminal(ctx, res.Session)
}

// ensureWorkspace runs the read-only pipeline, derives the actuator
// windows, and calls the controller's Ensure under the workspace lock.
//
// cwdFlag carries --cwd into Desired.Bind rather than binding first and
// opening second. Ensure persists it in the same critical section that
// registers the workspace, before the observation the windows are planned
// from: one lock acquisition, and no window built from a bind that changed
// underneath it (spec §4). An empty cwdFlag leaves any stored bind alone.
func ensureWorkspace(ctx context.Context, name, cwdFlag string) (controller.EnsureResult, resolve.Workspace, error) {
	var zero controller.EnsureResult
	root, err := config.Root()
	if err != nil {
		return zero, resolve.Workspace{}, err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return zero, resolve.Workspace{}, err
	}
	ws, err := selectWorkspace(name, defaults.Layer.RepositoryRoots)
	if err != nil {
		return zero, resolve.Workspace{}, err
	}
	var bind *string
	if cwdFlag != "" {
		rel, relErr := bindpath.Rel(ws.RepoRoot, cwdFlag)
		if relErr != nil {
			// A path that does not exist or leaves the repository is a
			// caller mistake, not a failure: spec §6 puts it at exit 2.
			return zero, ws, usagef("open: --cwd: %s", relErr)
		}
		bind = &rel
	}
	effective, err := config.Load(root, defaults, ws.Slug)
	if err != nil {
		return zero, ws, err
	}
	intents := windowIntents(effective.Config)

	st, err := openStore()
	if err != nil {
		return zero, ws, err
	}
	defer func() { _ = st.Close() }()

	stateRoot, err := state.Root()
	if err != nil {
		return zero, ws, err
	}
	ctrl := controller.Controller{
		Store:        st,
		Sessions:     newSessionObserver(),
		Containers:   newContainerObserver(),
		Clock:        systemClock{},
		Actuator:     newSessionActuator(),
		ContainerAct: newContainerActuator(),
	}
	res, err := ctrl.Ensure(ctx, controller.Desired{
		Workspace: ws,
		Config:    effective.Config,
		Digest:    effective.Digest,
		Bind:      bind,
	}, intents, filepath.Join(stateRoot, "locks"), lockTimeout)
	return res, ws, err
}
