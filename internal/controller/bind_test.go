package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/lock"
)

// bindRig is newEnsureRig without the session machinery: SetBind observes
// nothing and actuates nothing, so a store, a clock, and a lock directory
// are the whole controller it needs.
func bindRig(t *testing.T) (*controller.Controller, *fake.Store, string) {
	t.Helper()
	store := fake.NewStore()
	ctrl := &controller.Controller{Store: store, Clock: &fake.Clock{Time: ensureTime}}
	return ctrl, store, t.TempDir()
}

// Binding a session that has never been opened creates its record, with
// the bind and no applied digest, so BuildPlan's nil-digest reapply rule
// (plan.go:71-73) makes the first open converge with no special case.
func TestSetBindCreatesTheRecord(t *testing.T) {
	ctrl, store, lockDir := bindRig(t)
	rel := "services/api"

	created, err := ctrl.SetBind(context.Background(), ensureWorkspace(), &rel, lockDir, time.Second)
	if err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	if !created {
		t.Error("created = false, want true for an unregistered session")
	}
	rec, err := store.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("bind = %v, want %q", rec.Bind, rel)
	}
	if rec.AppliedDigest != nil {
		t.Errorf("applied digest = %v, want nil so the next open reapplies", rec.AppliedDigest)
	}
}

// Clearing removes the bind and leaves everything else, including the
// assigned session name, in place (spec §4).
func TestSetBindClearKeepsTheRecord(t *testing.T) {
	ctrl, store, lockDir := bindRig(t)
	ws := ensureWorkspace()
	if err := store.RegisterWorkspace(ws, "sha256:seed", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	name, err := store.AllocateSessionName(ws.ID, ensureTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	rel := "services/api"
	if _, err := ctrl.SetBind(context.Background(), ws, &rel, lockDir, time.Second); err != nil {
		t.Fatalf("SetBind: %v", err)
	}

	created, err := ctrl.SetBind(context.Background(), ws, nil, lockDir, time.Second)
	if err != nil {
		t.Fatalf("SetBind clear: %v", err)
	}
	if created {
		t.Error("created = true, want false for a session that already had a record")
	}
	rec, err := store.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("bind = %v, want nil after --clear", rec.Bind)
	}
	if rec.ActualSession == nil || *rec.ActualSession != name {
		t.Errorf("actual session = %v, want %q kept", rec.ActualSession, name)
	}
}

// Spec §4: bind has no container phase, so it must not queue behind a
// sibling's devcontainer up — but it must still serialize against another
// operation on the same session.
func TestSetBindTakesTheWorkspaceLockOnly(t *testing.T) {
	ctrl, _, lockDir := bindRig(t)
	ws := ensureWorkspace()
	rel := "services/api"

	repo, err := lock.Acquire(context.Background(), lockDir, ws.RepositoryID, time.Second)
	if err != nil {
		t.Fatalf("pre-acquiring the repository lock: %v", err)
	}
	defer func() { _ = repo.Release() }()

	if _, err := ctrl.SetBind(context.Background(), ws, &rel,
		lockDir, 200*time.Millisecond); err != nil {
		t.Fatalf("SetBind queued behind the repository lock: %v", err)
	}

	held, err := lock.Acquire(context.Background(), lockDir, ws.ID, time.Second)
	if err != nil {
		t.Fatalf("pre-acquiring the workspace lock: %v", err)
	}
	defer func() { _ = held.Release() }()

	_, err = ctrl.SetBind(context.Background(), ws, &rel, lockDir, 200*time.Millisecond)
	var lockErr *lock.ErrLockHeld
	if !errors.As(err, &lockErr) {
		t.Fatalf("err = %v, want *lock.ErrLockHeld", err)
	}
}
