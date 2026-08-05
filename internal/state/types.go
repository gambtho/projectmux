package state

import (
	"errors"
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

// Record is the joined current state of one workspace. Container and
// LastOperation are nil when never recorded.
type Record struct {
	ID              string
	Slug            string
	Worktree        string
	IsPrimary       bool
	ProposedSession string
	ActualSession   *string
	DesiredDigest   *string
	AppliedDigest   *string
	RegisteredAt    time.Time
	UpdatedAt       time.Time
	Container       *ContainerBinding
	LastOperation   *Operation
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
