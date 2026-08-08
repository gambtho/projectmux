// Package fake provides in-memory implementations of the controller's
// interfaces for tests in this and later slices. The fake store mirrors
// the semantics the real store guarantees: idempotent registration,
// unique session names, tri-state binding retention, and a nil applied
// digest never clearing drift.
package fake

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
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
	_ controller.SessionActuator   = (*SessionActuator)(nil)
	_ controller.ContainerActuator = (*ContainerActuator)(nil)
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
	AppliesResult  bool
	AppliesErr     error
	Probed         []state.ContainerBinding
	Discovered     []string
}

func (o *ContainerObserver) Applies(_ context.Context, _ resolve.Workspace, _ config.Config) (bool, error) {
	if o.AppliesErr != nil {
		return false, o.AppliesErr
	}
	return o.AppliesResult, nil
}

func (o *ContainerObserver) ProbeContainer(_ context.Context, binding state.ContainerBinding) (controller.ContainerObservation, error) {
	o.Probed = append(o.Probed, binding)
	if o.ProbeErr != nil {
		return controller.ContainerObservation{}, o.ProbeErr
	}
	return o.ProbeResult, nil
}

func (o *ContainerObserver) DiscoverContainer(_ context.Context, ws resolve.Workspace, _ config.Config) (*controller.ContainerObservation, error) {
	// Containers are per repository, so the repository ID is the honest
	// key to record here: the container phase never carries a session.
	o.Discovered = append(o.Discovered, ws.RepositoryID)
	if o.DiscoverErr != nil {
		return nil, o.DiscoverErr
	}
	return o.DiscoverResult, nil
}

// Store is an in-memory controller.Store. Repositories and container
// bindings live in maps of their own rather than on the record, mirroring
// the repositories and repository-keyed container_bindings tables: every
// session on a repository must read back the one binding its siblings
// wrote, which is what lets a shared container be started once.
type Store struct {
	mu           sync.Mutex
	records      map[string]*state.Record
	repositories map[string]*state.Repository
	containers   map[string]*state.ContainerBinding
}

func NewStore() *Store {
	return &Store{
		records:      map[string]*state.Record{},
		repositories: map[string]*state.Repository{},
		containers:   map[string]*state.ContainerBinding{},
	}
}

func (s *Store) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertRepositoryLocked(ws, now)
	digest := desiredDigest
	if rec, ok := s.records[ws.ID]; ok {
		rec.RepositoryID = ws.RepositoryID
		rec.Slug = ws.Slug
		rec.RepoRoot = ws.RepoRoot
		rec.Session = ws.Session
		rec.ProposedSession = ws.SessionName
		rec.DesiredDigest = &digest
		rec.UpdatedAt = now
		return nil
	}
	s.records[ws.ID] = &state.Record{
		ID:              ws.ID,
		RepositoryID:    ws.RepositoryID,
		Slug:            ws.Slug,
		RepoRoot:        ws.RepoRoot,
		Session:         ws.Session,
		ProposedSession: ws.SessionName,
		DesiredDigest:   &digest,
		RegisteredAt:    now,
		UpdatedAt:       now,
	}
	return nil
}

