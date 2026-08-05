package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/state"
)

// EnsureAction classifies what Ensure did about the session.
type EnsureAction string

const (
	EnsureCreated        EnsureAction = "created"
	EnsureAdopted        EnsureAction = "adopted"
	EnsureAlreadyRunning EnsureAction = "already-running"
)

// EnsureResult reports a successful Ensure. Drifted mirrors the digest
// comparison at observation time; a fresh creation is never drifted.
type EnsureResult struct {
	Action  EnsureAction
	Session string
	Drifted bool
}

// RefusalError carries a refusal out of Ensure or attach; the CLI maps
// it to exit 6. Refusals are conflicts or uncertainty — retrying blindly
// is wrong, which is why they are distinguishable from generic failure.
type RefusalError struct{ Reason string }

func (e *RefusalError) Error() string { return e.Reason }

// ErrContainerActionUnsupported reports a plan requiring container
// support this build does not have. The gate is capability-shaped: a
// controller with a container actuator (a later slice) executes these
// actions instead of refusing, without changes here.
var ErrContainerActionUnsupported = errors.New(
	"this workspace requires container support, which is not implemented in this build")

// Ensure is the design-§9 convergence loop: lock, register, final
// observation under the lock, plan, then at most one external mutation
// followed by re-observation and a transactional commit. It returns
// typed refusals and never mutates on uncertainty.
func (c *Controller) Ensure(ctx context.Context, d Desired, windows []WindowSpec, lockDir string, lockTimeout time.Duration) (EnsureResult, error) {
	lk, err := lock.Acquire(ctx, lockDir, d.Workspace.ID, lockTimeout)
	if err != nil {
		return EnsureResult{}, err
	}
	defer lk.Release()

	if err := c.Store.RegisterWorkspace(d.Workspace, d.Digest, c.Clock.Now()); err != nil {
		return EnsureResult{}, fmt.Errorf("registering the workspace: %w", err)
	}

	snap, err := c.Observe(ctx, d)
	if err != nil {
		return EnsureResult{}, err
	}
	plan := BuildPlan(snap)

	if plan.Session == SessionActionRefuse {
		c.recordFailure(d.Workspace.ID, plan.Refusal)
		return EnsureResult{}, &RefusalError{Reason: plan.Refusal}
	}
	if plan.Container != ContainerActionNone {
		// No container actuator exists in this build (open/attach spec
		// §5 step 6); the container slice executes these instead.
		c.recordFailure(d.Workspace.ID, ErrContainerActionUnsupported.Error())
		return EnsureResult{}, ErrContainerActionUnsupported
	}

	drifted := snap.Stored == nil || snap.Stored.AppliedDigest == nil ||
		*snap.Stored.AppliedDigest != d.Digest

	switch plan.Session {
	case SessionActionNone:
		if err := c.recordOK(d.Workspace.ID); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:  EnsureAlreadyRunning,
			Session: snap.Session.ByIdentity.Name,
			Drifted: drifted,
		}, nil

	case SessionActionAdopt:
		name := snap.Session.ByIdentity.Name
		if err := c.Store.AdoptSessionName(d.Workspace.ID, name, c.Clock.Now()); err != nil {
			var conflict *state.SessionNameConflictError
			if errors.As(err, &conflict) {
				reason := fmt.Sprintf(
					"session name %q is already recorded for another workspace; refusing to adopt it", name)
				c.recordFailure(d.Workspace.ID, reason)
				return EnsureResult{}, &RefusalError{Reason: reason}
			}
			return EnsureResult{}, fmt.Errorf("recording the adopted session name: %w", err)
		}
		if err := c.recordOK(d.Workspace.ID); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{Action: EnsureAdopted, Session: name, Drifted: drifted}, nil

	case SessionActionCreate:
		return c.createSession(ctx, d, windows)
	}
	return EnsureResult{}, fmt.Errorf("unexpected session action %q", plan.Session)
}

