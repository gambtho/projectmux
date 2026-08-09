// Package controller owns the workspace domain types, the interfaces it
// consumes, snapshot assembly (Observe), pure planning (BuildPlan), and
// reconciliation (Ensure, Stop). It depends on interfaces rather than
// subprocess details; the adapters that satisfy them live in internal/tmux,
// internal/container, and internal/state.
package controller

// The tmux session-scoped identity keys. The first three are reused verbatim
// from the Phase 1 Bash implementation (design §7); adoption of live
// Bash-created sessions depends on those exact spellings. KeySession was
// added when a repository gained the ability to hold more than one session:
// without it a live "<slug>--<session>" re-resolves to the default workspace
// ID and rebuild reports a false identity conflict. An absent key reads as
// "", which is exactly a default session, so no session created before it
// existed is invalidated.
const (
	KeyWorkspaceID = "@dev_workspace_id"
	KeySlug        = "@dev_slug"
	KeyWorktree    = "@dev_worktree"
	KeySession     = "@dev_session"
)

// SessionState is tri-state knowledge about the workspace's tmux session.
// Unknown means observation failed: a tmux outage is not absence, and no
// mutating action may be derived from it.
type SessionState string

const (
	SessionLive    SessionState = "live"
	SessionAbsent  SessionState = "absent"
	SessionUnknown SessionState = "unknown"
)

// LiveSession is one live tmux session and whatever identity keys it
// carries; the strings are empty when the key is absent. ID is the
// server-assigned tmux session ID (e.g. "$3") — unlike Name, it can
// never be reused by a replacement session, so destructive actions
// target it when present.
type LiveSession struct {
	ID          string
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string
	// Session is the session component, empty for the repository's default
	// session — which is also what an absent @dev_session decodes to.
	Session string
}

// SessionQuery asks the observer for the session carrying the workspace's
// identity keys and for any session occupying a candidate name, so
// planning can distinguish adoption from a foreign session squatting on a
// name this workspace would use.
type SessionQuery struct {
	WorkspaceID    string
	CandidateNames []string
}

// SessionObservation reports both halves of the query. The identity
// session, when live under a candidate name, also appears in ByName.
type SessionObservation struct {
	ByIdentity *LiveSession
	ByName     []LiveSession
}
