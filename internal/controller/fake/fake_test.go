package fake

import (
	"errors"
	"testing"
	"time"

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