// upsertRepositoryLocked mirrors the real store's two-statement
// registration: the repository row is written first and the session row
// references it. Registering a second session on a repository refreshes
// the repository's mutable columns and deliberately leaves its binding
// alone — a sibling opening a session must not disturb a running
// container.
func (s *Store) upsertRepositoryLocked(ws resolve.Workspace, now time.Time) {
	if repo, ok := s.repositories[ws.RepositoryID]; ok {
		repo.Slug = ws.Slug
		repo.RepoRoot = ws.RepoRoot
		repo.UpdatedAt = now
		return
	}
	s.repositories[ws.RepositoryID] = &state.Repository{
		ID:           ws.RepositoryID,
		Slug:         ws.Slug,
		RepoRoot:     ws.RepoRoot,
		RegisteredAt: now,
		UpdatedAt:    now,
	}
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

func (s *Store) RecordContainerObservation(repositoryID string, obs state.ContainerObservation, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordContainerLocked(repositoryID, obs, now)
}

func (s *Store) recordContainerLocked(repositoryID string, obs state.ContainerObservation, now time.Time) error {
	if _, ok := s.repositories[repositoryID]; !ok {
		return fmt.Errorf("repository %s: %w", repositoryID, state.ErrNotFound)
	}
	switch obs.Health {
	case state.HealthPresent:
		if obs.ContainerID == "" {
			return fmt.Errorf("a present container observation must carry a container ID")
		}
		s.containers[repositoryID] = &state.ContainerBinding{
			Kind:          obs.Kind,
			ContainerID:   obs.ContainerID,
			ContainerUser: obs.ContainerUser,
			Workdir:       obs.Workdir,
			Health:        obs.Health,
			ObservedAt:    now,
		}
	case state.HealthMissing, state.HealthUnknown:
		if b, ok := s.containers[repositoryID]; ok {
			b.Health = obs.Health
			b.ObservedAt = now
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
		// Mirror the real store's boundedSummary (internal/state/store.go):
		// trim to the byte bound, then drop any rune split by the cut.
		op.ErrorSummary = strings.ToValidUTF8(op.ErrorSummary[:state.MaxErrorSummaryBytes], "")
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
		// The observation is recorded against the repository the session
		// belongs to, not the session, so a sibling reads the same binding.
		if err := s.recordContainerLocked(rec.RepositoryID, *r.Container, now); err != nil {
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
	return s.copyRecordLocked(rec), nil
}

// Workspaces returns every registered workspace ordered by slug, then
// repository root, then session, mirroring the real store's ORDER BY
// (internal/state/store.go). The session key is not cosmetic: a repository
// holds several sessions at one root, so without it their order is the map
// iteration order and a test that lists them is flaky.
func (s *Store) Workspaces() ([]state.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, s.copyRecordLocked(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		if out[i].RepoRoot != out[j].RepoRoot {
			return out[i].RepoRoot < out[j].RepoRoot
		}
		return out[i].Session < out[j].Session
	})
	return out, nil
}

// copyRecordLocked deep-copies a record and attaches the repository's
// shared container binding. A session therefore sees whatever container
// its repository is bound to, including one a sibling session started.
func (s *Store) copyRecordLocked(rec *state.Record) state.Record {
	out := *rec
	if rec.ActualSession != nil {
		v := *rec.ActualSession
		out.ActualSession = &v
	}
	if rec.DesiredDigest != nil {
		v := *rec.DesiredDigest
		out.DesiredDigest = &v
	}
	if rec.AppliedDigest != nil {
		v := *rec.AppliedDigest
		out.AppliedDigest = &v
	}
	out.Container = nil
	if b, ok := s.containers[rec.RepositoryID]; ok {
		c := *b
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

// Repository returns one repository with its binding attached, or
// ErrNotFound. It mirrors the real store's single-row read, which the
// container phase uses to refresh a binding under the repository lock.
func (s *Store) Repository(id string) (state.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	repo, ok := s.repositories[id]
	if !ok {
		return state.Repository{}, fmt.Errorf("repository %s: %w", id, state.ErrNotFound)
	}
	return s.copyRepositoryLocked(repo), nil
}

// Repositories returns every registered repository ordered by slug, then
// repository root, mirroring the real store's ORDER BY
// (internal/state/store.go). Container is the repository's binding, copied
// out so a caller cannot mutate stored state through the result.
func (s *Store) Repositories() ([]state.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Repository, 0, len(s.repositories))
	for _, repo := range s.repositories {
		out = append(out, s.copyRepositoryLocked(repo))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].RepoRoot < out[j].RepoRoot
	})
	return out, nil
}

// DropRepository removes a repository and everything keyed to it,
// mirroring the cascade the real schema performs (migration 0002): the
// repository row, every workspace belonging to it, and its container
// binding. Each session's last operation goes with the record it hangs
// off, the way last_operations cascades from workspaces.
//
// Dropping an id that is not there succeeds, matching the real store: a
// second rebuild over an already-migrated installation must be a no-op
// rather than an error.
func (s *Store) DropRepository(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for wsID, rec := range s.records {
		if rec.RepositoryID == id {
			delete(s.records, wsID)
		}
	}
	delete(s.containers, id)
	delete(s.repositories, id)
	return nil
}

// copyRepositoryLocked copies a repository and attaches its binding, which
// is the LEFT JOIN the real store performs. The copy is what keeps a
// caller from mutating stored state through the result.
func (s *Store) copyRepositoryLocked(repo *state.Repository) state.Repository {
	out := *repo
	out.Container = nil
	if b, ok := s.containers[repo.ID]; ok {
		c := *b
		out.Container = &c
	}
	return out
}

// SessionActuator records the session specs it was asked to create and
// fails on demand.
type SessionActuator struct {
	Err     error
	KillErr error
	Created []controller.SessionSpec
	Killed  []string
}

func (a *SessionActuator) CreateSession(_ context.Context, spec controller.SessionSpec) error {
	a.Created = append(a.Created, spec)
	return a.Err
}

func (a *SessionActuator) KillSession(_ context.Context, name string) error {
	a.Killed = append(a.Killed, name)
	return a.KillErr
}

// AdoptSessionName mirrors the real store: typed conflict on a name
// another workspace holds, no-op on the workspace's own current name,
// repair of a stale assignment otherwise.
func (s *Store) AdoptSessionName(workspaceID, name string, now time.Time) error {
	// Validation order mirrors the real store, which rejects an empty name
	// before it looks the workspace up. Both checks fire for an unknown
	// workspace *and* an empty name, so a fake that reordered them would
	// hand tests a different error than production for the same call.
	if name == "" {
		return fmt.Errorf("adopting an empty session name for workspace %s", workspaceID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[workspaceID]
	if !ok {
		return fmt.Errorf("workspace %s: %w", workspaceID, state.ErrNotFound)
	}
	if rec.ActualSession != nil && *rec.ActualSession == name {
		return nil
	}
	for id, other := range s.records {
		if id != workspaceID && other.ActualSession != nil && *other.ActualSession == name {
			return &state.SessionNameConflictError{Name: name}
		}
	}
	adopted := name
	rec.ActualSession = &adopted
	rec.UpdatedAt = now
	return nil
}

// ContainerActuator records starts and renders a deterministic exec
// marker so command tests can assert container windows without real
// docker argv. ExecResult, when set, replaces the marker — lifecycle
// tests use a runnable command there, since a real tmux pane running
// the marker would exit immediately and close its window.
type ContainerActuator struct {
	StartResult controller.ContainerObservation
	StartErr    error
	StopErr     error
	ExecResult  string
	Started     []string
	Stopped     []string
	Execs       []string
}

func (a *ContainerActuator) StartContainer(_ context.Context, ws resolve.Workspace, _ config.Config) (controller.ContainerObservation, error) {
	a.Started = append(a.Started, ws.RepositoryID)
	if a.StartErr != nil {
		return controller.ContainerObservation{}, a.StartErr
	}
	return a.StartResult, nil
}

func (a *ContainerActuator) ExecCommand(b state.ContainerBinding, command, relDir string, env map[string]string) string {
	a.Execs = append(a.Execs, command)
	if a.ExecResult != "" {
		return a.ExecResult
	}
	return fmt.Sprintf("fake-exec %s %s %q env=%d",
		b.ContainerID, path.Join(b.Workdir, relDir), command, len(env))
}

func (a *ContainerActuator) StopContainer(_ context.Context, containerID string) error {
	a.Stopped = append(a.Stopped, containerID)
	return a.StopErr
}
