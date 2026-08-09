package controller

import (
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"time"

	"github.com/gambtho/projectmux/internal/bindpath"
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
	// BindWarning is set when the stored bind was unusable and the
	// repository root was used instead. An unusable bind is not fatal
	// (spec §4), so this is reported rather than returned as an error.
	BindWarning string
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
// support this controller was not given. The gate is capability-shaped
// rather than build-shaped: a controller constructed with a container
// actuator executes these actions instead of refusing, with no changes
// here. It fires when one was not supplied.
var ErrContainerActionUnsupported = errors.New(
	"this workspace requires container support, which is not implemented in this build")

// Ensure is the design-§9 convergence loop: lock, register, final
// observation under the lock, plan, then at most one external mutation
// followed by re-observation and a transactional commit. It returns
// typed refusals and never mutates on uncertainty.
func (c *Controller) Ensure(ctx context.Context, d Desired, intents []WindowIntent, lockDir string, lockTimeout time.Duration) (EnsureResult, error) {
	const opName = "open"
	release, err := lockPhases(ctx, lockDir,
		d.Workspace.RepositoryID, d.Workspace.ID, lockTimeout)
	if err != nil {
		return EnsureResult{}, err
	}
	defer release()

	if err := c.Store.RegisterWorkspace(d.Workspace, d.Digest, c.Clock.Now()); err != nil {
		return EnsureResult{}, fmt.Errorf("registering the workspace: %w", err)
	}

	// The bind is persisted inside the same critical section that
	// registers the workspace and before the observation below, so no
	// window is ever built from a bind that changed underneath it. It
	// persists even when the open fails afterwards: it is a declaration
	// about the session, not a side effect of a successful open (§4).
	if d.Bind != nil {
		if err := c.Store.SetBind(d.Workspace.ID, d.Bind, c.Clock.Now()); err != nil {
			c.recordFailure(d.Workspace.ID, opName, "recording the bind: "+err.Error())
			return EnsureResult{}, fmt.Errorf("recording the bind: %w", err)
		}
	}

	snap, err := c.Observe(ctx, d)
	if err != nil {
		// Best-effort: an Observe error is a store read failure, so this
		// write may fail too, but status should explain the open when it
		// can (open/attach spec §2).
		c.recordFailure(d.Workspace.ID, opName, "observing the workspace: "+err.Error())
		return EnsureResult{}, err
	}
	plan := BuildPlan(snap)

	if plan.Session == SessionActionRefuse {
		c.recordFailure(d.Workspace.ID, opName, plan.Refusal)
		return EnsureResult{}, &RefusalError{Reason: plan.Refusal}
	}

	containerObs, started, err := c.ensureContainer(ctx, d, snap, plan.Container, opName)
	if err != nil {
		return EnsureResult{}, err
	}

	base, bindWarning := resolveBindBase(d.Workspace.RepoRoot, snap.Stored)
	windows, err := renderWindows(intents, d, base, containerObs, c.ContainerAct)
	if err != nil {
		c.recordFailure(d.Workspace.ID, opName, err.Error())
		return EnsureResult{}, err
	}

	drifted := snap.Stored == nil || snap.Stored.AppliedDigest == nil ||
		*snap.Stored.AppliedDigest != d.Digest
	stale := started && wantsContainerWindows(intents, containerObs != nil)

	switch plan.Session {
	case SessionActionNone:
		if err := c.commitOutcome(d.Workspace.ID, opName, containerObs); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:                EnsureAlreadyRunning,
			Session:               snap.Session.ByIdentity.Name,
			Drifted:               drifted,
			Container:             containerObs,
			ContainerWindowsStale: stale,
			BindWarning:           bindWarning,
		}, nil

	case SessionActionAdopt:
		name := snap.Session.ByIdentity.Name
		if err := c.Store.AdoptSessionName(d.Workspace.ID, name, c.Clock.Now()); err != nil {
			var conflict *state.SessionNameConflictError
			if errors.As(err, &conflict) {
				reason := fmt.Sprintf(
					"session name %q is already recorded for another workspace; refusing to adopt it", name)
				c.recordFailure(d.Workspace.ID, opName, reason)
				return EnsureResult{}, &RefusalError{Reason: reason}
			}
			c.recordFailure(d.Workspace.ID, opName, "recording the adopted session name: "+err.Error())
			return EnsureResult{}, fmt.Errorf("recording the adopted session name: %w", err)
		}
		if err := c.commitOutcome(d.Workspace.ID, opName, containerObs); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:                EnsureAdopted,
			Session:               name,
			Drifted:               drifted,
			Container:             containerObs,
			ContainerWindowsStale: stale,
			BindWarning:           bindWarning,
		}, nil

	case SessionActionCreate:
		res, err := c.createSession(ctx, d, windows, containerObs)
		if err != nil {
			return EnsureResult{}, err
		}
		res.Container = containerObs
		res.BindWarning = bindWarning
		return res, nil
	}
	return EnsureResult{}, fmt.Errorf("unexpected session action %q", plan.Session)
}

