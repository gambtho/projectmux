// Package controller owns the workspace domain types, the interfaces it
// consumes, snapshot assembly (Observe), and pure planning (BuildPlan).
// It depends on interfaces rather than subprocess details; adapters are
// later slices.
package controller

// The tmux session-scoped identity keys, reused verbatim from the Phase 1
// Bash implementation (design §7). Adoption of live Bash-created sessions
// depends on these exact spellings.
const (
	KeyWorkspaceID = "@dev_workspace_id"
	KeySlug        = "@dev_slug"
	KeyWorktree    = "@dev_worktree"
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
// carries; the strings are empty when the key is absent.
type LiveSession struct {
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string
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
