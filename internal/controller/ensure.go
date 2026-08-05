package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
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
	Action                EnsureAction
	Session               string
	Drifted               bool
	Container             *ContainerObservation // nil when no container is in play
	ContainerWindowsStale bool
}

// RefusalError carries a refusal out of Ensure or attach; the CLI maps
// it to exit 6. Refusals are conflicts or uncertainty — retrying blindly
// is wrong, which is why they are distinguishable from generic failure.
type RefusalError struct{ Reason string }

func (e *RefusalError) Error() string { return e.Reason }

// ContainerStartError preserves what design §9 requires the recorded
// operation to keep: the real exit status, a bounded stderr summary, and
// whether the start timed out. The container adapter returns it; the
// controller persists it.
type ContainerStartError struct {
	ExitCode int
	Stderr   string
	TimedOut bool
	Reason   string
}

func (e *ContainerStartError) Error() string {
	if e.TimedOut {
		return "devcontainer up timed out: " + e.Reason
	}
	return fmt.Sprintf("devcontainer up failed (exit %d): %s", e.ExitCode, e.Reason)
}

// ContainerWindowError reports a window demanding a container when none
// applies to the workspace.
type ContainerWindowError struct{ Window string }

func (e *ContainerWindowError) Error() string {
	return fmt.Sprintf(
		"window %q requires a container, but no container applies to this workspace", e.Window)
}

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
func (c *Controller) Ensure(ctx context.Context, d Desired, intents []WindowIntent, lockDir string, lockTimeout time.Duration) (EnsureResult, error) {
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
		// Best-effort: an Observe error is a store read failure, so this
		// write may fail too, but status should explain the open when it
		// can (open/attach spec §2).
		c.recordFailure(d.Workspace.ID, "observing the workspace: "+err.Error())
		return EnsureResult{}, err
	}
	plan := BuildPlan(snap)

	if plan.Session == SessionActionRefuse {
		c.recordFailure(d.Workspace.ID, plan.Refusal)
		return EnsureResult{}, &RefusalError{Reason: plan.Refusal}
	}

	containerObs, started, err := c.ensureContainer(ctx, d, snap, plan.Container)
	if err != nil {
		return EnsureResult{}, err
	}

	windows, err := renderWindows(intents, d, containerObs, c.ContainerAct)
	if err != nil {
		c.recordFailure(d.Workspace.ID, err.Error())
		return EnsureResult{}, err
	}

	drifted := snap.Stored == nil || snap.Stored.AppliedDigest == nil ||
		*snap.Stored.AppliedDigest != d.Digest
	stale := started && wantsContainerWindows(intents, containerObs != nil)

	switch plan.Session {
	case SessionActionNone:
		if err := c.commitOutcome(d.Workspace.ID, containerObs); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:                EnsureAlreadyRunning,
			Session:               snap.Session.ByIdentity.Name,
			Drifted:               drifted,
			Container:             containerObs,
			ContainerWindowsStale: stale,
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
			c.recordFailure(d.Workspace.ID, "recording the adopted session name: "+err.Error())
			return EnsureResult{}, fmt.Errorf("recording the adopted session name: %w", err)
		}
		if err := c.commitOutcome(d.Workspace.ID, containerObs); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:                EnsureAdopted,
			Session:               name,
			Drifted:               drifted,
			Container:             containerObs,
			ContainerWindowsStale: stale,
		}, nil

	case SessionActionCreate:
		res, err := c.createSession(ctx, d, windows, containerObs)
		if err != nil {
			return EnsureResult{}, err
		}
		res.Container = containerObs
		return res, nil
	}
	return EnsureResult{}, fmt.Errorf("unexpected session action %q", plan.Session)
}

// ensureContainer executes the plan's container action and returns the
// observation the rest of the pass uses (nil when no container is in
// play) plus whether devcontainer up ran.
func (c *Controller) ensureContainer(ctx context.Context, d Desired, snap Snapshot, action ContainerAction) (*ContainerObservation, bool, error) {
	if action == ContainerActionProbeFirst {
		// One retry of the observation kind that failed (spec §4): a
		// stored binding re-probes; an unbound workspace re-discovers.
		retried, err := c.retryContainerObservation(ctx, d, snap)
		if err != nil {
			c.recordFailure(d.Workspace.ID, "re-observing the container: "+err.Error())
			return nil, false, fmt.Errorf("re-observing the container: %w", err)
		}
		snap.Container = ContainerSnapshot{Observed: retried}
		action = containerAction(snap)
	}

	switch action {
	case ContainerActionNone:
		if o := snap.Container.Observed; o != nil && o.Health == state.HealthPresent {
			return o, false, nil
		}
		return nil, false, nil
	case ContainerActionStart, ContainerActionAcquire:
		if c.ContainerAct == nil {
			c.recordFailure(d.Workspace.ID, ErrContainerActionUnsupported.Error())
			return nil, false, ErrContainerActionUnsupported
		}
		obs, err := c.ContainerAct.StartContainer(ctx, d.Workspace, d.Config)
		if err != nil {
			c.recordStartFailure(d.Workspace.ID, err)
			return nil, false, fmt.Errorf("starting the container: %w", err)
		}
		return &obs, true, nil
	}
	return nil, false, fmt.Errorf("unexpected container action %q", action)
}