// ensureContainer executes the plan's container action and returns the
// observation the rest of the pass uses (nil when no container is in
// play) plus whether a genuine start ran — an acquire's idempotent up
// onto an already-running container reports false, so replacement
// staleness is never claimed for it.
func (c *Controller) ensureContainer(ctx context.Context, d Desired, snap Snapshot, action ContainerAction, opName string) (*ContainerObservation, bool, error) {
	if action == ContainerActionProbeFirst {
		// One retry of the observation kind that failed (spec §4): a
		// stored binding re-probes; an unbound workspace re-discovers.
		// This retry is read-only observation, so it runs deliberately
		// even with a nil ContainerAct: the nil-actuator gate below
		// applies only to mutating actions (start/acquire), never to
		// resolving what a probe or discovery can tell us for free.
		retried, err := c.retryContainerObservation(ctx, d, snap)
		if err != nil {
			c.recordFailure(d.Workspace.ID, opName, "re-observing the container: "+err.Error())
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
			c.recordFailure(d.Workspace.ID, opName, ErrContainerActionUnsupported.Error())
			return nil, false, ErrContainerActionUnsupported
		}
		obs, err := c.ContainerAct.StartContainer(ctx, d.Workspace, d.Config)
		if err != nil {
			c.recordStartFailure(d.Workspace.ID, opName, err)
			return nil, false, fmt.Errorf("starting the container: %w", err)
		}
		// Acquire is a no-op up on an already-running container: the
		// container windows already in play were never replaced, so only a
		// real start (the container was missing) makes them stale.
		return &obs, action == ContainerActionStart, nil
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
func renderWindows(intents []WindowIntent, d Desired, base bindBase, container *ContainerObservation, act ContainerActuator) ([]WindowSpec, error) {
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
			panes := make([]PaneSpec, 0, len(in.Panes))
			for _, p := range in.Panes {
				// A pane inherits the window's directory unless it sets
				// its own; inside the container that is the exec relDir,
				// while the host-side -c stays the session's base
				// directory, matching the window itself.
				relDir := in.RelDir
				if p.RelDir != "" {
					relDir = p.RelDir
				}
				panes = append(panes, PaneSpec{
					Name:    p.Name,
					Command: act.ExecCommand(binding, p.Command, base.containerDir(relDir), d.Config.Environment),
					Dir:     base.Host,
					Focus:   p.Focus,
				})
			}
			specs = append(specs, WindowSpec{
				Name:    in.Name,
				Command: act.ExecCommand(binding, in.Command, base.containerDir(in.RelDir), d.Config.Environment),
				Dir:     base.Host,
				Focus:   in.Focus,
				Panes:   panes,
			})
			continue
		}
		dir := base.hostDir(in.RelDir)
		panes := make([]PaneSpec, 0, len(in.Panes))
		for _, p := range in.Panes {
			paneDir := dir
			if p.RelDir != "" {
				paneDir = base.hostDir(p.RelDir)
			}
			panes = append(panes, PaneSpec{
				Name: p.Name, Command: p.Command, Dir: paneDir, Focus: p.Focus,
			})
		}
		specs = append(specs, WindowSpec{
			Name: in.Name, Command: in.Command, Dir: dir, Focus: in.Focus, Panes: panes,
		})
	}
	return specs, nil
}

// bindBase is the session's base directory, computed once per Ensure and
// threaded to every site that turns a RelDir into a path — rather than
// prefixing at five call sites, where a sixth would eventually forget.
// Host is the absolute directory host-side paths join under; Rel is the
// repository-relative form folded into the container adapter's relDir,
// since container/exec.go joins that onto the binding's workdir. With no
// bind, Host is the repository root and Rel is "", so every join is a
// no-op.
type bindBase struct {
	Host string
	Rel  string
}

func (b bindBase) hostDir(relDir string) string {
	if relDir == "" {
		return b.Host
	}
	return filepath.Join(b.Host, relDir)
}

// containerDir composes with POSIX semantics: both halves are
// slash-separated container-side paths, never host paths.
func (b bindBase) containerDir(relDir string) string {
	return path.Join(b.Rel, relDir)
}

