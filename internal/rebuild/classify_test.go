package rebuild

import (
	"reflect"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// live builds a session carrying the three tmux identity keys.
func live(name, id, slug, worktree string) controller.LiveSession {
	return controller.LiveSession{
		ID:          "$0",
		Name:        name,
		WorkspaceID: id,
		Slug:        slug,
		Worktree:    worktree,
	}
}

// stored builds a registered row. actual is the recorded actual_session,
// empty meaning nil — the state a workspace is in when no session has been
// adopted for it.
func stored(id, slug, worktree, actual string) state.Record {
	rec := state.Record{
		ID:              id,
		Slug:            slug,
		Worktree:        worktree,
		IsPrimary:       true,
		ProposedSession: slug,
	}
	if actual != "" {
		rec.ActualSession = &actual
	}
	return rec
}

// onlyCandidate asserts the plan holds exactly one candidate in the
// expected case and no conflicts, and returns it.
func onlyCandidate(t *testing.T, plan Plan, want Case) Candidate {
	t.Helper()
	if len(plan.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", plan.Conflicts)
	}
	if len(plan.Candidates) != 1 {
		t.Fatalf("%d candidates, want 1: %+v", len(plan.Candidates), plan.Candidates)
	}
	if got := plan.Candidates[0].Case; got != want {
		t.Fatalf("case = %s, want %s", got, want)
	}
	return plan.Candidates[0]
}

// onlyConflict asserts the plan holds exactly one conflict with the
// expected subject and reason, and no candidates. The reason is compared
// in full: it is what the operator reads when rebuild declines to act.
func onlyConflict(t *testing.T, plan Plan, subject, reason string) {
	t.Helper()
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none: this session must not be written for", plan.Candidates)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("%d conflicts, want 1: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	got := plan.Conflicts[0]
	if got.Subject != subject {
		t.Errorf("subject = %q, want %q", got.Subject, subject)
	}
	if got.Reason != reason {
		t.Errorf("reason =\n %q\nwant\n %q", got.Reason, reason)
	}
}

// TestClassifyRegistersAnUnrecordedSession is the primary recovery path:
// the database lost the row, tmux still has the session.
func TestClassifyRegistersAnUnrecordedSession(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		nil,
	)
	cand := onlyCandidate(t, plan, CaseRegister)
	if cand.Session.Name != "slab" {
		t.Errorf("session = %q, want %q", cand.Session.Name, "slab")
	}
	if cand.Record != nil {
		t.Errorf("Record = %+v, want nil: there is no row to carry", cand.Record)
	}
}

// TestClassifyAdoptsARowWithNoSession covers the half-recovered state a
// partial application leaves behind, which the next run completes.
func TestClassifyAdoptsARowWithNoSession(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "")},
	)
	cand := onlyCandidate(t, plan, CaseAdopt)
	if cand.Record == nil {
		t.Fatal("Record = nil, want the stored row an adoption updates")
	}
	if cand.Record.ID != "id-1" {
		t.Errorf("Record.ID = %q, want %q", cand.Record.ID, "id-1")
	}
}

// TestClassifySettledSessionIsSilent is the idempotence claim. A fully
// recovered installation must produce an empty report, not a list of
// things that were already fine.
func TestClassifySettledSessionIsSilent(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "slab")},
	)
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty: a settled session is neither a candidate nor a conflict", plan)
	}
}

// TestClassifySessionMismatchIsAConflict is fill-only at its sharpest: a
// recorded session name is a recorded value, so rebuild reports the
// disagreement rather than replacing it.
func TestClassifySessionMismatchIsAConflict(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "slab-old")},
	)
	onlyConflict(t, plan, "slab",
		`workspace id-1 (slab) already records session "slab-old", but the live `+
			`session carrying its identity keys is named "slab"; rebuild fills in `+
			`missing state and never overwrites a recorded session name.`)
}

// TestClassifyIdentityMismatchIsAConflict is the case where an overwrite
// would do real damage: it would repoint a workspace at the wrong tree.
func TestClassifyIdentityMismatchIsAConflict(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "other", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" carries slug "other" and worktree "/w/slab", but workspace `+
			`id-1 is recorded as slug "slab" and worktree "/w/slab"; that `+
			`contradiction is evidence of corruption or collision rather than a `+
			`match, so nothing is written.`)
}

// TestClassifyDuplicateIDIsAConflictForEverySession matches ObserveSession,
// which already treats multiple claimants as uncertainty and picks none.
// Both sessions are reported, because the operator has to look at both.
func TestClassifyDuplicateIDIsAConflictForEverySession(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("slab", "id-1", "slab", "/w/slab"),
			live("slab--wt", "id-1", "slab", "/w/slab"),
		},
		nil,
	)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none: neither claimant may be registered", plan.Candidates)
	}
	want := `sessions "slab" and "slab--wt" all carry workspace ID id-1, so rebuild ` +
		`cannot tell which one is the workspace; none of them is registered.`
	if len(plan.Conflicts) != 2 {
		t.Fatalf("%d conflicts, want 2: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	for i, subject := range []string{"slab", "slab--wt"} {
		if plan.Conflicts[i].Subject != subject {
			t.Errorf("conflict %d subject = %q, want %q", i, plan.Conflicts[i].Subject, subject)
		}
		if plan.Conflicts[i].Reason != want {
			t.Errorf("conflict %d reason =\n %q\nwant\n %q", i, plan.Conflicts[i].Reason, want)
		}
	}
}

// TestClassifyNameTakenIsAConflict follows design §7: collision resolution
// happens in one transaction and a name conflict is a refusal, never an
// overwrite.
func TestClassifyNameTakenIsAConflict(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-2", "other", "/w/other", "slab")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" is already recorded as the session of workspace id-2 `+
			`(other), so rebuild will not also adopt it for workspace id-1.`)
}

// TestClassifyIgnoresSessionsWithoutAWorkspaceID keeps rebuild out of
// sessions that are not ours, as buildList and orphanedSessions already do.
// They are not conflicts either: there is nothing to report about them.
func TestClassifyIgnoresSessionsWithoutAWorkspaceID(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("someones-editor", "", "", ""),
			live("scratch", "", "scratch", "/w/scratch"),
		},
		nil,
	)
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty: foreign sessions are ignored entirely", plan)
	}
}

// TestClassifyEmptyInput covers the fresh installation with no tmux server.
func TestClassifyEmptyInput(t *testing.T) {
	if plan := Classify(nil, nil); !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty", plan)
	}
}
