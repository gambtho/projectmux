package rebuild

import (
	"context"
	"slices"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// MigrationStore is the slice of the state store the upgrade pass writes
// through. It is separate from Store because the two answer to different
// invariants: Store is fill-only, and this one deletes.
type MigrationStore interface {
	Repositories() ([]state.Repository, error)
	DropRepository(id string) error
}

// Retagger rewrites the identity keys of a live tmux session.
type Retagger interface {
	RetagSession(ctx context.Context, target, workspaceID, repoRoot string) error
}

// Migrated is one correction the upgrade pass made. Action is
// "collapsed", "dropped", "retagged", or "binding-discarded"; Into is the
// repository root the subject now belongs to, and is empty for a drop.
// Detail carries what an operator needs to finish the correction by hand
// and is empty unless the action leaves something behind.
type Migrated struct {
	Subject string
	Action  string
	Into    string
	Detail  string
}

// MigrationResult is the pass's output. Live carries the sessions
// classification should see — corrected where they were stale — rather
// than the ones tmux reported.
type MigrationResult struct {
	Live      []controller.LiveSession
	Migrated  []Migrated
	Conflicts []Conflict
}

// Migrate completes what migration 0002 could not. 0002 is pure SQL and
// moved every stored row verbatim, treating each recorded path as a
// repository root, because telling a main worktree from a linked one
// requires git and a schema migration must never fail on a filesystem
// that changed since the last run (design §9). The result is
// over-counted, never wrong: extra repository rows that resolve to the
// same real repository. This is the pass that merges them.
//
// It runs before classification, not as part of it, because both halves
// change the inputs Classify reads: rows move, and live sessions change
// the workspace ID they claim.
func (a *Applier) Migrate(ctx context.Context, live []controller.LiveSession) MigrationResult {
	res := MigrationResult{Live: live}
	a.collapseRows(ctx, &res)
	a.retagSessions(ctx, &res)
	return res
}

// collapseRows folds every repository row that is really a linked
// worktree into its parent, and drops the rows whose path is gone.
func (a *Applier) collapseRows(ctx context.Context, res *MigrationResult) {
	repos, err := a.Repos.Repositories()
	if err != nil {
		res.Conflicts = append(res.Conflicts, Conflict{
			Subject: "repositories",
			Reason: "reading the stored repositories failed: " + err.Error() +
				"; nothing was migrated",
		})
		return
	}

	for _, repo := range repos {
		ws, resolveErr := a.Resolver.Resolve(repo.RepoRoot, "")
		if resolveErr != nil {
			if a.Resolver.Exists(repo.RepoRoot) {
				// The directory is there and git would not answer for it.
				// Dropping the row here would discard state over a
				// transient failure, so this refuses instead.
				res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
					"resolving the repository failed: %v; the directory still exists, "+
						"so the row is kept and nothing was written", resolveErr))
				continue
			}
			dropped := append([]Migrated{{
				Subject: repo.RepoRoot, Action: "dropped",
			}}, appendDiscardedBinding(nil, repo, "")...)
			if a.DryRun {
				// A preview reports what the run would do; nothing is
				// attempted, so there is no outcome to wait for.
				res.Migrated = append(res.Migrated, dropped...)
				continue
			}
			if err := a.Repos.DropRepository(repo.ID); err != nil {
				res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
					"dropping the repository whose path is gone failed: %v", err))
				continue
			}
			res.Migrated = append(res.Migrated, dropped...)
			continue
		}
		if ws.RepoRoot == repo.RepoRoot {
			// Already a main worktree. Deliberately silent, so a second
			// run over a migrated installation reports nothing.
			continue
		}
		a.collapseInto(ctx, repo, ws, res)
	}
}

// collapseInto registers the parent repository and then drops the stale
// row. The order is load-bearing: a crash between the two leaves an extra
// row that resolves to the same repository — the over-counted state this
// pass already knows how to merge — where the other order would lose the
// registration outright.
//
// Parent wins on the container binding. Dropping the stale row cascades
// its container_bindings row away (0002_repositories.sql), so if both
// rows carried a binding the parent's survives and the stale one is
// discarded — and any container it named keeps running, unreferenced.
// The rule is deliberate rather than incidental: the parent is the row
// every session on the repository will key on after this pass, so
// overwriting its binding with the stale row's would move a live
// repository onto a container chosen for one worktree. The discard is
// reported instead, with the container ID, because reattaching or
// removing that container is a decision only the operator can make.
// A result is recorded only once the mutation it describes has succeeded.
// A dry run reports the whole intent, since nothing is attempted; a real
// run that fails partway reports a conflict and no success beside it.
func (a *Applier) collapseInto(ctx context.Context, repo state.Repository, ws resolve.Workspace, res *MigrationResult) {
	collapsed := append([]Migrated{{
		Subject: repo.RepoRoot, Action: "collapsed", Into: ws.RepoRoot,
	}}, appendDiscardedBinding(nil, repo, ws.RepoRoot)...)
	if a.DryRun {
		res.Migrated = append(res.Migrated, collapsed...)
		return
	}

	digest, err := a.Config.Digest(ws.Slug)
	if err != nil {
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"loading the configuration for %q failed: %v; nothing was written",
			ws.Slug, err))
		return
	}

	// The repository lock, not the workspace lock: this rewrites rows
	// every session on the repository shares. It is taken and released
	// here, before Apply takes any workspace lock, so the plan's global
	// repository-then-workspace ordering holds without nesting.
	release, err := a.Locker.Lock(ctx, ws.RepositoryID)
	if err != nil {
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"taking the repository lock: %v", err))
		return
	}
	defer release()

	if err := a.Store.RegisterWorkspace(ws, digest, a.Clock.Now()); err != nil {
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"registering the parent repository %s: %v", ws.RepoRoot, err))
		return
	}
	if err := a.Repos.DropRepository(repo.ID); err != nil {
		// Half of the collapse stands: the parent is registered and every
		// session will key on it. Reporting "collapsed" would overstate
		// that, and reporting nothing would hide a write that happened, so
		// the partial outcome gets its own action.
		res.Migrated = append(res.Migrated, Migrated{
			Subject: repo.RepoRoot, Action: "partially-collapsed", Into: ws.RepoRoot,
			Detail: "the parent repository was registered; the linked-worktree row remains",
		})
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"the parent repository %s was registered, but dropping the "+
				"linked-worktree row failed: %v; a later rebuild completes it",
			ws.RepoRoot, err))
		return
	}
	res.Migrated = append(res.Migrated, collapsed...)
}