// resolveBindBase turns the stored bind into the session's base
// directory, and returns a non-empty warning when the bind was unusable
// and the repository root was substituted. Containment is re-verified
// here rather than trusted from bind time: a stored in-repository path
// can later be replaced by a symlink pointing outside the repository
// (spec §4). An unusable bind is reported, never followed and never
// fatal.
func resolveBindBase(repoRoot string, stored *state.Record) (bindBase, string) {
	root := bindBase{Host: repoRoot}
	if stored == nil || stored.Bind == nil || *stored.Bind == "" {
		return root, ""
	}
	abs, err := bindpath.Resolve(repoRoot, *stored.Bind)
	if err != nil {
		return root, fmt.Sprintf(
			"the bind %q is unusable, so this session opened at the repository root instead: %v",
			*stored.Bind, err)
	}
	return bindBase{Host: abs, Rel: *stored.Bind}, ""
}

// BindWarning reports why a stored bind cannot be used, or "" when it is
// usable. It is exactly the check Ensure runs before rendering windows,
// exported so that status can report the condition without ensuring
// anything — one derivation, so the two commands cannot drift.
func BindWarning(repoRoot string, stored *state.Record) string {
	_, warning := resolveBindBase(repoRoot, stored)
	return warning
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

// commitOutcome records a successful operation, carrying the container
// observation into the same transaction when one exists.
func (c *Controller) commitOutcome(workspaceID, opName string, obs *ContainerObservation) error {
	op := state.Operation{Name: opName, Outcome: state.OutcomeOK}
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
func (c *Controller) recordStartFailure(workspaceID, opName string, err error) {
	op := state.Operation{Name: opName, Outcome: state.OutcomeFailed, ErrorSummary: err.Error()}
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
	const opName = "open"
	id := d.Workspace.ID
	name, err := c.Store.AllocateSessionName(id, c.Clock.Now())
	if err != nil {
		c.recordFailure(id, opName, "allocating a session name: "+err.Error())
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
		c.recordFailure(id, opName, reason)
		return EnsureResult{}, &RefusalError{Reason: reason}
	}
	if len(occ.ByName) > 0 {
		reason := fmt.Sprintf(
			"session %q exists but does not belong to this workspace; refusing to create over it",
			name)
		c.recordFailure(id, opName, reason)
		return EnsureResult{}, &RefusalError{Reason: reason}
	}

	spec := SessionSpec{
		Name:        name,
		WorkspaceID: id,
		Slug:        d.Workspace.Slug,
		Worktree:    d.Workspace.RepoRoot,
		Session:     d.Workspace.Session,
		Env:         d.Config.Environment,
		Windows:     windows,
	}
	if err := c.Actuator.CreateSession(ctx, spec); err != nil {
		c.recordFailure(id, opName, "creating the session: "+err.Error())
		return EnsureResult{}, fmt.Errorf("creating the session: %w", err)
	}

	// Post-create confirmation (open/attach spec §5): Observe reports
	// failures as snapshot uncertainty, never through its error return,
	// so only the observed shape below proves the creation.
	confirm, err := c.Observe(ctx, d)
	if err != nil {
		c.recordFailure(id, opName, "confirming the created session: "+err.Error())
		return EnsureResult{}, err
	}
	if reason := confirmCreation(confirm, d, name); reason != "" {
		c.recordFailure(id, opName, reason)
		return EnsureResult{}, fmt.Errorf("the created session could not be confirmed: %s", reason)
	}

	digest := d.Digest
	if err := c.Store.CommitReconciliation(id, state.ReconciliationResult{
		AppliedDigest: &digest,
		Container:     toStateObservation(containerObs),
		Operation:     state.Operation{Name: opName, Outcome: state.OutcomeOK},
	}, c.Clock.Now()); err != nil {
		c.recordFailure(id, opName, "committing the reconciliation: "+err.Error())
		return EnsureResult{}, fmt.Errorf("committing the reconciliation: %w", err)
	}
	return EnsureResult{Action: EnsureCreated, Session: name, Drifted: false}, nil
}

// confirmCreation reports why the post-create observation does not
// confirm the created session, or "" when it does: live, agreeing on
// all four identity keys, under the allocated name.
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
		live.Worktree != d.Workspace.RepoRoot || live.Session != d.Workspace.Session {
		return fmt.Sprintf("session %q carries contradictory identity keys after creation", live.Name)
	}
	if live.Name != allocated {
		return fmt.Sprintf(
			"the identity-matched session is named %q, not the allocated %q", live.Name, allocated)
	}
	return ""
}

// recordFailure best-effort records a failed operation under opName. The primary error is
// what the caller returns; a failing record write must not mask it.
func (c *Controller) recordFailure(workspaceID, opName, summary string) {
	_ = c.Store.RecordOperation(workspaceID, state.Operation{
		Name:         opName,
		Outcome:      state.OutcomeFailed,
		ErrorSummary: summary,
	}, c.Clock.Now())
}
