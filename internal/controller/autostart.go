package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// ContainerStartOutcome classifies what StartRepositoryContainer did.
type ContainerStartOutcome string

const (
	ContainerStarted        ContainerStartOutcome = "started"
	ContainerAlreadyRunning ContainerStartOutcome = "already-running"
	ContainerNoneApplies    ContainerStartOutcome = "none-applies"
)

// RepoDesired is the container phase's input. A container belongs to a
// repository, so starting one needs no session and reads no workspace
// row (spec §6.3).
type RepoDesired struct {
	Repository state.Repository
	Config     config.Config
	Digest     string
}

// containerIdentity renders the repository in the form the container
// adapters accept. Only the repository fields are populated: the adapters
// key on RepoRoot, and inventing a workspace ID here would invent the
// session autostart deliberately does not have.
func (d RepoDesired) containerIdentity() resolve.Workspace {
	return resolve.Workspace{
		RepositoryID: d.Repository.ID,
		Slug:         d.Repository.Slug,
		RepoRoot:     d.Repository.RepoRoot,
	}
}

// StartRepositoryContainer is autostart's engine: the container phase
// alone, under the repository lock. One row per repository is what keeps
// a shared container from being started once per session — the guarantee
// the dropped is_primary flag used to carry by filtering (spec §6.3).
// tmux is deliberately never consulted: at boot there is no tmux server,
// and going through Observe/BuildPlan would let the global
// session-unknown refusal block every container start (spec §3).
func (c *Controller) StartRepositoryContainer(ctx context.Context, d RepoDesired, lockDir string, lockTimeout time.Duration) (ContainerStartOutcome, *ContainerObservation, error) {
	lk, err := lock.Acquire(ctx, lockDir, d.Repository.ID, lockTimeout)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = lk.Release() }()

	desired := Desired{Workspace: d.containerIdentity(), Config: d.Config, Digest: d.Digest}
	snap := Snapshot{Desired: desired}
	snap.Container = c.observeContainer(ctx, desired, d.Repository.Container)
	action := containerAction(snap)
	if action == ContainerActionProbeFirst {
		// One retry of the observation that came back unknown: a
		// container is never started on uncertainty, and a docker socket
		// that is still coming up at boot is worth a second look.
		snap.Container = c.observeContainer(ctx, desired, d.Repository.Container)
		if snap.Container.Err != nil {
			return "", nil, fmt.Errorf("re-observing the container: %w", snap.Container.Err)
		}
		if action = containerAction(snap); action == ContainerActionProbeFirst {
			return "", nil, fmt.Errorf(
				"the container for %s could not be observed", d.Repository.Slug)
		}
	}

	switch action {
	case ContainerActionNone:
		obs := snap.Container.Observed
		if obs == nil || obs.Health != state.HealthPresent {
			return ContainerNoneApplies, nil, nil
		}
		if err := c.recordBinding(d.Repository.ID, obs); err != nil {
			return "", nil, err
		}
		return ContainerAlreadyRunning, obs, nil
	case ContainerActionStart, ContainerActionAcquire:
		if c.ContainerAct == nil {
			return "", nil, ErrContainerActionUnsupported
		}
		obs, err := c.ContainerAct.StartContainer(ctx, desired.Workspace, d.Config)
		if err != nil {
			return "", nil, fmt.Errorf("starting the container: %w", err)
		}
		if err := c.recordBinding(d.Repository.ID, &obs); err != nil {
			return "", nil, err
		}
		// Acquire is an idempotent up onto a container that was already
		// running, so only a real start may be reported as one.
		if action == ContainerActionStart {
			return ContainerStarted, &obs, nil
		}
		return ContainerAlreadyRunning, &obs, nil
	}
	return "", nil, fmt.Errorf("unexpected container action %q", action)
}

// recordBinding persists the observation against the repository, which is
// what container_bindings is keyed on (spec §5.2). No operation is
// recorded: last_operations belongs to a session, and autostart is
// performed by none.
func (c *Controller) recordBinding(repositoryID string, obs *ContainerObservation) error {
	if err := c.Store.RecordContainerObservation(
		repositoryID, *toStateObservation(obs), c.Clock.Now()); err != nil {
		return fmt.Errorf("recording the container binding: %w", err)
	}
	return nil
}
