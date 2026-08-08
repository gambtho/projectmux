package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

const stopHelp = `usage: projectmux stop [--container] [--force] [--json] [--compact] [<workspace>]

End the workspace's tmux session, and with --container also stop its
bound container. The only destructive command; idempotent — stopping an
already-stopped workspace succeeds.

  --container  also stop the container the workspace's repository shares
  --force      stop that container even while sibling sessions are live
  --json       emit the versioned JSON envelope instead of human text
  --compact    emit the JSON on a single line (implies --json)
`

// stopEnvelope is the versioned JSON structure for projectmux stop. On
// a partial failure the report still goes to stdout (the deliberate
// contract amendment, spec §5) with the failure detail in Error.
type stopEnvelope struct {
	SchemaVersion int                `json:"schema_version"`
	Workspace     workspaceInfo      `json:"workspace"`
	Session       stopSessionInfo    `json:"session"`
	Container     *stopContainerInfo `json:"container,omitempty"`
	Error         string             `json:"error,omitempty"`
}

type stopSessionInfo struct {
	Stopped bool   `json:"stopped"`
	Name    string `json:"name,omitempty"`
}

type stopContainerInfo struct {
	Stopped     bool   `json:"stopped"`
	ContainerID string `json:"container_id,omitempty"`
}

func runStop(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("stop")
	stopContainer := fs.Bool("container", false, "also stop the bound container")
	force := fs.Bool("force", false, "stop a shared container even while sibling sessions are live")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, stopHelp)
			return nil
		}
		return usagef("stop: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("stop: expected at most one workspace, got %d", fs.NArg())
	}
	if *compact {
		*asJSON = true
	}

	// Stop needs identity only — deliberately no workspace config load,
	// so a broken workspace YAML can never block stopping.
	root, err := config.Root()
	if err != nil {
		return err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("determining the current directory: %w", err)
	}
	ws, err := resolve.Resolve(fs.Arg(0), defaults.Layer.RepositoryRoots, cwd)
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

	ctrl := controller.Controller{
		Store:        st,
		Sessions:     newSessionObserver(),
		Clock:        systemClock{},
		Actuator:     newSessionActuator(),
		ContainerAct: newContainerActuator(),
	}
	res, stopErr := ctrl.Stop(ctx, controller.Desired{Workspace: ws},
		controller.StopOptions{Container: *stopContainer, Force: *force},
		filepath.Join(stateRoot, "locks"), lockTimeout)

	// Nothing was destroyed before a refusal or lock failure: normal
	// error path, nothing on stdout. After partial progress the report
	// is the output (spec §5).
	if stopErr != nil && !res.SessionStopped && !res.ContainerStopped {
		return stopErr
	}

	env := stopEnvelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			Worktree:    ws.RepoRoot,
			SessionName: ws.SessionName,
			IsPrimary:   true,
		},
		Session: stopSessionInfo{Stopped: res.SessionStopped, Name: res.SessionName},
	}
	if *stopContainer {
		env.Container = &stopContainerInfo{
			Stopped:     res.ContainerStopped,
			ContainerID: res.ContainerID,
		}
	}
	if stopErr != nil {
		env.Error = stopErr.Error()
	}

	if *asJSON {
		if err := writeJSON(stdout, env, *compact); err != nil {
			return err
		}
	} else {
		if res.SessionStopped {
			fmt.Fprintf(stdout, "stopped session %s\n", res.SessionName)
		} else {
			fmt.Fprintln(stdout, "no live session to stop")
		}
		if *stopContainer {
			switch {
			case res.ContainerStopped:
				fmt.Fprintf(stdout, "stopped container %s\n", res.ContainerID)
			case stopErr != nil:
				// The failure's origin may be persistence rather than
				// the stop itself; the error line below carries it.
				fmt.Fprintln(stdout, "container not stopped; see error below")
			default:
				fmt.Fprintln(stdout, "no bound container to stop")
			}
		}
		if stopErr != nil {
			fmt.Fprintf(stdout, "error: %s\n", stopErr)
		}
	}

	if stopErr != nil {
		return &reportedError{msg: "stop partially failed; details are in the report above"}
	}
	return nil
}
