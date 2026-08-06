package rebuild

import (
	"reflect"
	"slices"
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

// TestClassifyDuplicateIDBeatsNameTaken is the combination spec §8 calls
// out by name: two sessions share one workspace ID and one of them also
// collides with another workspace's recorded name. Duplicate ID is the
// broader uncertainty, so both sessions must report it — reporting the
// name collision instead would suggest that freeing the name is enough.
func TestClassifyDuplicateIDBeatsNameTaken(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("slab", "id-1", "slab", "/w/slab"),
			live("dup", "id-1", "slab", "/w/slab"),
		},
		[]state.Record{stored("id-2", "other", "/w/other", "slab")},
	)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none", plan.Candidates)
	}
	want := `sessions "dup" and "slab" all carry workspace ID id-1, so rebuild ` +
		`cannot tell which one is the workspace; none of them is registered.`
	if len(plan.Conflicts) != 2 {
		t.Fatalf("%d conflicts, want 2: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	for _, got := range plan.Conflicts {
		if got.Reason != want {
			t.Errorf("conflict %q reason =\n %q\nwant\n %q", got.Subject, got.Reason, want)
		}
	}
}

// TestClassifyNameTakenBeatsIdentityMismatch: the session both contradicts
// its own row's identity and squats another workspace's recorded name. The
// name collision is reported, because it is the one that would make a
// write fail outright.
func TestClassifyNameTakenBeatsIdentityMismatch(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "wrong", "/w/slab")},
		[]state.Record{
			stored("id-1", "slab", "/w/slab", ""),
			stored("id-2", "other", "/w/other", "slab"),
		},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" is already recorded as the session of workspace id-2 `+
			`(other), so rebuild will not also adopt it for workspace id-1.`)
}

// TestClassifyIdentityMismatchBeatsSettled is the one that would otherwise
// pass silently. The row already records this session name, so a
// name-first reading calls it settled and reports nothing — while the
// worktree it points at disagrees, which is exactly the corruption a
// rebuild run is there to surface.
func TestClassifyIdentityMismatchBeatsSettled(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/other", "slab")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" carries slug "slab" and worktree "/w/slab", but workspace `+
			`id-1 is recorded as slug "slab" and worktree "/w/other"; that `+
			`contradiction is evidence of corruption or collision rather than a `+
			`match, so nothing is written.`)
}

// TestClassifyIdentityMismatchBeatsAdopt: an empty actual_session is an
// invitation to adopt, but not into a row whose identity contradicts the
// session. Adopting here would attach the workspace to the wrong tree.
func TestClassifyIdentityMismatchBeatsAdopt(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "renamed", "/w/slab", "")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" carries slug "slab" and worktree "/w/slab", but workspace `+
			`id-1 is recorded as slug "renamed" and worktree "/w/slab"; that `+
			`contradiction is evidence of corruption or collision rather than a `+
			`match, so nothing is written.`)
}

// TestClassifySettledSessionIsNotNameTaken guards the near miss in the
// name-taken rule: a settled workspace owns its own recorded name, so the
// rule must compare owners rather than merely finding the name recorded.
// Getting this wrong makes every second run report every workspace.
func TestClassifySettledSessionIsNotNameTaken(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("slab", "id-1", "slab", "/w/slab"),
			live("mux", "id-2", "mux", "/w/mux"),
		},
		[]state.Record{
			stored("id-1", "slab", "/w/slab", "slab"),
			stored("id-2", "mux", "/w/mux", "mux"),
		},
	)
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty: a fully recovered installation reports nothing", plan)
	}
}

// permutations returns every ordering of s. The inputs here are small
// enough (4! = 24) to enumerate exhaustively, which beats a seeded shuffle:
// it cannot pass by luck.
func permutations[T any](s []T) [][]T {
	if len(s) <= 1 {
		return [][]T{slices.Clone(s)}
	}
	var out [][]T
	for i := range s {
		rest := make([]T, 0, len(s)-1)
		rest = append(rest, s[:i]...)
		rest = append(rest, s[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]T{s[i]}, tail...))
		}
	}
	return out
}

// TestClassifyIsIndependentOfInputOrder feeds one mixed installation —
// a registration, an adoption, a settled row, and a conflict — in every
// possible order and asserts the plan never moves. Map iteration inside
// Classify is the thing this would catch.
func TestClassifyIsIndependentOfInputOrder(t *testing.T) {
	sessions := []controller.LiveSession{
		live("fresh", "id-fresh", "fresh", "/w/fresh"),
		live("adoptme", "id-adopt", "adoptme", "/w/adoptme"),
		live("settled", "id-settled", "settled", "/w/settled"),
		live("drifted", "id-drift", "drifted", "/w/drifted"),
	}
	records := []state.Record{
		stored("id-adopt", "adoptme", "/w/adoptme", ""),
		stored("id-settled", "settled", "/w/settled", "settled"),
		stored("id-drift", "drifted", "/w/drifted", "drifted-old"),
		stored("id-gone", "gone", "/w/gone", ""),
	}

	want := Classify(sessions, records)
	if len(want.Candidates) != 2 || len(want.Conflicts) != 1 {
		t.Fatalf("fixture produced %d candidates and %d conflicts, want 2 and 1: %+v",
			len(want.Candidates), len(want.Conflicts), want)
	}

	for _, liveOrder := range permutations(sessions) {
		for _, recordOrder := range permutations(records) {
			if got := Classify(liveOrder, recordOrder); !reflect.DeepEqual(got, want) {
				t.Fatalf("order changed the plan:\n got %+v\nwant %+v", got, want)
			}
		}
	}
}

// TestClassifyOrdersCandidatesBySlugThenName pins the primary sort key.
// The names here sort opposite to the slugs, so a name-first
// implementation produces the reverse of this list.
func TestClassifyOrdersCandidatesBySlugThenName(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("zeta", "id-1", "alpha", "/w/alpha"),
			live("alpha", "id-2", "beta", "/w/beta"),
			live("beta", "id-3", "beta", "/w/beta2"),
		},
		nil,
	)
	var got []string
	for _, cand := range plan.Candidates {
		got = append(got, cand.Session.Slug+"/"+cand.Session.Name)
	}
	want := []string{"alpha/zeta", "beta/alpha", "beta/beta"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidate order = %v, want %v", got, want)
	}
}

// TestClassifyOrdersConflictsBySubject keeps the report stable for a
// reader diffing two runs.
func TestClassifyOrdersConflictsBySubject(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("zulu", "id-1", "zulu", "/w/zulu"),
			live("alfa", "id-2", "alfa", "/w/alfa"),
			live("mike", "id-3", "mike", "/w/mike"),
		},
		[]state.Record{
			stored("id-1", "zulu", "/w/zulu", "zulu-old"),
			stored("id-2", "alfa", "/w/alfa", "alfa-old"),
			stored("id-3", "mike", "/w/mike", "mike-old"),
		},
	)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none", plan.Candidates)
	}
	var got []string
	for _, c := range plan.Conflicts {
		got = append(got, c.Subject)
	}
	if want := []string{"alfa", "mike", "zulu"}; !slices.Equal(got, want) {
		t.Fatalf("conflict order = %v, want %v", got, want)
	}
}
