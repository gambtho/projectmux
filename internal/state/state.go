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
