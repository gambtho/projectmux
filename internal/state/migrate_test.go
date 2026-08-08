package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"modernc.org/sqlite"
)

func TestOpenCreatesTheLatestSchema(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != SchemaVersion {
		t.Errorf("user_version = %d, want %d", version, SchemaVersion)
	}
	for _, table := range []string{"repositories", "workspaces", "container_bindings", "last_operations"} {
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
		_ = s.Close()
	}
}

func TestOpenInADirectoryWithURIMetacharacters(t *testing.T) {
	// The DSN is a URI: an unescaped "?" or "#" in the path would be read
	// as the query string, silently dropping the pragma configuration.
	root := filepath.Join(t.TempDir(), "odd?state#dir")
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var timeout int
	if err := s.db.QueryRow("PRAGMA busy_timeout").Scan(&timeout); err != nil {
		t.Fatalf("busy_timeout: %v", err)
	}
	if timeout != 5000 {
		t.Errorf("busy_timeout = %d, want 5000: the DSN query was misparsed", timeout)
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
	_ = s.Close()

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
			defer func() { _ = stores[i].Close() }()
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
	defer func() { _ = s.Close() }()

	ctx := context.Background()
	conns := make([]*sql.Conn, 2)
	for i := range conns {
		conns[i], err = s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn #%d: %v", i, err)
		}
		defer func() { _ = conns[i].Close() }()
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

// TestBeginMigrationTxRetriesOnBusy covers the cold-start race that
// busy_timeout alone does not: two processes creating the database for
// the first time can each get SQLITE_BUSY back immediately, bypassing the
// busy handler, which is why beginMigrationTx retries on top of it.
//
// busy_timeout(0) reproduces that deterministically — the driver reports
// SQLITE_BUSY at once rather than waiting internally — so a Begin that
// succeeds here can only have come from the retry loop.
func TestBeginMigrationTxRetriesOnBusy(t *testing.T) {
	dsn := "file:" + uriPath(filepath.Join(t.TempDir(), "state.db")) +
		"?_txlock=immediate&_pragma=busy_timeout(0)"

	holder, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("opening the holder: %v", err)
	}
	defer func() { _ = holder.Close() }()
	contender, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("opening the contender: %v", err)
	}
	defer func() { _ = contender.Close() }()

	// _txlock=immediate takes the write lock at BEGIN, not at first write.
	held, err := holder.Begin()
	if err != nil {
		t.Fatalf("taking the write lock: %v", err)
	}

	// Prove the setup produces the exact error the retry loop keys on.
	// Without this the test would still pass against an uncontended
	// database, proving nothing; it also pins sqliteBusy to the code the
	// driver actually reports, so a driver change cannot silently turn
	// the retry into a pass-through.
	_, err = contender.Begin()
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		_ = held.Rollback()
		t.Fatalf("contending Begin = %v, want a *sqlite.Error", err)
	}
	if sqliteErr.Code() != sqliteBusy {
		_ = held.Rollback()
		t.Fatalf("code = %d, want %d (SQLITE_BUSY)", sqliteErr.Code(), sqliteBusy)
	}

	// Start the clock before launching the releaser: the goroutine's sleep
	// begins whenever it is scheduled, so timing from after the go
	// statement would subtract any preemption in between from elapsed and
	// could fail a run that retried correctly.
	const hold = 100 * time.Millisecond
	start := time.Now()
	go func() {
		time.Sleep(hold)
		_ = held.Rollback()
	}()

	tx, err := beginMigrationTx(contender)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("beginMigrationTx gave up instead of retrying: %v", err)
	}
	_ = tx.Rollback()
	// Returning before the holder released means it never waited at all.
	if elapsed < hold/2 {
		t.Errorf("returned after %v, want at least %v: it cannot have retried", elapsed, hold/2)
	}
}

// seedSchema1 writes a database in the pre-0002 shape using migration 0001's
// own SQL, so the fixture cannot drift from the schema it stands for. The
// recorded worktree path deliberately does not exist: 0002 must succeed with
// the filesystem entirely absent (design §9).
func seedSchema1(t *testing.T) string {
	t.Helper()
	return seedSchema1As(t, "w1", "/gone/slabledger")
}

// seedSchema1As is seedSchema1 with the stored workspace ID and worktree path
// chosen by the caller. The re-key test needs the seeded ID to be the *real*
// pre-0002 derivation — SHA-256 of the canonical path — because that is what
// makes the migrated row collide with the ID the new derivation produces.
// Passing a placeholder like "w1" would let a broken re-key pass.
func seedSchema1As(t *testing.T, workspaceID, worktree string) string {
	t.Helper()
	root := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+uriPath(DBPath(root))+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("opening the seed database: %v", err)
	}
	defer func() { _ = db.Close() }()

	body, err := migrations.ReadFile("migrations/0001_initial.sql")
	if err != nil {
		t.Fatalf("reading migration 1: %v", err)
	}
	stmts := []string{
		string(body),
		fmt.Sprintf(`INSERT INTO workspaces
			(id, slug, worktree, is_primary, proposed_session, actual_session,
			 desired_digest, applied_digest, registered_at, updated_at)
		 VALUES (%q, 'slabledger', %q, 1, 'slabledger', 'slabledger',
			 'sha256:aaaa', 'sha256:bbbb', '2026-08-05T12:00:00Z', '2026-08-05T12:00:00Z')`,
			workspaceID, worktree),
		fmt.Sprintf(`INSERT INTO container_bindings
			(workspace_id, kind, container_id, container_user, workdir, health, observed_at)
		 VALUES (%q, 'devcontainer', 'c-1', 'vscode', '/workspaces/slabledger',
			 'present', '2026-08-05T12:00:00Z')`, workspaceID),
		fmt.Sprintf(`INSERT INTO last_operations
			(workspace_id, operation, outcome, exit_status, error_summary, finished_at)
		 VALUES (%q, 'open', 'ok', 0, '', '2026-08-05T12:00:00Z')`, workspaceID),
		"PRAGMA user_version = 1",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seeding: %v\n%s", err, stmt)
		}
	}
	return root
}

