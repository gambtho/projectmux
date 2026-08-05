// Package state owns the SQLite database holding current operational
// metadata. It applies migrations on open and is the only package that
// issues SQL. Callers never see transactions: every exported method is one
// transaction internally.
package state

import (
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
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
	// The path rides in a URI, so escape it: a directory containing "?" or
	// "#" must not be read as the query string. SQLite %-decodes URI paths,
	// so the escaping round-trips. The pragma values need no encoding —
	// parentheses are legal in a URI query and the driver reads them
	// literally.
	dbPath := (&url.URL{Path: filepath.Join(root, "state.db")}).EscapedPath()
	dsn := "file:" + dbPath +
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
