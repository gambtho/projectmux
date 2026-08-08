// Package lock provides the filesystem locks that serialize projectmux's
// mutating commands (design §9). A command takes them before its final
// observation and holds them through external mutations and the
// resulting state commit. They are for local filesystems (the state
// directory), like SQLite itself.
//
// Two kinds of key are locked. Container work locks the repository ID,
// because every session on a repository shares one container; session
// work and the state commit lock the workspace ID (design §6.1). A
// command needing both — open, and stop --container — acquires the
// repository lock first and releases it last. The ordering is global and
// otherwise arbitrary; what it buys is that two commands on one
// repository can never each hold the lock the other is waiting for. It
// is written down here because the single-lock code this replaced had no
// ordering to inherit.
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

// ErrLockHeld reports that another operation holds a lock past the
// acquisition timeout. Key is the repository or workspace ID that was
// locked, and the message says which kinds those are: the two are
// indistinguishable hex digests at a glance.
type ErrLockHeld struct{ Key string }

func (e *ErrLockHeld) Error() string {
	return fmt.Sprintf(
		"another projectmux operation holds the lock for repository or workspace %s", e.Key)
}

// Lock is a held lock. The lock file is never deleted: unlinking races a
// concurrent flock on the same path.
type Lock struct{ f *os.File }

// Acquire takes an exclusive lock for the key, polling non-blocking
// flock until the timeout. The fd is close-on-exec (os.OpenFile's
// default on Linux): children spawned while the lock is held must never
// inherit it — a detached tmux server holding a leaked lock fd forever
// is the failure class this package exists to prevent (design §2).
// TestChildDoesNotInheritTheLock pins that property.
func Acquire(ctx context.Context, dir, key string, timeout time.Duration) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, key+".lock"),
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
			return nil, fmt.Errorf("locking %s: %w", key, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, &ErrLockHeld{Key: key}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for the lock on %s: %w", key, ctx.Err())
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