func TestMigration0002MovesEveryWorkspaceIntoARepository(t *testing.T) {
	root := seedSchema1(t)
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("%d repositories, want 1", len(repos))
	}
	r := repos[0]
	if r.Slug != "slabledger" || r.RepoRoot != "/gone/slabledger" {
		t.Errorf("repository = %+v, want the stored path verbatim", r)
	}
	if r.Container == nil || r.Container.ContainerID != "c-1" ||
		r.Container.Health != HealthPresent {
		t.Errorf("container binding = %+v, want the re-keyed binding", r.Container)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.RepositoryID != r.ID || rec.Session != "" ||
		rec.Slug != "slabledger" || rec.RepoRoot != "/gone/slabledger" {
		t.Errorf("record = %+v, want the default session on the migrated repository", rec)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slabledger" ||
		rec.AppliedDigest == nil || *rec.AppliedDigest != "sha256:bbbb" {
		t.Errorf("record = %+v, want the assignment and digests preserved", rec)
	}
	if rec.LastOperation == nil || rec.LastOperation.Name != "open" {
		t.Errorf("last operation = %+v, want the row preserved", rec.LastOperation)
	}
}

// TestRegisterAfterMigrationRekeysTheMigratedSession pins the half of the
// upgrade that migration 0002 cannot do by itself.
//
// Before 0002 a workspace ID was SHA-256 of the canonical path
// (internal/resolve/resolve.go:107). After 0002 the *repository* ID is that
// same hash, and the *workspace* ID hashes the session alongside the path. So a
// migrated row arrives with an ID that is byte-identical to its own repository
// ID and unequal to the ID the resolver now derives for it. The stale-row
// cleanup in RegisterWorkspace deletes by repo_root and skips the incoming ID,
// which matches nothing here — the row it would have to delete is the one being
// re-keyed, and deleting it would throw away the running session's assignment.
// Without the re-key branch the insert then violates UNIQUE (repository_id,
// session) and every first `open` after an upgrade fails with exit 1.
//
// The assertion that matters most is the last one: actual_session and
// applied_digest have to survive under the new ID, because that is what lets
// the next reconciliation adopt the tmux session that is still running rather
// than treat it as a foreign occupant of the name.
func TestRegisterAfterMigrationRekeysTheMigratedSession(t *testing.T) {
	const repoRoot = "/gone/slabledger"
	// The pre-0002 workspace ID and the post-0002 repository ID are the same
	// expression — SHA-256 of the canonical path — which is exactly why the
	// collision exists. Computing it once and using it for both roles keeps
	// that identity visible instead of asserting it.
	sum := sha256.Sum256([]byte(repoRoot))
	legacyID := hex.EncodeToString(sum[:])
	workspaceSum := sha256.Sum256([]byte(repoRoot + "\x00" + ""))
	registerTime := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	root := seedSchema1As(t, legacyID, repoRoot)
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ws := resolve.Workspace{
		ID:           hex.EncodeToString(workspaceSum[:]),
		RepositoryID: legacyID,
		Slug:         "slabledger",
		RepoRoot:     repoRoot,
		Session:      "",
		SessionName:  "slabledger",
	}
	if ws.ID == legacyID {
		t.Fatalf("workspace ID still equals the repository ID; the session is " +
			"not being hashed in and lockPhases would deadlock on one path")
	}

	if err := s.RegisterWorkspace(ws, "sha256:cccc", registerTime); err != nil {
		t.Fatalf("RegisterWorkspace over a migrated row: %v", err)
	}

	recs, err := s.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("%d workspaces, want 1 — the migrated row was duplicated "+
			"rather than re-keyed", len(recs))
	}
	rec := recs[0]
	if rec.ID != ws.ID {
		t.Errorf("workspace ID = %s, want %s", rec.ID, ws.ID)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slabledger" {
		t.Errorf("actual_session = %v, want the running assignment carried over",
			rec.ActualSession)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != "sha256:bbbb" {
		t.Errorf("applied_digest = %v, want the applied digest carried over",
			rec.AppliedDigest)
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:cccc" {
		t.Errorf("desired_digest = %v, want the digest this call supplied",
			rec.DesiredDigest)
	}
	if rec.LastOperation == nil || rec.LastOperation.Name != "open" {
		t.Errorf("last operation = %+v, want the row re-keyed alongside the "+
			"workspace rather than orphaned", rec.LastOperation)
	}
	if rec.Container == nil || rec.Container.ContainerID != "c-1" {
		t.Errorf("container = %+v, want the repository's binding still "+
			"projected onto the re-keyed session", rec.Container)
	}
}
