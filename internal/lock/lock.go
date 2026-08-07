// Package lock provides the per-workspace filesystem lock (design §9).
// Mutating commands take it before their final observation and hold it
// through external mutations and the resulting state commit. It is for
// local filesystems (the state directory), like SQLite itself.
package lock

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// pollInterval is how often a blocked Acquire retries.
const pollInterval = 100 * time.Millisecond

// ErrLockHeld reports that another operation holds the workspace lock
// past the acquisition timeout.
type ErrLockHeld struct{ WorkspaceID string }

func (e *ErrLockHeld) Error() string {
	return fmt.Sprintf(
		"another projectmux operation holds the lock for workspace %s", e.WorkspaceID)
}

// Lock is a held workspace lock. The lock file is never deleted:
// unlinking races a concurrent flock on the same path.
type Lock struct{ f *os.File }

// Acquire takes an exclusive lock for the workspace, polling
// non-blocking flock until the timeout. The fd is close-on-exec
// (os.OpenFile's default on Linux): children spawned while the lock is
// held must never inherit it — a detached tmux server holding a leaked
// lock fd forever is the failure class this package exists to prevent
// (design §2). TestChildDoesNotInheritTheLock pins that property.
func Acquire(ctx context.Context, dir, workspaceID string, timeout time.Duration) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, workspaceID+".lock"),
		os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening the lock file: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking workspace %s: %w", workspaceID, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, &ErrLockHeld{WorkspaceID: workspaceID}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for the workspace lock: %w", ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}

// Release unlocks and closes the lock file.
func (l *Lock) Release() error {
	if err := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN); err != nil {
		// The unlock error is the one worth reporting; closing is
		// best-effort cleanup of an fd we are abandoning either way.
		_ = l.f.Close()
		return fmt.Errorf("unlocking: %w", err)
	}
	return l.f.Close()
}
