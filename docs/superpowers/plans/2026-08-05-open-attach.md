# Open/Attach (`open` / `attach`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `projectmux open` (observe, ensure, record, attach) and `projectmux attach`, host-side only, with the per-workspace filesystem lock, the tmux session actuator, and the controller's `Ensure` loop.

**Architecture:** A new `internal/lock` package provides the design-§9 per-workspace flock. `internal/state` gains `AdoptSessionName`. `internal/controller` gains the `SessionActuator` interface, `WindowSpec`/`SessionSpec`, and `Ensure` — the §9 convergence loop over the existing `Observe`/`BuildPlan`. `internal/tmux` implements the actuator with one chained invocation. `internal/cli` adds `open`/`attach`, the bare `projectmux <workspace>` form, exit code 6, and host-only container gating.

**Tech Stack:** Go (module `github.com/gambtho/projectmux`), stdlib only. Spec: `docs/superpowers/specs/2026-08-05-open-attach-design.md`.

## Global Constraints

- No new module dependencies; stdlib only. Linux/WSL only (design §2).
- `gofmt -l .` empty, `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux` before every commit.
- Exit codes: existing 0–5 unchanged; new `ExitRefused = 6` is additive. Successful reports exit 0.
- `attach` never creates, takes no lock, and performs no store writes. `open` mutates only under the workspace lock.
- A refused or failed `open` records a failed `open` operation (bounded summary) so `status` can explain it; the applied digest is recorded only on confirmed creation.
- The exact-match `=` target prefix is used only with target-session commands (`attach-session`, `switch-client`); `set-option`, `new-window`, and `select-window` reject it (verified on tmux 3.4) and take the plain name — safe because tmux prefers an exact name match and the name is known to exist when targeted.
- Window commands are tmux shell-command arguments; projectmux never interpolates into a shell (design §11).
- Commit messages and code comments must not mention Claude or AI assistance.

---

### Task 1: `internal/lock` — per-workspace filesystem lock

**Files:**
- Create: `internal/lock/lock.go`
- Test: `internal/lock/lock_test.go`

**Interfaces:**
- Consumes: nothing project-internal.
- Produces: `lock.Acquire(ctx context.Context, dir, workspaceID string, timeout time.Duration) (*lock.Lock, error)`, `(*lock.Lock).Release() error`, typed `*lock.ErrLockHeld{WorkspaceID string}` (checked via `errors.As`).

- [ ] **Step 1: Write the failing tests**

Create `internal/lock/lock_test.go`:

```go
package lock

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireReleaseReacquire(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAcquireTimesOutWithTypedError(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	start := time.Now()
	_, err = Acquire(context.Background(), dir, "w1", 200*time.Millisecond)
	var heldErr *ErrLockHeld
	if !errors.As(err, &heldErr) {
		t.Fatalf("err = %v, want *ErrLockHeld", err)
	}
	if heldErr.WorkspaceID != "w1" {
		t.Errorf("WorkspaceID = %q, want w1", heldErr.WorkspaceID)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timed-out Acquire took %v", elapsed)
	}
}

func TestDifferentWorkspacesDoNotContend(t *testing.T) {
	dir := t.TempDir()
	l1, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire w1: %v", err)
	}
	defer l1.Release()
	l2, err := Acquire(context.Background(), dir, "w2", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire w2 while w1 held: %v", err)
	}
	defer l2.Release()
}

func TestMutualExclusionUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	var holders atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				l, err := Acquire(context.Background(), dir, "w1", 10*time.Second)
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				if n := holders.Add(1); n != 1 {
					t.Errorf("%d simultaneous holders", n)
				}
				time.Sleep(time.Millisecond)
				holders.Add(-1)
				if err := l.Release(); err != nil {
					t.Errorf("Release: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestCancelledContextStopsWaiting(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err = Acquire(ctx, dir, "w1", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestChildDoesNotInheritTheLock guards the PR-54 failure class: a child
// spawned while the lock is held (here: a long-lived sleep, standing in
// for a detached tmux server) must not hold the lock after the parent
// releases it.
func TestChildDoesNotInheritTheLock(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// If the child inherited the fd, this acquire would block until the
	// child exits; the short timeout catches that regression.
	l2, err := Acquire(context.Background(), dir, "w1", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire after Release with a live child: %v (the child inherited the lock)", err)
	}
	_ = l2.Release()
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/lock/ -count=1`
Expected: FAIL to build — `Acquire`, `Lock`, `ErrLockHeld` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/lock/lock.go`:

```go
// Package lock provides the per-workspace filesystem lock (design §9).
// Mutating commands take it before their final observation and hold it
// through external mutations and the resulting state commit. It is for
// local filesystems (the state directory), like SQLite itself.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// pollInterval is how often a blocked Acquire retries.
const pollInterval = 100 * time.Millisecond

// ErrLockHeld reports that another operation holds the workspace lock
// past the acquisition timeout.
type ErrLockHeld struct{ WorkspaceID string }

func (e *ErrLockHeld) Error() string {
	return fmt.Sprintf(
		"another projectmux operation holds the lock for workspace %s", e.WorkspaceID)
}

// Lock is a held workspace lock. The lock file is never deleted:
// unlinking races a concurrent flock on the same path.
type Lock struct{ f *os.File }

// Acquire takes an exclusive lock for the workspace, polling
// non-blocking flock until the timeout. The fd is close-on-exec
// (os.OpenFile's default on Linux): children spawned while the lock is
// held must never inherit it — a detached tmux server holding a leaked
// lock fd forever is the failure class this package exists to prevent
// (design §2). TestChildDoesNotInheritTheLock pins that property.
func Acquire(ctx context.Context, dir, workspaceID string, timeout time.Duration) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, workspaceID+".lock"),
		os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening the lock file: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			f.Close()
			return nil, fmt.Errorf("locking workspace %s: %w", workspaceID, err)
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, &ErrLockHeld{WorkspaceID: workspaceID}
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("waiting for the workspace lock: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Release unlocks and closes the lock file.
func (l *Lock) Release() error {
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		l.f.Close()
		return fmt.Errorf("unlocking: %w", err)
	}
	return l.f.Close()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/lock/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/lock/
git commit -m "Add the per-workspace filesystem lock"
```

---

### Task 2: `internal/state` — `AdoptSessionName`

**Files:**
- Modify: `internal/state/types.go` (add `SessionNameConflictError`)
- Modify: `internal/state/store.go` (add `AdoptSessionName`)
- Test: `internal/state/store_test.go` (append tests)

**Interfaces:**
- Consumes: existing store internals (`encodeTime`, `isUniqueViolation`, `ErrNotFound` — store.go:15/189).
- Produces: `(*state.Store).AdoptSessionName(workspaceID, name string, now time.Time) error`; typed `*state.SessionNameConflictError{Name string}`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/state/store_test.go`, reusing its existing helpers — `openTestStore(t)`, `testWorkspace(id)` (fixed slug/session `slabledger`), `mustRegister(t, s, ws)`, `testTime` (store_test.go:14-41):

```go
func TestAdoptSessionNameRecordsTheLiveName(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	if err := s.AdoptSessionName("w1", "slab--old", testTime); err != nil {
		t.Fatalf("AdoptSessionName: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slab--old" {
		t.Errorf("ActualSession = %v, want slab--old", rec.ActualSession)
	}
}

func TestAdoptSessionNameIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	if err := s.AdoptSessionName("w1", "slabledger", testTime); err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	if err := s.AdoptSessionName("w1", "slabledger", testTime); err != nil {
		t.Fatalf("re-adopting the same name: %v", err)
	}
}

func TestAdoptSessionNameRepairsAStaleAssignment(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	if _, err := s.AllocateSessionName("w1", testTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := s.AdoptSessionName("w1", "slab--live", testTime); err != nil {
		t.Fatalf("adopting over a stale assignment: %v", err)
	}
	rec, _ := s.Workspace("w1")
	if rec.ActualSession == nil || *rec.ActualSession != "slab--live" {
		t.Errorf("ActualSession = %v, want slab--live", rec.ActualSession)
	}
}

func TestAdoptSessionNameConflictIsTypedAndHarmless(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	mustRegister(t, s, testWorkspace("w2"))
	if err := s.AdoptSessionName("w2", "slabledger", testTime); err != nil {
		t.Fatalf("seeding w2: %v", err)
	}

	err := s.AdoptSessionName("w1", "slabledger", testTime)
	var conflict *SessionNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *SessionNameConflictError", err)
	}
	if conflict.Name != "slabledger" {
		t.Errorf("conflict.Name = %q", conflict.Name)
	}
	rec, _ := s.Workspace("w1")
	if rec.ActualSession != nil {
		t.Errorf("conflicting adopt changed the record: %v", *rec.ActualSession)
	}
}

func TestAdoptSessionNameRejectsUnknownWorkspaceAndEmptyName(t *testing.T) {
	s := openTestStore(t)
	if err := s.AdoptSessionName("nope", "x", testTime); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown workspace err = %v, want ErrNotFound", err)
	}
	mustRegister(t, s, testWorkspace("w1"))
	if err := s.AdoptSessionName("w1", "", testTime); err == nil {
		t.Error("an empty session name was accepted")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/state/ -count=1`
Expected: FAIL to build — `AdoptSessionName`, `SessionNameConflictError` undefined.

- [ ] **Step 3: Write the implementation**

Append to `internal/state/types.go`:

```go
// SessionNameConflictError reports an adoption target already recorded
// as another workspace's actual session. Callers treat it as a refusal,
// never an overwrite (open/attach spec §7).
type SessionNameConflictError struct{ Name string }

func (e *SessionNameConflictError) Error() string {
	return fmt.Sprintf("session name %q is already recorded for another workspace", e.Name)
}
```

(Add the `fmt` import to `types.go` if it is not already there.)

Append to `internal/state/store.go`:

```go
// AdoptSessionName records a live session's observed name as the
// workspace's actual session inside one transaction. The UNIQUE
// constraint still governs: a name recorded for another workspace is a
// typed conflict, never an overwrite. Re-adopting the workspace's own
// current name is a no-op; adopting over a stale assignment repairs the
// record to match reality (design §9 crash recovery, §13 step 7
// adoption).
func (s *Store) AdoptSessionName(workspaceID, name string, now time.Time) error {
	if name == "" {
		return fmt.Errorf("adopting an empty session name for workspace %s", workspaceID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()

	var current sql.NullString
	err = tx.QueryRow(
		"SELECT actual_session FROM workspaces WHERE id = ?",
		workspaceID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading workspace %s: %w", workspaceID, err)
	}
	if current.Valid && current.String == name {
		return tx.Commit()
	}

	_, err = tx.Exec(
		"UPDATE workspaces SET actual_session = ?, updated_at = ? WHERE id = ?",
		name, encodeTime(now), workspaceID)
	if isUniqueViolation(err) {
		return &SessionNameConflictError{Name: name}
	}
	if err != nil {
		return fmt.Errorf("adopting session name %q: %w", name, err)
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/state/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/state/
git commit -m "Add AdoptSessionName with typed name-conflict refusal"
```

---

### Task 3: controller types and fakes for actuation

**Files:**
- Modify: `internal/controller/interfaces.go` (extend `Store`; add `WindowSpec`, `SessionSpec`, `SessionActuator`)
- Modify: `internal/controller/fake/fake.go` (add `Store.AdoptSessionName`, `SessionActuator`)
- Test: `internal/controller/fake/fake_test.go` (append tests)

**Interfaces:**
- Consumes: Task 2's `AdoptSessionName` semantics and `state.SessionNameConflictError`.
- Produces:

```go
// In internal/controller (interfaces.go):
type WindowSpec struct {
	Name    string
	Command string // empty => default shell
	Dir     string // absolute working directory
	Focus   bool
}

type SessionSpec struct {
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string            // also the default window dir
	Env         map[string]string // config.Environment, applied to every window
	Windows     []WindowSpec      // at least one (derivation guarantees it)
}

type SessionActuator interface {
	CreateSession(ctx context.Context, spec SessionSpec) error
}
```

and the `Store` interface gains `AdoptSessionName(workspaceID, name string, now time.Time) error`. In `fake`: `fake.SessionActuator{Err error, Created []controller.SessionSpec}` implementing the interface, and `fake.Store.AdoptSessionName` mirroring the real store's semantics.

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/fake/fake_test.go`:

```go
func TestFakeStoreAdoptSessionName(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.RegisterWorkspace(testWorkspace("w2", "other"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := s.AdoptSessionName("w1", "slab--live", testTime); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slab--live" {
		t.Errorf("ActualSession = %v, want slab--live", rec.ActualSession)
	}
	if err := s.AdoptSessionName("w1", "slab--live", testTime); err != nil {
		t.Errorf("re-adopting the same name: %v", err)
	}

	err = s.AdoptSessionName("w2", "slab--live", testTime)
	var conflict *state.SessionNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *state.SessionNameConflictError", err)
	}
	if err := s.AdoptSessionName("nope", "x", testTime); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown workspace err = %v, want ErrNotFound", err)
	}
	if err := s.AdoptSessionName("w1", "", testTime); err == nil {
		t.Error("an empty session name was accepted")
	}
}

