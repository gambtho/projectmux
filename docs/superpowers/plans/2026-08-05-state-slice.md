# State Slice Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `docs/superpowers/specs/2026-08-05-state-slice-design.md`: the SQLite current-state store (`internal/state`) and the controller interfaces, fakes, and observe/plan core (`internal/controller`), verified entirely by tests — no CLI changes.

**Architecture:** `internal/state` owns the database file, embedded migrations, and every line of SQL, exposing coarse methods that are each one transaction internally. `internal/controller` imports `state`, defines the interfaces it consumes (`Store`, `SessionObserver`, `ContainerObserver`, `Clock`), assembles tri-state snapshots in `Observe`, and computes typed plans in the pure `BuildPlan`. Fakes live in `internal/controller/fake` for reuse by the next slice.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (pure-Go driver, v1.56.0), `database/sql`, `embed`. Existing packages `internal/config` and `internal/resolve` are consumed, never modified.

## Global Constraints

- Module: `github.com/gambtho/projectmux`; Go floor `1.25` (do not raise it).
- `CGO_ENABLED=0 go build ./cmd/projectmux` must keep working — `modernc.org/sqlite` is pure Go; add no other dependency.
- Gates on every task: `gofmt -l .` empty, `go vet ./...` clean, `go test ./... -count=1 -race` green.
- All timestamps are RFC3339Nano UTC text supplied by callers (`now time.Time`); never `CURRENT_TIMESTAMP`.
- Container health values are exactly `present`, `missing`, `unknown` (design §7).
- tmux identity keys are exactly `@dev_workspace_id`, `@dev_slug`, `@dev_worktree` — the spelling is load-bearing for Phase 1 adoption.
- The error-summary bound is 4096 bytes (`state.MaxErrorSummaryBytes`).
- Commit as the repo-configured git identity (already set); commit after every green task.
- The spec names the pure planner `Plan(snapshot)`; in Go the function is `BuildPlan` because the returned type is named `Plan`. This deviation is intentional — do not rename the type.

---

### Task 1: Dependency and state-root resolution

**Files:**
- Create: `internal/state/state.go`
- Create: `internal/state/state_test.go`
- Modify: `go.mod`, `go.sum` (via `go get`)

**Interfaces:**
- Consumes: nothing from this repo.
- Produces: `state.Root() (string, error)` — later tasks and future CLI slices call it; `PROJECTMUX_STATE_ROOT` override, then `$XDG_STATE_HOME/projectmux`, then `~/.local/state/projectmux`. Mirrors `config.Root()` (internal/config/load.go:19).

- [ ] **Step 1: Add the driver dependency**

```bash
go get modernc.org/sqlite@v1.56.0
go mod tidy
```

- [ ] **Step 2: Write the failing test**

Create `internal/state/state_test.go`:

```go
package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestRootPrefersExplicitOverrideThenXDG(t *testing.T) {
	t.Setenv("PROJECTMUX_STATE_ROOT", "/tmp/override")
	t.Setenv("XDG_STATE_HOME", "/tmp/xdg")
	got, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != "/tmp/override" {
		t.Errorf("Root = %q, want the explicit override", got)
	}

	t.Setenv("PROJECTMUX_STATE_ROOT", "")
	got, err = Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != filepath.Join("/tmp/xdg", "projectmux") {
		t.Errorf("Root = %q, want the XDG state home", got)
	}

	t.Setenv("XDG_STATE_HOME", "")
	got, err = Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if !strings.HasSuffix(got, filepath.Join(".local", "state", "projectmux")) {
		t.Errorf("Root = %q, want ~/.local/state/projectmux", got)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/state/ -run TestRoot -v`
Expected: FAIL (package does not exist / `Root` undefined).

- [ ] **Step 4: Implement `Root`**

Create `internal/state/state.go`:

```go
// Package state owns the SQLite database holding current operational
// metadata. It applies migrations on open and is the only package that
// issues SQL. Callers never see transactions: every exported method is one
// transaction internally.
package state

import (
	"fmt"
	"os"
	"path/filepath"
)

// Root resolves the state directory: an explicit override for tests and
// unusual installations, then the XDG state home, then the conventional
// fallback. It mirrors config.Root.
func Root() (string, error) {
	if v := os.Getenv("PROJECTMUX_STATE_ROOT"); v != "" {
		return v, nil
	}
	if v := os.Getenv("XDG_STATE_HOME"); v != "" {
		return filepath.Join(v, "projectmux"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate the state root: %w", err)
	}
	return filepath.Join(home, ".local", "state", "projectmux"), nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/state/ -run TestRoot -v`
Expected: PASS.

- [ ] **Step 6: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add go.mod go.sum internal/state/
git commit -m "feat: add the state package root resolution and the sqlite driver"
```

---

### Task 2: Open, schema 0001, and serialized migrations

**Files:**
- Create: `internal/state/migrations/0001_initial.sql`
- Create: `internal/state/migrate.go`
- Modify: `internal/state/state.go` (add `Store`, `Open`, `Close`)
- Create: `internal/state/migrate_test.go`

**Interfaces:**
- Consumes: `Root` semantics from Task 1 (tests use `t.TempDir()` directly).
- Produces: `state.Open(root string) (*Store, error)`, `(*Store).Close() error`, `state.SchemaVersion = 1`, `state.FutureSchemaError{Database, Supported int}`. The unexported `Store.db *sql.DB` field is used by later in-package tasks and white-box tests.

- [ ] **Step 1: Write the failing tests**

Create `internal/state/migrate_test.go`:

```go
package state

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
)

func TestOpenCreatesTheLatestSchema(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
	for _, table := range []string{"workspaces", "container_bindings", "last_operations"} {
		var n int
		err := s.db.QueryRow(
			"SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&n)
		if err != nil || n != 1 {
			t.Errorf("table %s missing (n=%d, err=%v)", table, n, err)
		}
	}
}

func TestOpenIsIdempotent(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 2; i++ {
		s, err := Open(root)
		if err != nil {
			t.Fatalf("Open #%d: %v", i+1, err)
		}
		s.Close()
	}
}

func TestOpenRefusesAFutureSchema(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatalf("setting future version: %v", err)
	}
	s.Close()

	_, err = Open(root)
	var future *FutureSchemaError
	if !errors.As(err, &future) {
		t.Fatalf("error = %v, want *FutureSchemaError", err)
	}
	if future.Database != 99 || future.Supported != SchemaVersion {
		t.Errorf("FutureSchemaError = %+v", future)
	}
}

func TestConcurrentOpenAppliesMigrationsExactlyOnce(t *testing.T) {
	root := t.TempDir()
	var wg sync.WaitGroup
	errs := make([]error, 4)
	stores := make([]*Store, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stores[i], errs[i] = Open(root)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("Open #%d: %v", i, err)
		}
		if stores[i] != nil {
			defer stores[i].Close()
		}
	}
	var version int
	if err := stores[0].db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
}

