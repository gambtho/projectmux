package state

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
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
// is not writable. The DSN is chosen from what the sidecars show, and
// where no DSN can both answer honestly and leave the directory alone
// the inspection reports uncertainty instead. See walStateOf.
func OpenReadOnly(root string) (*ReadOnlyStore, Inspection, error) {
	path := DBPath(root)
	if _, err := os.Stat(path); err != nil {
		return nil, Inspection{}, fmt.Errorf("the state database at %s: %w", path, err)
	}
	dsn := "file:" + uriPath(path) +
		"?mode=ro" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=query_only(1)"

	switch walStateOf(path) {
	case walComplete:
		// The sidecars are already there, so the ordinary path adds
		// nothing to the state root — and it is the only one that sees
		// what the -wal holds. Immutable reads of a live WAL database
		// silently omit committed rows.
		return inspect(dsn)
	case walIncomplete:
		return nil, Inspection{}, incompleteWAL(path)
	}

	// No -wal, so there is no uncheckpointed content to miss and the
	// database can be read as immutable: SQLite then skips the
	// shared-memory index and touches nothing in the directory. A
	// failure here is reported as it stands rather than retried the
	// ordinary way, which would create both sidecars.
	store, insp, err := inspect(dsn + "&immutable=1")
	if err != nil {
		return nil, Inspection{}, err
	}
	if walStateOf(path) != walNone {
		// A writer arrived mid-read. The immutable snapshot ignored
		// whatever it committed and may be torn besides, so neither its
		// rows nor its verdict on integrity are worth reporting.
		store.Close()
		return nil, Inspection{}, fmt.Errorf(
			"the state database at %s was written during the inspection; run the command again", path)
	}
	return store, insp, nil
}

// walState is what the files beside a database say about its write-ahead
// log. The third case exists because a -wal alone does not describe a
// readable WAL state: without the -shm index beside it, no connection can
// read the log without first creating that index.
type walState int

const (
	// walNone: no write-ahead log, so nothing is uncheckpointed.
	walNone walState = iota
	// walComplete: a -wal and the -shm index that reads it, both present.
	walComplete
	// walIncomplete: a -wal with no -shm, or sidecars that could not be
	// examined at all. Neither DSN serves this state — the ordinary path
	// creates the missing -shm and keeps it, and an immutable read
	// silently omits every row the -wal holds.
	walIncomplete
)

// walStateOf classifies the sidecars beside path. A stat that fails for
// any reason other than absence leaves the question open, and an open
// question is never resolved as "no log": that is the one reading under
// which a silent immutable read looks safe.
func walStateOf(path string) walState {
	wal, err := os.Stat(path + "-wal")
	if errors.Is(err, fs.ErrNotExist) {
		return walNone
	}
	if err != nil || wal.IsDir() {
		return walIncomplete
	}
	if _, err := os.Stat(path + "-shm"); err != nil {
		return walIncomplete
	}
	return walComplete
}

// incompleteWAL reports a log that cannot be read without writing.
func incompleteWAL(path string) error {
	return fmt.Errorf(
		"the state database at %s has a write-ahead log with no shared-memory index, "+
			"which means a writer did not shut down cleanly; reading it would require "+
			"recovering the log into the state directory, which an inspection must not do. "+
			"The next mutating command recovers it", path)
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