func TestFakeSessionActuatorRecordsSpecs(t *testing.T) {
	a := &SessionActuator{}
	spec := controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Windows:     []controller.WindowSpec{{Name: "shell"}},
	}
	if err := a.CreateSession(context.Background(), spec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(a.Created) != 1 || a.Created[0].Name != "slab" {
		t.Errorf("Created = %+v", a.Created)
	}

	a.Err = errors.New("boom")
	if err := a.CreateSession(context.Background(), spec); err == nil {
		t.Error("configured error was not returned")
	}
}
```

`fake_test.go`'s import block must gain `"context"` and `"github.com/gambtho/projectmux/internal/controller"` (its current imports are `errors`, `testing`, `time`, `resolve`, `state` — fake_test.go:3-10).

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/... -count=1`
Expected: FAIL to build — `AdoptSessionName`, `SessionActuator`, `SessionSpec` undefined.

- [ ] **Step 3: Write the implementation**

In `internal/controller/interfaces.go`, add `AdoptSessionName(workspaceID, name string, now time.Time) error` to the `Store` interface (after `AllocateSessionName`), and append:

```go
// WindowSpec is one window the actuator creates. An empty Command means
// the default shell; Dir is absolute (derivation resolves relative
// cwds against the worktree).
type WindowSpec struct {
	Name    string
	Command string
	Dir     string
	Focus   bool
}

// SessionSpec is everything CreateSession needs. Env carries the
// workspace's configured environment: it is part of the digested desired
// document and must reach every window (open/attach spec §4). Windows is
// never empty — derivation supplies an implicit shell window.
type SessionSpec struct {
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string
	Env         map[string]string
	Windows     []WindowSpec
}

// SessionActuator creates the workspace session. It is the mutating
// counterpart of SessionObserver; adapters implement both.
type SessionActuator interface {
	CreateSession(ctx context.Context, spec SessionSpec) error
}
```

In `internal/controller/fake/fake.go`, add to the compile-time assertions block:

```go
	_ controller.SessionActuator = (*SessionActuator)(nil)
```

and append:

```go
// SessionActuator records the session specs it was asked to create and
// fails on demand.
type SessionActuator struct {
	Err     error
	Created []controller.SessionSpec
}

func (a *SessionActuator) CreateSession(_ context.Context, spec controller.SessionSpec) error {
	a.Created = append(a.Created, spec)
	return a.Err
}

// AdoptSessionName mirrors the real store: typed conflict on a name
// another workspace holds, no-op on the workspace's own current name,
// repair of a stale assignment otherwise.
func (s *Store) AdoptSessionName(workspaceID, name string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[workspaceID]
	if !ok {
		return fmt.Errorf("workspace %s: %w", workspaceID, state.ErrNotFound)
	}
	if name == "" {
		return fmt.Errorf("adopting an empty session name for workspace %s", workspaceID)
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
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/... -count=1 -race`
Expected: PASS (including all pre-existing controller and fake tests).

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/controller/
git commit -m "Add session actuator types and adoption support to the controller surface"
```

---

### Task 4: `controller.Ensure` — the §9 loop

**Files:**
- Create: `internal/controller/ensure.go`
- Modify: `internal/controller/observe.go` (add the `Actuator` field to `Controller`)
- Test: `internal/controller/ensure_test.go`

**Interfaces:**
- Consumes: Task 1's `lock.Acquire`/`ErrLockHeld`; Task 2/3's `AdoptSessionName` + `state.SessionNameConflictError`; existing `Observe`, `BuildPlan`, `fake` package.
- Produces:

```go
type EnsureAction string // EnsureCreated "created", EnsureAdopted "adopted", EnsureAlreadyRunning "already-running"

type EnsureResult struct {
	Action  EnsureAction
	Session string
	Drifted bool
}

type RefusalError struct{ Reason string } // CLI maps to exit 6

var ErrContainerActionUnsupported error

func (c *Controller) Ensure(ctx context.Context, d Desired, windows []WindowSpec, lockDir string, lockTimeout time.Duration) (EnsureResult, error)
```

`Controller` gains `Actuator SessionActuator`.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/ensure_test.go`:

