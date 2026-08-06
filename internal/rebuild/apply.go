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

// Resolver derives a workspace from a worktree directory. It is an
// interface rather than a direct call to resolve.Resolve so application
// is testable without a real git repository.
type Resolver interface {
	Resolve(worktree string) (resolve.Workspace, error)
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

// Locker takes the per-workspace lock and returns its release. The
// release is a plain func so the caller cannot forget which lock it
// belongs to.
type Locker interface {
	Lock(ctx context.Context, workspaceID string) (func(), error)
}

// Registered is one workspace rebuild recovered.
type Registered struct {
	ID        string
	Slug      string
	Worktree  string
	IsPrimary bool
	Session   string
}

// Report is what one rebuild run did and declined to do. Both slices are
// non-nil even when empty: the JSON envelope always carries both arrays.
type Report struct {
	DryRun     bool
	Registered []Registered
	Conflicts  []Conflict
}

// Applier turns a classified Plan into store writes. Every dependency is
// an interface so the whole of application is testable without git,
// tmux, or SQLite.
type Applier struct {
	Store    Store
	Sessions controller.SessionObserver
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

	ws, err := a.Resolver.Resolve(sess.Worktree)
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
		return nil, conflictf(sess.Name,
			"session carries workspace %s, slug %q, worktree %q, but %s resolves to "+
				"workspace %s, slug %q, worktree %q; refusing to register it",
			sess.WorkspaceID, sess.Slug, sess.Worktree,
			sess.Worktree, ws.ID, ws.Slug, ws.Worktree)
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
		// Everything above is read-only, which is exactly what lets a dry
		// run predict the real run's verdict and exit code (spec §2). A
		// preview that stopped after pure classification would report a
		// clean 0 for a vanished worktree the real run refuses.
		if digestErr != nil {
			return nil, conflictf(sess.Name,
				"loading the configuration for %q failed: %v", ws.Slug, digestErr)
		}
		return registeredFor(ws, sess.Name), nil
	}

	release, err := a.Locker.Lock(ctx, ws.ID)
	if err != nil {
		return nil, conflictf(sess.Name, "taking the workspace lock: %v", err)
	}
	defer release()

	return a.writeUnderLock(ctx, ws, cand, digest, digestErr)
}

// writeUnderLock re-observes, re-reads, and re-classifies with the lock
// held, then writes what that says rather than what the first pass said.
// The lock package's rule is that the observation a mutation is decided
// from must be taken after the lock (internal/lock/lock.go:1-5): the
// classification pass is a work list, not evidence.
//
// digestErr is the configuration failure from before the lock, if any. It
// is consulted only where a digest is actually written, so that it stops
// exactly the candidates that still need one.
func (a *Applier) writeUnderLock(ctx context.Context, ws resolve.Workspace, cand Candidate, digest string, digestErr error) (*Registered, *Conflict) {
	sess := cand.Session

	obs, err := a.Sessions.ObserveSession(ctx, controller.SessionQuery{
		WorkspaceID:    ws.ID,
		CandidateNames: []string{sess.Name},
	})
	if err != nil {
		return nil, conflictf(sess.Name, "re-observing tmux under the workspace lock: %v", err)
	}
	if obs.ByIdentity == nil {
		return nil, conflictf(sess.Name,
			"the session was no longer live when the workspace lock was taken; nothing was written")
	}
	live := *obs.ByIdentity

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
		// branch overwrites slug, worktree, is_primary, proposed_session,
		// and desired_digest (internal/state/store.go:43-49), which is the
		// exact opposite of the fill-only guarantee. Fill-only is a
		// property of which primitive each case calls, not of the
		// primitives themselves.
		if err := a.Store.AdoptSessionName(ws.ID, live.Name, now); err != nil {
			return nil, conflictf(live.Name, "adopting session name %q: %v", live.Name, err)
		}
	default:
		return nil, conflictf(live.Name,
			"the workspace state changed under the lock and no longer calls for recovery; nothing was written")
	}
	return registeredFor(ws, live.Name), nil
}

func registeredFor(ws resolve.Workspace, session string) *Registered {
	return &Registered{
		ID:        ws.ID,
		Slug:      ws.Slug,
		Worktree:  ws.Worktree,
		IsPrimary: ws.IsPrimary,
		Session:   session,
	}
}

func conflictf(subject, format string, args ...any) *Conflict {
	return &Conflict{Subject: subject, Reason: fmt.Sprintf(format, args...)}
}
