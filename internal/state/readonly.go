package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

// PendingMigrationError reports a database older than this build's schema.
// It is a diagnosis, never an action: the migration runs on the next
// mutating command, not from an inspection.
type PendingMigrationError struct {
	Database  int
	Supported int
}

func (e *PendingMigrationError) Error() string {
	return fmt.Sprintf(
		"the state database is schema version %d, but this build expects %d; migrations run on the next mutating command",
		e.Database, e.Supported)
}

// Inspection is what a read-only open learned about the database file.
// IntegrityErr is nil exactly when PRAGMA integrity_check returned "ok".
type Inspection struct {
	UserVersion  int
	IntegrityErr error
}

// Usable reports why the read-only view must not be queried, or nil when
// queries are safe. A corrupt file is refused outright; a schema version
// that is not this build's means the rows would be read under the wrong
// shape.
func (i Inspection) Usable() error {
	if i.IntegrityErr != nil {
		return i.IntegrityErr
	}
	switch {
	case i.UserVersion > SchemaVersion:
		return &FutureSchemaError{Database: i.UserVersion, Supported: SchemaVersion}
	case i.UserVersion < SchemaVersion:
		return &PendingMigrationError{Database: i.UserVersion, Supported: SchemaVersion}
	}
	return nil
}

// ReadOnlyStore reads an existing database without creating, migrating,
// or otherwise writing it. It exposes only the queries an inspection
// needs; every mutation lives on *Store.
type ReadOnlyStore struct {
	db *sql.DB
}

// OpenReadOnly opens root's state database for inspection and reports
// what it found. The connection carries mode=ro, which never creates the
// file and never writes it — a missing database is an error carrying
// fs.ErrNotExist rather than a freshly created empty one.
//
// The integrity check is a finding, not a failure: the store and the
// inspection are both returned so a caller can report corruption and
// still close cleanly. Consult Inspection.Usable before querying, and
// Close the store whenever the error is nil.
//
// Nothing is created beside the database either. Reading a WAL database
// the ordinary way materializes the -shm and -wal sidecars, because
// readers need the shared-memory index — and those files outlive the
// connection, which would leave a diagnosis-only command permanently
// altering the state root and unable to run at all where the directory
// is not writable. The DSN choice below avoids that without giving up a
// WAL-consistent read.
func OpenReadOnly(root string) (*ReadOnlyStore, Inspection, error) {
	path := DBPath(root)
	if _, err := os.Stat(path); err != nil {
		return nil, Inspection{}, fmt.Errorf("the state database at %s: %w", path, err)
	}
	dsn := "file:" + uriPath(path) +
		"?mode=ro" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=query_only(1)"

	// With no -wal beside the database there is no uncheckpointed
	// content to miss, so it can be read as immutable: SQLite then skips
	// the shared-memory index and touches nothing in the directory.
	// Where a -wal does exist the sidecars are already there, so the
	// ordinary path adds nothing to the state root anyway — and it is
	// the only one that sees what the -wal holds. Immutable reads of a
	// live WAL database silently omit committed rows.
	if !walPresent(path) {
		store, insp, err := inspect(dsn + "&immutable=1")
		// A -wal that appeared mid-read means a writer was active and
		// the immutable snapshot may be torn, so its verdict on
		// integrity is unfounded. Anything else unexpected falls back
		// too: immutable is an optimization that has to prove itself.
		if err == nil && !(insp.IntegrityErr != nil && walPresent(path)) {
			return store, insp, nil
		}
		if store != nil {
			store.Close()
		}
	}
	return inspect(dsn)
}

// walPresent reports whether a write-ahead log sits beside the database.
func walPresent(path string) bool {
	_, err := os.Stat(path + "-wal")
	return err == nil
}

// inspect connects and reads the two facts an inspection is made of.
func inspect(dsn string) (*ReadOnlyStore, Inspection, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, Inspection{}, fmt.Errorf("opening the state database read-only: %w", err)
	}

	insp := Inspection{}
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		// The driver connects lazily, so this first query is where both
		// a damaged file and an unreadable one report. Only the codes
		// that name malformation establish corruption; anything else —
		// a permission failure, a directory that will not take the WAL
		// index — leaves the contents unexamined and is uncertainty.
		if !isMalformed(err) {
			db.Close()
			return nil, Inspection{}, fmt.Errorf("reading the state database: %w", err)
		}
		insp.IntegrityErr = fmt.Errorf("checking database integrity: %w", err)
		return &ReadOnlyStore{db: db}, insp, nil
	}
	if result != "ok" {
		insp.IntegrityErr = fmt.Errorf("database integrity check reported: %s", result)
		return &ReadOnlyStore{db: db}, insp, nil
	}
	if err := db.QueryRow("PRAGMA user_version").Scan(&insp.UserVersion); err != nil {
		db.Close()
		return nil, Inspection{}, fmt.Errorf("reading the schema version: %w", err)
	}
	return &ReadOnlyStore{db: db}, insp, nil
}

// isMalformed reports whether an error is SQLite saying the file's
// contents are not a usable database: SQLITE_CORRUPT for a damaged
// image, SQLITE_NOTADB for a file that was never one.
func isMalformed(err error) bool {
	var se *sqlite.Error
	if !errors.As(err, &se) {
		return false
	}
	// Extended result codes carry the primary code in the low byte.
	switch se.Code() & 0xff {
	case sqlite3.SQLITE_CORRUPT, sqlite3.SQLITE_NOTADB:
		return true
	}
	return false
}

// Close closes the connection pool.
func (s *ReadOnlyStore) Close() error { return s.db.Close() }

// Workspace returns the joined record for one workspace, or ErrNotFound.
func (s *ReadOnlyStore) Workspace(id string) (Record, error) { return queryWorkspace(s.db, id) }

// Workspaces returns every registered workspace ordered by slug, then
// worktree.
func (s *ReadOnlyStore) Workspaces() ([]Record, error) { return queryWorkspaces(s.db) }

// IsMissingDatabase reports whether an OpenReadOnly error means the
// database file does not exist — a fresh installation, in which nothing
// is registered. Any other error is uncertainty, not absence.
func IsMissingDatabase(err error) bool { return errors.Is(err, os.ErrNotExist) }
