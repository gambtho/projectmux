package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// Controller observes, plans, and (in later slices) ensures, stops, and
// reports a workspace. It depends on interfaces rather than subprocess
// details.
type Controller struct {
	Store      Store
	Sessions   SessionObserver
	Containers ContainerObserver
	Clock      Clock
}

// Desired is everything the configuration and resolver slices established
// about what should exist.
type Desired struct {
	Workspace resolve.Workspace
	Config    config.Config
	Digest    string
}

// Snapshot is one observation pass: desired state, stored state (nil when
// unregistered), and tri-state knowledge about the session and container.
type Snapshot struct {
	Desired   Desired
	Stored    *state.Record
	Session   SessionSnapshot
	Container ContainerSnapshot
}

// SessionSnapshot is tri-state session knowledge. Err is set exactly when
// State is SessionUnknown.
type SessionSnapshot struct {
	State      SessionState
	ByIdentity *LiveSession
	ByName     []LiveSession
	Err        error
}

// ContainerSnapshot is the container observation for this pass. Observed
// is nil when no observation applies (devcontainer disabled); Err is set
// when the observation failed, in which case Observed carries
// HealthUnknown.
type ContainerSnapshot struct {
	Observed *ContainerObservation
	Err      error
}

// Observe assembles a snapshot. Observer failures become typed uncertainty
// inside the snapshot, never a guess in either direction; only a store
// failure aborts, because nothing can be decided without stored state.
func (c *Controller) Observe(ctx context.Context, d Desired) (Snapshot, error) {
	snap := Snapshot{Desired: d}

	rec, err := c.Store.Workspace(d.Workspace.ID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// Unregistered: Stored stays nil.
	case err != nil:
		return Snapshot{}, fmt.Errorf("reading stored state: %w", err)
	default:
		snap.Stored = &rec
	}

	q := SessionQuery{
		WorkspaceID:    d.Workspace.ID,
		CandidateNames: []string{d.Workspace.SessionName},
	}
	if snap.Stored != nil && snap.Stored.ActualSession != nil &&
		*snap.Stored.ActualSession != d.Workspace.SessionName {
		q.CandidateNames = append(q.CandidateNames, *snap.Stored.ActualSession)
	}
	obs, err := c.Sessions.ObserveSession(ctx, q)
	switch {
	case err != nil:
		// A failed observation is uncertainty, not absence: nothing may be
		// created or repaired on the strength of an unobservable tmux.
		snap.Session = SessionSnapshot{State: SessionUnknown, Err: err}
	case obs.ByIdentity != nil:
		snap.Session = SessionSnapshot{
			State: SessionLive, ByIdentity: obs.ByIdentity, ByName: obs.ByName,
		}
	default:
		snap.Session = SessionSnapshot{State: SessionAbsent, ByName: obs.ByName}
	}

	snap.Container = c.observeContainer(ctx, d, snap.Stored)
	return snap, nil
}

func (c *Controller) observeContainer(ctx context.Context, d Desired, stored *state.Record) ContainerSnapshot {
	if d.Config.DevContainer.Enabled == "false" {
		return ContainerSnapshot{}
	}
	if stored != nil && stored.Container != nil {
		obs, err := c.Containers.ProbeContainer(ctx, *stored.Container)
		if err != nil {
			// Design §9: a failed probe yields unknown, never loss.
			return ContainerSnapshot{
				Observed: &ContainerObservation{Health: state.HealthUnknown},
				Err:      err,
			}
		}
		return ContainerSnapshot{Observed: &obs}
	}
	obs, err := c.Containers.DiscoverContainer(ctx, d.Workspace, d.Config)
	if err != nil {
		return ContainerSnapshot{
			Observed: &ContainerObservation{Health: state.HealthUnknown},
			Err:      err,
		}
	}
	// A nil observation means no container applies: "auto" resolved to
	// none, treated exactly like enabled "false".
	return ContainerSnapshot{Observed: obs}
}
