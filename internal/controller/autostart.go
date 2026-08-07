package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/state"
)

// ContainerStartOutcome classifies what StartWorkspaceContainer did.
type ContainerStartOutcome string

const (
	ContainerStarted        ContainerStartOutcome = "started"
	ContainerAlreadyRunning ContainerStartOutcome = "already-running"
	ContainerNoneApplies    ContainerStartOutcome = "none-applies"
)

// StartWorkspaceContainer is autostart's engine: the container phase
// alone, under the workspace lock. tmux is deliberately never consulted
// — at boot there is no tmux server, and going through Observe/BuildPlan
// would let the global session-unknown refusal block every container
// start (spec §3). Callers pass registered workspaces only.
func (c *Controller) StartWorkspaceContainer(ctx context.Context, d Desired, lockDir string, lockTimeout time.Duration) (ContainerStartOutcome, *ContainerObservation, error) {
	const opName = "autostart"
	lk, err := lock.Acquire(ctx, lockDir, d.Workspace.ID, lockTimeout)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = lk.Release() }()

	var stored *state.Record
	rec, err := c.Store.Workspace(d.Workspace.ID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// Tolerated for robustness; autostart iterates the store, so a
		// record normally exists.
	case err != nil:
		return "", nil, fmt.Errorf("reading stored state: %w", err)
	default:
		stored = &rec
	}

	snap := Snapshot{Desired: d, Stored: stored}
	snap.Container = c.observeContainer(ctx, d, stored)
	obs, started, err := c.ensureContainer(ctx, d, snap, containerAction(snap), opName)
	if err != nil {
		return "", nil, err
	}
	if stored != nil {
		if err := c.commitOutcome(d.Workspace.ID, opName, obs); err != nil {
			return "", nil, err
		}
	}

	switch {
	case started:
		return ContainerStarted, obs, nil
	case obs != nil:
		return ContainerAlreadyRunning, obs, nil
	}
	return ContainerNoneApplies, nil, nil
}