```go
// The ensure tests live in the external test package: they need the
// exported fakes, and controller/fake imports controller, so an internal
// test package would form an import cycle.
package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var ensureTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

// scriptedSessions returns one canned step per ObserveSession call, so a
// single Ensure can see different observations across its initial
// observation, squat check, and post-create confirmation.
type scriptedSessions struct {
	steps   []func(controller.SessionQuery) (controller.SessionObservation, error)
	queries []controller.SessionQuery
}

func (s *scriptedSessions) ObserveSession(_ context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	s.queries = append(s.queries, q)
	if len(s.steps) == 0 {
		return controller.SessionObservation{}, errors.New("scripted observer exhausted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step(q)
}

func absentStep() func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{}, nil
	}
}

func liveStep(s controller.LiveSession) func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{
			ByIdentity: &s, ByName: []controller.LiveSession{s},
		}, nil
	}
}

func errorStep(err error) func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{}, err
	}
}

func ensureWorkspace() resolve.Workspace {
	return resolve.Workspace{
		ID:          "w1",
		Slug:        "slab",
		Worktree:    "/w/slab",
		SessionName: "slab",
		IsPrimary:   true,
	}
}

func ensureDesired() controller.Desired {
	return controller.Desired{
		Workspace: ensureWorkspace(),
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: "false"},
			Environment:  map[string]string{"FOO": "bar"},
		},
		Digest: "sha256:desired",
	}
}

func ownSession(name string) controller.LiveSession {
	return controller.LiveSession{
		Name: name, WorkspaceID: "w1", Slug: "slab", Worktree: "/w/slab",
	}
}

type ensureRig struct {
	store    *fake.Store
	sessions *scriptedSessions
	actuator *fake.SessionActuator
	ctrl     *controller.Controller
	lockDir  string
}

func newEnsureRig(t *testing.T, steps ...func(controller.SessionQuery) (controller.SessionObservation, error)) *ensureRig {
	t.Helper()
	r := &ensureRig{
		store:    fake.NewStore(),
		sessions: &scriptedSessions{steps: steps},
		actuator: &fake.SessionActuator{},
		lockDir:  t.TempDir(),
	}
	r.ctrl = &controller.Controller{
		Store:      r.store,
		Sessions:   r.sessions,
		Containers: &fake.ContainerObserver{},
		Clock:      &fake.Clock{Time: ensureTime},
		Actuator:   r.actuator,
	}
	return r
}

func (r *ensureRig) ensure(t *testing.T, d controller.Desired) (controller.EnsureResult, error) {
	t.Helper()
	windows := []controller.WindowSpec{{Name: "shell", Dir: d.Workspace.Worktree}}
	return r.ctrl.Ensure(context.Background(), d, windows, r.lockDir, time.Second)
}

func lastOp(t *testing.T, s *fake.Store, id string) *state.Operation {
	t.Helper()
	rec, err := s.Workspace(id)
	if err != nil {
		t.Fatalf("Workspace(%s): %v", id, err)
	}
	return rec.LastOperation
}

func TestEnsureCreates(t *testing.T) {
	r := newEnsureRig(t,
		absentStep(),                 // initial observation
		absentStep(),                 // allocated-name squat check
		liveStep(ownSession("slab")), // post-create confirmation
	)
	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureCreated || res.Session != "slab" || res.Drifted {
		t.Errorf("result = %+v", res)
	}
	if len(r.actuator.Created) != 1 {
		t.Fatalf("actuator calls = %d, want 1", len(r.actuator.Created))
	}
	spec := r.actuator.Created[0]
	if spec.Name != "slab" || spec.WorkspaceID != "w1" || spec.Env["FOO"] != "bar" {
		t.Errorf("actuated spec = %+v", spec)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.AppliedDigest == nil || *rec.AppliedDigest != "sha256:desired" {
		t.Errorf("AppliedDigest = %v, want the desired digest", rec.AppliedDigest)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want open/ok", op)
	}
}

func TestEnsureAlreadyRunningReportsDrift(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab")))
	if err := r.store.RegisterWorkspace(ensureWorkspace(), "sha256:desired", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning || res.Session != "slab" {
		t.Errorf("result = %+v", res)
	}
	if !res.Drifted {
		t.Error("Drifted = false; no applied digest exists")
	}
	if len(r.actuator.Created) != 0 {
		t.Errorf("actuator was called on an already-running session")
	}
}

func TestEnsureAdoptsAndRecordsTheLiveName(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab--phase1")))
	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureAdopted || res.Session != "slab--phase1" {
		t.Errorf("result = %+v", res)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.ActualSession == nil || *rec.ActualSession != "slab--phase1" {
		t.Errorf("ActualSession = %v", rec.ActualSession)
	}
	if rec.AppliedDigest != nil {
		t.Error("adoption recorded an applied digest; drift must stay honest")
	}
	if len(r.actuator.Created) != 0 {
		t.Error("actuator was called during adoption")
	}
}

func TestEnsureAdoptConflictRefuses(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab")))
	other := resolve.Workspace{
		ID: "w2", Slug: "other", Worktree: "/w/other", SessionName: "other",
	}
	if err := r.store.RegisterWorkspace(other, "sha256:x", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.store.AdoptSessionName("w2", "slab", ensureTime); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}

	_, err := r.ensure(t, ensureDesired())
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureRefusesOnUnknownSessionState(t *testing.T) {
	r := newEnsureRig(t, errorStep(errors.New("tmux exploded")))
	_, err := r.ensure(t, ensureDesired())
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a refusing plan reached the actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureContainerGateFiresBeforeActuation(t *testing.T) {
	r := newEnsureRig(t, absentStep())
	r.ctrl.Containers = &fake.ContainerObserver{DiscoverErr: errors.New("no adapter")}
	d := ensureDesired()
	d.Config.DevContainer.Enabled = "auto"

	_, err := r.ensure(t, d)
	if !errors.Is(err, controller.ErrContainerActionUnsupported) {
		t.Fatalf("err = %v, want ErrContainerActionUnsupported", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("the container gate did not fire before actuation")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureSquatOnAllocatedNameRefuses(t *testing.T) {
	foreign := controller.LiveSession{
		Name: "slab", WorkspaceID: "w9", Slug: "elsewhere", Worktree: "/w/x",
	}
	r := newEnsureRig(t,
		absentStep(),
		func(controller.SessionQuery) (controller.SessionObservation, error) {
			return controller.SessionObservation{ByName: []controller.LiveSession{foreign}}, nil
		},
	)
	_, err := r.ensure(t, ensureDesired())
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("creation proceeded over a squatted allocation")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureCreationFailureIsRecorded(t *testing.T) {
	r := newEnsureRig(t, absentStep(), absentStep())
	r.actuator.Err = errors.New("tmux said no")
	_, err := r.ensure(t, ensureDesired())
	if err == nil {
		t.Fatal("Ensure succeeded despite a failing actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsurePostCreateConfirmationRejects(t *testing.T) {
	mismatched := controller.LiveSession{
		Name: "slab", WorkspaceID: "w1", Slug: "slab", Worktree: "/w/OTHER",
	}
	wrongName := ownSession("slab-2")
	cases := map[string]func(controller.SessionQuery) (controller.SessionObservation, error){
		"unknown after create": errorStep(errors.New("tmux vanished")),
		"absent after create":  absentStep(),
		"contradictory keys":   liveStep(mismatched),
		"wrong name":           liveStep(wrongName),
	}
	for label, confirm := range cases {
		t.Run(label, func(t *testing.T) {
			r := newEnsureRig(t, absentStep(), absentStep(), confirm)
			_, err := r.ensure(t, ensureDesired())
			if err == nil {
				t.Fatal("unconfirmed creation was committed")
			}
			rec, _ := r.store.Workspace("w1")
			if rec.AppliedDigest != nil {
				t.Error("an unconfirmed creation recorded the applied digest")
			}
			if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
				t.Errorf("last operation = %+v, want open/failed", op)
			}
		})
	}
}

func TestEnsureConvergesAfterCrashBetweenCreateAndCommit(t *testing.T) {
	r := newEnsureRig(t, absentStep(), absentStep(), absentStep())
	if _, err := r.ensure(t, ensureDesired()); err == nil {
		t.Fatal("expected the unconfirmed first Ensure to fail")
	}
	// The allocation persisted; the session exists now (the crash was
	// after create). The next Ensure converges without recreating.
	r.sessions.steps = []func(controller.SessionQuery) (controller.SessionObservation, error){
		liveStep(ownSession("slab")),
	}
	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("recovery Ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning || res.Session != "slab" {
		t.Errorf("recovery result = %+v", res)
	}
	if !res.Drifted {
		t.Error("recovery cleared drift without applying configuration")
	}
	if calls := len(r.actuator.Created); calls != 1 {
		t.Errorf("actuator calls across both runs = %d, want 1", calls)
	}
}

func TestEnsureRespectsTheWorkspaceLock(t *testing.T) {
	r := newEnsureRig(t, absentStep(), absentStep(), liveStep(ownSession("slab")))
	held, err := lock.Acquire(context.Background(), r.lockDir, "w1", time.Second)
	if err != nil {
		t.Fatalf("pre-acquiring the lock: %v", err)
	}
	defer held.Release()

	_, err = r.ctrl.Ensure(context.Background(), ensureDesired(),
		[]controller.WindowSpec{{Name: "shell", Dir: "/w/slab"}},
		r.lockDir, 200*time.Millisecond)
	var lockErr *lock.ErrLockHeld
	if !errors.As(err, &lockErr) {
		t.Fatalf("err = %v, want *lock.ErrLockHeld", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a locked-out Ensure reached the actuator")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/controller/ -count=1`
Expected: FAIL to build — `Ensure`, `EnsureResult`, `RefusalError`, `Actuator` field undefined.

- [ ] **Step 3: Write the implementation**

In `internal/controller/observe.go`, add one field to the `Controller` struct after `Clock`:

```go
	// Actuator performs session mutations for Ensure. Nil in
	// observation-only wiring.
	Actuator SessionActuator
```

Create `internal/controller/ensure.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/state"
)

// EnsureAction classifies what Ensure did about the session.
type EnsureAction string

const (
	EnsureCreated        EnsureAction = "created"
	EnsureAdopted        EnsureAction = "adopted"
	EnsureAlreadyRunning EnsureAction = "already-running"
)

// EnsureResult reports a successful Ensure. Drifted mirrors the digest
// comparison at observation time; a fresh creation is never drifted.
type EnsureResult struct {
	Action  EnsureAction
	Session string
	Drifted bool
}

// RefusalError carries a refusal out of Ensure or attach; the CLI maps
// it to exit 6. Refusals are conflicts or uncertainty — retrying blindly
// is wrong, which is why they are distinguishable from generic failure.
type RefusalError struct{ Reason string }

func (e *RefusalError) Error() string { return e.Reason }

// ErrContainerActionUnsupported reports a plan requiring container
// support this build does not have. The gate is capability-shaped: a
// controller with a container actuator (a later slice) executes these
// actions instead of refusing, without changes here.
var ErrContainerActionUnsupported = errors.New(
	"this workspace requires container support, which is not implemented in this build")

// Ensure is the design-§9 convergence loop: lock, register, final
// observation under the lock, plan, then at most one external mutation
// followed by re-observation and a transactional commit. It returns
// typed refusals and never mutates on uncertainty.
func (c *Controller) Ensure(ctx context.Context, d Desired, windows []WindowSpec, lockDir string, lockTimeout time.Duration) (EnsureResult, error) {
	lk, err := lock.Acquire(ctx, lockDir, d.Workspace.ID, lockTimeout)
	if err != nil {
		return EnsureResult{}, err
	}
	defer lk.Release()

	if err := c.Store.RegisterWorkspace(d.Workspace, d.Digest, c.Clock.Now()); err != nil {
		return EnsureResult{}, fmt.Errorf("registering the workspace: %w", err)
	}

	snap, err := c.Observe(ctx, d)
	if err != nil {
		return EnsureResult{}, err
	}
	plan := BuildPlan(snap)

	if plan.Session == SessionActionRefuse {
		c.recordFailure(d.Workspace.ID, plan.Refusal)
		return EnsureResult{}, &RefusalError{Reason: plan.Refusal}
	}
	if plan.Container != ContainerActionNone {
		// No container actuator exists in this build (open/attach spec
		// §5 step 6); the container slice executes these instead.
		c.recordFailure(d.Workspace.ID, ErrContainerActionUnsupported.Error())
		return EnsureResult{}, ErrContainerActionUnsupported
	}

	drifted := snap.Stored == nil || snap.Stored.AppliedDigest == nil ||
		*snap.Stored.AppliedDigest != d.Digest

	switch plan.Session {
	case SessionActionNone:
		if err := c.recordOK(d.Workspace.ID); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:  EnsureAlreadyRunning,
			Session: snap.Session.ByIdentity.Name,
			Drifted: drifted,
		}, nil

	case SessionActionAdopt:
		name := snap.Session.ByIdentity.Name
		if err := c.Store.AdoptSessionName(d.Workspace.ID, name, c.Clock.Now()); err != nil {
			var conflict *state.SessionNameConflictError
			if errors.As(err, &conflict) {
				reason := fmt.Sprintf(
					"session name %q is already recorded for another workspace; refusing to adopt it", name)
				c.recordFailure(d.Workspace.ID, reason)
				return EnsureResult{}, &RefusalError{Reason: reason}
			}
			return EnsureResult{}, fmt.Errorf("recording the adopted session name: %w", err)
		}
		if err := c.recordOK(d.Workspace.ID); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{Action: EnsureAdopted, Session: name, Drifted: drifted}, nil

	case SessionActionCreate:
		return c.createSession(ctx, d, windows)
	}
	return EnsureResult{}, fmt.Errorf("unexpected session action %q", plan.Session)
}

func (c *Controller) createSession(ctx context.Context, d Desired, windows []WindowSpec) (EnsureResult, error) {
	id := d.Workspace.ID
	name, err := c.Store.AllocateSessionName(id, c.Clock.Now())
	if err != nil {
		c.recordFailure(id, "allocating a session name: "+err.Error())
		return EnsureResult{}, fmt.Errorf("allocating a session name: %w", err)
	}

	// Allocated-name squat check (open/attach spec §5): the initial
	// observation queried only the proposed and previously recorded
	// names; the allocation may be a suffixed variant a foreign live
	// session holds. The plan said create, so any occupant is foreign.
	occ, err := c.Sessions.ObserveSession(ctx, SessionQuery{
		WorkspaceID:    id,
		CandidateNames: []string{name},
	})
	if err != nil {
		reason := "tmux could not be observed before creating the session; refusing to act"
		c.recordFailure(id, reason)
		return EnsureResult{}, &RefusalError{Reason: reason}
	}
	if len(occ.ByName) > 0 {
		reason := fmt.Sprintf(
			"session %q exists but does not belong to this workspace; refusing to create over it",
			name)
		c.recordFailure(id, reason)
		return EnsureResult{}, &RefusalError{Reason: reason}
	}

	spec := SessionSpec{
		Name:        name,
		WorkspaceID: id,
		Slug:        d.Workspace.Slug,
		Worktree:    d.Workspace.Worktree,
		Env:         d.Config.Environment,
		Windows:     windows,
	}
	if err := c.Actuator.CreateSession(ctx, spec); err != nil {
		c.recordFailure(id, "creating the session: "+err.Error())
		return EnsureResult{}, fmt.Errorf("creating the session: %w", err)
	}

	// Post-create confirmation (open/attach spec §5): Observe reports
	// failures as snapshot uncertainty, never through its error return,
	// so only the observed shape below proves the creation.
	confirm, err := c.Observe(ctx, d)
	if err != nil {
		return EnsureResult{}, err
	}
	if reason := confirmCreation(confirm, d, name); reason != "" {
		c.recordFailure(id, reason)
		return EnsureResult{}, fmt.Errorf("the created session could not be confirmed: %s", reason)
	}

	digest := d.Digest
	if err := c.Store.CommitReconciliation(id, state.ReconciliationResult{
		AppliedDigest: &digest,
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, c.Clock.Now()); err != nil {
		return EnsureResult{}, fmt.Errorf("committing the reconciliation: %w", err)
	}
	return EnsureResult{Action: EnsureCreated, Session: name, Drifted: false}, nil
}

// confirmCreation reports why the post-create observation does not
// confirm the created session, or "" when it does: live, agreeing on
// all three identity keys, under the allocated name.
func confirmCreation(snap Snapshot, d Desired, allocated string) string {
	switch snap.Session.State {
	case SessionUnknown:
		return "tmux became unobservable after creation"
	case SessionAbsent:
		return "the session is absent after creation"
	}
	live := snap.Session.ByIdentity
	if live == nil {
		return "no identity-matched session was observed after creation"
	}
	if live.WorkspaceID != d.Workspace.ID || live.Slug != d.Workspace.Slug ||
		live.Worktree != d.Workspace.Worktree {
		return fmt.Sprintf("session %q carries contradictory identity keys after creation", live.Name)
	}
	if live.Name != allocated {
		return fmt.Sprintf(
			"the identity-matched session is named %q, not the allocated %q", live.Name, allocated)
	}
	return ""
}

// recordFailure best-effort records a failed open. The primary error is
// what the caller returns; a failing record write must not mask it.
func (c *Controller) recordFailure(workspaceID, summary string) {
	_ = c.Store.RecordOperation(workspaceID, state.Operation{
		Name:         "open",
		Outcome:      state.OutcomeFailed,
		ErrorSummary: summary,
	}, c.Clock.Now())
}

func (c *Controller) recordOK(workspaceID string) error {
	if err := c.Store.RecordOperation(workspaceID, state.Operation{
		Name:    "open",
		Outcome: state.OutcomeOK,
	}, c.Clock.Now()); err != nil {
		return fmt.Errorf("recording the operation: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/controller/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/controller/
git commit -m "Add controller Ensure implementing the reconciliation loop"
```

