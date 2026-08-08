package state

import (
	"errors"
	"fmt"
	"time"
)

// Health is the tri-state container liveness from design §7.
type Health string

const (
	HealthPresent Health = "present"
	HealthMissing Health = "missing"
	HealthUnknown Health = "unknown"
)

// Outcome classifies a finished operation.
type Outcome string

const (
	OutcomeOK     Outcome = "ok"
	OutcomeFailed Outcome = "failed"
)

// ErrNotFound reports a workspace that has never been registered.
var ErrNotFound = errors.New("workspace not recorded")

// MaxErrorSummaryBytes bounds stored error summaries.
const MaxErrorSummaryBytes = 4096

// Record is the joined current state of one session on one repository. Slug
// and RepoRoot are the repository's, read through the join; LastOperation is
// nil when never recorded.
//
// Container is the *repository's* binding, projected onto every session that
// shares it, and is nil when no binding has been recorded. It is a read-only
// projection: writes go through RecordContainerObservation, which takes a
// repository ID, and there is no per-session binding to write. Keeping it on
// the record is what lets a session observe the container a sibling session
// started (design §5.2, §6.1) without every caller having to carry a second
// identifier into a result it assembles per session.
type Record struct {
	ID              string
	RepositoryID    string
	Slug            string
	RepoRoot        string
	Session         string
	ProposedSession string
	ActualSession   *string
	DesiredDigest   *string
	AppliedDigest   *string
	RegisteredAt    time.Time
	UpdatedAt       time.Time
	Container       *ContainerBinding
	LastOperation   *Operation
}

// Repository is one project and the container every session on it shares.
// Container is nil when no binding has ever been recorded.
type Repository struct {
	ID           string
	Slug         string
	RepoRoot     string
	RegisteredAt time.Time
	UpdatedAt    time.Time
	Container    *ContainerBinding
}

// ContainerBinding is the stored container identity plus its last observed
// health. Missing or unknown health never clears the identity fields; only
// a successful replacement overwrites them (design §7).
type ContainerBinding struct {
	Kind          string
	ContainerID   string
	ContainerUser string
	Workdir       string
	Health        Health
	ObservedAt    time.Time
}

// ContainerObservation is one observation to record. HealthPresent must
// carry the container identity; HealthMissing and HealthUnknown update
// health and freshness only.
type ContainerObservation struct {
	Kind          string
	ContainerID   string
	ContainerUser string
	Workdir       string
	Health        Health
}

// Operation is a finished operation's outcome. On writes the store sets
// FinishedAt from its now parameter; the field is populated on reads.
type Operation struct {
	Name         string
	Outcome      Outcome
	ExitStatus   *int
	ErrorSummary string
	FinishedAt   time.Time
}

// ReconciliationResult is design §9 step 5: everything one reconciliation
// pass learned, committed in a single transaction. AppliedDigest is set
// only when the desired configuration was fully applied; leaving it nil on
// failure preserves recorded drift.
type ReconciliationResult struct {
	AppliedDigest *string
	Container     *ContainerObservation
	Operation     Operation
}

// SessionNameConflictError reports an adoption target already recorded
// as another workspace's actual session. Callers treat it as a refusal,
// never an overwrite (open/attach spec §7).
type SessionNameConflictError struct{ Name string }

func (e *SessionNameConflictError) Error() string {
	return fmt.Sprintf("session name %q is already recorded for another workspace", e.Name)
}
