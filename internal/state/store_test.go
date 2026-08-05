package state

import (
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
)

var testTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func testWorkspace(id string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        "slabledger",
		Worktree:    "/home/u/workspace/slabledger-" + id,
		SessionName: "slabledger",
		IsPrimary:   true,
	}
}

func mustRegister(t *testing.T, s *Store, ws resolve.Workspace) {
	t.Helper()
	if err := s.RegisterWorkspace(ws, "sha256:aaaa", testTime); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
}

func TestRegisterWorkspaceRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ID != ws.ID || rec.Slug != ws.Slug || rec.Worktree != ws.Worktree ||
		rec.ProposedSession != ws.SessionName || !rec.IsPrimary {
		t.Errorf("record = %+v, want the registered identity", rec)
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:aaaa" {
		t.Errorf("desired digest = %v", rec.DesiredDigest)
	}
	if rec.ActualSession != nil || rec.AppliedDigest != nil ||
		rec.Container != nil || rec.LastOperation != nil {
		t.Errorf("fresh registration should have no assignment, binding, or operation: %+v", rec)
	}
	if !rec.RegisteredAt.Equal(testTime) || !rec.UpdatedAt.Equal(testTime) {
		t.Errorf("timestamps = %v / %v, want %v", rec.RegisteredAt, rec.UpdatedAt, testTime)
	}
}

func TestRegisterWorkspaceIsAnIdempotentRefresh(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	later := testTime.Add(time.Hour)
	ws.Slug = "renamed"
	if err := s.RegisterWorkspace(ws, "sha256:bbbb", later); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Slug != "renamed" || *rec.DesiredDigest != "sha256:bbbb" {
		t.Errorf("refresh did not apply: %+v", rec)
	}
	if !rec.RegisteredAt.Equal(testTime) {
		t.Errorf("registered_at changed on refresh: %v", rec.RegisteredAt)
	}
	if !rec.UpdatedAt.Equal(later) {
		t.Errorf("updated_at = %v, want %v", rec.UpdatedAt, later)
	}
}

func TestWorkspaceNotFoundIsTyped(t *testing.T) {
	s := openTestStore(t)
	_, err := s.Workspace("absent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestWorkspacesListsAllOrdered(t *testing.T) {
	s := openTestStore(t)
	a := testWorkspace("w1")
	a.Slug = "bravo"
	b := testWorkspace("w2")
	b.Slug = "alpha"
	mustRegister(t, s, a)
	mustRegister(t, s, b)

	all, err := s.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(all) != 2 || all[0].Slug != "alpha" || all[1].Slug != "bravo" {
		t.Errorf("Workspaces = %+v, want ordered by slug", all)
	}
}