func TestPragmasApplyToEveryPooledConnection(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	conns := make([]*sql.Conn, 2)
	for i := range conns {
		conns[i], err = s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn #%d: %v", i, err)
		}
		defer conns[i].Close()
	}
	for i, conn := range conns {
		var journal string
		if err := conn.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatalf("journal_mode #%d: %v", i, err)
		}
		if journal != "wal" {
			t.Errorf("connection %d journal_mode = %q, want wal", i, journal)
		}
		var timeout int
		if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&timeout); err != nil {
			t.Fatalf("busy_timeout #%d: %v", i, err)
		}
		if timeout != 5000 {
			t.Errorf("connection %d busy_timeout = %d, want 5000", i, timeout)
		}
		var fk int
		if err := conn.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatalf("foreign_keys #%d: %v", i, err)
		}
		if fk != 1 {
			t.Errorf("connection %d foreign_keys = %d, want 1", i, fk)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/state/ -run 'TestOpen|TestConcurrent|TestPragmas' -v`
Expected: FAIL (`Open` undefined).

- [ ] **Step 3: Write the schema migration**

Create `internal/state/migrations/0001_initial.sql`:

```sql
-- Schema version 1: current operational metadata only (design §7).
-- No event stream, no history. Timestamps are RFC3339Nano UTC text
-- supplied by the application.

CREATE TABLE workspaces (
    id               TEXT PRIMARY KEY,
    slug             TEXT NOT NULL,
    worktree         TEXT NOT NULL UNIQUE,
    is_primary       INTEGER NOT NULL CHECK (is_primary IN (0, 1)),
    proposed_session TEXT NOT NULL,
    actual_session   TEXT UNIQUE,
    desired_digest   TEXT,
    applied_digest   TEXT,
    registered_at    TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- A row exists only once a binding has been recorded: no row means no
-- binding has ever existed, and health is non-null whenever one does.
-- health=missing or health=unknown never clears the identity columns;
-- only a successful replacement overwrites them.
CREATE TABLE container_bindings (
    workspace_id   TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    container_id   TEXT NOT NULL,
    container_user TEXT,
    workdir        TEXT,
    health         TEXT NOT NULL CHECK (health IN ('present', 'missing', 'unknown')),
    observed_at    TEXT NOT NULL
);

CREATE TABLE last_operations (
    workspace_id  TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    operation     TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN ('ok', 'failed')),
    exit_status   INTEGER,
    error_summary TEXT,
    finished_at   TEXT NOT NULL
);
```

- [ ] **Step 4: Implement migrations**

Create `internal/state/migrate.go`:

```go
package state

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
)

// SchemaVersion is the newest schema this build understands.
const SchemaVersion = 1

//go:embed migrations/*.sql
var migrations embed.FS

// FutureSchemaError reports a database written by a newer build. It is
// refused, never "repaired" (spec §2).
type FutureSchemaError struct {
	Database  int
	Supported int
}

func (e *FutureSchemaError) Error() string {
	return fmt.Sprintf(
		"the state database is schema version %d, but this build supports up to %d; upgrade projectmux",
		e.Database, e.Supported)
}

// migrate brings the database to SchemaVersion. Each migration runs in its
// own immediate transaction (the DSN's _txlock=immediate takes the write
// lock at BEGIN), and the version is re-read inside that transaction, so
// concurrent opens serialize on the lock and never replay a migration.
func migrate(db *sql.DB) error {
	for {
		done, err := applyNext(db)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func applyNext(db *sql.DB) (done bool, err error) {
	tx, err := db.Begin()
	if err != nil {
		return false, fmt.Errorf("beginning a migration transaction: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return false, fmt.Errorf("reading the schema version: %w", err)
	}
	if version > SchemaVersion {
		return false, &FutureSchemaError{Database: version, Supported: SchemaVersion}
	}
	if version == SchemaVersion {
		return true, nil
	}

	next := version + 1
	matches, err := fs.Glob(migrations, fmt.Sprintf("migrations/%04d_*.sql", next))
	if err != nil {
		return false, fmt.Errorf("locating migration %d: %w", next, err)
	}
	if len(matches) != 1 {
		return false, fmt.Errorf("migration %d: found %d files, want exactly one", next, len(matches))
	}
	body, err := migrations.ReadFile(matches[0])
	if err != nil {
		return false, fmt.Errorf("reading migration %d: %w", next, err)
	}
	if _, err := tx.Exec(string(body)); err != nil {
		return false, fmt.Errorf("applying migration %d: %w", next, err)
	}
	// PRAGMA does not accept bound parameters; next is an int under our
	// control, not user input.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", next)); err != nil {
		return false, fmt.Errorf("recording schema version %d: %w", next, err)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("committing migration %d: %w", next, err)
	}
	return false, nil
}
```

- [ ] **Step 5: Implement `Store`, `Open`, `Close`**

Append to `internal/state/state.go` (add `"database/sql"` and the blank driver import `_ "modernc.org/sqlite"` to the import block):

```go
// Store is the application's only SQL issuer.
type Store struct {
	db *sql.DB
}

// Open creates the state directory and database as needed, configures
// every pooled connection, and applies pending migrations.
//
// The pragmas ride in the DSN so the driver applies them to each new
// connection in the pool; a one-off Exec would configure only whichever
// connection happened to run it (design §11). _txlock=immediate makes
// every transaction take the write lock at BEGIN, so concurrent writers
// queue on the busy timeout instead of failing at commit.
func Open(root string) (*Store, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("creating the state directory: %w", err)
	}
	dsn := "file:" + filepath.Join(root, "state.db") +
		"?_txlock=immediate" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening the state database: %w", err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close closes the connection pool.
func (s *Store) Close() error { return s.db.Close() }
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/state/ -count=1 -race -v`
Expected: all PASS, including the concurrent-open and per-connection pragma tests.

- [ ] **Step 7: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add internal/state/
git commit -m "feat: open the state database with schema 0001 and serialized migrations"
```

---

### Task 3: Record types, RegisterWorkspace, and reads

**Files:**
- Create: `internal/state/types.go`
- Create: `internal/state/store.go`
- Create: `internal/state/store_test.go`

**Interfaces:**
- Consumes: `Store`/`Open` from Task 2; `resolve.Workspace` (fields `ID, Slug, Worktree, SessionName, IsPrimary`).
- Produces:
  - Types: `Health` (`HealthPresent/HealthMissing/HealthUnknown`), `Outcome` (`OutcomeOK/OutcomeFailed`), `Record`, `ContainerBinding`, `ContainerObservation`, `Operation`, `ReconciliationResult`, `ErrNotFound`, `MaxErrorSummaryBytes = 4096`.
  - Methods: `RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error`, `Workspace(id string) (Record, error)`, `Workspaces() ([]Record, error)`.

- [ ] **Step 1: Write the types**

Create `internal/state/types.go`:

```go
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
```

- [ ] **Step 2: Write the failing tests**

Create `internal/state/store_test.go`:

```go
package state

import (
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
)

var testTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testWorkspace(id string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        "slabledger",
		Worktree:    "/home/u/workspace/slabledger-" + id,
		SessionName: "slabledger",
		IsPrimary:   true,
	}
}

func mustRegister(t *testing.T, s *Store, ws resolve.Workspace) {
	t.Helper()
	if err := s.RegisterWorkspace(ws, "sha256:aaaa", testTime); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
}

func TestRegisterWorkspaceRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ID != ws.ID || rec.Slug != ws.Slug || rec.Worktree != ws.Worktree ||
		rec.ProposedSession != ws.SessionName || !rec.IsPrimary {
		t.Errorf("record = %+v, want the registered identity", rec)
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:aaaa" {
		t.Errorf("desired digest = %v", rec.DesiredDigest)
	}
	if rec.ActualSession != nil || rec.AppliedDigest != nil ||
		rec.Container != nil || rec.LastOperation != nil {
		t.Errorf("fresh registration should have no assignment, binding, or operation: %+v", rec)
	}
	if !rec.RegisteredAt.Equal(testTime) || !rec.UpdatedAt.Equal(testTime) {
		t.Errorf("timestamps = %v / %v, want %v", rec.RegisteredAt, rec.UpdatedAt, testTime)
	}
}

func TestRegisterWorkspaceIsAnIdempotentRefresh(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	later := testTime.Add(time.Hour)
	ws.Slug = "renamed"
	if err := s.RegisterWorkspace(ws, "sha256:bbbb", later); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Slug != "renamed" || *rec.DesiredDigest != "sha256:bbbb" {
		t.Errorf("refresh did not apply: %+v", rec)
	}
	if !rec.RegisteredAt.Equal(testTime) {
		t.Errorf("registered_at changed on refresh: %v", rec.RegisteredAt)
	}
	if !rec.UpdatedAt.Equal(later) {
		t.Errorf("updated_at = %v, want %v", rec.UpdatedAt, later)
	}
}

func TestWorkspaceNotFoundIsTyped(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Workspace("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestWorkspacesListsAllOrdered(t *testing.T) {
	s := openTestStore(t)
	a := testWorkspace("w1")
	a.Slug = "bravo"
	b := testWorkspace("w2")
	b.Slug = "alpha"
	mustRegister(t, s, a)
	mustRegister(t, s, b)

	all, err := s.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(all) != 2 || all[0].Slug != "alpha" || all[1].Slug != "bravo" {
		t.Errorf("Workspaces = %+v, want ordered by slug", all)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/state/ -run 'TestRegister|TestWorkspace' -v`
Expected: FAIL (`RegisterWorkspace`, `Workspace`, `Workspaces` undefined).

- [ ] **Step 4: Implement registration and reads**

Create `internal/state/store.go`:

```go
package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
)

func encodeTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func decodeTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RegisterWorkspace upserts the workspace identity. Re-registration
// refreshes everything derivable from resolution and configuration while
// preserving registered_at, the assigned session name, the applied digest,
// and any binding — rebuilding the database is simply re-running
// registration (design §7).
func (s *Store) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO workspaces
			(id, slug, worktree, is_primary, proposed_session,
			 desired_digest, registered_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			slug             = excluded.slug,
			worktree         = excluded.worktree,
			is_primary       = excluded.is_primary,
			proposed_session = excluded.proposed_session,
			desired_digest   = excluded.desired_digest,
			updated_at       = excluded.updated_at`,
		ws.ID, ws.Slug, ws.Worktree, boolInt(ws.IsPrimary), ws.SessionName,
		desiredDigest, encodeTime(now), encodeTime(now))
	if err != nil {
		return fmt.Errorf("registering workspace %s: %w", ws.ID, err)
	}
	return tx.Commit()
}