func (c *Controller) retryContainerObservation(ctx context.Context, d Desired, snap Snapshot) (*ContainerObservation, error) {
	if snap.Stored != nil && snap.Stored.Container != nil {
		obs, err := c.Containers.ProbeContainer(ctx, *snap.Stored.Container)
		if err != nil {
			return nil, err
		}
		return &obs, nil
	}
	return c.Containers.DiscoverContainer(ctx, d.Workspace, d.Config)
}

// renderWindows turns intents into concrete window specs, now that the
// binding (if any) exists. Auto follows the container; an explicit
// container demand without one is a typed error.
func renderWindows(intents []WindowIntent, d Desired, container *ContainerObservation, act ContainerActuator) ([]WindowSpec, error) {
	specs := make([]WindowSpec, 0, len(intents))
	for _, in := range intents {
		inContainer := false
		switch in.Location {
		case WindowContainer:
			if container == nil {
				return nil, &ContainerWindowError{Window: in.Name}
			}
			inContainer = true
		case WindowAuto:
			inContainer = container != nil
		}
		if inContainer {
			if act == nil {
				return nil, ErrContainerActionUnsupported
			}
			binding := state.ContainerBinding{
				Kind:          container.Kind,
				ContainerID:   container.ContainerID,
				ContainerUser: container.ContainerUser,
				Workdir:       container.Workdir,
			}
			specs = append(specs, WindowSpec{
				Name:    in.Name,
				Command: act.ExecCommand(binding, in.Command, in.RelDir, d.Config.Environment),
				Dir:     d.Workspace.Worktree,
				Focus:   in.Focus,
			})
			continue
		}
		dir := d.Workspace.Worktree
		if in.RelDir != "" {
			dir = filepath.Join(d.Workspace.Worktree, in.RelDir)
		}
		specs = append(specs, WindowSpec{
			Name: in.Name, Command: in.Command, Dir: dir, Focus: in.Focus,
		})
	}
	return specs, nil
}

// wantsContainerWindows reports whether any intent resolves to the
// container, given whether one applies.
func wantsContainerWindows(intents []WindowIntent, containerApplies bool) bool {
	for _, in := range intents {
		if in.Location == WindowContainer ||
			(in.Location == WindowAuto && containerApplies) {
			return true
		}
	}
	return false
}

// commitOutcome records a successful open, carrying the container
// observation into the same transaction when one exists.
func (c *Controller) commitOutcome(workspaceID string, obs *ContainerObservation) error {
	op := state.Operation{Name: "open", Outcome: state.OutcomeOK}
	if obs == nil {
		if err := c.Store.RecordOperation(workspaceID, op, c.Clock.Now()); err != nil {
			return fmt.Errorf("recording the operation: %w", err)
		}
		return nil
	}
	if err := c.Store.CommitReconciliation(workspaceID, state.ReconciliationResult{
		Container: toStateObservation(obs),
		Operation: op,
	}, c.Clock.Now()); err != nil {
		return fmt.Errorf("committing the outcome: %w", err)
	}
	return nil
}

func toStateObservation(obs *ContainerObservation) *state.ContainerObservation {
	if obs == nil {
		return nil
	}
	return &state.ContainerObservation{
		Kind:          obs.Kind,
		ContainerID:   obs.ContainerID,
		ContainerUser: obs.ContainerUser,
		Workdir:       obs.Workdir,
		Health:        obs.Health,
	}
}

// recordStartFailure persists a failed container start with the real
// exit status and bounded stderr (design §9).
func (c *Controller) recordStartFailure(workspaceID string, err error) {
	op := state.Operation{Name: "open", Outcome: state.OutcomeFailed, ErrorSummary: err.Error()}
	var start *ContainerStartError
	if errors.As(err, &start) {
		code := start.ExitCode
		op.ExitStatus = &code
		if start.Stderr != "" {
			op.ErrorSummary = start.Reason + ": " + start.Stderr
		}
	}
	_ = c.Store.RecordOperation(workspaceID, op, c.Clock.Now())
}

func (c *Controller) createSession(ctx context.Context, d Desired, windows []WindowSpec, containerObs *ContainerObservation) (EnsureResult, error) {
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
		c.recordFailure(id, "confirming the created session: "+err.Error())
		return EnsureResult{}, err
	}
	if reason := confirmCreation(confirm, d, name); reason != "" {
		c.recordFailure(id, reason)
		return EnsureResult{}, fmt.Errorf("the created session could not be confirmed: %s", reason)
	}

	digest := d.Digest
	if err := c.Store.CommitReconciliation(id, state.ReconciliationResult{
		AppliedDigest: &digest,
		Container:     toStateObservation(containerObs),
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, c.Clock.Now()); err != nil {
		c.recordFailure(id, "committing the reconciliation: "+err.Error())
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
