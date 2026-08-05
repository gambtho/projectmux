package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/state"
)

// StopResult reports what Stop actually did. On error it still carries
// what succeeded before the failure, so the CLI can report a partial
// stop honestly (spec §1).
type StopResult struct {
	SessionStopped   bool
	SessionName      string
	ContainerStopped bool
	ContainerID      string
}

// Stop is the destructive counterpart of Ensure and deliberately
// idempotent: absent sessions and unbound containers are success, and
// nothing is ever destroyed on uncertainty. It targets only the
// identity-matched session, performs no registration, and records
// operations only for workspaces that already have a record.
func (c *Controller) Stop(ctx context.Context, d Desired, stopContainer bool, lockDir string, lockTimeout time.Duration) (StopResult, error) {
	const opName = "stop"
	lk, err := lock.Acquire(ctx, lockDir, d.Workspace.ID, lockTimeout)
	if err != nil {
		return StopResult{}, err
	}
	defer lk.Release()

	var stored *state.Record
	rec, err := c.Store.Workspace(d.Workspace.ID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// Unregistered: identity-only observation, and nothing below
		// writes a record.
	case err != nil:
		return StopResult{}, fmt.Errorf("reading stored state: %w", err)
	default:
		stored = &rec
	}
	registered := stored != nil

	q := SessionQuery{
		WorkspaceID:    d.Workspace.ID,
		CandidateNames: []string{d.Workspace.SessionName},
	}
	if stored != nil && stored.ActualSession != nil &&
		*stored.ActualSession != d.Workspace.SessionName {
		q.CandidateNames = append(q.CandidateNames, *stored.ActualSession)
	}
	obs, err := c.Sessions.ObserveSession(ctx, q)
	if err != nil {
		reason := "tmux could not be observed; refusing to stop on an unknown session state"
		if registered {
			c.recordFailure(d.Workspace.ID, opName, reason)
		}
		return StopResult{}, &RefusalError{Reason: reason}
	}

	var res StopResult
	if live := obs.ByIdentity; live != nil {
		if !SessionBelongsTo(*live, d.Workspace) {
			reason := fmt.Sprintf(
				"session %q carries contradictory identity keys; refusing to stop it", live.Name)
			if registered {
				c.recordFailure(d.Workspace.ID, opName, reason)
			}
			return StopResult{}, &RefusalError{Reason: reason}
		}
		if err := c.Actuator.KillSession(ctx, live.Name); err != nil {
			if registered {
				c.recordFailure(d.Workspace.ID, opName, "killing the session: "+err.Error())
			}
			return StopResult{}, fmt.Errorf("killing the session: %w", err)
		}
		res.SessionStopped = true
		res.SessionName = live.Name
	}

	var containerObs *ContainerObservation
	if stopContainer && stored != nil && stored.Container != nil {
		if c.ContainerAct == nil {
			c.recordFailure(d.Workspace.ID, opName, ErrContainerActionUnsupported.Error())
			return res, ErrContainerActionUnsupported
		}
		if err := c.ContainerAct.StopContainer(ctx, stored.Container.ContainerID); err != nil {
			// Partial: the session kill (if any) already happened; res
			// reports it and the caller renders the partial outcome.
			c.recordFailure(d.Workspace.ID, opName, "stopping the container: "+err.Error())
			return res, fmt.Errorf("stopping the container: %w", err)
		}
		res.ContainerStopped = true
		res.ContainerID = stored.Container.ContainerID
		// Confirmed absence by our own hand: record missing on the
		// retained binding (spec §1, design §7).
		containerObs = &ContainerObservation{Health: state.HealthMissing}
	}

	if registered {
		if err := c.commitOutcome(d.Workspace.ID, opName, containerObs); err != nil {
			return res, err
		}
	}
	return res, nil
}
