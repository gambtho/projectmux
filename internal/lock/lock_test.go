package lock

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcquireReleaseReacquire(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	l2, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("re-Acquire after Release: %v", err)
	}
	if err := l2.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

func TestAcquireTimesOutWithTypedError(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	start := time.Now()
	_, err = Acquire(context.Background(), dir, "w1", 200*time.Millisecond)
	var heldErr *ErrLockHeld
	if !errors.As(err, &heldErr) {
		t.Fatalf("err = %v, want *ErrLockHeld", err)
	}
	if heldErr.WorkspaceID != "w1" {
		t.Errorf("WorkspaceID = %q, want w1", heldErr.WorkspaceID)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timed-out Acquire took %v", elapsed)
	}
}

func TestDifferentWorkspacesDoNotContend(t *testing.T) {
	dir := t.TempDir()
	l1, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire w1: %v", err)
	}
	defer l1.Release()
	l2, err := Acquire(context.Background(), dir, "w2", 200*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire w2 while w1 held: %v", err)
	}
	defer l2.Release()
}

func TestMutualExclusionUnderConcurrency(t *testing.T) {
	dir := t.TempDir()
	var holders atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				l, err := Acquire(context.Background(), dir, "w1", 10*time.Second)
				if err != nil {
					t.Errorf("Acquire: %v", err)
					return
				}
				if n := holders.Add(1); n != 1 {
					t.Errorf("%d simultaneous holders", n)
				}
				time.Sleep(time.Millisecond)
				holders.Add(-1)
				if err := l.Release(); err != nil {
					t.Errorf("Release: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestCancelledContextStopsWaiting(t *testing.T) {
	dir := t.TempDir()
	held, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	defer held.Release()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	_, err = Acquire(ctx, dir, "w1", time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestChildDoesNotInheritTheLock guards the PR-54 failure class: a child
// spawned while the lock is held (here: a long-lived sleep, standing in
// for a detached tmux server) must not hold the lock after the parent
// releases it.
func TestChildDoesNotInheritTheLock(t *testing.T) {
	dir := t.TempDir()
	l, err := Acquire(context.Background(), dir, "w1", time.Second)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	child := exec.Command("sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatalf("starting the child: %v", err)
	}
	t.Cleanup(func() {
		_ = child.Process.Kill()
		_, _ = child.Process.Wait()
	})

	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	// If the child inherited the fd, this acquire would block until the
	// child exits; the short timeout catches that regression.
	l2, err := Acquire(context.Background(), dir, "w1", 500*time.Millisecond)
	if err != nil {
		t.Fatalf("Acquire after Release with a live child: %v (the child inherited the lock)", err)
	}
	_ = l2.Release()
}