---

### Task 5: tmux session actuator

**Files:**
- Create: `internal/tmux/actuate.go`
- Test: `internal/tmux/actuate_test.go` (argv unit tests + real-tmux integration)

**Interfaces:**
- Consumes: Task 3's `controller.SessionSpec`/`WindowSpec`/`SessionActuator`; existing `Client.exec`, `run.Result`, `controller.Key*` constants.
- Produces: `(*tmux.Client).CreateSession(ctx, controller.SessionSpec) error`; package-private `createArgv(spec controller.SessionSpec) []string`. `*tmux.Client` satisfies `controller.SessionActuator`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tmux/actuate_test.go`:

```go
package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
)

func actuateSpec() controller.SessionSpec {
	return controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    "/w/slab",
		Env:         map[string]string{"B_KEY": "2", "A_KEY": "1"},
		Windows: []controller.WindowSpec{
			{Name: "agent-1", Command: "claude", Dir: "/w/slab", Focus: true},
			{Name: "shell", Dir: "/w/slab/sub"},
		},
	}
}

func TestCreateArgvShape(t *testing.T) {
	argv := createArgv(actuateSpec())
	joined := strings.Join(argv, " ")

	for _, want := range []string{
		"new-session -d -s slab -n agent-1 -c /w/slab -e A_KEY=1 -e B_KEY=2 claude",
		"; set-option -t slab @dev_workspace_id w1",
		"; set-option -t slab @dev_slug proj",
		"; set-option -t slab @dev_worktree /w/slab",
		"; new-window -d -t slab -n shell -c /w/slab/sub -e A_KEY=1 -e B_KEY=2",
		"; select-window -t slab:agent-1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q\nmissing %q", joined, want)
		}
	}
	if strings.Count(joined, "select-window") != 1 {
		t.Errorf("select-window should appear exactly once: %q", joined)
	}
}

func TestCreateArgvNoFocusMeansNoSelect(t *testing.T) {
	spec := actuateSpec()
	spec.Windows[0].Focus = false
	argv := createArgv(spec)
	if slices.Contains(argv, "select-window") {
		t.Errorf("select-window emitted without an explicit focus: %v", argv)
	}
}

func TestCreateArgvShellWindowHasNoCommand(t *testing.T) {
	spec := actuateSpec()
	spec.Windows = []controller.WindowSpec{{Name: "shell", Dir: "/w/slab"}}
	spec.Env = nil
	argv := createArgv(spec)
	joined := strings.Join(argv, " ")
	if !strings.HasPrefix(joined, "new-session -d -s slab -n shell -c /w/slab ;") {
		t.Errorf("argv = %q", joined)
	}
}

func TestCreateSessionRejectsZeroWindows(t *testing.T) {
	c := &Client{}
	err := c.CreateSession(context.Background(), controller.SessionSpec{Name: "x"})
	if err == nil {
		t.Fatal("a zero-window spec was accepted")
	}
}

func TestIntegrationCreateSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("projectmux-actuate-%d", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	dir := t.TempDir()
	spec := controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    dir,
		Env:         map[string]string{"PROJECTMUX_TEST_ENV": "visible"},
		Windows: []controller.WindowSpec{
			{Name: "first", Dir: dir, Focus: true},
			{Name: "second", Dir: dir},
		},
	}
	c := &Client{Socket: socket}
	if err := c.CreateSession(context.Background(), spec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	live, err := c.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != "slab" || live[0].WorkspaceID != "w1" ||
		live[0].Slug != "proj" || live[0].Worktree != dir {
		t.Fatalf("observed = %+v", live)
	}

	out, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", "slab",
		"-F", "#{window_name} #{window_active}").Output()
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	windows := strings.TrimSpace(string(out))
	if !strings.Contains(windows, "first 1") || !strings.Contains(windows, "second 0") {
		t.Errorf("windows = %q, want first active and second inactive", windows)
	}

	env, err := exec.Command("tmux", "-L", socket, "show-environment", "-t", "slab").Output()
	if err == nil && strings.Contains(string(env), "PROJECTMUX_TEST_ENV=visible") {
		return // session-level env visible; done
	}
	// -e sets pane environment rather than the session table on some
	// versions; verify via a pane instead.
	if err := exec.Command("tmux", "-L", socket, "send-keys", "-t", "slab:first",
		"printf 'ENVRESULT=%s\\n' \"$PROJECTMUX_TEST_ENV\"", "Enter").Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	deadline := 50
	for i := 0; i < deadline; i++ {
		out, err := exec.Command("tmux", "-L", socket, "capture-pane", "-p",
			"-t", "slab:first").Output()
		if err == nil && strings.Contains(string(out), "ENVRESULT=visible") {
			return
		}
		exec.Command("sleep", "0.1").Run()
	}
	t.Error("PROJECTMUX_TEST_ENV did not reach the first window's pane")
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tmux/ -count=1`
Expected: FAIL to build — `createArgv`, `CreateSession` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/tmux/actuate.go`:

```go
package tmux

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/gambtho/projectmux/internal/controller"
)

var _ controller.SessionActuator = (*Client)(nil)

// CreateSession creates the workspace session in one chained tmux
// invocation (verified on tmux 3.4): new-session with the first window,
// the three identity keys via set-option, remaining windows detached,
// and an explicit focus selection when configured. One subprocess makes
// creation-with-identity near-atomic (open/attach spec §4).
func (c *Client) CreateSession(ctx context.Context, spec controller.SessionSpec) error {
	if len(spec.Windows) == 0 {
		return fmt.Errorf("creating session %q: the spec carries no windows", spec.Name)
	}
	res, err := c.exec(ctx, createArgv(spec)...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tmux new-session exited %d: %s",
			res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	return nil
}

// createArgv renders the chained command list. Targets are the plain
// session name: set-option, new-window, and select-window reject the
// "=" exact-match prefix (their -t is not a target-session — verified
// on tmux 3.4), and tmux prefers an exact name match over a prefix, so
// targeting the just-created name is unambiguous. Window commands are
// tmux shell-command arguments: tmux runs them in the pane's default
// shell; nothing here interpolates into a shell (design §11).
func createArgv(spec controller.SessionSpec) []string {
	target := spec.Name
	first := spec.Windows[0]

	argv := []string{"new-session", "-d", "-s", spec.Name, "-n", first.Name,
		"-c", windowDir(first, spec)}
	argv = append(argv, envArgs(spec.Env)...)
	if first.Command != "" {
		argv = append(argv, first.Command)
	}

	argv = append(argv,
		";", "set-option", "-t", target, controller.KeyWorkspaceID, spec.WorkspaceID,
		";", "set-option", "-t", target, controller.KeySlug, spec.Slug,
		";", "set-option", "-t", target, controller.KeyWorktree, spec.Worktree,
	)

	for _, w := range spec.Windows[1:] {
		// -d keeps the first window active unless a focus is selected
		// below (open/attach spec §4).
		argv = append(argv, ";", "new-window", "-d", "-t", target,
			"-n", w.Name, "-c", windowDir(w, spec))
		argv = append(argv, envArgs(spec.Env)...)
		if w.Command != "" {
			argv = append(argv, w.Command)
		}
	}

	for _, w := range spec.Windows {
		if w.Focus {
			argv = append(argv, ";", "select-window", "-t", target+":"+w.Name)
		}
	}
	return argv
}

// envArgs renders -e KEY=VALUE pairs in sorted key order (deterministic
// argv). The environment is part of the digested desired configuration
// and must reach every window's panes; -e on new-session and new-window
// is the only mechanism covering the first window, which new-session
// itself creates (open/attach spec §4, verified on tmux 3.4).
func envArgs(env map[string]string) []string {
	var args []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	return args
}

func windowDir(w controller.WindowSpec, spec controller.SessionSpec) string {
	if w.Dir != "" {
		return w.Dir
	}
	return spec.Worktree
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tmux/ -count=1 -race`
Expected: PASS (integration test runs — tmux is installed here).

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/tmux/
git commit -m "Add the tmux session actuator with chained creation"
```

---

### Task 6: CLI wiring — derivation, host-only gating, attach seams, guard, exit 6

**Files:**
- Modify: `internal/cli/wiring.go` (window derivation, host-only observer, actuator seam, attach seams)
- Modify: `internal/cli/cli.go` (`ExitRefused`, `exitCode` mapping)
- Modify: `internal/cli/wiring_test.go` (guardedStore shadow + new tests)

**Interfaces:**
- Consumes: Tasks 3–5's types; existing seam conventions in `wiring.go`; `run.Run`; `tmux.Client`.
- Produces (package-private, used by Tasks 7–8):
  - `windowSpecs(cfg config.Config, worktree string) ([]controller.WindowSpec, error)` with typed `*containerWindowError`
  - `hostOnlyContainerObserver{}` (a `controller.ContainerObserver`)
  - seam `newSessionActuator func() controller.SessionActuator`
  - `attachTerminal(ctx context.Context, session string) error` with seams `execAttach func(string) error`, `switchClient func(context.Context, string) error`, `insideTmux func() bool`
  - `ExitRefused = 6`; `exitCode` maps `*controller.RefusalError` → 6
  - `guardedStore` additionally forbids `AdoptSessionName`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/cli/wiring_test.go` (and add the guard shadow there too — it is part of this step's test-side change):

```go
func (g guardedStore) AdoptSessionName(string, string, time.Time) error {
	return g.forbidden("AdoptSessionName")
}

func strPtr(s string) *string { return &s }

func TestWindowSpecsDerivation(t *testing.T) {
	cfg := config.Config{
		Windows: []config.Window{
			{Name: "agent-1", Agent: strPtr("claude"), Focus: true},
			{Name: "build", Command: strPtr("make watch"), Cwd: strPtr("sub")},
			{Name: "shell", Shell: true},
		},
	}
	specs, err := windowSpecs(cfg, "/w/slab")
	if err != nil {
		t.Fatalf("windowSpecs: %v", err)
	}
	want := []controller.WindowSpec{
		{Name: "agent-1", Command: "claude", Dir: "/w/slab", Focus: true},
		{Name: "build", Command: "make watch", Dir: "/w/slab/sub"},
		{Name: "shell", Dir: "/w/slab"},
	}
	if len(specs) != len(want) {
		t.Fatalf("specs = %+v", specs)
	}
	for i := range want {
		if specs[i] != want[i] {
			t.Errorf("spec %d = %+v, want %+v", i, specs[i], want[i])
		}
	}
}

func TestWindowSpecsImplicitShellWindow(t *testing.T) {
	specs, err := windowSpecs(config.Config{}, "/w/slab")
	if err != nil {
		t.Fatalf("windowSpecs: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "shell" || specs[0].Dir != "/w/slab" ||
		specs[0].Command != "" {
		t.Errorf("specs = %+v, want one implicit shell window", specs)
	}
}

func TestWindowSpecsRejectContainerLocation(t *testing.T) {
	cfg := config.Config{
		Windows: []config.Window{
			{Name: "agent-1", Agent: strPtr("claude"), Location: strPtr("container")},
		},
	}
	_, err := windowSpecs(cfg, "/w/slab")
	var cw *containerWindowError
	if !errors.As(err, &cw) {
		t.Fatalf("err = %v, want *containerWindowError", err)
	}
	if !strings.Contains(err.Error(), "agent-1") {
		t.Errorf("error %q does not name the window", err)
	}
}

func TestHostOnlyContainerObserver(t *testing.T) {
	observer := hostOnlyContainerObserver{}
	worktree := t.TempDir()
	ws := resolve.Workspace{ID: "w1", Worktree: worktree}
	auto := config.Config{DevContainer: config.DevContainer{Enabled: "auto"}}

	// auto with no devcontainer configuration: no container applies.
	obs, err := observer.DiscoverContainer(context.Background(), ws, auto)
	if err != nil || obs != nil {
		t.Errorf("auto/no-config = (%v, %v), want (nil, nil)", obs, err)
	}

	// auto with a devcontainer.json on disk: unsupported (error funnel).
	if err := os.MkdirAll(filepath.Join(worktree, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".devcontainer", "devcontainer.json"),
		[]byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.DiscoverContainer(context.Background(), ws, auto); err == nil {
		t.Error("auto with a devcontainer config discovered nothing to refuse")
	}

	// enabled true is unsupported regardless of files.
	if _, err := observer.DiscoverContainer(context.Background(),
		resolve.Workspace{Worktree: t.TempDir()},
		config.Config{DevContainer: config.DevContainer{Enabled: "true"}}); err == nil {
		t.Error("enabled true discovered nothing to refuse")
	}

	// A stored binding cannot be probed in this build.
	if _, err := observer.ProbeContainer(context.Background(),
		state.ContainerBinding{ContainerID: "c1"}); err == nil {
		t.Error("ProbeContainer pretended to probe")
	}
}

func TestAttachTerminalChoosesByTmuxEnv(t *testing.T) {
	var execCalls, switchCalls []string
	origExec, origSwitch, origInside := execAttach, switchClient, insideTmux
	t.Cleanup(func() { execAttach, switchClient, insideTmux = origExec, origSwitch, origInside })
	execAttach = func(session string) error {
		execCalls = append(execCalls, session)
		return nil
	}
	switchClient = func(_ context.Context, session string) error {
		switchCalls = append(switchCalls, session)
		return nil
	}

	insideTmux = func() bool { return false }
	if err := attachTerminal(context.Background(), "slab"); err != nil {
		t.Fatalf("attachTerminal: %v", err)
	}
	insideTmux = func() bool { return true }
	if err := attachTerminal(context.Background(), "slab"); err != nil {
		t.Fatalf("attachTerminal: %v", err)
	}
	if len(execCalls) != 1 || len(switchCalls) != 1 {
		t.Errorf("execCalls = %v, switchCalls = %v; want one each", execCalls, switchCalls)
	}
}

func TestRefusalErrorMapsToExitRefused(t *testing.T) {
	if got := exitCode(&controller.RefusalError{Reason: "nope"}); got != ExitRefused {
		t.Errorf("exitCode(RefusalError) = %d, want %d", got, ExitRefused)
	}
	if ExitRefused != 6 {
		t.Errorf("ExitRefused = %d, want 6", ExitRefused)
	}
}
```

Add any missing imports to `wiring_test.go`: `"os"`, `"path/filepath"`, `"strings"`.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to build — `windowSpecs`, `hostOnlyContainerObserver`, `attachTerminal`, `ExitRefused` undefined.

- [ ] **Step 3: Write the implementation**

First replace `internal/cli/wiring.go`'s import block with exactly this — the `run` package MUST be aliased to `runner`, because `cli_test.go` declares a helper named `run` and the plain import collides with it in the test binary:

```go
import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	runner "github.com/gambtho/projectmux/internal/run"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
)
```

Then append to `internal/cli/wiring.go`:

```go
// newSessionActuator is the mutation seam mirroring newSessionObserver.
var newSessionActuator = func() controller.SessionActuator {
	return &tmux.Client{}
}

// containerWindowError reports an explicitly container-located window,
// which this build cannot actuate faithfully: running it on the host
// would silently violate the user's stated intent (open/attach spec §4).
type containerWindowError struct{ window string }

func (e *containerWindowError) Error() string {
	return fmt.Sprintf(
		"window %q is configured with location: container, and container support is not implemented in this build",
		e.window)
}

// windowSpecs derives the actuator windows from merged configuration:
// implicit shell window when none is configured, first window active
// unless one is explicitly focused, relative cwds joined to the
// worktree. It runs before any lock or mutation.
func windowSpecs(cfg config.Config, worktree string) ([]controller.WindowSpec, error) {
	if len(cfg.Windows) == 0 {
		return []controller.WindowSpec{{Name: "shell", Dir: worktree}}, nil
	}
	specs := make([]controller.WindowSpec, 0, len(cfg.Windows))
	for _, w := range cfg.Windows {
		if w.Location != nil && *w.Location == "container" {
			return nil, &containerWindowError{window: w.Name}
		}
		spec := controller.WindowSpec{Name: w.Name, Dir: worktree, Focus: w.Focus}
		switch {
		case w.Agent != nil:
			spec.Command = *w.Agent
		case w.Command != nil:
			spec.Command = *w.Command
		}
		if w.Cwd != nil && *w.Cwd != "" {
			spec.Dir = filepath.Join(worktree, *w.Cwd)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// hostOnlyContainerObserver is open's container observer while no real
// adapter exists: it answers "does a container apply?" from the
// filesystem alone and funnels everything it cannot honestly answer
// into the unsupported path (open/attach spec §6). Docker is never
// touched. status keeps the plain unprobedObserver.
type hostOnlyContainerObserver struct{}

var _ controller.ContainerObserver = hostOnlyContainerObserver{}

func (hostOnlyContainerObserver) ProbeContainer(context.Context, state.ContainerBinding) (controller.ContainerObservation, error) {
	return controller.ContainerObservation{}, errUnprobed
}

func (hostOnlyContainerObserver) DiscoverContainer(_ context.Context, ws resolve.Workspace, cfg config.Config) (*controller.ContainerObservation, error) {
	if cfg.DevContainer.Enabled == "true" {
		return nil, errUnprobed
	}
	// Observe calls this only for "auto" (and "true", handled above): a
	// container applies exactly when a devcontainer configuration
	// exists on disk.
	for _, p := range devcontainerConfigPaths(ws.Worktree, cfg) {
		if _, err := os.Stat(p); err == nil {
			return nil, errUnprobed
		}
	}
	return nil, nil
}

func devcontainerConfigPaths(worktree string, cfg config.Config) []string {
	if cfg.DevContainer.Config != nil && *cfg.DevContainer.Config != "" {
		return []string{filepath.Join(worktree, *cfg.DevContainer.Config)}
	}
	return []string{
		filepath.Join(worktree, ".devcontainer", "devcontainer.json"),
		filepath.Join(worktree, ".devcontainer.json"),
	}
}

// Terminal attachment seams: a real attach replaces the process and a
// real switch-client needs a live tmux client, so tests substitute all
// three (open/attach spec §8).
var (
	execAttach = func(session string) error {
		path, err := exec.LookPath("tmux")
		if err != nil {
			return fmt.Errorf("finding tmux: %w", err)
		}
		return syscall.Exec(path,
			[]string{"tmux", "attach-session", "-t", "=" + session}, os.Environ())
	}
	switchClient = func(ctx context.Context, session string) error {
		res, err := runner.Run(ctx, runner.Command{
			Argv:    []string{"tmux", "switch-client", "-t", "=" + session},
			Timeout: tmux.DefaultTimeout,
		})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("tmux switch-client exited %d: %s",
				res.ExitCode, bytes.TrimSpace(res.Stderr))
		}
		return nil
	}
	insideTmux = func() bool { return os.Getenv("TMUX") != "" }
)

// attachTerminal connects the terminal to the session: switch-client
// inside tmux, an exec of attach-session outside (open/attach spec §2).
func attachTerminal(ctx context.Context, session string) error {
	if insideTmux() {
		return switchClient(ctx, session)
	}
	return execAttach(session)
}
```

(`bytes` also joins the import block.)

In `internal/cli/cli.go`:

1. Extend the exit-code block:

```go
	ExitRefused = 6 // the plan refused: conflict or uncertainty, do not blindly retry
```

2. Add to `exitCode`'s variable block and switch (import `"github.com/gambtho/projectmux/internal/controller"`):

```go
		refusal *controller.RefusalError
```

```go
	case errors.As(err, &refusal):
		return ExitRefused
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/cli/
git commit -m "Add open wiring: window derivation, host-only gating, attach seams, exit 6"
```

---

### Task 7: `projectmux open` and the bare-workspace form

**Files:**
- Create: `internal/cli/open.go`
- Modify: `internal/cli/cli.go` (dispatch cases, usage text)
- Test: `internal/cli/open_test.go`

**Interfaces:**
- Consumes: Task 6's wiring; `controller.Ensure`; existing `workspaceInfo`, `writeJSON`, `workspace(t, files)`/`run(t, ...)` test helpers, `validConfig` const.
- Produces: `runOpen(ctx context.Context, args []string, stdout io.Writer) error`; JSON `openEnvelope{SchemaVersion, Workspace workspaceInfo, Action, Session string, Drifted bool}` (`schema_version`, `workspace`, `action`, `session`, `drifted`). Dispatch: `open` case plus bare-workspace fallback.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/open_test.go`:

```go
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
)

