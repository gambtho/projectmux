package state

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
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
// SQLite may create the -shm/-wal sidecars for a WAL database opened
// read-only. The database file itself is never touched, which is what
// "diagnose without mutating" protects.
func OpenReadOnly(root string) (*ReadOnlyStore, Inspection, error) {
	path := DBPath(root)
	if _, err := os.Stat(path); err != nil {
		return nil, Inspection{}, fmt.Errorf("the state database at %s: %w", path, err)
	}
	dsn := "file:" + uriPath(path) +
		"?mode=ro" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, Inspection{}, fmt.Errorf("opening the state database read-only: %w", err)
	}

	insp := Inspection{}
	var result string
	if err := db.QueryRow("PRAGMA integrity_check").Scan(&result); err != nil {
		// A file too damaged to read reports here rather than on open:
		// the driver connects lazily.
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
