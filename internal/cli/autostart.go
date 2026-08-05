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

const autostartHelp = `usage: projectmux autostart [--json] [--compact]

Start containers for eligible registered primary worktrees: those whose
configuration sets autostart: true and to which a container applies. No
tmux sessions are created. Intended for the systemd user unit.

  --json     emit the versioned JSON envelope instead of human text
  --compact  emit the JSON on a single line (implies --json)
`

// autostartEnvelope is the versioned JSON structure for autostart. It
// is written to stdout even when the command exits 1 (some workspaces
// failed) — the report is the output (spec §5).
type autostartEnvelope struct {
	SchemaVersion int              `json:"schema_version"`
	Workspaces    []autostartEntry `json:"workspaces"`
}

type autostartEntry struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	Outcome     string `json:"outcome"` // started | already-running | skipped | failed
	Reason      string `json:"reason,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
}

func runAutostart(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("autostart")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, autostartHelp)
			return nil
		}
		return usagef("autostart: %s", err)
	}
	if fs.NArg() > 0 {
		return usagef("autostart: unexpected argument %q", fs.Arg(0))
	}
	if *compact {
		*asJSON = true
	}

	root, err := config.Root()
	if err != nil {
		return err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return err
	}
	st, err := openStore()
	if err != nil {
		return err
	}
	defer st.Close()
	records, err := st.Workspaces()
	if err != nil {
		return fmt.Errorf("reading stored workspaces: %w", err)
	}
	stateRoot, err := state.Root()
	if err != nil {
		return err
	}
	lockDir := filepath.Join(stateRoot, "locks")

	ctrl := controller.Controller{
		Store:        st,
		Sessions:     newSessionObserver(), // never consulted; wired for completeness
		Containers:   newContainerObserver(),
		Clock:        systemClock{},
		ContainerAct: newContainerActuator(),
	}

	env := autostartEnvelope{SchemaVersion: OutputSchemaVersion, Workspaces: []autostartEntry{}}
	failed := 0
	for _, rec := range records {
		if !rec.IsPrimary {
			continue
		}
		entry := autostartEntry{ID: rec.ID, Slug: rec.Slug}

		// The stored worktree must exist before anything else: config
		// loads never touch the worktree, and auto's applicability
		// check would misread absence as "does not apply" — a vanished
		// worktree must be a visible boot-log failure, not a silent
		// skip (spec §3, Codex review finding).
		if _, statErr := os.Stat(rec.Worktree); statErr != nil {
			entry.Outcome = "failed"
			// Only confirmed absence may claim the worktree is gone;
			// permission or I/O failures keep their own story.
			if errors.Is(statErr, os.ErrNotExist) {
				entry.Reason = "worktree no longer exists: " + rec.Worktree
			} else {
				entry.Reason = "statting the worktree: " + statErr.Error()
			}
			failed++
			env.Workspaces = append(env.Workspaces, entry)
			continue
		}

		effective, loadErr := config.Load(root, defaults, rec.Slug)
		if loadErr != nil {
			entry.Outcome = "failed"
			entry.Reason = loadErr.Error()
			failed++
			env.Workspaces = append(env.Workspaces, entry)
			continue
		}
		if !effective.Config.Autostart {
			entry.Outcome = "skipped"
			entry.Reason = "autostart is not enabled"
			env.Workspaces = append(env.Workspaces, entry)
			continue
		}

		d := controller.Desired{
			Workspace: resolve.Workspace{
				ID:          rec.ID,
				Slug:        rec.Slug,
				Worktree:    rec.Worktree,
				SessionName: rec.ProposedSession,
				IsPrimary:   rec.IsPrimary,
			},
			Config: effective.Config,
			Digest: effective.Digest,
		}
		outcome, obs, startErr := ctrl.StartWorkspaceContainer(ctx, d, lockDir, lockTimeout)
		switch {
		case startErr != nil:
			entry.Outcome = "failed"
			entry.Reason = startErr.Error()
			failed++
		case outcome == controller.ContainerNoneApplies:
			entry.Outcome = "skipped"
			entry.Reason = "no container applies"
		default:
			entry.Outcome = string(outcome)
			if obs != nil {
				entry.ContainerID = obs.ContainerID
			}
		}
		env.Workspaces = append(env.Workspaces, entry)
	}

	if *asJSON {
		if err := writeJSON(stdout, env, *compact); err != nil {
			return err
		}
	} else {
		if len(env.Workspaces) == 0 {
			fmt.Fprintln(stdout, "no registered primary workspaces")
		}
		for _, e := range env.Workspaces {
			line := fmt.Sprintf("%s\t%s", e.Slug, e.Outcome)
			if e.ContainerID != "" {
				line += "\t" + e.ContainerID
			}
			if e.Reason != "" {
				line += "\t(" + e.Reason + ")"
			}
			fmt.Fprintln(stdout, line)
		}
	}

	if failed > 0 {
		return &reportedError{msg: fmt.Sprintf(
			"autostart failed for %d workspace(s); details are in the report above", failed)}
	}
	return nil
}
