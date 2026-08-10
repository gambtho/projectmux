package rebuild

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// Resolver derives a workspace from a repository root. It is an interface
// rather than a direct call to resolve.Resolve so application is testable
// without a real git repository.
//
// Exists is separate from Resolve's error because the two failures call
// for opposite actions: a path the filesystem no longer has is a row to
// drop, while a path that is present but will not resolve is a refusal.
// Resolve reports both as a plain error, so the distinction has to be
// asked for.
type Resolver interface {
	Resolve(repoRoot, session string) (resolve.Workspace, error)
	Exists(path string) bool
}

// ConfigLoader returns the desired digest for a workspace slug.
type ConfigLoader interface {
	Digest(slug string) (string, error)
}

// Store is the slice of the state store rebuild writes through. It is
// deliberately narrower than controller.Store: rebuild adds no new state
// mutation, it only composes the two that already exist.
type Store interface {
	Workspace(id string) (state.Record, error)
	Workspaces() ([]state.Record, error)
	RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error
	AdoptSessionName(workspaceID, name string, now time.Time) error
}

// Locker takes a filesystem lock and returns its release. The release is a
// plain func so the caller cannot forget which lock it belongs to. The key
// is a workspace ID for session work and a repository ID for work that is
// shared by every session on a repository.
type Locker interface {
	Lock(ctx context.Context, key string) (func(), error)
}

// Registered is one workspace rebuild recovered.
type Registered struct {
	ID       string
	Slug     string
	RepoRoot string
	Session  string
}

// Report is what one rebuild run did and declined to do. Every slice is
// non-nil even when empty: the JSON envelope always carries the arrays.
type Report struct {
	DryRun     bool
	Migrated   []Migrated
	Registered []Registered
	Conflicts  []Conflict
}

// Applier turns a classified Plan into store writes. Every dependency is
// an interface so the whole of application is testable without git,
// tmux, or SQLite.
type Applier struct {
	Store    Store
	Repos    MigrationStore
	Sessions controller.SessionObserver
	Retagger Retagger
	Resolver Resolver
	Config   ConfigLoader
	Locker   Locker
	Clock    controller.Clock
	DryRun   bool
}

// Apply works the plan candidate by candidate. One candidate's failure
// is that candidate's conflict, never the batch's, matching how
// autostart treats one workspace's failure.
func (a *Applier) Apply(ctx context.Context, plan Plan) Report {
	report := Report{
		DryRun:     a.DryRun,
		Migrated:   []Migrated{},
		Registered: []Registered{},
		Conflicts:  append([]Conflict{}, plan.Conflicts...),
	}
	for _, cand := range plan.Candidates {
		reg, conflict := a.applyCandidate(ctx, cand)
		switch {
		case conflict != nil:
			report.Conflicts = append(report.Conflicts, *conflict)
		case reg != nil:
			report.Registered = append(report.Registered, *reg)
		}
		// Both nil is a deliberate no-op, not an unhandled case: a
		// candidate found already settled under the lock did nothing and
		// refused nothing, so it belongs in neither list. Dropping it here
		// is what keeps that outcome a clean exit 0.
	}
	slices.SortFunc(report.Registered, func(a, b Registered) int {
		if c := cmp.Compare(a.Slug, b.Slug); c != 0 {
			return c
		}
		return cmp.Compare(a.Session, b.Session)
	})
	return report
}

