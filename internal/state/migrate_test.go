package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
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

func TestOpenInADirectoryWithURIMetacharacters(t *testing.T) {
	// The DSN is a URI: an unescaped "?" or "#" in the path would be read
	// as the query string, silently dropping the pragma configuration.
	root := filepath.Join(t.TempDir(), "odd?state#dir")
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

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
	defer holder.Close()
	contender, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("opening the contender: %v", err)
	}
	defer contender.Close()

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
		held.Rollback()
		t.Fatalf("contending Begin = %v, want a *sqlite.Error", err)
	}
	if sqliteErr.Code() != sqliteBusy {
		held.Rollback()
		t.Fatalf("code = %d, want %d (SQLITE_BUSY)", sqliteErr.Code(), sqliteBusy)
	}

	const hold = 100 * time.Millisecond
	go func() {
		time.Sleep(hold)
		held.Rollback()
	}()

	start := time.Now()
	tx, err := beginMigrationTx(contender)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("beginMigrationTx gave up instead of retrying: %v", err)
	}
	tx.Rollback()
	// Returning before the holder released means it never waited at all.
	if elapsed < hold/2 {
		t.Errorf("returned after %v, want at least %v: it cannot have retried", elapsed, hold/2)
	}
}
