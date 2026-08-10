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

Start containers for registered repositories: those whose configuration
sets autostart: true and to which a container applies. Every session on a
repository shares one container, so this starts one container per
repository. No tmux sessions are created. Intended for the systemd user
unit.

  --json     emit the versioned JSON envelope instead of human text
  --compact  emit the JSON on a single line (implies --json)
`

// autostartEnvelope is the versioned JSON structure for autostart. It is
// written to stdout even when the command exits 1 (some repositories
// failed) — the report is the output (spec §5). It is keyed by repository
// because that is what owns a container (spec §6.3).
type autostartEnvelope struct {
	SchemaVersion int              `json:"schema_version"`
	Repositories  []autostartEntry `json:"repositories"`
}

type autostartEntry struct {
	ID          string `json:"id"` // repository ID
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
	defer func() { _ = st.Close() }()
	repos, err := st.Repositories()
	if err != nil {
		return fmt.Errorf("reading stored repositories: %w", err)
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

	env := autostartEnvelope{SchemaVersion: OutputSchemaVersion, Repositories: []autostartEntry{}}
	failed := 0
	for _, repo := range repos {
		entry := autostartEntry{ID: repo.ID, Slug: repo.Slug}

		// The repository root must exist before anything else: config
		// loads never touch it, and auto's applicability check would
		// misread absence as "does not apply" — a vanished repository
		// must be a visible boot-log failure, not a silent skip.
		if _, statErr := os.Stat(repo.RepoRoot); statErr != nil {
			entry.Outcome = "failed"
			// Only confirmed absence may claim the repository is gone;
			// permission or I/O failures keep their own story.
			if errors.Is(statErr, os.ErrNotExist) {
				entry.Reason = "repository root no longer exists: " + repo.RepoRoot
			} else {
				entry.Reason = "statting the repository root: " + statErr.Error()
			}
			failed++
			env.Repositories = append(env.Repositories, entry)
			continue
		}

		// A row recorded at a linked worktree is skipped. Migration 0002
		// promotes every stored path to a repository row, deliberately
		// over-counting, because pure SQL cannot ask git which of them are
		// linked worktrees; `rebuild` collapses them afterwards. This unit
		// runs unattended on the first boot after the upgrade, which can
		// fall in the window before that rebuild — and without this check
		// it would run `devcontainer up` once per worktree, N containers
		// where a repository is promised exactly one.
		//
		// The predicate is staleRepositoryRoots' (status.go), minus the
		// slug filter: that filter exists to keep status to a couple of git
		// calls, and autostart has no workspace of its own to filter
		// against. The cost is one `git rev-parse` per registered
		// repository, once per boot, in a command that is about to spend
		// seconds per repository starting containers.
		//
		// A row that no longer resolves at all falls through rather than
		// being skipped, as it does in status: git declining to answer is
		// not evidence that the row is a worktree, and treating it as one
		// would silently disable autostart for a whole repository on a
		// transient failure. The row proceeds and reports whatever the
		// start actually does.
		if resolved, resolveErr := resolve.Resolve("", "", nil, repo.RepoRoot); resolveErr == nil &&
			resolved.RepoRoot != repo.RepoRoot {
			entry.Outcome = "skipped"
			entry.Reason = "recorded root is a linked worktree of " + resolved.RepoRoot +
				"; run projectmux rebuild to collapse it"
			env.Repositories = append(env.Repositories, entry)
			continue
		}

		effective, loadErr := config.Load(root, defaults, repo.Slug)
		if loadErr != nil {
			entry.Outcome = "failed"
			entry.Reason = loadErr.Error()
			failed++
			env.Repositories = append(env.Repositories, entry)
			continue
		}
		if !effective.Config.Autostart {
			entry.Outcome = "skipped"
			entry.Reason = "autostart is not enabled"
			env.Repositories = append(env.Repositories, entry)
			continue
		}

		outcome, obs, startErr := ctrl.StartRepositoryContainer(ctx, controller.RepoDesired{
			Repository: repo,
			Config:     effective.Config,
			Digest:     effective.Digest,
		}, lockDir, lockTimeout)
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
		env.Repositories = append(env.Repositories, entry)
	}

	if *asJSON {
		if err := writeJSON(stdout, env, *compact); err != nil {
			return err
		}
	} else {
		if len(env.Repositories) == 0 {
			fmt.Fprintln(stdout, "no registered repositories")
		}
		for _, e := range env.Repositories {
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
			"autostart failed for %d repository(ies); details are in the report above", failed)}
	}
	return nil
}
