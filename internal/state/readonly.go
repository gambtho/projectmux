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

// IncompleteWALError reports a database whose write-ahead log cannot be
// read without writing: a -wal with no -shm beside it, which is what a
// writer that did not shut down cleanly leaves behind. It is typed rather
// than a bare message because its two readers want opposite things — an
// inspection must refuse, since recovering the log would alter the state
// root, while a mutating command is precisely what recovers it.
//
// It is deliberately narrower than "the sidecars were not readable". A
// stat that failed is uncertainty, and a mutating command that treated
// uncertainty as this error would open a database it never examined.
// walStateOf keeps the two apart; see walUnknown.
type IncompleteWALError struct {
	Path string
}

func (e *IncompleteWALError) Error() string {
	return fmt.Sprintf(
		"the state database at %s has a write-ahead log with no shared-memory index, "+
			"which means a writer did not shut down cleanly; reading it would require "+
			"recovering the log into the state directory, which an inspection must not do. "+
			"The next mutating command recovers it", e.Path)
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
		return nil, Inspection{}, &IncompleteWALError{Path: path}
	case walUnknown:
		return nil, Inspection{}, unexaminableSidecars(path)
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
		_ = store.Close()
		return nil, Inspection{}, fmt.Errorf(
			"the state database at %s was written during the inspection; run the command again", path)
	}
	return store, insp, nil
}

// walState is what the files beside a database say about its write-ahead
// log. The last two cases exist because a -wal alone does not describe a
// readable WAL state, and because failing to examine the sidecars is a
// different answer from examining them and finding a crash.
type walState int

const (
	// walNone: no write-ahead log, so nothing is uncheckpointed.
	walNone walState = iota
	// walComplete: a -wal and the -shm index that reads it, both present.
	walComplete
	// walIncomplete: a regular -wal with -shm confirmed absent. Neither
	// DSN serves this state — the ordinary path creates the missing -shm
	// and keeps it, and an immutable read silently omits every row the
	// -wal holds. This is what an unclean shutdown leaves, and the one
	// state a mutating command may proceed through: opening read-write
	// recovers the log.
	walIncomplete
	// walUnknown: the sidecars could not be examined — a stat that failed
	// for any reason other than absence, or a -wal that is not a regular
	// file. Everyone refuses. Reporting this as walIncomplete would tell
	// a mutating command it may proceed, on the strength of a question
	// that was never answered.
	walUnknown
)

// walStateOf classifies the sidecars beside path. A stat that fails for
// any reason other than absence leaves the question open, and an open
// question is never resolved as "no log" — that is the one reading under
// which a silent immutable read looks safe — nor as "a writer crashed",
// which is the one reading under which a read-write open looks safe.
func walStateOf(path string) walState {
	wal, err := os.Stat(path + "-wal")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return walNone
	case err != nil:
		return walUnknown
	case !wal.Mode().IsRegular():
		// A directory, device, or socket named state.db-wal is not a log
		// this code understands, whatever else it may be.
		return walUnknown
	}
	shm, err := os.Stat(path + "-shm")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return walIncomplete
	case err != nil:
		return walUnknown
	case !shm.Mode().IsRegular():
		// Same rule as the -wal above: a directory or device named
		// state.db-shm is not the index this code would be reasoning
		// about, so its presence establishes nothing about the log.
		return walUnknown
	}
	return walComplete
}

// unexaminableSidecars reports sidecars the filesystem would not describe.
// It is not IncompleteWALError: that one asserts a writer crashed, which
// is a licence to recover the log, and nothing here established that.
func unexaminableSidecars(path string) error {
	return fmt.Errorf(
		"the state database at %s has write-ahead log files that could not be examined, "+
			"so whether the log needs recovering is unknown; check the permissions and "+
			"contents of the state directory", path)
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
			_ = db.Close()
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
		_ = db.Close()
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

// Workspaces returns every registered session ordered by slug, repository
// root, then session.
func (s *ReadOnlyStore) Workspaces() ([]Record, error) { return queryWorkspaces(s.db) }

// IsMissingDatabase reports whether an OpenReadOnly error means the
// database file does not exist — a fresh installation, in which nothing
// is registered. Any other error is uncertainty, not absence.
func IsMissingDatabase(err error) bool { return errors.Is(err, os.ErrNotExist) }

// IsIncompleteWAL reports whether an OpenReadOnly error means the database
// has a write-ahead log no reader can open without recovering it. Alone
// among an inspection's refusals it is not a reason for a mutating command
// to stop: opening the database read-write is what recovers the log. It is
// false for sidecars that could not be examined, which stop everyone.
func IsIncompleteWAL(err error) bool {
	var e *IncompleteWALError
	return errors.As(err, &e)
}
