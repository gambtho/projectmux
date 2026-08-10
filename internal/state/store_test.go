package state

import (
	"errors"
	"fmt"
	"strings"
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
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testWorkspace(id string) resolve.Workspace {
	return resolve.Workspace{
		ID:           id,
		RepositoryID: "repo-" + id,
		Slug:         "slabledger",
		RepoRoot:     "/home/u/workspace/slabledger-" + id,
		SessionName:  "slabledger",
	}
}

func mustRegister(t *testing.T, s *Store, ws resolve.Workspace) {
	t.Helper()
	if err := s.RegisterWorkspace(ws, "sha256:aaaa", testTime); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
}

// repositoryOf returns the stored repository with id. A container binding is
// the repository's now, so a test that seeded one through a workspace reads it
// back from here.
func repositoryOf(t *testing.T, s *Store, id string) Repository {
	t.Helper()
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, r := range repos {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("repository %s is not stored", id)
	return Repository{}
}

func TestRegisterWorkspaceRoundTrips(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ID != ws.ID || rec.RepositoryID != ws.RepositoryID || rec.Slug != ws.Slug ||
		rec.RepoRoot != ws.RepoRoot || rec.ProposedSession != ws.SessionName {
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

	if err := s.RecordContainerObservation("repo-w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("RecordContainerObservation: %v", err)
	}
	b := repositoryOf(t, s, "repo-w1").Container
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
			if err := s.RecordContainerObservation("repo-w1", presentObservation("c-1"), testTime); err != nil {
				t.Fatalf("seed: %v", err)
			}

			later := testTime.Add(time.Hour)
			err := s.RecordContainerObservation("repo-w1", ContainerObservation{Health: health}, later)
			if err != nil {
				t.Fatalf("record %s: %v", health, err)
			}
			b := repositoryOf(t, s, "repo-w1").Container
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
	if err := s.RecordContainerObservation("repo-w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.RecordContainerObservation("repo-w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing: %v", err)
	}

	if err := s.RecordContainerObservation("repo-w1", presentObservation("c-2"), testTime.Add(time.Hour)); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	b := repositoryOf(t, s, "repo-w1").Container
	if b == nil || b.ContainerID != "c-2" || b.Health != HealthPresent {
		t.Errorf("binding = %+v, want the replacement c-2", b)
	}
}

func TestObservationsForNeverBoundAndUnknownRepositories(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	// missing/unknown with no existing binding record nothing: there is no
	// identity to retain and none to invent.
	if err := s.RecordContainerObservation("repo-w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing on never-bound: %v", err)
	}
	if b := repositoryOf(t, s, "repo-w1").Container; b != nil {
		t.Errorf("never-bound repository grew a binding: %+v", b)
	}

	err := s.RecordContainerObservation("absent", presentObservation("c-1"), testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown repository error = %v, want ErrNotFound", err)
	}

	err = s.RecordContainerObservation("repo-w1", ContainerObservation{Health: HealthPresent}, testTime)
	if err == nil {
		t.Error("present without a container ID should be rejected")
	}
}

func TestRecordOperationRoundTripsAndTruncates(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	exit := 7
	op := Operation{
		Name:         "open",
		Outcome:      OutcomeFailed,
		ExitStatus:   &exit,
		ErrorSummary: strings.Repeat("x", MaxErrorSummaryBytes+100),
	}
	if err := s.RecordOperation("w1", op, testTime); err != nil {
		t.Fatalf("RecordOperation: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	got := rec.LastOperation
	if got == nil || got.Name != "open" || got.Outcome != OutcomeFailed ||
		got.ExitStatus == nil || *got.ExitStatus != 7 ||
		!got.FinishedAt.Equal(testTime) {
		t.Fatalf("operation = %+v", got)
	}
	if len(got.ErrorSummary) != MaxErrorSummaryBytes {
		t.Errorf("summary length = %d, want the %d-byte bound", len(got.ErrorSummary), MaxErrorSummaryBytes)
	}

	// The row is an upsert: the next operation replaces it.
	if err := s.RecordOperation("w1", Operation{Name: "stop", Outcome: OutcomeOK}, testTime.Add(time.Hour)); err != nil {
		t.Fatalf("second RecordOperation: %v", err)
	}
	rec, err = s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.LastOperation.Name != "stop" || rec.LastOperation.ExitStatus != nil {
		t.Errorf("operation = %+v, want the replacement", rec.LastOperation)
	}
}

func TestCommitReconciliationAppliesEverythingAtomically(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	digest := "sha256:aaaa"
	obs := presentObservation("c-1")
	err := s.CommitReconciliation("w1", ReconciliationResult{
		AppliedDigest: &digest,
		Container:     &obs,
		Operation:     Operation{Name: "open", Outcome: OutcomeOK},
	}, testTime)
	if err != nil {
		t.Fatalf("CommitReconciliation: %v", err)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != digest {
		t.Errorf("applied digest = %v, want %q", rec.AppliedDigest, digest)
	}
	if b := repositoryOf(t, s, "repo-w1").Container; b == nil || b.ContainerID != "c-1" {
		t.Errorf("container = %+v", b)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != OutcomeOK {
		t.Errorf("operation = %+v", rec.LastOperation)
	}
}

// TestFailedReconciliationLeavesDriftRecorded is the spec-§4 gate: a
// failure commits its outcome without advancing applied_digest.
func TestFailedReconciliationLeavesDriftRecorded(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	seeded := "sha256:old"
	if err := s.CommitReconciliation("w1", ReconciliationResult{
		AppliedDigest: &seeded,
		Operation:     Operation{Name: "open", Outcome: OutcomeOK},
	}, testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}

	err := s.CommitReconciliation("w1", ReconciliationResult{
		AppliedDigest: nil,
		Operation:     Operation{Name: "open", Outcome: OutcomeFailed, ErrorSummary: "devcontainer up timed out"},
	}, testTime.Add(time.Hour))
	if err != nil {
		t.Fatalf("failed reconciliation: %v", err)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != seeded {
		t.Errorf("applied digest = %v, want the seeded %q untouched", rec.AppliedDigest, seeded)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != OutcomeFailed {
		t.Errorf("operation = %+v, want the failure recorded", rec.LastOperation)
	}
}

func TestCommitReconciliationForUnknownWorkspace(t *testing.T) {
	s := openTestStore(t)
	err := s.CommitReconciliation("absent", ReconciliationResult{
		Operation: Operation{Name: "open", Outcome: OutcomeOK},
	}, testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound", err)
	}
}

func TestAdoptSessionNameRecordsTheLiveName(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	if err := s.AdoptSessionName("w1", "slab--old", testTime); err != nil {
		t.Fatalf("AdoptSessionName: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slab--old" {
		t.Errorf("ActualSession = %v, want slab--old", rec.ActualSession)
	}
}

func TestAdoptSessionNameIsIdempotent(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	if err := s.AdoptSessionName("w1", "slabledger", testTime); err != nil {
		t.Fatalf("first adopt: %v", err)
	}
	if err := s.AdoptSessionName("w1", "slabledger", testTime); err != nil {
		t.Fatalf("re-adopting the same name: %v", err)
	}
}

func TestAdoptSessionNameRepairsAStaleAssignment(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	if _, err := s.AllocateSessionName("w1", testTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := s.AdoptSessionName("w1", "slab--live", testTime); err != nil {
		t.Fatalf("adopting over a stale assignment: %v", err)
	}
	rec, _ := s.Workspace("w1")
	if rec.ActualSession == nil || *rec.ActualSession != "slab--live" {
		t.Errorf("ActualSession = %v, want slab--live", rec.ActualSession)
	}
}

func TestAdoptSessionNameConflictIsTypedAndHarmless(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	mustRegister(t, s, testWorkspace("w2"))
	if err := s.AdoptSessionName("w2", "slabledger", testTime); err != nil {
		t.Fatalf("seeding w2: %v", err)
	}

	err := s.AdoptSessionName("w1", "slabledger", testTime)
	var conflict *SessionNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *SessionNameConflictError", err)
	}
	if conflict.Name != "slabledger" {
		t.Errorf("conflict.Name = %q", conflict.Name)
	}
	rec, _ := s.Workspace("w1")
	if rec.ActualSession != nil {
		t.Errorf("conflicting adopt changed the record: %v", *rec.ActualSession)
	}
}

func TestAdoptSessionNameRejectsUnknownWorkspaceAndEmptyName(t *testing.T) {
	s := openTestStore(t)
	if err := s.AdoptSessionName("nope", "x", testTime); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown workspace err = %v, want ErrNotFound", err)
	}
	mustRegister(t, s, testWorkspace("w1"))
	if err := s.AdoptSessionName("w1", "", testTime); err == nil {
		t.Error("an empty session name was accepted")
	}
	// When both failures apply, the name check wins because it runs
	// before the workspace lookup. The fake store pins the same order;
	// see TestFakeStoreAdoptSessionName.
	err := s.AdoptSessionName("nope", "", testTime)
	if err == nil {
		t.Fatal("an unknown workspace with an empty name was accepted")
	}
	if errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want the empty-name error, not ErrNotFound", err)
	}
}

// TestTwoSessionsOnOneRepositoryCoexist is the design-§5.2 finding as a test:
// the 0001 schema made workspaces.worktree UNIQUE, so this pair could not be
// represented at all and a column rename would not have helped.
func TestTwoSessionsOnOneRepositoryCoexist(t *testing.T) {
	s := openTestStore(t)
	a := testWorkspace("w1")
	b := testWorkspace("w2")
	b.RepositoryID = a.RepositoryID
	b.RepoRoot = a.RepoRoot
	b.Session = "feature-a"
	b.SessionName = "slabledger--feature-a"
	mustRegister(t, s, a)
	mustRegister(t, s, b)

	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 || repos[0].RepoRoot != a.RepoRoot {
		t.Fatalf("repositories = %+v, want the one both sessions share", repos)
	}
	all, err := s.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("%d workspaces, want 2", len(all))
	}
	if all[0].Session != "" || all[1].Session != "feature-a" {
		t.Errorf("sessions = %q, %q; want the default first", all[0].Session, all[1].Session)
	}
	for _, rec := range all {
		if rec.RepositoryID != a.RepositoryID || rec.RepoRoot != a.RepoRoot {
			t.Errorf("record %s = %+v, want the shared repository", rec.ID, rec)
		}
	}
}

// TestRegisteringReplacesAStaleRepositoryForTheSamePath covers the state 0002
// leaves behind: repository IDs were carried over from the old workspace rows
// rather than recomputed, so the first registration after an upgrade brings a
// new ID for a repo_root that is UNIQUE. Registration re-keys the row instead
// of failing on the constraint.
func TestRegisteringReplacesAStaleRepositoryForTheSamePath(t *testing.T) {
	s := openTestStore(t)
	stale := testWorkspace("w1")
	mustRegister(t, s, stale)

	fresh := stale
	fresh.ID = "w1-rekeyed"
	fresh.RepositoryID = "repo-w1-rekeyed"
	mustRegister(t, s, fresh)

	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 || repos[0].ID != fresh.RepositoryID {
		t.Fatalf("repositories = %+v, want only the re-keyed row", repos)
	}
	if _, err := s.Workspace("w1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale workspace error = %v, want ErrNotFound: it should have cascaded", err)
	}
	if _, err := s.Workspace("w1-rekeyed"); err != nil {
		t.Errorf("re-keyed workspace: %v", err)
	}
}

func TestSetBindRoundTripsAndClears(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	bind := "services/api"
	if err := s.SetBind(ws.ID, &bind, testTime); err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != bind {
		t.Fatalf("bind = %v, want %q", rec.Bind, bind)
	}

	if err := s.SetBind(ws.ID, nil, testTime); err != nil {
		t.Fatalf("SetBind(nil): %v", err)
	}
	rec, err = s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("bind = %v, want nil after clearing", rec.Bind)
	}
}

// TestSetBindOnAnUnregisteredWorkspace pins that SetBind records a bind and
// does not create the session: `bind` on a session that does not exist yet is
// a register-then-bind, and the registration is the caller's job.
func TestSetBindOnAnUnregisteredWorkspace(t *testing.T) {
	s := openTestStore(t)
	bind := "services/api"
	err := s.SetBind("absent", &bind, testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetBind on an unregistered workspace = %v, want ErrNotFound", err)
	}
}

// TestRegisterWorkspacePreservesTheBind is what protects `rebuild`. Rebuild
// re-runs registration over every recovered session, and registration that
// wrote the bind column would clear the bind of every session it touched.
func TestRegisterWorkspacePreservesTheBind(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	bind := "services/api"
	if err := s.SetBind(ws.ID, &bind, testTime); err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	if err := s.RegisterWorkspace(ws, "sha256:bbbb", testTime); err != nil {
		t.Fatalf("re-registering: %v", err)
	}

	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != bind {
		t.Fatalf("bind = %v, want %q preserved across re-registration", rec.Bind, bind)
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:bbbb" {
		t.Errorf("desired digest = %v, want the re-registration's value", rec.DesiredDigest)
	}
}