const selectRecord = `
SELECT
	w.id, w.slug, w.worktree, w.is_primary, w.proposed_session,
	w.actual_session, w.desired_digest, w.applied_digest,
	w.registered_at, w.updated_at,
	b.kind, b.container_id, b.container_user, b.workdir, b.health, b.observed_at,
	o.operation, o.outcome, o.exit_status, o.error_summary, o.finished_at
FROM workspaces w
LEFT JOIN container_bindings b ON b.workspace_id = w.id
LEFT JOIN last_operations o ON o.workspace_id = w.id`

// Workspace returns the joined record for one workspace, or ErrNotFound.
func (s *Store) Workspace(id string) (Record, error) {
	rec, err := scanRecord(s.db.QueryRow(selectRecord+" WHERE w.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("workspace %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Record{}, fmt.Errorf("reading workspace %s: %w", id, err)
	}
	return rec, nil
}

// Workspaces returns every registered workspace ordered by slug, then
// worktree.
func (s *Store) Workspaces() ([]Record, error) {
	rows, err := s.db.Query(selectRecord + " ORDER BY w.slug, w.worktree")
	if err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	defer rows.Close()

	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a workspace row: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(r rowScanner) (Record, error) {
	var (
		rec                 Record
		isPrimary           int
		actual, desired     sql.NullString
		applied             sql.NullString
		registered, updated string
		cKind, cID          sql.NullString
		cUser, cWorkdir     sql.NullString
		cHealth, cObserved  sql.NullString
		oName, oOutcome     sql.NullString
		oSummary, oFinished sql.NullString
		oExit               sql.NullInt64
	)
	err := r.Scan(
		&rec.ID, &rec.Slug, &rec.Worktree, &isPrimary, &rec.ProposedSession,
		&actual, &desired, &applied, &registered, &updated,
		&cKind, &cID, &cUser, &cWorkdir, &cHealth, &cObserved,
		&oName, &oOutcome, &oExit, &oSummary, &oFinished)
	if err != nil {
		return Record{}, err
	}

	rec.IsPrimary = isPrimary == 1
	rec.ActualSession = nullable(actual)
	rec.DesiredDigest = nullable(desired)
	rec.AppliedDigest = nullable(applied)
	if rec.RegisteredAt, err = decodeTime(registered); err != nil {
		return Record{}, fmt.Errorf("registered_at: %w", err)
	}
	if rec.UpdatedAt, err = decodeTime(updated); err != nil {
		return Record{}, fmt.Errorf("updated_at: %w", err)
	}

	if cKind.Valid {
		observedAt, err := decodeTime(cObserved.String)
		if err != nil {
			return Record{}, fmt.Errorf("observed_at: %w", err)
		}
		rec.Container = &ContainerBinding{
			Kind:          cKind.String,
			ContainerID:   cID.String,
			ContainerUser: cUser.String,
			Workdir:       cWorkdir.String,
			Health:        Health(cHealth.String),
			ObservedAt:    observedAt,
		}
	}

	if oName.Valid {
		finishedAt, err := decodeTime(oFinished.String)
		if err != nil {
			return Record{}, fmt.Errorf("finished_at: %w", err)
		}
		op := &Operation{
			Name:         oName.String,
			Outcome:      Outcome(oOutcome.String),
			ErrorSummary: oSummary.String,
			FinishedAt:   finishedAt,
		}
		if oExit.Valid {
			exit := int(oExit.Int64)
			op.ExitStatus = &exit
		}
		rec.LastOperation = op
	}
	return rec, nil
}

func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/state/ -count=1 -race`
Expected: PASS.

- [ ] **Step 6: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add internal/state/
git commit -m "feat: add state record types, workspace registration, and joined reads"
```

---

### Task 4: AllocateSessionName under the UNIQUE constraint

**Files:**
- Modify: `internal/state/store.go` (append)
- Modify: `internal/state/store_test.go` (append)

**Interfaces:**
- Consumes: Task 3's `Store` methods and test helpers (`openTestStore`, `testWorkspace`, `mustRegister`, `testTime`).
- Produces: `AllocateSessionName(workspaceID string, now time.Time) (string, error)` — returns the already-assigned name when present; otherwise assigns `proposed`, `proposed-2`, `proposed-3`, … The `UNIQUE` constraint on `actual_session`, not a SELECT-then-INSERT, prevents duplicates (design §7).

- [ ] **Step 1: Write the failing tests**

Append to `internal/state/store_test.go` (add `"sync"` to its imports):

```go
func TestAllocateSessionNameAssignsTheProposedNameFirst(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	name, err := s.AllocateSessionName("w1", testTime)
	if err != nil {
		t.Fatalf("AllocateSessionName: %v", err)
	}
	if name != "slabledger" {
		t.Errorf("name = %q, want the proposed name", name)
	}

	again, err := s.AllocateSessionName("w1", testTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("second AllocateSessionName: %v", err)
	}
	if again != name {
		t.Errorf("reallocation = %q, want the stable assignment %q", again, name)
	}
}

func TestAllocateSessionNameSuffixesOnCollision(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	mustRegister(t, s, testWorkspace("w2"))

	first, err := s.AllocateSessionName("w1", testTime)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.AllocateSessionName("w2", testTime)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != "slabledger" || second != "slabledger-2" {
		t.Errorf("names = %q, %q; want slabledger and slabledger-2", first, second)
	}
}

func TestAllocateSessionNameForUnknownWorkspace(t *testing.T) {
	s := openTestStore(t)
	_, err := s.AllocateSessionName("absent", testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestConcurrentAllocationYieldsDistinctNames is the design-§12 gate: the
// database constraint, not application convention, prevents duplicates.
func TestConcurrentAllocationYieldsDistinctNames(t *testing.T) {
	s := openTestStore(t)
	const n = 8
	for i := 0; i < n; i++ {
		mustRegister(t, s, testWorkspace(fmt.Sprintf("w%d", i)))
	}

	names := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names[i], errs[i] = s.AllocateSessionName(fmt.Sprintf("w%d", i), testTime)
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("allocation %d: %v", i, errs[i])
		}
		seen[names[i]]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("name %q assigned %d times", name, count)
		}
	}
}
```

Also add `"fmt"` to the test file's imports.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/state/ -run TestAllocate -v -race`
Expected: FAIL (`AllocateSessionName` undefined).

- [ ] **Step 3: Implement allocation**

Append to `internal/state/store.go` (add the two driver imports to the file's import block):

```go
import (
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)
```

(merge into the existing import block, not a second one), then:

```go
// maxNameCandidates bounds the collision scan; hitting it means something
// is systematically wrong, not that the loop should keep going.
const maxNameCandidates = 100

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// AllocateSessionName assigns the workspace's actual session name inside
// one immediate transaction. The proposed name is tried first, then
// numbered variants; the UNIQUE constraint on actual_session is the
// collision-prevention mechanism — there is deliberately no
// SELECT-then-INSERT check (design §7). An already-assigned workspace gets
// its existing name back.
func (s *Store) AllocateSessionName(workspaceID string, now time.Time) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()

	var proposed string
	var actual sql.NullString
	err = tx.QueryRow(
		"SELECT proposed_session, actual_session FROM workspaces WHERE id = ?",
		workspaceID).Scan(&proposed, &actual)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("reading workspace %s: %w", workspaceID, err)
	}
	if actual.Valid {
		return actual.String, tx.Commit()
	}

	for i := 1; i <= maxNameCandidates; i++ {
		candidate := proposed
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", proposed, i)
		}
		_, err := tx.Exec(
			"UPDATE workspaces SET actual_session = ?, updated_at = ? WHERE id = ?",
			candidate, encodeTime(now), workspaceID)
		if isUniqueViolation(err) {
			// SQLite rolls back only the failed statement; the
			// transaction stays usable for the next candidate.
			continue
		}
		if err != nil {
			return "", fmt.Errorf("assigning session name %q: %w", candidate, err)
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("committing the session name: %w", err)
		}
		return candidate, nil
	}
	return "", fmt.Errorf(
		"no free session name near %q after %d candidates", proposed, maxNameCandidates)
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/state/ -count=1 -race`
Expected: PASS, including the concurrent test under `-race`.

- [ ] **Step 5: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add internal/state/
git commit -m "feat: allocate session names transactionally under the unique constraint"
```

---

### Task 5: RecordContainerObservation with tri-state retention

**Files:**
- Modify: `internal/state/store.go` (append)
- Modify: `internal/state/store_test.go` (append)

**Interfaces:**
- Consumes: Tasks 3–4.
- Produces: `RecordContainerObservation(workspaceID string, obs ContainerObservation, now time.Time) error` and the internal helper `recordContainer(tx, workspaceID, obs, now)` reused by Task 6's `CommitReconciliation`.

- [ ] **Step 1: Write the failing tests**

Append to `internal/state/store_test.go`:

```go
func presentObservation(id string) ContainerObservation {
	return ContainerObservation{
		Kind:          "devcontainer",
		ContainerID:   id,
		ContainerUser: "vscode",
		Workdir:       "/workspaces/slabledger",
		Health:        HealthPresent,
	}
}

func TestContainerObservationRoundTrips(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	if err := s.RecordContainerObservation("w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("RecordContainerObservation: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	b := rec.Container
	if b == nil || b.ContainerID != "c-1" || b.Health != HealthPresent ||
		b.Kind != "devcontainer" || b.ContainerUser != "vscode" ||
		!b.ObservedAt.Equal(testTime) {
		t.Errorf("binding = %+v", b)
	}
}

// TestMissingAndUnknownRetainTheBinding is the design-§7 tri-state gate:
// neither confirmed absence nor a failed probe erases the identity needed
// for repair.
func TestMissingAndUnknownRetainTheBinding(t *testing.T) {
	for _, health := range []Health{HealthMissing, HealthUnknown} {
		t.Run(string(health), func(t *testing.T) {
			s := openTestStore(t)
			mustRegister(t, s, testWorkspace("w1"))
			if err := s.RecordContainerObservation("w1", presentObservation("c-1"), testTime); err != nil {
				t.Fatalf("seed: %v", err)
			}

			later := testTime.Add(time.Hour)
			err := s.RecordContainerObservation("w1", ContainerObservation{Health: health}, later)
			if err != nil {
				t.Fatalf("record %s: %v", health, err)
			}
			rec, err := s.Workspace("w1")
			if err != nil {
				t.Fatalf("Workspace: %v", err)
			}
			b := rec.Container
			if b == nil || b.ContainerID != "c-1" || b.Kind != "devcontainer" {
				t.Fatalf("identity was not retained: %+v", b)
			}
			if b.Health != health || !b.ObservedAt.Equal(later) {
				t.Errorf("health/freshness not updated: %+v", b)
			}
		})
	}
}

func TestReplacementOverwritesTheBinding(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	if err := s.RecordContainerObservation("w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.RecordContainerObservation("w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing: %v", err)
	}

	if err := s.RecordContainerObservation("w1", presentObservation("c-2"), testTime.Add(time.Hour)); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Container == nil || rec.Container.ContainerID != "c-2" ||
		rec.Container.Health != HealthPresent {
		t.Errorf("binding = %+v, want the replacement c-2", rec.Container)
	}
}

func TestObservationsForNeverBoundAndUnknownWorkspaces(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	// missing/unknown with no existing binding record nothing: there is no
	// identity to retain and none to invent.
	if err := s.RecordContainerObservation("w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing on never-bound: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Container != nil {
		t.Errorf("never-bound workspace grew a binding: %+v", rec.Container)
	}

	err = s.RecordContainerObservation("absent", presentObservation("c-1"), testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown workspace error = %v, want ErrNotFound", err)
	}

	err = s.RecordContainerObservation("w1", ContainerObservation{Health: HealthPresent}, testTime)
	if err == nil {
		t.Error("present without a container ID should be rejected")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/state/ -run TestContainer -v`
Expected: FAIL (`RecordContainerObservation` undefined). (`TestMissingAndUnknown…`, `TestReplacement…`, and `TestObservations…` also fail to compile — that is the same failure.)

- [ ] **Step 3: Implement observation recording**

Append to `internal/state/store.go`:

```go
// txExecer is the slice of *sql.Tx the record helpers need, so
// CommitReconciliation can reuse them inside its own transaction.
type txExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// RecordContainerObservation upserts the container binding. A present
// observation replaces the binding; missing and unknown update health and
// freshness while retaining the stored identity (design §7). With no
// existing binding, missing and unknown record nothing.
func (s *Store) RecordContainerObservation(workspaceID string, obs ContainerObservation, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()
	if err := recordContainer(tx, workspaceID, obs, now); err != nil {
		return err
	}
	return tx.Commit()
}

func recordContainer(tx txExecer, workspaceID string, obs ContainerObservation, now time.Time) error {
	if err := requireWorkspace(tx, workspaceID); err != nil {
		return err
	}
	switch obs.Health {
	case HealthPresent:
		if obs.ContainerID == "" {
			return errors.New("a present container observation must carry a container ID")
		}
		_, err := tx.Exec(`
			INSERT INTO container_bindings
				(workspace_id, kind, container_id, container_user, workdir, health, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(workspace_id) DO UPDATE SET
				kind           = excluded.kind,
				container_id   = excluded.container_id,
				container_user = excluded.container_user,
				workdir        = excluded.workdir,
				health         = excluded.health,
				observed_at    = excluded.observed_at`,
			workspaceID, obs.Kind, obs.ContainerID, obs.ContainerUser,
			obs.Workdir, string(obs.Health), encodeTime(now))
		if err != nil {
			return fmt.Errorf("recording the container binding: %w", err)
		}
		return nil
	case HealthMissing, HealthUnknown:
		_, err := tx.Exec(
			"UPDATE container_bindings SET health = ?, observed_at = ? WHERE workspace_id = ?",
			string(obs.Health), encodeTime(now), workspaceID)
		if err != nil {
			return fmt.Errorf("recording container health: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid container health %q", obs.Health)
	}
}

func requireWorkspace(tx txExecer, workspaceID string) error {
	var n int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM workspaces WHERE id = ?", workspaceID).Scan(&n); err != nil {
		return fmt.Errorf("checking workspace %s: %w", workspaceID, err)
	}
	if n == 0 {
		return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
	}
	return nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/state/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add internal/state/
git commit -m "feat: record container observations with tri-state binding retention"
```

---

### Task 6: RecordOperation and CommitReconciliation

**Files:**
- Modify: `internal/state/store.go` (append)
- Modify: `internal/state/store_test.go` (append)

**Interfaces:**
- Consumes: Task 5's `recordContainer` and `requireWorkspace` helpers.
- Produces: `RecordOperation(workspaceID string, op Operation, now time.Time) error` and `CommitReconciliation(workspaceID string, r ReconciliationResult, now time.Time) error`. A nil `ReconciliationResult.AppliedDigest` never touches `applied_digest` (spec §4: failure never clears drift).

- [ ] **Step 1: Write the failing tests**

Append to `internal/state/store_test.go` (add `"strings"` to its imports):

```go
func TestRecordOperationRoundTripsAndTruncates(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	exit := 7
	op := Operation{
		Name:         "open",
		Outcome:      OutcomeFailed,
		ExitStatus:   &exit,
		ErrorSummary: strings.Repeat("x", MaxErrorSummaryBytes+100),
	}
	if err := s.RecordOperation("w1", op, testTime); err != nil {
		t.Fatalf("RecordOperation: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	got := rec.LastOperation
	if got == nil || got.Name != "open" || got.Outcome != OutcomeFailed ||
		got.ExitStatus == nil || *got.ExitStatus != 7 ||
		!got.FinishedAt.Equal(testTime) {
		t.Fatalf("operation = %+v", got)
	}
	if len(got.ErrorSummary) != MaxErrorSummaryBytes {
		t.Errorf("summary length = %d, want the %d-byte bound", len(got.ErrorSummary), MaxErrorSummaryBytes)
	}

	// The row is an upsert: the next operation replaces it.
	if err := s.RecordOperation("w1", Operation{Name: "stop", Outcome: OutcomeOK}, testTime.Add(time.Hour)); err != nil {
		t.Fatalf("second RecordOperation: %v", err)
	}
	rec, err = s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.LastOperation.Name != "stop" || rec.LastOperation.ExitStatus != nil {
		t.Errorf("operation = %+v, want the replacement", rec.LastOperation)
	}
}

func TestCommitReconciliationAppliesEverythingAtomically(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	digest := "sha256:aaaa"
	obs := presentObservation("c-1")
	err := s.CommitReconciliation("w1", ReconciliationResult{
		AppliedDigest: &digest,
		Container:     &obs,
		Operation:     Operation{Name: "open", Outcome: OutcomeOK},
	}, testTime)
	if err != nil {
		t.Fatalf("CommitReconciliation: %v", err)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != digest {
		t.Errorf("applied digest = %v, want %q", rec.AppliedDigest, digest)
	}
	if rec.Container == nil || rec.Container.ContainerID != "c-1" {
		t.Errorf("container = %+v", rec.Container)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != OutcomeOK {
		t.Errorf("operation = %+v", rec.LastOperation)
	}
}

// TestFailedReconciliationLeavesDriftRecorded is the spec-§4 gate: a
// failure commits its outcome without advancing applied_digest.
func TestFailedReconciliationLeavesDriftRecorded(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	seeded := "sha256:old"
	if err := s.CommitReconciliation("w1", ReconciliationResult{
		AppliedDigest: &seeded,
		Operation:     Operation{Name: "open", Outcome: OutcomeOK},
	}, testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := s.CommitReconciliation("w1", ReconciliationResult{
		AppliedDigest: nil,
		Operation:     Operation{Name: "open", Outcome: OutcomeFailed, ErrorSummary: "devcontainer up timed out"},
	}, testTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("failed reconciliation: %v", err)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != seeded {
		t.Errorf("applied digest = %v, want the seeded %q untouched", rec.AppliedDigest, seeded)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != OutcomeFailed {
		t.Errorf("operation = %+v, want the failure recorded", rec.LastOperation)
	}
}

func TestCommitReconciliationForUnknownWorkspace(t *testing.T) {
	s := openTestStore(t)
	err := s.CommitReconciliation("absent", ReconciliationResult{
		Operation: Operation{Name: "open", Outcome: OutcomeOK},
	}, testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/state/ -run 'TestRecordOperation|TestCommitReconciliation|TestFailedReconciliation' -v`
Expected: FAIL (`RecordOperation`, `CommitReconciliation` undefined).

- [ ] **Step 3: Implement operations and the composite commit**

Append to `internal/state/store.go` (add `"strings"` to the import block):

```go
// boundedSummary enforces the 4 KiB error-summary bound, trimming any
// UTF-8 rune the byte cut split.
func boundedSummary(s string) string {
	if len(s) <= MaxErrorSummaryBytes {
		return s
	}
	return strings.ToValidUTF8(s[:MaxErrorSummaryBytes], "")
}

// RecordOperation upserts the workspace's last operation outcome. The
// store sets finished_at from now.
func (s *Store) RecordOperation(workspaceID string, op Operation, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()
	if err := recordOperation(tx, workspaceID, op, now); err != nil {
		return err
	}
	return tx.Commit()
}

func recordOperation(tx txExecer, workspaceID string, op Operation, now time.Time) error {
	if err := requireWorkspace(tx, workspaceID); err != nil {
		return err
	}
	var exit any
	if op.ExitStatus != nil {
		exit = *op.ExitStatus
	}
	_, err := tx.Exec(`
		INSERT INTO last_operations
			(workspace_id, operation, outcome, exit_status, error_summary, finished_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id) DO UPDATE SET
			operation     = excluded.operation,
			outcome       = excluded.outcome,
			exit_status   = excluded.exit_status,
			error_summary = excluded.error_summary,
			finished_at   = excluded.finished_at`,
		workspaceID, op.Name, string(op.Outcome), exit,
		boundedSummary(op.ErrorSummary), encodeTime(now))
	if err != nil {
		return fmt.Errorf("recording the operation outcome: %w", err)
	}
	return nil
}

// CommitReconciliation is design §9 step 5 as one transaction: the applied
// digest (only when the desired configuration was fully applied), any
// container observation, and the operation outcome commit together. A nil
// AppliedDigest leaves applied_digest untouched, so a failed
// reconciliation never clears recorded drift.
func (s *Store) CommitReconciliation(workspaceID string, r ReconciliationResult, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()

	if r.AppliedDigest != nil {
		res, err := tx.Exec(
			"UPDATE workspaces SET applied_digest = ?, updated_at = ? WHERE id = ?",
			*r.AppliedDigest, encodeTime(now), workspaceID)
		if err != nil {
			return fmt.Errorf("recording the applied digest: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("recording the applied digest: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
		}
	}
	if r.Container != nil {
		if err := recordContainer(tx, workspaceID, *r.Container, now); err != nil {
			return err
		}
	}
	if err := recordOperation(tx, workspaceID, r.Operation, now); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/state/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add internal/state/
git commit -m "feat: record operation outcomes and the composite reconciliation commit"
```

---

### Task 7: Controller types, interfaces, and fakes

**Files:**
- Create: `internal/controller/types.go`
- Create: `internal/controller/interfaces.go`
- Create: `internal/controller/fake/fake.go`
- Create: `internal/controller/fake/fake_test.go`

**Interfaces:**
- Consumes: everything `internal/state` exports (Tasks 2–6), `resolve.Workspace`, `config.Config`.
- Produces:
  - Constants: `KeyWorkspaceID = "@dev_workspace_id"`, `KeySlug = "@dev_slug"`, `KeyWorktree = "@dev_worktree"`.
  - Types: `SessionState` (`SessionLive/SessionAbsent/SessionUnknown`), `LiveSession{Name, WorkspaceID, Slug, Worktree string}`, `SessionQuery{WorkspaceID string; CandidateNames []string}`, `SessionObservation{ByIdentity *LiveSession; ByName []LiveSession}`, `ContainerObservation{Kind, ContainerID, ContainerUser, Workdir string; Health state.Health}`.
  - Interfaces: `Store`, `SessionObserver`, `ContainerObserver`, `Clock` — exact signatures below; `*state.Store` must satisfy `Store` via a compile-time assertion.
  - Fakes: `fake.NewStore() *fake.Store`, `fake.SessionObserver`, `fake.ContainerObserver`, `fake.Clock` — all satisfying the controller interfaces via compile-time assertions.

- [ ] **Step 1: Write the controller types**

Create `internal/controller/types.go`:

```go
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
```

Create `internal/controller/interfaces.go`:

```go
package controller

import (
	"context"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// ContainerObservation is a live observation from the container adapter,
// as opposed to state.ContainerObservation, which is the form the store
// records.
type ContainerObservation struct {
	Kind          string
	ContainerID   string
	ContainerUser string
	Workdir       string
	Health        state.Health
}

// Store is the slice of the state store the controller uses. *state.Store
// satisfies it; fakes mirror its semantics for tests.
type Store interface {
	RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error
	AllocateSessionName(workspaceID string, now time.Time) (string, error)
	RecordContainerObservation(workspaceID string, obs state.ContainerObservation, now time.Time) error
	RecordOperation(workspaceID string, op state.Operation, now time.Time) error
	CommitReconciliation(workspaceID string, r state.ReconciliationResult, now time.Time) error
	Workspace(id string) (state.Record, error)
	Workspaces() ([]state.Record, error)
}

var _ Store = (*state.Store)(nil)

// SessionObserver reports live tmux sessions. Its granularity is
// provisional until the tmux adapter slice exists.
type SessionObserver interface {
	ObserveSession(ctx context.Context, q SessionQuery) (SessionObservation, error)
}

// ContainerObserver probes an existing binding or discovers a container
// for a workspace that has none — post-rebuild reacquisition and
// devcontainer.enabled "auto" both need the workspace and configuration.
type ContainerObserver interface {
	ProbeContainer(ctx context.Context, binding state.ContainerBinding) (ContainerObservation, error)
	DiscoverContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (ContainerObservation, error)
}

// Clock supplies the timestamps the store persists.
type Clock interface {
	Now() time.Time
}
```

- [ ] **Step 2: Verify the state store satisfies the interface**

Run: `go build ./...`
Expected: success. (If this fails, the `Store` interface and `internal/state` method signatures have drifted — fix the interface to match Tasks 3–6, not the store.)

- [ ] **Step 3: Write the failing fake tests**

Create `internal/controller/fake/fake_test.go`:

```go
package fake

import (
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var testTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func testWorkspace(id, session string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        "slabledger",
		Worktree:    "/w/" + id,
		SessionName: session,
		IsPrimary:   true,
	}
}

func TestFakeStoreMirrorsAllocationAndRetention(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.RegisterWorkspace(testWorkspace("w2", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}

	first, err := s.AllocateSessionName("w1", testTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	second, err := s.AllocateSessionName("w2", testTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first != "slab" || second != "slab-2" {
		t.Errorf("names = %q, %q", first, second)
	}

	obs := state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}
	if err := s.RecordContainerObservation("w1", obs, testTime); err != nil {
		t.Fatalf("observation: %v", err)
	}
	if err := s.RecordContainerObservation("w1",
		state.ContainerObservation{Health: state.HealthMissing}, testTime); err != nil {
		t.Fatalf("missing: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.Container == nil || rec.Container.ContainerID != "c-1" ||
		rec.Container.Health != state.HealthMissing {
		t.Errorf("binding = %+v, want retained identity with missing health", rec.Container)
	}

	if _, err := s.Workspace("absent"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("error = %v, want state.ErrNotFound", err)
	}
}

func TestFakeStoreCommitReconciliationRespectsNilDigest(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	digest := "sha256:old"
	if err := s.CommitReconciliation("w1", state.ReconciliationResult{
		AppliedDigest: &digest,
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.CommitReconciliation("w1", state.ReconciliationResult{
		Operation: state.Operation{Name: "open", Outcome: state.OutcomeFailed},
	}, testTime); err != nil {
		t.Fatalf("failure: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != digest {
		t.Errorf("applied digest = %v, want %q untouched", rec.AppliedDigest, digest)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != state.OutcomeFailed {
		t.Errorf("operation = %+v", rec.LastOperation)
	}
}
```

- [ ] **Step 4: Run the tests to verify they fail**

Run: `go test ./internal/controller/... -v`
Expected: FAIL (package `fake` does not exist).

- [ ] **Step 5: Implement the fakes**

Create `internal/controller/fake/fake.go`:

```go
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
// what it was asked.
type ContainerObserver struct {
	ProbeResult    controller.ContainerObservation
	ProbeErr       error
	DiscoverResult controller.ContainerObservation
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

func (o *ContainerObserver) DiscoverContainer(_ context.Context, ws resolve.Workspace, _ config.Config) (controller.ContainerObservation, error) {
	o.Discovered = append(o.Discovered, ws.ID)
	if o.DiscoverErr != nil {
		return controller.ContainerObservation{}, o.DiscoverErr
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
	if r.AppliedDigest != nil {
		digest := *r.AppliedDigest
		rec.AppliedDigest = &digest
		rec.UpdatedAt = now
	}
	if r.Container != nil {
		if err := s.recordContainerLocked(workspaceID, *r.Container, now); err != nil {
			return err
		}
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
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/controller/... -count=1 -race`
Expected: PASS.

- [ ] **Step 7: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add internal/controller/
git commit -m "feat: add controller domain types, interfaces, and in-memory fakes"
```

---

### Task 8: Observe — tri-state snapshot assembly

**Files:**
- Create: `internal/controller/observe.go`
- Create: `internal/controller/observe_test.go`

**Interfaces:**
- Consumes: Task 7's types, interfaces, and fakes.
- Produces: `Controller{Store, Sessions, Containers, Clock}`, `Desired{Workspace resolve.Workspace; Config config.Config; Digest string}`, `Snapshot{Desired, Stored *state.Record, Session SessionSnapshot, Container ContainerSnapshot}`, `SessionSnapshot{State SessionState; ByIdentity *LiveSession; ByName []LiveSession; Err error}`, `ContainerSnapshot{Observed *ContainerObservation; Err error}`, and `(*Controller).Observe(ctx context.Context, d Desired) (Snapshot, error)`. Task 9's `BuildPlan` consumes `Snapshot`.

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/observe_test.go`:

```go
package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var testTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func testDesired(enabled string) controller.Desired {
	return controller.Desired{
		Workspace: resolve.Workspace{
			ID:          "w1",
			Slug:        "slabledger",
			Worktree:    "/w/slabledger",
			SessionName: "slabledger",
			IsPrimary:   true,
		},
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: enabled},
		},
		Digest: "sha256:desired",
	}
}

type deps struct {
	store      *fake.Store
	sessions   *fake.SessionObserver
	containers *fake.ContainerObserver
	ctrl       *controller.Controller
}

func newDeps() *deps {
	d := &deps{
		store:      fake.NewStore(),
		sessions:   &fake.SessionObserver{},
		containers: &fake.ContainerObserver{},
	}
	d.ctrl = &controller.Controller{
		Store:      d.store,
		Sessions:   d.sessions,
		Containers: d.containers,
		Clock:      &fake.Clock{Time: testTime},
	}
	return d
}

func TestObserveUnregisteredWorkspace(t *testing.T) {
	d := newDeps()
	snap, err := d.ctrl.Observe(context.Background(), testDesired("false"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Stored != nil {
		t.Errorf("stored = %+v, want nil for an unregistered workspace", snap.Stored)
	}
	if snap.Session.State != controller.SessionAbsent {
		t.Errorf("session state = %q, want absent", snap.Session.State)
	}
	if len(d.sessions.Queries) != 1 ||
		d.sessions.Queries[0].WorkspaceID != "w1" ||
		len(d.sessions.Queries[0].CandidateNames) != 1 ||
		d.sessions.Queries[0].CandidateNames[0] != "slabledger" {
		t.Errorf("session query = %+v", d.sessions.Queries)
	}
}

func TestObserveQueriesTheAssignedNameToo(t *testing.T) {
	d := newDeps()
	ws := testDesired("false").Workspace
	if err := d.store.RegisterWorkspace(ws, "sha256:desired", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Occupy "slabledger" with another workspace so w1 gets a suffixed name.
	other := ws
	other.ID = "w0"
	other.Worktree = "/w/other"
	if err := d.store.RegisterWorkspace(other, "sha256:x", testTime); err != nil {
		t.Fatalf("register other: %v", err)
	}
	if _, err := d.store.AllocateSessionName("w0", testTime); err != nil {
		t.Fatalf("allocate other: %v", err)
	}
	if _, err := d.store.AllocateSessionName("w1", testTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	if _, err := d.ctrl.Observe(context.Background(), testDesired("false")); err != nil {
		t.Fatalf("Observe: %v", err)
	}
	q := d.sessions.Queries[0]
	if len(q.CandidateNames) != 2 || q.CandidateNames[0] != "slabledger" ||
		q.CandidateNames[1] != "slabledger-2" {
		t.Errorf("candidate names = %v, want proposed plus assigned", q.CandidateNames)
	}
}

// TestSessionObservationFailureIsUnknownNotAbsence is the spec-§5 gate: a
// tmux outage must not read as absence.
func TestSessionObservationFailureIsUnknownNotAbsence(t *testing.T) {
	d := newDeps()
	d.sessions.Err = errors.New("tmux: no server and no socket permission")
	snap, err := d.ctrl.Observe(context.Background(), testDesired("false"))
	if err != nil {
		t.Fatalf("Observe should not fail on an observer error: %v", err)
	}
	if snap.Session.State != controller.SessionUnknown {
		t.Errorf("session state = %q, want unknown", snap.Session.State)
	}
	if snap.Session.Err == nil {
		t.Error("the observer error should be retained in the snapshot")
	}
}

func TestObserveProbesAnExistingBinding(t *testing.T) {
	d := newDeps()
	ws := testDesired("auto").Workspace
	if err := d.store.RegisterWorkspace(ws, "sha256:desired", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}, testTime); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	d.containers.ProbeResult = controller.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}

	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(d.containers.Probed) != 1 || d.containers.Probed[0].ContainerID != "c-1" {
		t.Errorf("probes = %+v, want the stored binding", d.containers.Probed)
	}
	if len(d.containers.Discovered) != 0 {
		t.Errorf("discovery ran despite an existing binding")
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthPresent {
		t.Errorf("container snapshot = %+v", snap.Container)
	}
}

func TestObserveDiscoversWhenNeverBound(t *testing.T) {
	d := newDeps()
	d.containers.DiscoverResult = controller.ContainerObservation{Health: state.HealthMissing}
	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if len(d.containers.Discovered) != 1 || d.containers.Discovered[0] != "w1" {
		t.Errorf("discoveries = %v", d.containers.Discovered)
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthMissing {
		t.Errorf("container snapshot = %+v", snap.Container)
	}
}

func TestObserveSkipsContainersWhenDisabled(t *testing.T) {
	d := newDeps()
	snap, err := d.ctrl.Observe(context.Background(), testDesired("false"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed != nil {
		t.Errorf("container snapshot = %+v, want none when disabled", snap.Container)
	}
	if len(d.containers.Probed)+len(d.containers.Discovered) != 0 {
		t.Error("no container observation should run when devcontainer is disabled")
	}
}

// TestContainerProbeFailureIsUnknownNotLoss is the design-§9 gate.
func TestContainerProbeFailureIsUnknownNotLoss(t *testing.T) {
	d := newDeps()
	ws := testDesired("true").Workspace
	if err := d.store.RegisterWorkspace(ws, "sha256:desired", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}, testTime); err != nil {
		t.Fatalf("seed binding: %v", err)
	}
	d.containers.ProbeErr = errors.New("docker daemon unavailable")

	snap, err := d.ctrl.Observe(context.Background(), testDesired("true"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthUnknown {
		t.Errorf("container snapshot = %+v, want health unknown", snap.Container)
	}
	if snap.Container.Err == nil {
		t.Error("the probe error should be retained in the snapshot")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -v`
Expected: FAIL (`Controller`, `Desired`, `Observe` undefined).

- [ ] **Step 3: Implement Observe**

Create `internal/controller/observe.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// Controller observes, plans, and (in later slices) ensures, stops, and
// reports a workspace. It depends on interfaces rather than subprocess
// details.
type Controller struct {
	Store      Store
	Sessions   SessionObserver
	Containers ContainerObserver
	Clock      Clock
}

// Desired is everything the configuration and resolver slices established
// about what should exist.
type Desired struct {
	Workspace resolve.Workspace
	Config    config.Config
	Digest    string
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

	snap.Container = c.observeContainer(ctx, d, snap.Stored)
	return snap, nil
}

func (c *Controller) observeContainer(ctx context.Context, d Desired, stored *state.Record) ContainerSnapshot {
	if d.Config.DevContainer.Enabled == "false" {
		return ContainerSnapshot{}
	}
	var (
		obs ContainerObservation
		err error
	)
	if stored != nil && stored.Container != nil {
		obs, err = c.Containers.ProbeContainer(ctx, *stored.Container)
	} else {
		obs, err = c.Containers.DiscoverContainer(ctx, d.Workspace, d.Config)
	}
	if err != nil {
		// Design §9: a failed probe yields unknown, never loss.
		return ContainerSnapshot{
			Observed: &ContainerObservation{Health: state.HealthUnknown},
			Err:      err,
		}
	}
	return ContainerSnapshot{Observed: &obs}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/... -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
git add internal/controller/
git commit -m "feat: assemble tri-state workspace snapshots in the controller"
```

---

### Task 9: BuildPlan — the pure planner

**Files:**
- Create: `internal/controller/plan.go`
- Create: `internal/controller/plan_test.go`

**Interfaces:**
- Consumes: Task 8's `Snapshot` and Task 7's types.
- Produces: `SessionAction` (`SessionActionNone/Adopt/Create/Refuse`), `ContainerAction` (`ContainerActionNone/Start/ProbeFirst`), `Plan{Session SessionAction; RecordName bool; Container ContainerAction; Reapply bool; Refusal string}`, and `BuildPlan(snap Snapshot) Plan`. (The spec calls this `Plan(snapshot)`; the function is `BuildPlan` because the type is `Plan` — an intentional, recorded deviation.)

- [ ] **Step 1: Write the failing tests**

Create `internal/controller/plan_test.go`:

```go
package controller_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// snapshotFor builds one Snapshot from compact test parameters.
func snapshotFor(t *testing.T, mutate func(*controller.Snapshot)) controller.Snapshot {
	t.Helper()
	snap := controller.Snapshot{
		Desired: testDesired("auto"),
		Session: controller.SessionSnapshot{State: controller.SessionAbsent},
	}
	if mutate != nil {
		mutate(&snap)
	}
	return snap
}

func stringPtr(s string) *string { return &s }

func storedRecord(actual, applied *string) *state.Record {
	return &state.Record{
		ID:              "w1",
		Slug:            "slabledger",
		Worktree:        "/w/slabledger",
		ProposedSession: "slabledger",
		ActualSession:   actual,
		AppliedDigest:   applied,
	}
}

func ourLiveSession() *controller.LiveSession {
	return &controller.LiveSession{
		Name:        "slabledger",
		WorkspaceID: "w1",
		Slug:        "slabledger",
		Worktree:    "/w/slabledger",
	}
}

func TestPlanTable(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*controller.Snapshot)
		want   controller.Plan
	}{
		{
			name:   "unregistered and absent creates and records everything",
			mutate: nil,
			want: controller.Plan{
				Session:    controller.SessionActionCreate,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    true,
			},
		},
		{
			name: "recorded, live, applied, and healthy is a no-op",
			mutate: func(s *controller.Snapshot) {
				s.Stored = storedRecord(stringPtr("slabledger"), stringPtr("sha256:desired"))
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: ourLiveSession(),
					ByName:     []controller.LiveSession{*ourLiveSession()},
				}
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{
						ContainerID: "c-1", Health: state.HealthPresent,
					},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionNone,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "live with matching identity but no record adopts",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: ourLiveSession(),
					ByName:     []controller.LiveSession{*ourLiveSession()},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionAdopt,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    true,
			},
		},
		{
			name: "unknown session state refuses every mutating action",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State: controller.SessionUnknown,
					Err:   errors.New("tmux unobservable"),
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionRefuse,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    true,
			},
		},
		{
			name: "a foreign session on a candidate name refuses",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State: controller.SessionAbsent,
					ByName: []controller.LiveSession{{
						Name:        "slabledger",
						WorkspaceID: "someone-else",
					}},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionRefuse,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    true,
			},
		},
		{
			name: "a keyless session on a candidate name refuses",
			mutate: func(s *controller.Snapshot) {
				s.Session = controller.SessionSnapshot{
					State:  controller.SessionAbsent,
					ByName: []controller.LiveSession{{Name: "slabledger"}},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionRefuse,
				RecordName: true,
				Container:  controller.ContainerActionNone,
				Reapply:    true,
			},
		},
		{
			name: "digest drift on a live recorded session plans reapply",
			mutate: func(s *controller.Snapshot) {
				s.Stored = storedRecord(stringPtr("slabledger"), stringPtr("sha256:stale"))
				s.Session = controller.SessionSnapshot{
					State:      controller.SessionLive,
					ByIdentity: ourLiveSession(),
					ByName:     []controller.LiveSession{*ourLiveSession()},
				}
			},
			want: controller.Plan{
				Session:   controller.SessionActionNone,
				Reapply:   true,
				Container: controller.ContainerActionNone,
			},
		},
		{
			name: "a missing container plans start",
			mutate: func(s *controller.Snapshot) {
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{Health: state.HealthMissing},
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionCreate,
				RecordName: true,
				Container:  controller.ContainerActionStart,
				Reapply:    true,
			},
		},
		{
			name: "an unknown container plans probe-first, not start",
			mutate: func(s *controller.Snapshot) {
				s.Container = controller.ContainerSnapshot{
					Observed: &controller.ContainerObservation{Health: state.HealthUnknown},
					Err:      errors.New("docker daemon unavailable"),
				}
			},
			want: controller.Plan{
				Session:    controller.SessionActionCreate,
				RecordName: true,
				Container:  controller.ContainerActionProbeFirst,
				Reapply:    true,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := controller.BuildPlan(snapshotFor(t, tc.mutate))
			if got.Session != tc.want.Session ||
				got.RecordName != tc.want.RecordName ||
				got.Container != tc.want.Container ||
				got.Reapply != tc.want.Reapply {
				t.Errorf("plan = %+v, want %+v", got, tc.want)
			}
			if tc.want.Session == controller.SessionActionRefuse && got.Refusal == "" {
				t.Error("a refusing plan must carry an explanation")
			}
		})
	}
}

func TestRefusalNamesTheOccupiedSession(t *testing.T) {
	snap := snapshotFor(t, func(s *controller.Snapshot) {
		s.Session = controller.SessionSnapshot{
			State: controller.SessionAbsent,
			ByName: []controller.LiveSession{{
				Name: "slabledger", WorkspaceID: "someone-else",
			}},
		}
	})
	p := controller.BuildPlan(snap)
	if !strings.Contains(p.Refusal, "slabledger") {
		t.Errorf("refusal %q should name the occupied session", p.Refusal)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run TestPlan -v`
Expected: FAIL (`BuildPlan`, `Plan` undefined).

- [ ] **Step 3: Implement the planner**

Create `internal/controller/plan.go`:

```go
package controller

import (
	"fmt"

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
// exactly when Session is SessionActionRefuse.
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
	p := Plan{Container: containerAction(snap)}

	p.RecordName = snap.Stored == nil || snap.Stored.ActualSession == nil
	p.Reapply = snap.Stored == nil ||
		snap.Stored.AppliedDigest == nil ||
		*snap.Stored.AppliedDigest != snap.Desired.Digest

	switch {
	case snap.Session.State == SessionUnknown:
		// Design §9 via spec §5: no mutating action may be derived from an
		// unobservable tmux.
		p.Session = SessionActionRefuse
		p.Refusal = "tmux could not be observed; refusing to act on an unknown session state"
	case foreignOccupant(snap) != nil:
		occupant := foreignOccupant(snap)
		// Design §7: never adopt or rename a session that does not carry
		// this workspace's identity keys.
		p.Session = SessionActionRefuse
		p.Refusal = fmt.Sprintf(
			"session %q exists but does not belong to this workspace; refusing to adopt or rename it",
			occupant.Name)
	case snap.Session.State == SessionLive:
		p.Session = sessionActionForLive(snap)
	default:
		p.Session = SessionActionCreate
	}
	return p
}

// foreignOccupant returns a live session occupying one of the candidate
// names without this workspace's identity — including sessions with no
// identity keys at all.
func foreignOccupant(snap Snapshot) *LiveSession {
	for i := range snap.Session.ByName {
		s := snap.Session.ByName[i]
		if s.WorkspaceID != snap.Desired.Workspace.ID {
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
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/... -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Full gates and final commit**

```bash
gofmt -l . && go vet ./... && go test ./... -count=1 -race
CGO_ENABLED=0 go build ./cmd/projectmux
git add internal/controller/
git commit -m "feat: compute typed workspace plans from tri-state snapshots"
```

---

## Completion checklist (after Task 9)

- `gofmt -l .` empty; `go vet ./...` clean; `go test ./... -count=1 -race` green; `CGO_ENABLED=0 go build ./cmd/projectmux` succeeds.
- `git diff main..HEAD` contains no CLI changes (`internal/cli`, `cmd/` untouched) and no adapter subprocess code — `git`, still only in `internal/resolve`, remains the sole subprocess.
- Spec cross-check: every §6 test gate in `docs/superpowers/specs/2026-08-05-state-slice-design.md` maps to a named test above.
