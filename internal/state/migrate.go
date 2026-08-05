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
