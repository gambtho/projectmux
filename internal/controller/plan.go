package controller

import (
	"fmt"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// SessionAction is what the execution slice should do about the session.
type SessionAction string

const (
	SessionActionNone   SessionAction = "none"
	SessionActionAdopt  SessionAction = "adopt"
	SessionActionCreate SessionAction = "create"
	SessionActionRefuse SessionAction = "refuse"
)

// ContainerAction is what the execution slice should do about the
// container.
type ContainerAction string

const (
	ContainerActionNone       ContainerAction = "none"
	ContainerActionStart      ContainerAction = "start"
	ContainerActionProbeFirst ContainerAction = "probe-first"
)

// Plan is the typed outcome of one planning pass. Refusal is non-empty
// exactly when Session is SessionActionRefuse, and a refusing plan is
// global: it carries no mutating action of any kind (spec §5), so the
// execution slice may act on any non-refusing plan field without
// re-checking session state.
type Plan struct {
	Session    SessionAction
	RecordName bool
	Container  ContainerAction
	Reapply    bool
	Refusal    string
}

// BuildPlan compares desired configuration, stored metadata, and
// observations to decide the next required actions. It is pure: same
// snapshot, same plan. (The spec names this operation Plan(snapshot); the
// Go function is BuildPlan because the result type claims the name.)
func BuildPlan(snap Snapshot) Plan {
	if refusal := refusalFor(snap); refusal != "" {
		return Plan{
			Session:   SessionActionRefuse,
			Container: ContainerActionNone,
			Refusal:   refusal,
		}
	}

	p := Plan{Container: containerAction(snap)}
	p.RecordName = snap.Stored == nil || snap.Stored.ActualSession == nil
	p.Reapply = snap.Stored == nil ||
		snap.Stored.AppliedDigest == nil ||
		*snap.Stored.AppliedDigest != snap.Desired.Digest

	if snap.Session.State == SessionLive {
		p.Session = sessionActionForLive(snap)
	} else {
		p.Session = SessionActionCreate
	}
	return p
}

// refusalFor returns the reason no mutating action may be planned, or ""
// when acting is safe.
func refusalFor(snap Snapshot) string {
	// Design §9 via spec §5: no mutating action may be derived from an
	// unobservable tmux.
	if snap.Session.State == SessionUnknown {
		return "tmux could not be observed; refusing to act on an unknown session state"
	}
	// Design §7: never adopt a session whose identity keys contradict the
	// workspace, even when the observer matched it by ID.
	if live := snap.Session.ByIdentity; live != nil && !belongsTo(*live, snap.Desired.Workspace) {
		return fmt.Sprintf(
			"session %q carries contradictory identity keys; refusing to adopt it", live.Name)
	}
	// Design §7: never adopt or rename a session occupying a candidate name
	// without this workspace's identity — including keyless sessions.
	if occupant := foreignOccupant(snap); occupant != nil {
		return fmt.Sprintf(
			"session %q exists but does not belong to this workspace; refusing to adopt or rename it",
			occupant.Name)
	}
	// A live state with no identity-matched session is an inconsistent
	// snapshot (it should never occur from Observe, but callers may build
	// one by hand): refuse rather than dereference a nil session.
	if snap.Session.State == SessionLive && snap.Session.ByIdentity == nil {
		return "session state is live but no identity-matched session was observed; refusing to act on an inconsistent snapshot"
	}
	return ""
}

// belongsTo compares all three load-bearing identity keys (design §7): a
// session with the right workspace ID but a contradictory slug or worktree
// is evidence of corruption or collision, not a match.
func belongsTo(s LiveSession, ws resolve.Workspace) bool {
	return s.WorkspaceID == ws.ID && s.Slug == ws.Slug && s.Worktree == ws.Worktree
}

func foreignOccupant(snap Snapshot) *LiveSession {
	for i := range snap.Session.ByName {
		s := snap.Session.ByName[i]
		if !belongsTo(s, snap.Desired.Workspace) {
			return &s
		}
	}
	return nil
}

func sessionActionForLive(snap Snapshot) SessionAction {
	live := snap.Session.ByIdentity
	if snap.Stored != nil && snap.Stored.ActualSession != nil &&
		*snap.Stored.ActualSession == live.Name {
		return SessionActionNone
	}
	// Live with matching identity keys but an absent or stale record:
	// adopt it and let execution repair the record (design §9 crash
	// recovery; §13 step 7 Phase 1 adoption).
	return SessionActionAdopt
}

func containerAction(snap Snapshot) ContainerAction {
	obs := snap.Container.Observed
	if obs == nil {
		return ContainerActionNone
	}
	switch obs.Health {
	case state.HealthPresent:
		return ContainerActionNone
	case state.HealthUnknown:
		return ContainerActionProbeFirst
	default:
		return ContainerActionStart
	}
}
