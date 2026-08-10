package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// Controller observes, plans, ensures, and stops a workspace. It depends on
// interfaces rather than subprocess details.
type Controller struct {
	Store      Store
	Sessions   SessionObserver
	Containers ContainerObserver
	Clock      Clock
	// Actuator performs session mutations for Ensure. Nil in
	// observation-only wiring.
	Actuator SessionActuator
	// ContainerAct performs container mutations for Ensure. Nil refuses
	// any container action (the pre-adapter capability gate).
	ContainerAct ContainerActuator
}

// Desired is everything the configuration and resolver slices established
// about what should exist.
type Desired struct {
	Workspace resolve.Workspace
	Config    config.Config
	Digest    string
	// Bind is the session's base directory, repository-relative. A nil
	// pointer leaves whatever is stored alone — an open that carries no
	// --cwd must not clear an existing bind. Clearing goes through a
	// nil-valued SetBind from the CLI instead.
	Bind *string
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

	// A container belongs to a repository, so the observation needs the
	// binding and nothing else about the session that asked for it.
	//
	// A registered session already carries the repository's binding on its
	// record. An unregistered one carries no record at all, and reading the
	// repository directly is what keeps `status` on a session that has never
	// been opened from reporting a container its siblings are using as
	// absent. Ensure never reaches that branch — it registers first — so this
	// is the inspection path.
	//
	// The three-way switch is the same one the workspace lookup above uses,
	// and for the same reason: only ErrNotFound means "no binding". Any
	// other failure leaves it unknown whether one exists, and falling
	// through to discovery on that would let a store outage rebind a
	// repository that is already bound.
	var binding *state.ContainerBinding
	if snap.Stored != nil {
		binding = snap.Stored.Container
	} else {
		repo, err := c.Store.Repository(d.Workspace.RepositoryID)
		switch {
		case errors.Is(err, state.ErrNotFound):
			// No repository row: no binding, which is the zero value.
		case err != nil:
			return Snapshot{}, fmt.Errorf("reading the repository's container binding: %w", err)
		default:
			binding = repo.Container
		}
	}
	snap.Container = c.observeContainer(ctx, d, binding)
	return snap, nil
}

func (c *Controller) observeContainer(ctx context.Context, d Desired, binding *state.ContainerBinding) ContainerSnapshot {
	// Observe only on "auto" or "true"; anything else — including "false"
	// and the unnormalized zero value "" — is treated as disabled.
	enabled := d.Config.DevContainer.Enabled
	if enabled != "auto" && enabled != "true" {
		return ContainerSnapshot{}
	}
	if enabled == "auto" {
		// Applicability precedes the stored binding under auto: deleting
		// the devcontainer configuration must de-containerize the
		// workspace even while a binding is retained (spec §4).
		applies, err := c.Containers.Applies(ctx, d.Workspace, d.Config)
		if err != nil {
			return ContainerSnapshot{
				Observed: &ContainerObservation{Health: state.HealthUnknown},
				Err:      err,
			}
		}
		if !applies {
			return ContainerSnapshot{}
		}
	}
	if binding != nil {
		obs, err := c.Containers.ProbeContainer(ctx, *binding)
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
