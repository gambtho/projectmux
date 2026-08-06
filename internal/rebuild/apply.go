package rebuild

import (
	"cmp"
	"context"
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

	digest, err := a.Config.Digest(ws.Slug)
	if err != nil {
		return nil, conflictf(sess.Name,
			"loading the configuration for %q failed: %v", ws.Slug, err)
	}

	release, err := a.Locker.Lock(ctx, ws.ID)
	if err != nil {
		return nil, conflictf(sess.Name, "taking the workspace lock: %v", err)
	}
	defer release()

	now := a.Clock.Now()
	if err := a.Store.RegisterWorkspace(ws, digest, now); err != nil {
		return nil, conflictf(sess.Name, "registering workspace %s: %v", ws.Slug, err)
	}
	if err := a.Store.AdoptSessionName(ws.ID, sess.Name, now); err != nil {
		return nil, conflictf(sess.Name, "adopting session name %q: %v", sess.Name, err)
	}
	return registeredFor(ws, sess.Name), nil
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
