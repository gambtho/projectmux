package fake

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var testTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

func testWorkspace(id, session string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        "slabledger",
		Worktree:    "/w/" + id,
		SessionName: session,
		IsPrimary:   true,
	}
}

func TestFakeStoreMirrorsAllocationAndRetention(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.RegisterWorkspace(testWorkspace("w2", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}

	first, err := s.AllocateSessionName("w1", testTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	second, err := s.AllocateSessionName("w2", testTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if first != "slab" || second != "slab-2" {
		t.Errorf("names = %q, %q", first, second)
	}

	obs := state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c-1", Health: state.HealthPresent,
	}
	if err := s.RecordContainerObservation("w1", obs, testTime); err != nil {
		t.Fatalf("observation: %v", err)
	}
	if err := s.RecordContainerObservation("w1",
		state.ContainerObservation{Health: state.HealthMissing}, testTime); err != nil {
		t.Fatalf("missing: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.Container == nil || rec.Container.ContainerID != "c-1" ||
		rec.Container.Health != state.HealthMissing {
		t.Errorf("binding = %+v, want retained identity with missing health", rec.Container)
	}

	if _, err := s.Workspace("absent"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("error = %v, want state.ErrNotFound", err)
	}
}

// TestFakeStoreWorkspacesOrdersBySlugThenWorktree mirrors the real store's
// ORDER BY w.slug, w.worktree (internal/state/store.go): the fake iterates a
// map, so without an explicit sort the order would be nondeterministic.
func TestFakeStoreWorkspacesOrdersBySlugThenWorktree(t *testing.T) {
	s := NewStore()
	register := func(id, slug, worktree string) {
		t.Helper()
		ws := resolve.Workspace{
			ID: id, Slug: slug, Worktree: worktree,
			SessionName: id, IsPrimary: true,
		}
		if err := s.RegisterWorkspace(ws, "sha256:a", testTime); err != nil {
			t.Fatalf("register %s: %v", id, err)
		}
	}
	// Registered out of order so a map-iteration bug would show up.
	register("w3", "zeta", "/w/zeta")
	register("w1", "alpha", "/w/b")
	register("w2", "alpha", "/w/a")

	recs, err := s.Workspaces()
	if err != nil {
		t.Fatalf("workspaces: %v", err)
	}
	var got []string
	for _, r := range recs {
		got = append(got, r.ID)
	}
	want := []string{"w2", "w1", "w3"}
	if len(got) != len(want) {
		t.Fatalf("workspaces = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("workspaces = %v, want %v ordered by (slug, worktree)", got, want)
			break
		}
	}
}

func TestFakeStoreCommitReconciliationRespectsNilDigest(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	digest := "sha256:old"
	if err := s.CommitReconciliation("w1", state.ReconciliationResult{
		AppliedDigest: &digest,
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.CommitReconciliation("w1", state.ReconciliationResult{
		Operation: state.Operation{Name: "open", Outcome: state.OutcomeFailed},
	}, testTime); err != nil {
		t.Fatalf("failure: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != digest {
		t.Errorf("applied digest = %v, want %q untouched", rec.AppliedDigest, digest)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != state.OutcomeFailed {
		t.Errorf("operation = %+v", rec.LastOperation)
	}
}

// TestFakeStoreCommitReconciliationIsAllOrNothing mirrors the real store's
// transaction rollback: a rejected container observation must leave every
// other field untouched.
func TestFakeStoreCommitReconciliationIsAllOrNothing(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	digest := "sha256:new"
	err := s.CommitReconciliation("w1", state.ReconciliationResult{
		AppliedDigest: &digest,
		// Present without a container ID is invalid and must be rejected.
		Container: &state.ContainerObservation{Health: state.HealthPresent},
		Operation: state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, testTime)
	if err == nil {
		t.Fatal("an invalid observation should fail the commit")
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.AppliedDigest != nil {
		t.Errorf("applied digest = %v, want untouched nil after the failed commit", rec.AppliedDigest)
	}
	if rec.LastOperation != nil {
		t.Errorf("operation = %+v, want none after the failed commit", rec.LastOperation)
	}
}

func TestFakeStoreAdoptSessionName(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.RegisterWorkspace(testWorkspace("w2", "other"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := s.AdoptSessionName("w1", "slab--live", testTime); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slab--live" {
		t.Errorf("ActualSession = %v, want slab--live", rec.ActualSession)
	}
	if err := s.AdoptSessionName("w1", "slab--live", testTime); err != nil {
		t.Errorf("re-adopting the same name: %v", err)
	}

	err = s.AdoptSessionName("w2", "slab--live", testTime)
	var conflict *state.SessionNameConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v, want *state.SessionNameConflictError", err)
	}
	if err := s.AdoptSessionName("nope", "x", testTime); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("unknown workspace err = %v, want ErrNotFound", err)
	}
	if err := s.AdoptSessionName("w1", "", testTime); err == nil {
		t.Error("an empty session name was accepted")
	}
}

func TestFakeSessionActuatorRecordsSpecs(t *testing.T) {
	a := &SessionActuator{}
	spec := controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Windows:     []controller.WindowSpec{{Name: "shell"}},
	}
	if err := a.CreateSession(context.Background(), spec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(a.Created) != 1 || a.Created[0].Name != "slab" {
		t.Errorf("Created = %+v", a.Created)
	}

	a.Err = errors.New("boom")
	if err := a.CreateSession(context.Background(), spec); err == nil {
		t.Error("configured error was not returned")
	}
}

func TestFakeContainerObserverApplies(t *testing.T) {
	o := &ContainerObserver{AppliesResult: true}
	ok, err := o.Applies(context.Background(), resolve.Workspace{}, config.Config{})
	if err != nil || !ok {
		t.Errorf("Applies = (%t, %v), want (true, nil)", ok, err)
	}
	o.AppliesErr = errors.New("stat exploded")
	if _, err := o.Applies(context.Background(), resolve.Workspace{}, config.Config{}); err == nil {
		t.Error("configured Applies error was not returned")
	}
}

func TestFakeContainerActuator(t *testing.T) {
	a := &ContainerActuator{
		StartResult: controller.ContainerObservation{
			Health: state.HealthPresent, ContainerID: "c1", Workdir: "/workspaces/w",
		},
	}
	obs, err := a.StartContainer(context.Background(),
		resolve.Workspace{ID: "w1"}, config.Config{})
	if err != nil || obs.ContainerID != "c1" {
		t.Errorf("StartContainer = (%+v, %v)", obs, err)
	}
	if len(a.Started) != 1 || a.Started[0] != "w1" {
		t.Errorf("Started = %v", a.Started)
	}
	cmd := a.ExecCommand(state.ContainerBinding{ContainerID: "c1", Workdir: "/workspaces/w"},
		"make", "sub", map[string]string{"A": "1"})
	if cmd != `fake-exec c1 /workspaces/w/sub "make" env=1` {
		t.Errorf("ExecCommand = %q", cmd)
	}

	a.StartErr = errors.New("boom")
	if _, err := a.StartContainer(context.Background(), resolve.Workspace{}, config.Config{}); err == nil {
		t.Error("configured start error was not returned")
	}
}

func TestFakeActuatorsKillAndStop(t *testing.T) {
	sa := &SessionActuator{}
	if err := sa.KillSession(context.Background(), "slab"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	if len(sa.Killed) != 1 || sa.Killed[0] != "slab" {
		t.Errorf("Killed = %v", sa.Killed)
	}
	sa.KillErr = errors.New("boom")
	if err := sa.KillSession(context.Background(), "slab"); err == nil {
		t.Error("configured kill error was not returned")
	}

	ca := &ContainerActuator{}
	if err := ca.StopContainer(context.Background(), "c1"); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}
	if len(ca.Stopped) != 1 || ca.Stopped[0] != "c1" {
		t.Errorf("Stopped = %v", ca.Stopped)
	}
	ca.StopErr = errors.New("boom")
	if err := ca.StopContainer(context.Background(), "c1"); err == nil {
		t.Error("configured stop error was not returned")
	}
}
