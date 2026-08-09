package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
)

const attachHelp = `usage: projectmux attach [--json] [--compact] [<workspace>]

Attach to the live workspace session, resolved either from <workspace>
or from the current directory. attach never creates a session and never
modifies state; use projectmux open to create one.

  --json     emit the versioned JSON envelope instead of attaching
  --compact  emit the JSON on a single line (implies --json)
`

// attachEnvelope is the versioned JSON structure for projectmux attach.
type attachEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Workspace     workspaceInfo `json:"workspace"`
	Session       sessionInfo   `json:"session"`
}

func runAttach(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("attach")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, attachHelp)
			return nil
		}
		return usagef("attach: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("attach: expected at most one workspace, got %d", fs.NArg())
	}
	if *compact {
		*asJSON = true
	}

	env, session, err := buildAttach(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	// Refuse before announcing: a successful attach execs away and can
	// never print afterwards, so the line has to come first — which
	// makes "attaching to X" a lie if we then decline. Everything the
	// refusal depends on is knowable here.
	if err := crossServerRefusal(); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "attaching to %s\n", session)
	return attachTerminal(ctx, session)
}

// buildAttach observes (no lock, no store writes — attach is an
// observation command that ends in a terminal connect) and requires a
// live session whose identity keys agree on all three values.
func buildAttach(ctx context.Context, name string) (attachEnvelope, string, error) {
	var zero attachEnvelope
	root, err := config.Root()
	if err != nil {
		return zero, "", err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return zero, "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return zero, "", fmt.Errorf("determining the current directory: %w", err)
	}
	ws, err := resolve.Resolve(name, "", defaults.Layer.RepositoryRoots, cwd)
	if err != nil {
		return zero, "", err
	}
	effective, err := config.Load(root, defaults, ws.Slug)
	if err != nil {
		return zero, "", err
	}

	st, err := openStore()
	if err != nil {
		return zero, "", err
	}
	defer func() { _ = st.Close() }()

	ctrl := controller.Controller{
		Store:      st,
		Sessions:   newSessionObserver(),
		Containers: newContainerObserver(),
		Clock:      systemClock{},
	}
	snap, err := ctrl.Observe(ctx, controller.Desired{
		Workspace: ws,
		Config:    effective.Config,
		Digest:    effective.Digest,
	})
	if err != nil {
		return zero, "", err
	}

	switch snap.Session.State {
	case controller.SessionUnknown:
		return zero, "", &controller.RefusalError{
			Reason: "tmux could not be observed; refusing to attach on an unknown session state"}
	case controller.SessionAbsent:
		return zero, "", fmt.Errorf(
			"no live session for workspace %s; run `projectmux open`", ws.Slug)
	}

	live := snap.Session.ByIdentity
	if !controller.SessionBelongsTo(*live, ws) {
		return zero, "", &controller.RefusalError{Reason: fmt.Sprintf(
			"session %q carries contradictory identity keys; refusing to attach to it", live.Name)}
	}

	name = live.Name
	verdict := "match"
	env := attachEnvelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			RepoRoot:    ws.RepoRoot,
			Session:     ws.Session,
			SessionName: ws.SessionName,
		},
		Session: sessionInfo{
			State:    string(snap.Session.State),
			Name:     &name,
			Identity: &verdict,
		},
	}
	return env, live.Name, nil
}