// applyCandidate is a function rather than the body of Apply's loop so
// each candidate's lock release can be deferred: a bare defer inside the
// loop would hold every lock until the batch finished.
func (a *Applier) applyCandidate(ctx context.Context, cand Candidate) (*Registered, *Conflict) {
	sess := cand.Session

	// The session component is an input to the workspace ID (decision
	// 0001), so re-deriving identity without it resolves every named
	// session to its repository's default workspace and fails the gate
	// below as a false identity conflict.
	ws, err := a.Resolver.Resolve(sess.Worktree, sess.Session)
	if err != nil {
		return nil, conflictf(sess.Name,
			"resolving the workspace from %s failed: %v; a session whose worktree is gone cannot be re-registered",
			sess.Worktree, err)
	}

	// All three identity keys, not just the derived ID (spec §3). A
	// session carrying a stale or hand-set @dev_slug would otherwise be
	// registered from resolved values that silently disagree with it, and
	// the next rebuild would report that row as a mismatch conflict
	// instead of a clean no-op.
	if !controller.SessionBelongsTo(sess, ws) {
		return nil, identityConflict(sess, ws)
	}

	// Only registration writes a digest, so only registration needs one.
	// A workspace whose configuration is broken can still have its live
	// session adopted: adoption does not depend on the digest.
	//
	// The failure is carried rather than returned. The case decided here
	// is a work list, not a verdict — if a row appeared in the meantime
	// this candidate becomes an adoption under the lock, which needs no
	// digest at all. Refusing at the load would make the outcome depend
	// on a requirement the candidate may no longer have.
	var digest string
	var digestErr error
	if cand.Case == CaseRegister {
		digest, digestErr = a.Config.Digest(ws.Slug)
	}

	if a.DryRun {
		// A preview runs every step except the two that change something:
		// taking the lock and writing. It goes through the same finalize
		// as the real run so the two cannot drift — a preview that
		// answered from the first pass's case would report a clean 0 for a
		// workspace whose row moved underneath it, which is the one thing
		// --dry-run must never do (spec §2).
		return a.finalize(ctx, ws, cand, digest, digestErr)
	}

	release, err := a.Locker.Lock(ctx, ws.ID)
	if err != nil {
		return nil, conflictf(sess.Name, "taking the workspace lock: %v", err)
	}
	defer release()

	return a.finalize(ctx, ws, cand, digest, digestErr)
}

// finalize re-observes, re-reads, and re-classifies, then acts on what
// that says rather than on what the first pass said. The real run calls
// it with the lock held; the dry run calls it without one and returns
// before each write. The lock package's rule is that the observation a
// mutation is decided from must be taken after the lock
// (internal/lock/lock.go:1-5): the classification pass is a work list,
// not evidence.
//
// Everything it does up to a write is read-only, which is what lets the
// dry run share it and thereby predict the real run's report exactly,
// including the settled no-op and the digest a re-classification newly
// requires.
//
// digestErr is the configuration failure from before the lock, if any. It
// is consulted only where a digest is actually written, so that it stops
// exactly the candidates that still need one.
func (a *Applier) finalize(ctx context.Context, ws resolve.Workspace, cand Candidate, digest string, digestErr error) (*Registered, *Conflict) {
	sess := cand.Session

	observed, conflict := a.observeLive(ctx, ws, sess)
	if conflict != nil {
		return nil, conflict
	}
	live := *observed

	var records []state.Record
	rec, err := a.Store.Workspace(ws.ID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// No row: the register case, unless the re-classification says
		// otherwise.
	case err != nil:
		return nil, conflictf(live.Name, "re-reading the workspace record under the lock: %v", err)
	default:
		records = []state.Record{rec}
	}

	final := Classify([]controller.LiveSession{live}, records)
	if len(final.Candidates) != 1 {
		// A row that already records this session is the settled case:
		// another rebuild or an open completed the workspace in the gap
		// this re-classification exists to absorb. The desired end state
		// is confirmed to hold, so the run did nothing and refused
		// nothing — reporting a conflict would map a verified ok onto the
		// refusal exit code, which is the tri-state rule inverted.
		//
		// Confirmed from the row rather than inferred from Classify's
		// silence about settled sessions: silence also covers sessions it
		// ignored, and an ignored session is an unknown, not an ok.
		if len(records) == 1 && records[0].ActualSession != nil &&
			*records[0].ActualSession == live.Name {
			return nil, nil
		}
		// Everything else really did change adversely under the lock.
		reason := "the workspace no longer needs recovery: something else completed it " +
			"before the lock was taken; nothing was written"
		if len(final.Conflicts) > 0 {
			reason = final.Conflicts[0].Reason
		}
		return nil, &Conflict{Subject: live.Name, Reason: reason}
	}

	now := a.Clock.Now()
	switch final.Candidates[0].Case {
	case CaseRegister:
		if cand.Case != CaseRegister {
			// The row the first pass saw has since disappeared. The store
			// has no delete primitive, so this needs an external actor —
			// load the digest now rather than registering an empty one.
			digest, digestErr = a.Config.Digest(ws.Slug)
		}
		// This is the one branch that writes a digest, so it is the one
		// branch the failure stops — whether it came from before the lock
		// or from the re-load just above.
		if digestErr != nil {
			return nil, conflictf(live.Name,
				"loading the configuration for %q failed: %v", ws.Slug, digestErr)
		}
		if a.DryRun {
			return registeredFor(ws, live.Name), nil
		}
		if err := a.Store.RegisterWorkspace(ws, digest, now); err != nil {
			return nil, conflictf(live.Name, "registering workspace %s: %v", ws.Slug, err)
		}
		if err := a.Store.AdoptSessionName(ws.ID, live.Name, now); err != nil {
			// Registration and adoption are separate transactions, so this
			// leaves a row with no recorded session. That is not
			// corruption — it is precisely the adopt case, which the next
			// run completes. Both halves are named because reporting only
			// the failure would tell the operator nothing was written.
			return nil, conflictf(live.Name,
				"workspace %s was registered, but adopting session name %q failed: %v; "+
					"the row has no recorded session and a later rebuild will complete it",
				ws.Slug, live.Name, err)
		}
	case CaseAdopt:
		// Never RegisterWorkspace here. It is an upsert whose conflict
		// branch overwrites slug, repository root, proposed_session,
		// and desired_digest (internal/state/store.go:43-49), which is the
		// exact opposite of the fill-only guarantee. Fill-only is a
		// property of which primitive each case calls, not of the
		// primitives themselves.
		if a.DryRun {
			return registeredFor(ws, live.Name), nil
		}
		if err := a.Store.AdoptSessionName(ws.ID, live.Name, now); err != nil {
			return nil, conflictf(live.Name, "adopting session name %q: %v", live.Name, err)
		}
	default:
		return nil, conflictf(live.Name,
			"the workspace state changed under the lock and no longer calls for recovery; nothing was written")
	}
	return registeredFor(ws, live.Name), nil
}

