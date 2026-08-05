package state

import (
	"errors"
	"fmt"
	"sync"
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

func TestAllocateSessionNameAssignsTheProposedNameFirst(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	name, err := s.AllocateSessionName("w1", testTime)
	if err != nil {
		t.Fatalf("AllocateSessionName: %v", err)
	}
	if name != "slabledger" {
		t.Errorf("name = %q, want the proposed name", name)
	}

	again, err := s.AllocateSessionName("w1", testTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("second AllocateSessionName: %v", err)
	}
	if again != name {
		t.Errorf("reallocation = %q, want the stable assignment %q", again, name)
	}
}

func TestAllocateSessionNameSuffixesOnCollision(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	mustRegister(t, s, testWorkspace("w2"))

	first, err := s.AllocateSessionName("w1", testTime)
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	second, err := s.AllocateSessionName("w2", testTime)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if first != "slabledger" || second != "slabledger-2" {
		t.Errorf("names = %q, %q; want slabledger and slabledger-2", first, second)
	}
}

func TestAllocateSessionNameForUnknownWorkspace(t *testing.T) {
	s := openTestStore(t)
	_, err := s.AllocateSessionName("absent", testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

// TestConcurrentAllocationYieldsDistinctNames is the design-§12 gate: the
// database constraint, not application convention, prevents duplicates.
func TestConcurrentAllocationYieldsDistinctNames(t *testing.T) {
	s := openTestStore(t)
	const n = 8
	for i := 0; i < n; i++ {
		mustRegister(t, s, testWorkspace(fmt.Sprintf("w%d", i)))
	}

	names := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			names[i], errs[i] = s.AllocateSessionName(fmt.Sprintf("w%d", i), testTime)
		}(i)
	}
	wg.Wait()

	seen := map[string]int{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("allocation %d: %v", i, errs[i])
		}
		seen[names[i]]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Errorf("name %q assigned %d times", name, count)
		}
	}
}

func presentObservation(id string) ContainerObservation {
	return ContainerObservation{
		Kind:          "devcontainer",
		ContainerID:   id,
		ContainerUser: "vscode",
		Workdir:       "/workspaces/slabledger",
		Health:        HealthPresent,
	}
}

func TestContainerObservationRoundTrips(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	if err := s.RecordContainerObservation("w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("RecordContainerObservation: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	b := rec.Container
	if b == nil || b.ContainerID != "c-1" || b.Health != HealthPresent ||
		b.Kind != "devcontainer" || b.ContainerUser != "vscode" ||
		!b.ObservedAt.Equal(testTime) {
		t.Errorf("binding = %+v", b)
	}
}

// TestMissingAndUnknownRetainTheBinding is the design-§7 tri-state gate:
// neither confirmed absence nor a failed probe erases the identity needed
// for repair.
func TestMissingAndUnknownRetainTheBinding(t *testing.T) {
	for _, health := range []Health{HealthMissing, HealthUnknown} {
		t.Run(string(health), func(t *testing.T) {
			s := openTestStore(t)
			mustRegister(t, s, testWorkspace("w1"))
			if err := s.RecordContainerObservation("w1", presentObservation("c-1"), testTime); err != nil {
				t.Fatalf("seed: %v", err)
			}

			later := testTime.Add(time.Hour)
			err := s.RecordContainerObservation("w1", ContainerObservation{Health: health}, later)
			if err != nil {
				t.Fatalf("record %s: %v", health, err)
			}
			rec, err := s.Workspace("w1")
			if err != nil {
				t.Fatalf("Workspace: %v", err)
			}
			b := rec.Container
			if b == nil || b.ContainerID != "c-1" || b.Kind != "devcontainer" {
				t.Fatalf("identity was not retained: %+v", b)
			}
			if b.Health != health || !b.ObservedAt.Equal(later) {
				t.Errorf("health/freshness not updated: %+v", b)
			}
		})
	}
}

func TestReplacementOverwritesTheBinding(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	if err := s.RecordContainerObservation("w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.RecordContainerObservation("w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing: %v", err)
	}

	if err := s.RecordContainerObservation("w1", presentObservation("c-2"), testTime.Add(time.Hour)); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Container == nil || rec.Container.ContainerID != "c-2" ||
		rec.Container.Health != HealthPresent {
		t.Errorf("binding = %+v, want the replacement c-2", rec.Container)
	}
}

func TestObservationsForNeverBoundAndUnknownWorkspaces(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	// missing/unknown with no existing binding record nothing: there is no
	// identity to retain and none to invent.
	if err := s.RecordContainerObservation("w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing on never-bound: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Container != nil {
		t.Errorf("never-bound workspace grew a binding: %+v", rec.Container)
	}

	err = s.RecordContainerObservation("absent", presentObservation("c-1"), testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown workspace error = %v, want ErrNotFound", err)
	}

	err = s.RecordContainerObservation("w1", ContainerObservation{Health: HealthPresent}, testTime)
	if err == nil {
		t.Error("present without a container ID should be rejected")
	}
}