// openTestStore lets open mutate a fake store (guardedStore is for
// observation commands only).
type openTestStore struct{ *fake.Store }

func (openTestStore) Close() error { return nil }

func installOpenStore(t *testing.T, s *fake.Store) {
	t.Helper()
	orig := openStore
	t.Cleanup(func() { openStore = orig })
	openStore = func() (stateStore, error) { return openTestStore{Store: s}, nil }
}

// scriptedCLISessions sequences ObserveSession results across Ensure's
// observation, squat check, and confirmation calls.
type scriptedCLISessions struct {
	steps []func(controller.SessionQuery) (controller.SessionObservation, error)
}

func (s *scriptedCLISessions) ObserveSession(_ context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	if len(s.steps) == 0 {
		return controller.SessionObservation{}, errors.New("scripted observer exhausted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step(q)
}

func installScriptedSessions(t *testing.T, steps ...func(controller.SessionQuery) (controller.SessionObservation, error)) {
	t.Helper()
	orig := newSessionObserver
	t.Cleanup(func() { newSessionObserver = orig })
	obs := &scriptedCLISessions{steps: steps}
	newSessionObserver = func() controller.SessionObserver { return obs }
}

func installFakeActuator(t *testing.T) *fake.SessionActuator {
	t.Helper()
	orig := newSessionActuator
	t.Cleanup(func() { newSessionActuator = orig })
	a := &fake.SessionActuator{}
	newSessionActuator = func() controller.SessionActuator { return a }
	return a
}

func installAttachSpies(t *testing.T) (execs, switches *[]string) {
	t.Helper()
	origExec, origSwitch, origInside := execAttach, switchClient, insideTmux
	t.Cleanup(func() { execAttach, switchClient, insideTmux = origExec, origSwitch, origInside })
	var e, s []string
	execAttach = func(session string) error { e = append(e, session); return nil }
	switchClient = func(_ context.Context, session string) error { s = append(s, session); return nil }
	insideTmux = func() bool { return false }
	return &e, &s
}

func cliAbsent() func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{}, nil
	}
}

func cliLive(s controller.LiveSession) func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{ByIdentity: &s, ByName: []controller.LiveSession{s}}, nil
	}
}