func (c *Controller) createSession(ctx context.Context, d Desired, windows []WindowSpec) (EnsureResult, error) {
	id := d.Workspace.ID
	name, err := c.Store.AllocateSessionName(id, c.Clock.Now())
	if err != nil {
		c.recordFailure(id, "allocating a session name: "+err.Error())
		return EnsureResult{}, fmt.Errorf("allocating a session name: %w", err)
	}

	// Allocated-name squat check (open/attach spec §5): the initial
	// observation queried only the proposed and previously recorded
	// names; the allocation may be a suffixed variant a foreign live
	// session holds. The plan said create, so any occupant is foreign.
	occ, err := c.Sessions.ObserveSession(ctx, SessionQuery{
		WorkspaceID:    id,
		CandidateNames: []string{name},
	})
	if err != nil {
		reason := "tmux could not be observed before creating the session; refusing to act"
		c.recordFailure(id, reason)
		return EnsureResult{}, &RefusalError{Reason: reason}
	}
	if len(occ.ByName) > 0 {
		reason := fmt.Sprintf(
			"session %q exists but does not belong to this workspace; refusing to create over it",
			name)
		c.recordFailure(id, reason)
		return EnsureResult{}, &RefusalError{Reason: reason}
	}

	spec := SessionSpec{
		Name:        name,
		WorkspaceID: id,
		Slug:        d.Workspace.Slug,
		Worktree:    d.Workspace.Worktree,
		Env:         d.Config.Environment,
		Windows:     windows,
	}
	if err := c.Actuator.CreateSession(ctx, spec); err != nil {
		c.recordFailure(id, "creating the session: "+err.Error())
		return EnsureResult{}, fmt.Errorf("creating the session: %w", err)
	}

	// Post-create confirmation (open/attach spec §5): Observe reports
	// failures as snapshot uncertainty, never through its error return,
	// so only the observed shape below proves the creation.
	confirm, err := c.Observe(ctx, d)
	if err != nil {
		return EnsureResult{}, err
	}
	if reason := confirmCreation(confirm, d, name); reason != "" {
		c.recordFailure(id, reason)
		return EnsureResult{}, fmt.Errorf("the created session could not be confirmed: %s", reason)
	}

	digest := d.Digest
	if err := c.Store.CommitReconciliation(id, state.ReconciliationResult{
		AppliedDigest: &digest,
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, c.Clock.Now()); err != nil {
		return EnsureResult{}, fmt.Errorf("committing the reconciliation: %w", err)
	}
	return EnsureResult{Action: EnsureCreated, Session: name, Drifted: false}, nil
}

// confirmCreation reports why the post-create observation does not
// confirm the created session, or "" when it does: live, agreeing on
// all three identity keys, under the allocated name.
func confirmCreation(snap Snapshot, d Desired, allocated string) string {
	switch snap.Session.State {
	case SessionUnknown:
		return "tmux became unobservable after creation"
	case SessionAbsent:
		return "the session is absent after creation"
	}
	live := snap.Session.ByIdentity
	if live == nil {
		return "no identity-matched session was observed after creation"
	}
	if live.WorkspaceID != d.Workspace.ID || live.Slug != d.Workspace.Slug ||
		live.Worktree != d.Workspace.Worktree {
		return fmt.Sprintf("session %q carries contradictory identity keys after creation", live.Name)
	}
	if live.Name != allocated {
		return fmt.Sprintf(
			"the identity-matched session is named %q, not the allocated %q", live.Name, allocated)
	}
	return ""
}

// recordFailure best-effort records a failed open. The primary error is
// what the caller returns; a failing record write must not mask it.
func (c *Controller) recordFailure(workspaceID, summary string) {
	_ = c.Store.RecordOperation(workspaceID, state.Operation{
		Name:         "open",
		Outcome:      state.OutcomeFailed,
		ErrorSummary: summary,
	}, c.Clock.Now())
}

func (c *Controller) recordOK(workspaceID string) error {
	if err := c.Store.RecordOperation(workspaceID, state.Operation{
		Name:    "open",
		Outcome: state.OutcomeOK,
	}, c.Clock.Now()); err != nil {
		return fmt.Errorf("recording the operation: %w", err)
	}
	return nil
}
