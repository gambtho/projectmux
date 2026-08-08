package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/rebuild"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

const rebuildHelp = `usage: projectmux rebuild [--dry-run] [--json] [--compact]

Recover workspace registrations the state database has lost, using the
identity keys live tmux sessions carry. A live session no stored row
matches is registered again and its session name adopted.

Rebuild only fills in what is missing: it never overwrites a recorded
value, and anything ambiguous is reported as a conflict and skipped.

It does NOT rediscover worktrees from repository_roots — only workspaces
whose tmux session is still alive can be recovered — and it does NOT
restore container bindings, which the next open reacquires.

  --dry-run  perform every read-only step and report what would change,
             writing nothing
  --json     emit the versioned JSON envelope instead of human text
  --compact  emit the JSON on a single line (implies --json)
`

// rebuildEnvelope is the versioned JSON structure for projectmux
// rebuild. Registered and Conflicts are always arrays, empty rather than
// absent, matching doctor's always-full checks. The envelope is written
// to stdout even when the command exits 6 — the report is the output
// (stop/autostart spec §5).
type rebuildEnvelope struct {
	SchemaVersion int                 `json:"schema_version"`
	DryRun        bool                `json:"dry_run"`
	Registered    []rebuildRegistered `json:"registered"`
	Conflicts     []rebuildConflict   `json:"conflicts"`
}

type rebuildRegistered struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Worktree  string `json:"worktree"`
	IsPrimary bool   `json:"is_primary"`
	Session   string `json:"session"`
}

type rebuildConflict struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

func runRebuild(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("rebuild")
	dryRun := fs.Bool("dry-run", false, "report what would change, writing nothing")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, rebuildHelp)
			return nil
		}
		return usagef("rebuild: %s", err)
	}
	if fs.NArg() > 0 {
		// Rebuild works over the whole installation; there is no
		// workspace to scope it to.
		return usagef("rebuild: unexpected argument %q", fs.Arg(0))
	}
	if *compact {
		*asJSON = true
	}

	report, err := buildRebuild(ctx, *dryRun)
	if err != nil {
		return err
	}
	env := rebuildEnvelopeFrom(report)

	if *asJSON {
		if err := writeJSON(stdout, env, *compact); err != nil {
			return err
		}
	} else if err := writeRebuildHuman(stdout, env); err != nil {
		return err
	}

	if n := len(env.Conflicts); n > 0 {
		// The report already went to stdout, so only a one-line summary
		// reaches stderr. The wrapped refusal is what makes this exit 6:
		// a conflict is uncertainty about the world, not a failure of
		// the command.
		return &reportedError{
			msg: fmt.Sprintf(
				"rebuild left %d conflict(s) unresolved; details are in the report above", n),
			err: &controller.RefusalError{
				Reason: "rebuild found conflicts it will not resolve by guessing",
			},
		}
	}
	return nil
}

func rebuildEnvelopeFrom(report rebuild.Report) rebuildEnvelope {
	env := rebuildEnvelope{
		SchemaVersion: OutputSchemaVersion,
		DryRun:        report.DryRun,
		Registered:    []rebuildRegistered{},
		Conflicts:     []rebuildConflict{},
	}
	for _, r := range report.Registered {
		env.Registered = append(env.Registered, rebuildRegistered{
			ID:        r.ID,
			Slug:      r.Slug,
			Worktree:  r.Worktree,
			IsPrimary: r.IsPrimary,
			Session:   r.Session,
		})
	}
	for _, c := range report.Conflicts {
		env.Conflicts = append(env.Conflicts, rebuildConflict{
			Subject: c.Subject,
			Reason:  c.Reason,
		})
	}
	return env
}

// writeRebuildHuman renders one line per registration and one per
// conflict. This layout may change in any release; automation should
// use --json.
func writeRebuildHuman(w io.Writer, env rebuildEnvelope) error {
	if len(env.Registered) == 0 && len(env.Conflicts) == 0 {
		fmt.Fprintln(w, "nothing to rebuild: every live session is already recorded")
		return nil
	}
	registered := "registered"
	if env.DryRun {
		registered = "would-register"
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range env.Registered {
		fmt.Fprintln(tw, cells(registered, r.Slug, r.Session))
	}
	for _, c := range env.Conflicts {
		fmt.Fprintln(tw, cells("conflict", c.Subject, c.Reason))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

// buildRebuild is spec §6's flow: load defaults, classify the database
// read-only, open the store read-write, list live sessions, read
// records, classify, apply.
func buildRebuild(ctx context.Context, dryRun bool) (rebuild.Report, error) {
	configRoot, err := config.Root()
	if err != nil {
		return rebuild.Report{}, err
	}
	// An unloadable defaults layer makes the digest underivable for
	// every workspace, so it is fatal (exit 5) rather than one
	// workspace's conflict — mirroring doctor's fatal DefaultsErr
	// branch.
	defaults, err := config.LoadDefaults(configRoot)
	if err != nil {
		return rebuild.Report{}, err
	}

	stateRoot, err := state.Root()
	if err != nil {
		return rebuild.Report{}, err
	}
	if err := rebuildDatabaseCheck(stateRoot); err != nil {
		return rebuild.Report{}, err
	}

	st, err := openStore()
	if err != nil {
		return rebuild.Report{}, err
	}
	defer func() { _ = st.Close() }()

	live, err := liveSessions(ctx)
	if err != nil {
		// A tmux outage is not "no live sessions". Registering nothing
		// and reporting success would be exactly the tri-state
		// violation doctor exists to prevent, so this is a refusal.
		return rebuild.Report{}, &controller.RefusalError{
			Reason: "cannot observe tmux sessions, so there is nothing to rebuild from: " +
				err.Error(),
		}
	}
	records, err := st.Workspaces()
	if err != nil {
		return rebuild.Report{}, fmt.Errorf("reading stored workspaces: %w", err)
	}

	applier := &rebuild.Applier{
		Store:    st,
		Sessions: newSessionObserver(),
		Resolver: worktreeResolver{},
		Config:   configDigests{root: configRoot, defaults: defaults},
		Locker:   workspaceLocker{dir: filepath.Join(stateRoot, "locks")},
		Clock:    systemClock{},
		DryRun:   dryRun,
	}
	return applier.Apply(ctx, rebuild.Classify(live, records)), nil
}

// worktreeResolver re-derives a workspace's identity the way every other
// command does: from the directory, never from the tmux keys. That is
// what recovers IsPrimary and the proposed session name, neither of
// which tmux carries (spec §3), and it is what lets rebuild verify the
// keys it was handed.
type worktreeResolver struct{}

func (worktreeResolver) Resolve(worktree string) (resolve.Workspace, error) {
	// No name and no roots: roots feed only lookup by name, and rebuild
	// resolves from a directory.
	return resolve.Resolve("", nil, worktree)
}

// configDigests supplies the desired digest from current configuration.
// Registering today's desired digest with no applied digest means the
// next open sees drift and reconciles — correct, since the configuration
// was never applied to this database (spec §3).
type configDigests struct {
	root     string
	defaults config.Source
}

func (c configDigests) Digest(slug string) (string, error) {
	effective, err := config.Load(c.root, c.defaults, slug)
	if err != nil {
		return "", err
	}
	return effective.Digest, nil
}

// workspaceLocker is the per-workspace filesystem lock every mutating
// command takes before its final observation and holds through the
// resulting state commit.
type workspaceLocker struct{ dir string }

func (w workspaceLocker) Lock(ctx context.Context, workspaceID string) (func(), error) {
	l, err := lock.Acquire(ctx, w.dir, workspaceID, lockTimeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}