// openWorkspace builds the standard repo, points the state root at a
// temp dir (the lock directory derives from it), and returns the
// resolved workspace.
func openWorkspace(t *testing.T) resolve.Workspace {
	t.Helper()
	workspace(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/slabledger.yaml": validConfig,
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	ws, err := resolve.Resolve("", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ws
}

func ownLive(ws resolve.Workspace, name string) controller.LiveSession {
	return controller.LiveSession{
		Name: name, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
}

func decodeOpen(t *testing.T, stdout string) openEnvelope {
	t.Helper()
	var env openEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding open JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestOpenCreatesAndReportsJSON(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	actuator := installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeOpen(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion || env.Action != "created" ||
		env.Session != ws.SessionName || env.Drifted {
		t.Errorf("envelope = %+v", env)
	}
	if len(actuator.Created) != 1 {
		t.Fatalf("actuator calls = %d, want 1", len(actuator.Created))
	}
	spec := actuator.Created[0]
	// validConfig: agent-1 (agent claude, focus), shell, scratch.
	if len(spec.Windows) != 3 || spec.Windows[0].Name != "agent-1" ||
		spec.Windows[0].Command != "claude" || !spec.Windows[0].Focus {
		t.Errorf("windows = %+v", spec.Windows)
	}
	if spec.Env["CGO_ENABLED"] != "1" {
		t.Errorf("env = %+v; validConfig's environment was dropped", spec.Env)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.AppliedDigest == nil {
		t.Error("creation did not record the applied digest")
	}
}

func TestOpenAttachesByDefaultAndHonorsNoAttach(t *testing.T) {
	ws := openWorkspace(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	execs, switches := installAttachSpies(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, _, stderr := run(t, "open")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if len(*execs) != 1 || (*execs)[0] != ws.SessionName {
		t.Errorf("execAttach calls = %v", *execs)
	}
	if len(*switches) != 0 {
		t.Errorf("switchClient calls = %v", *switches)
	}

	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))
	code, _, _ = run(t, "open", "--no-attach")
	if code != 0 {
		t.Fatalf("no-attach exit %d", code)
	}
	if len(*execs) != 1 {
		t.Errorf("--no-attach attached anyway: %v", *execs)
	}
}

func TestOpenSwitchesClientInsideTmux(t *testing.T) {
	ws := openWorkspace(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	execs, switches := installAttachSpies(t)
	insideTmux = func() bool { return true }
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	if code, _, stderr := run(t, "open"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if len(*switches) != 1 || len(*execs) != 0 {
		t.Errorf("switch = %v, exec = %v; want switch-client inside tmux", *switches, *execs)
	}
}

func TestOpenReportsAlreadyRunningWithDrift(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	actual, err := s.AllocateSessionName(ws.ID, cliTestTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	installOpenStore(t, s)
	actuator := installFakeActuator(t)
	installScriptedSessions(t, cliLive(ownLive(ws, actual)))

	code, stdout, _ := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeOpen(t, stdout)
	if env.Action != "already-running" || !env.Drifted {
		t.Errorf("envelope = %+v, want already-running and drifted", env)
	}
	if len(actuator.Created) != 0 {
		t.Error("already-running called the actuator")
	}
}

func TestOpenRefusalExitsSix(t *testing.T) {
	openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	installScriptedSessions(t,
		func(controller.SessionQuery) (controller.SessionObservation, error) {
			return controller.SessionObservation{}, errors.New("tmux exploded")
		})

	code, stdout, stderr := run(t, "open", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitRefused, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
}

func TestOpenContainerWindowIsRejectedBeforeAnyMutation(t *testing.T) {
	workspace(t, map[string]string{
		"defaults.yaml": "version: 1\n",
		"workspaces/slabledger.yaml": "version: 1\nwindows:\n" +
			"  - name: agent-1\n    agent: claude\n    location: container\n",
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())
	s := fake.NewStore()
	installOpenStore(t, s)
	actuator := installFakeActuator(t)

	code, _, stderr := run(t, "open", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if !strings.Contains(stderr, "container support is not implemented") {
		t.Errorf("stderr = %q", stderr)
	}
	if len(actuator.Created) != 0 {
		t.Error("a container-located window reached the actuator")
	}
	if recs, _ := s.Workspaces(); len(recs) != 0 {
		t.Errorf("derivation failure mutated the store: %+v", recs)
	}
}

func TestOpenEnabledTrueIsUnsupported(t *testing.T) {
	workspace(t, map[string]string{
		"defaults.yaml": "version: 1\n",
		"workspaces/slabledger.yaml": "version: 1\ndevcontainer:\n  enabled: true\n" +
			"windows:\n  - name: shell\n    shell: true\n",
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())
	installOpenStore(t, fake.NewStore())
	actuator := installFakeActuator(t)
	installScriptedSessions(t, cliAbsent())

	code, _, stderr := run(t, "open", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitError, stderr)
	}
	if !strings.Contains(stderr, "container support") {
		t.Errorf("stderr = %q", stderr)
	}
	if len(actuator.Created) != 0 {
		t.Error("unsupported container plan reached the actuator")
	}
}

func TestBareWorkspaceDispatchesToOpen(t *testing.T) {
	openWorkspace(t)
	installOpenStore(t, fake.NewStore())

	// An unknown name proves the fallback reached workspace resolution
	// (exit 4), not the unknown-command usage path (exit 2).
	code, _, stderr := run(t, "no-such-workspace-name")
	if code != ExitUnknownWorkspace {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitUnknownWorkspace, stderr)
	}
}
```

(`cliTestTime` is a value defined in `wiring_test.go`, so this file does not import `time`.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to build — `runOpen`, `openEnvelope` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/open.go`:

```go
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

const openHelp = `usage: projectmux open [--no-attach] [--json] [--compact] [<workspace>]

Observe, ensure, record, and attach the workspace session, resolved
either from <workspace> or from the current directory. The bare form
"projectmux <workspace>" is shorthand for this command (no flags).

  --no-attach  ensure and record without attaching the terminal
  --json       emit the versioned JSON envelope (implies --no-attach)
  --compact    emit the JSON on a single line (implies --json)
`

// lockTimeout bounds how long open waits for another operation on the
// same workspace before failing with a typed error.
const lockTimeout = 10 * time.Second

// openEnvelope is the versioned JSON structure for projectmux open.
type openEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Workspace     workspaceInfo `json:"workspace"`
	Action        string        `json:"action"`
	Session       string        `json:"session"`
	Drifted       bool          `json:"drifted"`
}

func runOpen(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("open")
	noAttach := fs.Bool("no-attach", false, "ensure without attaching the terminal")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, openHelp)
			return nil
		}
		return usagef("open: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("open: expected at most one workspace, got %d", fs.NArg())
	}
	if *compact {
		*asJSON = true
	}
	if *asJSON {
		*noAttach = true
	}

	res, ws, err := ensureWorkspace(ctx, fs.Arg(0))
	if err != nil {
		return err
	}

	if *asJSON {
		return writeJSON(stdout, openEnvelope{
			SchemaVersion: OutputSchemaVersion,
			Workspace: workspaceInfo{
				ID:          ws.ID,
				Slug:        ws.Slug,
				Worktree:    ws.Worktree,
				SessionName: ws.SessionName,
				IsPrimary:   ws.IsPrimary,
			},
			Action:  string(res.Action),
			Session: res.Session,
			Drifted: res.Drifted,
		}, *compact)
	}

	fmt.Fprintf(stdout, "session %s (%s)\n", res.Session, res.Action)
	if res.Drifted {
		fmt.Fprintln(stdout, "configuration has drifted; run `projectmux status` for details")
	}
	if *noAttach {
		return nil
	}
	return attachTerminal(ctx, res.Session)
}

// ensureWorkspace runs the read-only pipeline, derives the actuator
// windows, and calls the controller's Ensure under the workspace lock.
func ensureWorkspace(ctx context.Context, name string) (controller.EnsureResult, resolve.Workspace, error) {
	var zero controller.EnsureResult
	root, err := config.Root()
	if err != nil {
		return zero, resolve.Workspace{}, err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return zero, resolve.Workspace{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return zero, resolve.Workspace{}, fmt.Errorf("determining the current directory: %w", err)
	}
	ws, err := resolve.Resolve(name, defaults.RepositoryRoots, cwd)
	if err != nil {
		return zero, resolve.Workspace{}, err
	}
	effective, err := config.Load(root, defaults, ws.Slug)
	if err != nil {
		return zero, ws, err
	}
	windows, err := windowSpecs(effective.Config, ws.Worktree)
	if err != nil {
		return zero, ws, err
	}

	st, err := openStore()
	if err != nil {
		return zero, ws, err
	}
	defer st.Close()

	stateRoot, err := state.Root()
	if err != nil {
		return zero, ws, err
	}
	ctrl := controller.Controller{
		Store:      st,
		Sessions:   newSessionObserver(),
		Containers: hostOnlyContainerObserver{},
		Clock:      systemClock{},
		Actuator:   newSessionActuator(),
	}
	res, err := ctrl.Ensure(ctx, controller.Desired{
		Workspace: ws,
		Config:    effective.Config,
		Digest:    effective.Digest,
	}, windows, filepath.Join(stateRoot, "locks"), lockTimeout)
	return res, ws, err
}
```

In `internal/cli/cli.go`:

1. Add the dispatch case after `config` (before `list`):

```go
	case "open":
		return runOpen(ctx, rest, stdout)
```

2. Replace the `default` arm of `dispatch` with the bare-workspace fallback (import `"strings"` is already present):

```go
	default:
		if !strings.HasPrefix(command, "-") {
			// Design §8: `projectmux <workspace>` is shorthand for
			// open. A mistyped command therefore resolves as a
			// workspace name and exits 4, not 2 — the documented trade.
			return runOpen(ctx, append([]string{command}, rest...), stdout)
		}
		return usagef("unknown command %q", command)
```

3. In the `usage` constant, insert before the `config` entry:

```text
  <workspace>
        shorthand for: open <workspace>
  open [--no-attach] [--json] [--compact] [<workspace>]
        observe, ensure, record, and attach the workspace session
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -count=1 -race`
Expected: one pre-existing failure — the dispatch fallback changes unknown-command behavior, so `TestUnknownCommandIsAUsageError` (cli_test.go) now sees exit 4. Update that test to a flag-shaped token, which still exits 2:

```go
func TestUnknownCommandIsAUsageError(t *testing.T) {
	// A bare non-flag token is the design-§8 workspace shorthand and
	// resolves as a workspace name (exit 4, covered in open_test.go);
	// only flag-shaped tokens still reach the unknown-command path.
	code, _, stderr := run(t, "--frobnicate")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "--frobnicate") {
		t.Errorf("stderr %q should name the unknown command", stderr)
	}
}
```

Then re-run: all PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/cli/
git commit -m "Add projectmux open and the bare-workspace shorthand"
```

---

### Task 8: `projectmux attach`

**Files:**
- Create: `internal/cli/attach.go`
- Modify: `internal/cli/cli.go` (dispatch case, usage entry)
- Test: `internal/cli/attach_test.go`

**Interfaces:**
- Consumes: Task 6's seams and `attachTerminal`; existing `sessionInfo` (status.go), `workspaceInfo`, `installFakeStore` (guarded — attach must not mutate), `installSessionObserver`.
- Produces: `runAttach(ctx context.Context, args []string, stdout io.Writer) error`; JSON `attachEnvelope{SchemaVersion int, Workspace workspaceInfo, Session sessionInfo}`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/attach_test.go`:

```go
package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
)

func decodeAttach(t *testing.T, stdout string) attachEnvelope {
	t.Helper()
	var env attachEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding attach JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestAttachLiveSessionJSON(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{
		ByIdentity: &live, ByName: []controller.LiveSession{live},
	}, nil)

	code, stdout, stderr := run(t, "attach", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeAttach(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d", env.SchemaVersion)
	}
	if env.Session.State != "live" || env.Session.Name == nil ||
		*env.Session.Name != ws.SessionName {
		t.Errorf("session = %+v", env.Session)
	}
	if env.Session.Identity == nil || *env.Session.Identity != "match" {
		t.Errorf("identity = %v, want match", env.Session.Identity)
	}
}

func TestAttachPerformsTerminalAttachment(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{ByIdentity: &live}, nil)
	execs, switches := installAttachSpies(t)

	code, _, stderr := run(t, "attach")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if len(*execs) != 1 || (*execs)[0] != ws.SessionName || len(*switches) != 0 {
		t.Errorf("exec = %v, switch = %v", *execs, *switches)
	}
}

func TestAttachAbsentSessionFailsWithHint(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "attach")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "projectmux open") {
		t.Errorf("stderr %q lacks the open hint", stderr)
	}
}

func TestAttachUnknownSessionStateRefuses(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, errors.New("tmux exploded"))

	code, _, _ := run(t, "attach")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
}

func TestAttachContradictoryIdentityRefuses(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: "/somewhere/else",
	}
	installSessionObserver(t, controller.SessionObservation{ByIdentity: &live}, nil)

	code, _, _ := run(t, "attach")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
}

func TestAttachNeverMutates(t *testing.T) {
	// installFakeStore wraps the store in guardedStore, which fails the
	// test on any mutating call — running attach at all is the
	// assertion (design §8/§12).
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{ByIdentity: &live}, nil)
	installAttachSpies(t)

	if code, _, stderr := run(t, "attach"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to build — `runAttach`, `attachEnvelope` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/attach.go`:

```go
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
)

const attachHelp = `usage: projectmux attach [--json] [--compact] [<workspace>]

Attach to the live workspace session, resolved either from <workspace>
or from the current directory. attach never creates a session and never
modifies state; use projectmux open to create one.

  --json     emit the versioned JSON envelope instead of attaching
  --compact  emit the JSON on a single line (implies --json)
`

// attachEnvelope is the versioned JSON structure for projectmux attach.
type attachEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Workspace     workspaceInfo `json:"workspace"`
	Session       sessionInfo   `json:"session"`
}

func runAttach(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("attach")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, attachHelp)
			return nil
		}
		return usagef("attach: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("attach: expected at most one workspace, got %d", fs.NArg())
	}
	if *compact {
		*asJSON = true
	}

	env, session, err := buildAttach(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	fmt.Fprintf(stdout, "attaching to %s\n", session)
	return attachTerminal(ctx, session)
}

// buildAttach observes (no lock, no store writes — attach is an
// observation command that ends in a terminal connect) and requires a
// live session whose identity keys agree on all three values.
func buildAttach(ctx context.Context, name string) (attachEnvelope, string, error) {
	var zero attachEnvelope
	root, err := config.Root()
	if err != nil {
		return zero, "", err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return zero, "", err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return zero, "", fmt.Errorf("determining the current directory: %w", err)
	}
	ws, err := resolve.Resolve(name, defaults.RepositoryRoots, cwd)
	if err != nil {
		return zero, "", err
	}
	effective, err := config.Load(root, defaults, ws.Slug)
	if err != nil {
		return zero, "", err
	}

	st, err := openStore()
	if err != nil {
		return zero, "", err
	}
	defer st.Close()

	ctrl := controller.Controller{
		Store:      st,
		Sessions:   newSessionObserver(),
		Containers: unprobedObserver{},
		Clock:      systemClock{},
	}
	snap, err := ctrl.Observe(ctx, controller.Desired{
		Workspace: ws,
		Config:    effective.Config,
		Digest:    effective.Digest,
	})
	if err != nil {
		return zero, "", err
	}

	switch snap.Session.State {
	case controller.SessionUnknown:
		return zero, "", &controller.RefusalError{
			Reason: "tmux could not be observed; refusing to attach on an unknown session state"}
	case controller.SessionAbsent:
		return zero, "", fmt.Errorf(
			"no live session for workspace %s; run `projectmux open`", ws.Slug)
	}

	live := snap.Session.ByIdentity
	if live.WorkspaceID != ws.ID || live.Slug != ws.Slug || live.Worktree != ws.Worktree {
		return zero, "", &controller.RefusalError{Reason: fmt.Sprintf(
			"session %q carries contradictory identity keys; refusing to attach to it", live.Name)}
	}

	name = live.Name
	verdict := "match"
	env := attachEnvelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			Worktree:    ws.Worktree,
			SessionName: ws.SessionName,
			IsPrimary:   ws.IsPrimary,
		},
		Session: sessionInfo{
			State:    string(snap.Session.State),
			Name:     &name,
			Identity: &verdict,
		},
	}
	return env, live.Name, nil
}
```

In `internal/cli/cli.go`:

1. Add the dispatch case after `open`:

```go
	case "attach":
		return runAttach(ctx, rest, stdout)
```

2. In the `usage` constant, insert after the `open` entry:

```text
  attach [--json] [--compact] [<workspace>]
        attach to the live workspace session; never creates one
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/cli/
git commit -m "Add projectmux attach as a non-mutating terminal connector"
```

---

### Task 9: real-tmux lifecycle tests

**Files:**
- Test: `internal/cli/lifecycle_test.go`

**Interfaces:**
- Consumes: everything above; real tmux on isolated sockets; the real SQLite store via `PROJECTMUX_STATE_ROOT`; seams to point the observer/actuator at the isolated socket.
- Produces: the design-§12 lifecycle proof: open → idempotent reopen → attach, Phase-1 adoption, squatter refusal, concurrent opens.

- [ ] **Step 1: Write the tests**

Create `internal/cli/lifecycle_test.go`:

```go
package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/tmux"
)

// lifecycleRig points the CLI seams at a real tmux server on an
// isolated socket and the real SQLite store in a temp state root, then
// runs commands through Main exactly as a user would.
func lifecycleRig(t *testing.T, label string) (resolve.Workspace, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	workspace(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/slabledger.yaml": validConfig,
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())

	socket := fmt.Sprintf("projectmux-life-%s-%d", label, os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	origObs, origAct := newSessionObserver, newSessionActuator
	t.Cleanup(func() { newSessionObserver, newSessionActuator = origObs, origAct })
	newSessionObserver = func() controller.SessionObserver {
		return &tmux.Client{Socket: socket}
	}
	newSessionActuator = func() controller.SessionActuator {
		return &tmux.Client{Socket: socket}
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	ws, err := resolve.Resolve("", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ws, socket
}

func openJSON(t *testing.T) openEnvelope {
	t.Helper()
	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("open exit %d, stderr: %s", code, stderr)
	}
	var env openEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding: %v\n%s", err, stdout)
	}
	return env
}

func TestLifecycleOpenReopenAttach(t *testing.T) {
	ws, socket := lifecycleRig(t, "openreopen")

	created := openJSON(t)
	if created.Action != "created" || created.Session != ws.SessionName {
		t.Fatalf("first open = %+v", created)
	}

	live, err := (&tmux.Client{Socket: socket}).Sessions(t.Context())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].WorkspaceID != ws.ID || live[0].Worktree != ws.Worktree {
		t.Fatalf("live = %+v", live)
	}

	reopened := openJSON(t)
	if reopened.Action != "already-running" || reopened.Session != ws.SessionName {
		t.Errorf("reopen = %+v, want already-running (idempotent reopen)", reopened)
	}
	if reopened.Drifted {
		t.Errorf("reopen drifted; creation should have recorded the applied digest")
	}

	code, stdout, stderr := run(t, "attach", "--json")
	if code != 0 {
		t.Fatalf("attach exit %d, stderr: %s", code, stderr)
	}
	var att attachEnvelope
	if err := json.Unmarshal([]byte(stdout), &att); err != nil {
		t.Fatalf("decoding attach: %v", err)
	}
	if att.Session.State != "live" || att.Session.Identity == nil ||
		*att.Session.Identity != "match" {
		t.Errorf("attach = %+v", att.Session)
	}
}

// TestLifecycleAdoptsPhaseOneSession is design §13 step 7: a live
// session created outside projectmux but carrying the three identity
// keys is adopted — never recreated, renamed, or wrongly attached.
func TestLifecycleAdoptsPhaseOneSession(t *testing.T) {
	ws, socket := lifecycleRig(t, "adopt")

	for _, args := range [][]string{
		{"new-session", "-d", "-s", "bash-era", "-c", ws.Worktree},
		{"set-option", "-t", "bash-era", controller.KeyWorkspaceID, ws.ID},
		{"set-option", "-t", "bash-era", controller.KeySlug, ws.Slug},
		{"set-option", "-t", "bash-era", controller.KeyWorktree, ws.Worktree},
	} {
		full := append([]string{"-L", socket}, args...)
		if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}

	env := openJSON(t)
	if env.Action != "adopted" || env.Session != "bash-era" {
		t.Fatalf("open = %+v, want adoption of bash-era", env)
	}
	if !env.Drifted {
		t.Error("adoption cleared drift without applying configuration")
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(t.Context())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != "bash-era" {
		t.Errorf("live = %+v; adoption created or renamed sessions", live)
	}
}

func TestLifecycleForeignSquatterRefuses(t *testing.T) {
	ws, socket := lifecycleRig(t, "squat")

	full := []string{"-L", socket, "new-session", "-d", "-s", ws.SessionName}
	if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
		t.Fatalf("seeding the squatter: %v\n%s", err, out)
	}

	code, stdout, stderr := run(t, "open", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d (stdout %q, stderr %q)", code, ExitRefused, stdout, stderr)
	}

	code, _, _ = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
}

func TestLifecycleConcurrentOpensCreateExactlyOnce(t *testing.T) {
	ws, socket := lifecycleRig(t, "race")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, _ := run(t, "open", "--json")
			codes[i] = code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != 0 {
			t.Errorf("open %d exited %d", i, code)
		}
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(t.Context())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].WorkspaceID != ws.ID {
		t.Errorf("live = %+v, want exactly one session", live)
	}
}
```

Note: `run(t, ...)` uses `Main` directly, so the two concurrent opens run in one process; the flock still serializes them (flock is per open file description, and each `Ensure` opens the lock file independently). If `t.Context()` is unavailable in the module's Go version, use `context.Background()` and add the import.

- [ ] **Step 2: Run the tests**

Run: `go test ./internal/cli/ -count=1 -race -run TestLifecycle -v`
Expected: PASS (tmux is installed here; the suite skips without it).

- [ ] **Step 3: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/cli/lifecycle_test.go
git commit -m "Add real-tmux lifecycle tests for open, adoption, refusal, and racing opens"
```

---

### Task 10: Final verification sweep

**Files:**
- Modify: none expected; fix anything the sweep finds.

- [ ] **Step 1: Run the full gates**

- `gofmt -l .` → empty
- `go vet ./...` → clean
- `go test ./... -count=1 -race` → all PASS (including lifecycle and integration tests)
- `CGO_ENABLED=0 go build ./cmd/projectmux` → builds

- [ ] **Step 2: Isolated smoke test with exit codes asserted**

```bash
SMOKE=$(mktemp -d)
export PROJECTMUX_STATE_ROOT="$SMOKE/state"
export PROJECTMUX_CONFIG_ROOT="$SMOKE/config"
export TMUX_TMPDIR="$SMOKE/tmux"
mkdir -p "$PROJECTMUX_CONFIG_ROOT" "$TMUX_TMPDIR"
printf 'version: 1\n' > "$PROJECTMUX_CONFIG_ROOT/defaults.yaml"
go build -o "$SMOKE/projectmux" ./cmd/projectmux

"$SMOKE/projectmux" open --json;        echo "open exit: $?"       # want 0 (created)
"$SMOKE/projectmux" open --json;        echo "reopen exit: $?"     # want 0 (already-running)
"$SMOKE/projectmux" attach --json;      echo "attach exit: $?"     # want 0 (live/match)
"$SMOKE/projectmux" list;               echo "list exit: $?"       # want 0, shows the live recorded workspace
"$SMOKE/projectmux" status;             echo "status exit: $?"     # want 0, plan session=none
"$SMOKE/projectmux" attach no-such-ws;  echo "unknown exit: $?"    # want 4
tmux kill-server 2>/dev/null
"$SMOKE/projectmux" attach;             echo "absent exit: $?"     # want 1, hint to run open
```

Expected: every `want` matches; the smoke run exercises the real binary end to end — creation on the isolated `TMUX_TMPDIR` server, idempotent reopen, live attach report, `list`/`status` agreeing, and the absent-session hint after the server dies.

- [ ] **Step 3: Spec cross-check**

Re-read `docs/superpowers/specs/2026-08-05-open-attach-design.md` §2–§9 and confirm each observable behavior maps to a test or the smoke transcript. Confirm `attach` cannot mutate: `grep -n "AdoptSessionName\|AllocateSessionName\|RegisterWorkspace\|RecordOperation\|CommitReconciliation\|RecordContainerObservation" internal/cli/attach.go` must return nothing.

- [ ] **Step 4: Commit any fixes**

`git status` — if clean, done; otherwise fix, re-run gates, commit with a message describing the fix.

---

## Self-review notes

- Spec §2 (open behavior, bare form, attach behavior, exit 6) → Tasks 7–8 + cli.go edits in each; §3 (lock) → Task 1; §4 (actuator, env, derivation, container-window gate) → Tasks 5–6; §5 (Ensure loop, squat check, confirmation, per-action names) → Task 4; §6 (host-only gating) → Task 6; §7 (AdoptSessionName) → Task 2 (+fake in Task 3); §8 (CLI wiring/seams) → Tasks 6–8; §9 (testing incl. lifecycle) → per-task tests + Task 9; §10 exclusions — nothing here implements them.
- Type consistency verified: `WindowSpec`/`SessionSpec`/`SessionActuator` (Task 3) are consumed with identical shapes in Tasks 4–7; `RefusalError` (Task 4) is mapped in Task 6 and asserted in Tasks 7–8; `AdoptSessionName` signature identical in Tasks 2/3/4 and the Task 6 guard shadow.
- Existing-test interaction called out where behavior changes: the Task 7 dispatch fallback alters unknown-command handling; its Step 4 says how to reconcile any pre-existing exit-2 assertion.
