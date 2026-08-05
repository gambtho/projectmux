// Package fake provides in-memory implementations of the controller's
// interfaces for tests in this and later slices. The fake store mirrors
// the semantics the real store guarantees: idempotent registration,
// unique session names, tri-state binding retention, and a nil applied
// digest never clearing drift.
package fake

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var (
	_ controller.Store             = (*Store)(nil)
	_ controller.SessionObserver   = (*SessionObserver)(nil)
	_ controller.ContainerObserver = (*ContainerObserver)(nil)
	_ controller.Clock             = (*Clock)(nil)
)

// Clock returns a fixed time.
type Clock struct{ Time time.Time }

func (c *Clock) Now() time.Time { return c.Time }

// SessionObserver returns a canned observation or error and records the
// queries it was asked.
type SessionObserver struct {
	Observation controller.SessionObservation
	Err         error
	Queries     []controller.SessionQuery
}

func (o *SessionObserver) ObserveSession(_ context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	o.Queries = append(o.Queries, q)
	if o.Err != nil {
		return controller.SessionObservation{}, o.Err
	}
	return o.Observation, nil
}

// ContainerObserver returns canned probe and discovery results and records
// what it was asked. A nil DiscoverResult with a nil DiscoverErr means no
// container applies (auto resolved to none), mirroring the interface
// contract.
type ContainerObserver struct {
	ProbeResult    controller.ContainerObservation
	ProbeErr       error
	DiscoverResult *controller.ContainerObservation
	DiscoverErr    error
	Probed         []state.ContainerBinding
	Discovered     []string
}

func (o *ContainerObserver) ProbeContainer(_ context.Context, binding state.ContainerBinding) (controller.ContainerObservation, error) {
	o.Probed = append(o.Probed, binding)
	if o.ProbeErr != nil {
		return controller.ContainerObservation{}, o.ProbeErr
	}
	return o.ProbeResult, nil
}

func (o *ContainerObserver) DiscoverContainer(_ context.Context, ws resolve.Workspace, _ config.Config) (*controller.ContainerObservation, error) {
	o.Discovered = append(o.Discovered, ws.ID)
	if o.DiscoverErr != nil {
		return nil, o.DiscoverErr
	}
	return o.DiscoverResult, nil
}

// Store is an in-memory controller.Store.
type Store struct {
	mu      sync.Mutex
	records map[string]*state.Record
}

func NewStore() *Store {
	return &Store{records: map[string]*state.Record{}}
}

func (s *Store) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := desiredDigest
	if rec, ok := s.records[ws.ID]; ok {
		rec.Slug = ws.Slug
		rec.Worktree = ws.Worktree
		rec.IsPrimary = ws.IsPrimary
		rec.ProposedSession = ws.SessionName
		rec.DesiredDigest = &digest
		rec.UpdatedAt = now
		return nil
	}
	s.records[ws.ID] = &state.Record{
		ID:              ws.ID,
		Slug:            ws.Slug,
		Worktree:        ws.Worktree,
		IsPrimary:       ws.IsPrimary,
		ProposedSession: ws.SessionName,
		DesiredDigest:   &digest,
		RegisteredAt:    now,
		UpdatedAt:       now,
	}
	return nil
}

func (s *Store) AllocateSessionName(workspaceID string, now time.Time) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[workspaceID]
	if !ok {
		return "", fmt.Errorf("workspace %s: %w", workspaceID, state.ErrNotFound)
	}
	if rec.ActualSession != nil {
		return *rec.ActualSession, nil
	}
	taken := map[string]bool{}
	for _, other := range s.records {
		if other.ActualSession != nil {
			taken[*other.ActualSession] = true
		}
	}
	for i := 1; ; i++ {
		candidate := rec.ProposedSession
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", rec.ProposedSession, i)
		}
		if !taken[candidate] {
			rec.ActualSession = &candidate
			rec.UpdatedAt = now
			return candidate, nil
		}
	}
}

func (s *Store) RecordContainerObservation(workspaceID string, obs state.ContainerObservation, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordContainerLocked(workspaceID, obs, now)
}

func (s *Store) recordContainerLocked(workspaceID string, obs state.ContainerObservation, now time.Time) error {
	rec, ok := s.records[workspaceID]
	if !ok {
		return fmt.Errorf("workspace %s: %w", workspaceID, state.ErrNotFound)
	}
	switch obs.Health {
	case state.HealthPresent:
		if obs.ContainerID == "" {
			return fmt.Errorf("a present container observation must carry a container ID")
		}
		rec.Container = &state.ContainerBinding{
			Kind:          obs.Kind,
			ContainerID:   obs.ContainerID,
			ContainerUser: obs.ContainerUser,
			Workdir:       obs.Workdir,
			Health:        obs.Health,
			ObservedAt:    now,
		}
	case state.HealthMissing, state.HealthUnknown:
		if rec.Container != nil {
			rec.Container.Health = obs.Health
			rec.Container.ObservedAt = now
		}
	default:
		return fmt.Errorf("invalid container health %q", obs.Health)
	}
	return nil
}

func (s *Store) RecordOperation(workspaceID string, op state.Operation, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordOperationLocked(workspaceID, op, now)
}

func (s *Store) recordOperationLocked(workspaceID string, op state.Operation, now time.Time) error {
	rec, ok := s.records[workspaceID]
	if !ok {
		return fmt.Errorf("workspace %s: %w", workspaceID, state.ErrNotFound)
	}
	op.FinishedAt = now
	if len(op.ErrorSummary) > state.MaxErrorSummaryBytes {
		op.ErrorSummary = op.ErrorSummary[:state.MaxErrorSummaryBytes]
	}
	rec.LastOperation = &op
	return nil
}

func (s *Store) CommitReconciliation(workspaceID string, r state.ReconciliationResult, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[workspaceID]
	if !ok {
		return fmt.Errorf("workspace %s: %w", workspaceID, state.ErrNotFound)
	}
	// The container observation is the only step that can fail, so it runs
	// first: a rejected observation must leave the record untouched, the
	// same all-or-nothing behavior the SQLite transaction gives the real
	// store.
	if r.Container != nil {
		if err := s.recordContainerLocked(workspaceID, *r.Container, now); err != nil {
			return err
		}
	}
	if r.AppliedDigest != nil {
		digest := *r.AppliedDigest
		rec.AppliedDigest = &digest
		rec.UpdatedAt = now
	}
	return s.recordOperationLocked(workspaceID, r.Operation, now)
}

func (s *Store) Workspace(id string) (state.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok {
		return state.Record{}, fmt.Errorf("workspace %s: %w", id, state.ErrNotFound)
	}
	return copyRecord(rec), nil
}

func (s *Store) Workspaces() ([]state.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, copyRecord(rec))
	}
	return out, nil
}

func copyRecord(rec *state.Record) state.Record {
	out := *rec
	if rec.Container != nil {
		c := *rec.Container
		out.Container = &c
	}
	if rec.LastOperation != nil {
		o := *rec.LastOperation
		out.LastOperation = &o
		if rec.LastOperation.ExitStatus != nil {
			e := *rec.LastOperation.ExitStatus
			out.LastOperation.ExitStatus = &e
		}
	}
	return out
}
