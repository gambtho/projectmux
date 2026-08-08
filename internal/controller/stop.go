package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
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

// StopOptions selects a stop's destructive scope. Force overrides the
// shared-container refusal below and nothing else.
type StopOptions struct {
	Container bool
	Force     bool
}

// Stop is the destructive counterpart of Ensure and deliberately
// idempotent: absent sessions and unbound containers are success, and
// nothing is ever destroyed on uncertainty. It targets only the
// identity-matched session, performs no registration, and records
// operations only for workspaces that already have a record.
func (c *Controller) Stop(ctx context.Context, d Desired, opts StopOptions, lockDir string, lockTimeout time.Duration) (StopResult, error) {
	const opName = "stop"
	// Only a stop that touches the shared container needs the repository
	// lock, and it must hold it across the sibling check and the stop
	// that follows (design §6.1). A session-only stop takes the
	// workspace lock alone rather than waiting on a sibling's container
	// work. Holding it through the whole call is what the sibling check
	// below depends on: a sibling opening between the check and the
	// stop is exactly the race the check exists to prevent.
	repositoryID := ""
	if opts.Container {
		repositoryID = d.Workspace.RepositoryID
	}
	release, err := lockPhases(ctx, lockDir, repositoryID, d.Workspace.ID, lockTimeout)
	if err != nil {
		return StopResult{}, err
	}
	defer release()

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

	// A container is shared by every session on its repository, so
	// stopping one can kill a container a sibling is working in. Refuse
	// and name them; --force is how the user says they meant it. This
	// runs before the workspace's own session is ever observed or
	// touched, and under the repository lock acquired above, so the
	// answer it gets cannot go stale before the stop that follows it.
	if opts.Container && !opts.Force && stored != nil && stored.Container != nil {
		siblings, sibErr := c.liveSiblings(ctx, d.Workspace)
		if sibErr != nil {
			reason := "tmux could not be observed; refusing to stop a shared container on an unknown session state"
			if registered {
				c.recordFailure(d.Workspace.ID, opName, reason)
			}
			return StopResult{}, &RefusalError{Reason: reason}
		}
		if len(siblings) > 0 {
			reason := fmt.Sprintf(
				"the container is shared with live session(s) %s; refusing to stop it (use --force)",
				strings.Join(siblings, ", "))
			if registered {
				c.recordFailure(d.Workspace.ID, opName, reason)
			}
			return StopResult{}, &RefusalError{Reason: reason}
		}
	}

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
		// Kill by the observed session ID when the observer provides
		// one: a replacement session reusing the name between
		// observation and kill must survive.
		target := live.ID
		if target == "" {
			target = live.Name
		}
		if err := c.Actuator.KillSession(ctx, target); err != nil {
			if registered {
				c.recordFailure(d.Workspace.ID, opName, "killing the session: "+err.Error())
			}
			return StopResult{}, fmt.Errorf("killing the session: %w", err)
		}
		res.SessionStopped = true
		res.SessionName = live.Name
	}

	var containerObs *ContainerObservation
	if opts.Container && stored != nil && stored.Container != nil {
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

// liveSiblings names the live sessions of other workspaces on the same
// repository. It starts from the store rather than from tmux alone
// because a session's repository is recorded state: the tmux identity
// keys carry a workspace ID and a root, not the sibling relation.
func (c *Controller) liveSiblings(ctx context.Context, ws resolve.Workspace) ([]string, error) {
	records, err := c.Store.Workspaces()
	if err != nil {
		return nil, fmt.Errorf("reading stored workspaces: %w", err)
	}
	var names []string
	for _, rec := range records {
		if rec.ID == ws.ID || rec.RepositoryID != ws.RepositoryID {
			continue
		}
		q := SessionQuery{
			WorkspaceID:    rec.ID,
			CandidateNames: []string{rec.ProposedSession},
		}
		if rec.ActualSession != nil && *rec.ActualSession != rec.ProposedSession {
			q.CandidateNames = append(q.CandidateNames, *rec.ActualSession)
		}
		obs, err := c.Sessions.ObserveSession(ctx, q)
		if err != nil {
			return nil, err
		}
		if obs.ByIdentity != nil {
			names = append(names, obs.ByIdentity.Name)
		}
	}
	return names, nil
}
