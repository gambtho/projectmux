// Package doctor diagnoses a projectmux installation: dependencies,
// configuration, the state database, and the drift between what the
// store records and what tmux and Docker actually hold.
//
// It is strictly read-only. Nothing here creates, migrates, kills, or
// starts anything — a doctor that repaired what it found could not be
// trusted to report it. The tri-state discipline binds throughout:
// uncertainty renders "unknown", never a finding, and only confirmed
// absence produces "warn" or "fail".
package doctor

import (
	"context"
	"errors"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// Status is one check's or item's verdict.
type Status string

const (
	StatusOK      Status = "ok"
	StatusWarn    Status = "warn"
	StatusUnknown Status = "unknown"
	StatusFail    Status = "fail"
)

// severity orders the statuses for aggregation: a check reports the
// worst of its items. Unknown outranks warn because unexamined ground is
// more alarming than a known, bounded finding.
func severity(s Status) int {
	switch s {
	case StatusFail:
		return 3
	case StatusUnknown:
		return 2
	case StatusWarn:
		return 1
	default:
		return 0
	}
}

func worst(statuses ...Status) Status {
	out := StatusOK
	for _, s := range statuses {
		if severity(s) > severity(out) {
			out = s
		}
	}
	return out
}

// Item is one diagnosed subject inside a check.
type Item struct {
	Subject string
	Status  Status
	Detail  string
}

// Check is one diagnostic area. Status is the worst of Items unless the
// check could not run at all.
type Check struct {
	Name   string
	Status Status
	Detail string
	Items  []Item
}

// aggregate sets the check's status from its items.
func (c Check) aggregate() Check {
	for _, item := range c.Items {
		c.Status = worst(c.Status, item.Status)
	}
	return c
}

// verdict builds an itemless check: one that could not run, or one whose
// answer is a single sentence.
func verdict(name string, status Status, detail string) Check {
	return Check{Name: name, Status: status, Detail: detail}
}

// Report is the full diagnosis: always every check, in a fixed order.
type Report struct{ Checks []Check }

// ProbeResult carries every fact the dependency policy branches on. A
// stdout-only seam would be too narrow: an unreachable Docker daemon
// exits 1 with empty stdout and the reason on stderr.
type ProbeResult struct {
	Stdout   string // trimmed
	Stderr   string // trimmed, bounded by the runner's capture cap
	ExitCode int
	Found    bool // the binary resolved on PATH
}

// VersionRunner probes a tool's version.
type VersionRunner interface {
	// Probe runs one argv (never a shell) under the observation
	// timeout. The error reports execution failures — a timeout, a
	// permission problem; a nonzero exit is a result, not an error,
	// matching run.Run.
	Probe(ctx context.Context, argv ...string) (ProbeResult, error)
}

// SessionLister is the bulk tmux enumeration doctor needs.
// *tmux.Client satisfies it.
type SessionLister interface {
	Sessions(ctx context.Context) ([]controller.LiveSession, error)
}

// Store is the read-only slice of the state store doctor reads.
// *state.ReadOnlyStore satisfies it.
type Store interface {
	Workspaces() ([]state.Record, error)
	Workspace(id string) (state.Record, error)
}

// Database is what the caller's read-only inspection found. Exactly one
// of Missing, OpenErr, IntegrityErr, or a clean read holds.
type Database struct {
	Path string
	// Missing is confirmed absence: a fresh installation, in which
	// nothing is registered. That is a fact, not uncertainty, so the
	// store-backed checks still run — against an empty set.
	Missing bool
	// OpenErr means the inspection itself could not be performed.
	OpenErr error
	// IntegrityErr means the file is damaged. It is reported, never
	// repaired: rebuilding is a separate, explicit command.
	IntegrityErr error
	Version      int
	Supported    int
}

// unavailable explains why the store view must not be queried, or
// returns nil when it may be.
func (d Database) unavailable() error {
	switch {
	case d.OpenErr != nil:
		return d.OpenErr
	case d.IntegrityErr != nil:
		return errors.New("the state database is corrupt")
	case d.Version > d.Supported:
		return errors.New("the state database is from a newer projectmux")
	case d.Version < d.Supported:
		return errors.New("migrations are pending; they run on the next mutating command")
	}
	return nil
}

// Runner holds everything a diagnosis reads. Every dependency is an
// interface so each check is testable without a live environment.
type Runner struct {
	ConfigRoot string
	Defaults   config.Source
	// DefaultsErr, when set, is why the defaults layer could not be
	// read; per-workspace validation cannot run without it.
	DefaultsErr error
	DB          Database
	// Store is non-nil only when the read-only view is safe to query.
	Store      Store
	Sessions   SessionLister
	Containers controller.ContainerObserver
	Versions   VersionRunner
}

// Diagnose runs every check in order and always returns a complete
// report: a check that could not run reports "unknown" in place rather
// than truncating the diagnosis.
func (r *Runner) Diagnose(ctx context.Context) Report {
	deps, dockerAbsent := r.dependencies(ctx)
	return Report{Checks: []Check{
		deps,
		r.configuration(),
		r.database(),
		r.orphanedSessions(ctx),
		r.staleBindings(ctx, dockerAbsent),
	}}
}

// records is the registered set the store-backed checks diagnose
// against: the stored rows, an empty set when the database is
// confirmed absent, or a reason the set is unknown.
func (r *Runner) records() ([]state.Record, error) {
	if r.DB.Missing {
		return nil, nil
	}
	if err := r.DB.unavailable(); err != nil {
		return nil, err
	}
	if r.Store == nil {
		return nil, errors.New("no state store is available")
	}
	return r.Store.Workspaces()
}