// retagSessions points sessions created before the change at their
// repository.
//
// Existing workspace IDs change even for repositories that were already
// main worktrees, because the session name is now an input to the hash.
// The ID lives on the session as @dev_workspace_id, so a mismatch alone
// says nothing about whether this is the same workspace. The session is
// therefore matched by @dev_worktree — the one key whose value identifies
// a tree rather than a derivation — and the ID is rewritten rather than
// read as evidence of a different workspace.
//
// @dev_worktree keeps its name. Renaming it would strand every running
// session from the rebuild that is supposed to recover it (design §7);
// only its value changes, to the repository root.
//
// The retag target is not unique per session: resolve.Resolve derives the
// workspace ID from the repository, so every tree of one project resolves
// to the same ID. Retagging each session independently would therefore
// collide every session of a repository onto one ID — a state Classify's
// duplicate-ID case rejects on every later run, and one this pass could
// not undo, because the retag overwrites the only keys that told the
// sessions apart. The sessions are grouped by their resolved ID first and
// a group with more than one member is refused whole, which is the rule
// the rest of this package already follows: never mutate on uncertainty.
func (a *Applier) retagSessions(ctx context.Context, res *MigrationResult) {
	// Resolved targets are computed up front, in a pass that writes
	// nothing, so the collision is known before the first retag rather
	// than discovered halfway through one. A nil entry is a session this
	// pass has nothing to say about.
	targets := make([]*resolve.Workspace, len(res.Live))
	claimants := make(map[string][]string, len(res.Live))
	for i, sess := range res.Live {
		if sess.WorkspaceID == "" {
			continue
		}
		ws, err := a.Resolver.Resolve(sess.Worktree, sess.Session)
		if err != nil {
			// A session whose tree is gone is not this pass's problem:
			// applyCandidate already reports it, with the reason an
			// operator needs.
			continue
		}
		targets[i] = &ws
		claimants[ws.ID] = append(claimants[ws.ID], sess.Name)
	}
	// Sorted so the refusal reads the same however tmux ordered its
	// output, matching duplicateIDReason.
	for id := range claimants {
		slices.Sort(claimants[id])
	}

	for i, sess := range res.Live {
		ws := targets[i]
		if ws == nil {
			continue
		}
		if len(claimants[ws.ID]) > 1 {
			// Reported once per claimant rather than once per group, the
			// shape Classify uses for the same finding: the operator's
			// next step is to look at each named session and decide which
			// one survives. A session already carrying the right keys is
			// named too, because it is one of the claimants the operator
			// has to choose between.
			res.Conflicts = append(res.Conflicts, conflictAt(sess.Name,
				"sessions %s all resolve to workspace %s, so migrating them would "+
					"leave several sessions claiming one workspace; none of them was "+
					"retagged. Kill or rename all but one, then run rebuild again.",
				quotedList(claimants[ws.ID]), ws.ID))
			continue
		}
		if sess.WorkspaceID == ws.ID && sess.Worktree == ws.RepoRoot {
			continue
		}

		retagged := Migrated{
			Subject: sess.Name, Action: "retagged", Into: ws.RepoRoot,
		}
		if a.DryRun {
			res.Migrated = append(res.Migrated, retagged)
			continue
		}

		release, err := a.Locker.Lock(ctx, ws.ID)
		if err != nil {
			res.Conflicts = append(res.Conflicts, conflictAt(sess.Name,
				"taking the workspace lock: %v", err))
			continue
		}
		err = a.Retagger.RetagSession(ctx, sess.Name, ws.ID, ws.RepoRoot)
		release()
		if err != nil {
			res.Conflicts = append(res.Conflicts, conflictAt(sess.Name,
				"rewriting the identity keys of session %q failed: %v; the session "+
					"keeps its old keys and a later rebuild retries", sess.Name, err))
			continue
		}
		res.Migrated = append(res.Migrated, retagged)
		res.Live[i].WorkspaceID = ws.ID
		res.Live[i].Worktree = ws.RepoRoot
	}
}

// appendDiscardedBinding records that removing a repository row takes its
// container binding with it. Losing the row is correct; losing it in
// silence is not, because the container itself survives the cascade and
// nothing left in the database names it.
func appendDiscardedBinding(migrated []Migrated, repo state.Repository, into string) []Migrated {
	if repo.Container == nil || repo.Container.ContainerID == "" {
		return migrated
	}
	return append(migrated, Migrated{
		Subject: repo.RepoRoot,
		Action:  "binding-discarded",
		Into:    into,
		Detail:  repo.Container.ContainerID,
	})
}

func conflictAt(subject, format string, args ...any) Conflict {
	return *conflictf(subject, format, args...)
}