// observeLive re-observes the workspace's session and returns it only
// when it is confirmed present and its identity keys agree with the
// workspace. Both the lock-held write and the dry run call it: the
// observation is read-only, so a preview can make it and thereby predict
// the refusals it discovers.
//
// The refusals are worded identically on both paths — report parity is
// asserted with reflect.DeepEqual — so they cannot claim a lock the dry
// run never takes.
//
// This is also what closes the duplicate-workspace-ID race for
// finalize's re-classification: matchSessions
// (internal/tmux/decode.go:63-73) errors the moment a second session
// claims the queried workspace ID, so a duplicate becomes a conflict here
// before re-classification ever runs against a single live session. That
// guarantee lives in this production observer, not in the
// controller.SessionObserver interface contract — a future observer that
// picked a claimant instead of erroring would silently reopen the race.
func (a *Applier) observeLive(ctx context.Context, ws resolve.Workspace, sess controller.LiveSession) (*controller.LiveSession, *Conflict) {
	obs, err := a.Sessions.ObserveSession(ctx, controller.SessionQuery{
		WorkspaceID:    ws.ID,
		CandidateNames: []string{sess.Name},
	})
	if err != nil {
		return nil, conflictf(sess.Name, "re-observing tmux before writing: %v", err)
	}
	if obs.ByIdentity == nil {
		return nil, conflictf(sess.Name,
			"the session was no longer live when rebuild went to write; nothing was written")
	}
	live := *obs.ByIdentity

	// The observer matches on the workspace-ID tag alone
	// (internal/tmux/decode.go:63-67), so this may not be the session the
	// pre-lock gate validated: another session could have acquired the tag
	// with a contradictory @dev_slug or @dev_worktree. Planning re-checks
	// ByIdentity for exactly this reason (internal/controller/plan.go:87,
	// design §7), and the writes are made from live, not from sess.
	if !controller.SessionBelongsTo(live, ws) {
		return nil, identityConflict(live, ws)
	}
	return &live, nil
}

// registeredFor reports the resolver's identity for ws rather than the
// stored row's. The identity gate above requires the two to agree on slug
// and repository root, so the only field that can differ is the session
// name, which is what this run just recorded.
func registeredFor(ws resolve.Workspace, session string) *Registered {
	return &Registered{
		ID:       ws.ID,
		Slug:     ws.Slug,
		RepoRoot: ws.RepoRoot,
		Session:  session,
	}
}

func conflictf(subject, format string, args ...any) *Conflict {
	return &Conflict{Subject: subject, Reason: fmt.Sprintf(format, args...)}
}

// identityConflict is the refusal shared by the pre-lock gate and the
// lock-held re-check. One function so the two can never drift: they are
// the same finding about two different sessions, and an operator who
// sees one has to be able to read it the same way as the other.
func identityConflict(s controller.LiveSession, ws resolve.Workspace) *Conflict {
	return conflictf(s.Name,
		"session carries workspace %s, slug %q, worktree %q, but %s resolves to "+
			"workspace %s, slug %q, worktree %q; refusing to register it",
		s.WorkspaceID, s.Slug, s.Worktree,
		s.Worktree, ws.ID, ws.Slug, ws.RepoRoot)
}
