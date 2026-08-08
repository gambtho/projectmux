package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
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
// rebuild. Migrated, Registered and Conflicts are always arrays, empty
// rather than absent, matching doctor's always-full checks. The envelope
// is written to stdout even when the command exits 6 — the report is the
// output (stop/autostart spec §5).
type rebuildEnvelope struct {
	SchemaVersion int                 `json:"schema_version"`
	DryRun        bool                `json:"dry_run"`
	Migrated      []rebuildMigrated   `json:"migrated"`
	Registered    []rebuildRegistered `json:"registered"`
	Conflicts     []rebuildConflict   `json:"conflicts"`
}

// rebuildMigrated is one correction the upgrade pass made to state
// migration 0002 left structurally valid but semantically stale.
type rebuildMigrated struct {
	Subject string `json:"subject"`
	Action  string `json:"action"`
	Into    string `json:"into,omitempty"`
	// Detail is what the operator must act on by hand. For
	// binding-discarded it is the ID of the container left running with
	// nothing in the database referring to it.
	Detail string `json:"detail,omitempty"`
}

// rebuildRegistered names the tmux session a registration recorded, not the
// session component of the workspace identity. The key is "session_name"
// because every other envelope's "session" carries the component — "" for
// the default, "review" for slab--review — and one key meaning two things
// across envelopes is worse than a longer key.
type rebuildRegistered struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	RepoRoot    string `json:"repo_root"`
	SessionName string `json:"session_name"`
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
		Migrated:      []rebuildMigrated{},
		Registered:    []rebuildRegistered{},
		Conflicts:     []rebuildConflict{},
	}
	for _, m := range report.Migrated {
		env.Migrated = append(env.Migrated, rebuildMigrated{
			Subject: m.Subject, Action: m.Action, Into: m.Into, Detail: m.Detail,
		})
	}
	for _, r := range report.Registered {
		env.Registered = append(env.Registered, rebuildRegistered{
			ID:          r.ID,
			Slug:        r.Slug,
			RepoRoot:    r.RepoRoot,
			SessionName: r.Session,
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
	if len(env.Registered) == 0 && len(env.Conflicts) == 0 && len(env.Migrated) == 0 {
		fmt.Fprintln(w, "nothing to rebuild: every live session is already recorded")
		return nil
	}
	registered := "registered"
	if env.DryRun {
		registered = "would-register"
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	// Migrations print before registrations, since a collapse explains the
	// registration that follows it.
	for _, m := range env.Migrated {
		trailer := m.Into
		if m.Detail != "" {
			if trailer != "" {
				trailer += " "
			}
			trailer += "container " + m.Detail
		}
		fmt.Fprintln(tw, cells(m.Action, m.Subject, trailer))
	}
	for _, r := range env.Registered {
		fmt.Fprintln(tw, cells(registered, r.Slug, r.SessionName))
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
	applier := &rebuild.Applier{
		Store:    st,
		Repos:    st,
		Sessions: newSessionObserver(),
		Retagger: newSessionRetagger(),
		Resolver: worktreeResolver{},
		Config:   configDigests{root: configRoot, defaults: defaults},
		Locker:   stateLocker{dir: filepath.Join(stateRoot, "locks")},
		Clock:    systemClock{},
		DryRun:   dryRun,
	}

	// The upgrade pass runs before the records are read: it moves rows and
	// rewrites the workspace IDs live sessions claim, so a classification
	// over the pre-pass state would register duplicates for sessions that
	// are already running (design §9).
	migration := applier.Migrate(ctx, live)

	records, err := st.Workspaces()
	if err != nil {
		return rebuild.Report{}, fmt.Errorf("reading stored workspaces: %w", err)
	}

	report := applier.Apply(ctx, rebuild.Classify(migration.Live, records))
	report.Migrated = migration.Migrated
	report.Conflicts = append(report.Conflicts, migration.Conflicts...)
	return report, nil
}

// worktreeResolver re-derives a workspace's identity the way every other
// command does: from the directory, never from the tmux keys. That is
// what recovers the proposed session name, which tmux does not carry
// (spec §3), and it is what lets rebuild verify the keys it was handed.
type worktreeResolver struct{}

func (worktreeResolver) Resolve(repoRoot string) (resolve.Workspace, error) {
	// No name and no roots: roots feed only lookup by name, and rebuild
	// resolves from a directory.
	return resolve.Resolve("", nil, repoRoot)
}

// Exists separates "the directory is gone" from "git would not answer",
// which Resolve reports identically. A row whose path is gone is dropped;
// a row whose path is present is never discarded on a resolution failure.
func (worktreeResolver) Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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

// stateLocker is the filesystem lock every mutating command takes before
// its final observation and holds through the resulting state commit. The
// key is a workspace ID or a repository ID depending on what is being
// changed; the lock directory is the same either way.
type stateLocker struct{ dir string }

func (w stateLocker) Lock(ctx context.Context, key string) (func(), error) {
	l, err := lock.Acquire(ctx, w.dir, key, lockTimeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}
