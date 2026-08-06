package rebuild

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var testTime = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// scriptedObserver returns a different observation per call.
// fake.SessionObserver cannot serve here: it returns one canned
// observation for every call, so a batch of two candidates cannot give
// each its own, and "the session vanished before the lock" cannot be
// expressed alongside a live one in the same run.
type scriptedObserver struct {
	results []controller.SessionObservation
	errs    []error
	calls   int
	queries []controller.SessionQuery
}

func (o *scriptedObserver) ObserveSession(_ context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	i := o.calls
	o.calls++
	o.queries = append(o.queries, q)
	if i < len(o.errs) && o.errs[i] != nil {
		return controller.SessionObservation{}, o.errs[i]
	}
	if i < len(o.results) {
		return o.results[i], nil
	}
	return controller.SessionObservation{}, fmt.Errorf("scriptedObserver: unscripted call %d", i+1)
}

// mapResolver stands in for resolve.Resolve, which shells out to git. A
// missing entry is the vanished-worktree case; errs models a tree that
// exists but will not resolve.
type mapResolver struct {
	byWorktree map[string]resolve.Workspace
	errs       map[string]error
}

func (r *mapResolver) Resolve(worktree string) (resolve.Workspace, error) {
	if err := r.errs[worktree]; err != nil {
		return resolve.Workspace{}, err
	}
	ws, ok := r.byWorktree[worktree]
	if !ok {
		return resolve.Workspace{}, fmt.Errorf("no worktree at %s", worktree)
	}
	return ws, nil
}

type mapConfig struct {
	digests map[string]string
	errs    map[string]error
}

func (c *mapConfig) Digest(slug string) (string, error) {
	if err := c.errs[slug]; err != nil {
		return "", err
	}
	digest, ok := c.digests[slug]
	if !ok {
		return "", fmt.Errorf("no configuration for slug %s", slug)
	}
	return digest, nil
}

type countingLocker struct {
	locked   []string
	released int
	err      error
}

func (l *countingLocker) Lock(_ context.Context, workspaceID string) (func(), error) {
	if l.err != nil {
		return nil, l.err
	}
	l.locked = append(l.locked, workspaceID)
	return func() { l.released++ }, nil
}

// countingStore counts writes so a dry run can be asserted to have made
// none, rather than inferred to have made none from the resulting rows.
type countingStore struct {
	*fake.Store
	registers int
	adopts    int
}

func (s *countingStore) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	s.registers++
	return s.Store.RegisterWorkspace(ws, desiredDigest, now)
}

func (s *countingStore) AdoptSessionName(workspaceID, name string, now time.Time) error {
	s.adopts++
	return s.Store.AdoptSessionName(workspaceID, name, now)
}

// adoptFailStore fails the first adoption only, so one store can serve
// both the half-applied run and the second run that completes it.
type adoptFailStore struct {
	*fake.Store
	err error
}

func (s *adoptFailStore) AdoptSessionName(workspaceID, name string, now time.Time) error {
	if s.err != nil {
		err := s.err
		s.err = nil
		return err
	}
	return s.Store.AdoptSessionName(workspaceID, name, now)
}

type harness struct {
	fakeStore *fake.Store
	store     Store
	observer  *scriptedObserver
	resolver  *mapResolver
	config    *mapConfig
	locker    *countingLocker
	dryRun    bool
}

func newHarness() *harness {
	fs := fake.NewStore()
	return &harness{
		fakeStore: fs,
		store:     fs,
		observer:  &scriptedObserver{},
		resolver:  &mapResolver{byWorktree: map[string]resolve.Workspace{}, errs: map[string]error{}},
		config:    &mapConfig{digests: map[string]string{}, errs: map[string]error{}},
		locker:    &countingLocker{},
	}
}

// know teaches the resolver and the configuration loader about one
// workspace, the way a real git tree and a real defaults.yaml would.
func (h *harness) know(ws resolve.Workspace, digest string) {
	h.resolver.byWorktree[ws.Worktree] = ws
	h.config.digests[ws.Slug] = digest
}

func (h *harness) applier() *Applier {
	return &Applier{
		Store:    h.store,
		Sessions: h.observer,
		Resolver: h.resolver,
		Config:   h.config,
		Locker:   h.locker,
		Clock:    &fake.Clock{Time: testTime},
		DryRun:   h.dryRun,
	}
}

func workspace(id, slug, worktree, sessionName string, primary bool) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        slug,
		Worktree:    worktree,
		SessionName: sessionName,
		IsPrimary:   primary,
	}
}

func projectmux() resolve.Workspace {
	return workspace(
		"1111111111111111111111111111111111111111111111111111111111111111",
		"projectmux", "/src/projectmux", "projectmux", true)
}

// liveSession is a session carrying identity keys that agree with the
// workspace. Tests that need disagreement overwrite one field after.
func liveSession(ws resolve.Workspace, name string) controller.LiveSession {
	return controller.LiveSession{
		ID:          "$1",
		Name:        name,
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.Worktree,
	}
}

func observing(s controller.LiveSession) controller.SessionObservation {
	return controller.SessionObservation{ByIdentity: &s, ByName: []controller.LiveSession{s}}
}

func TestApplyRegistersAndAdoptsAWorkspaceWithNoRow(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", report.Conflicts)
	}
	want := []Registered{{
		ID:        ws.ID,
		Slug:      "projectmux",
		Worktree:  "/src/projectmux",
		IsPrimary: true,
		Session:   "projectmux",
	}}
	if !reflect.DeepEqual(report.Registered, want) {
		t.Fatalf("Registered = %+v, want %+v", report.Registered, want)
	}

	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	if !rec.IsPrimary {
		t.Errorf("IsPrimary = false, want true — it comes from the resolver, not the session keys")
	}
	if rec.ProposedSession != "projectmux" {
		t.Errorf("ProposedSession = %q, want %q", rec.ProposedSession, "projectmux")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:desired" {
		t.Errorf("DesiredDigest = %v, want %q", rec.DesiredDigest, "sha256:desired")
	}
	if rec.AppliedDigest != nil {
		t.Errorf("AppliedDigest = %q, want nil so the next open reconciles", *rec.AppliedDigest)
	}
	if got := h.locker.locked; !reflect.DeepEqual(got, []string{ws.ID}) {
		t.Errorf("locked = %v, want [%s]", got, ws.ID)
	}
	if h.locker.released != 1 {
		t.Errorf("lock released %d times, want 1", h.locker.released)
	}
}

func TestApplyResolverFailureIsAConflictAndTheBatchContinues(t *testing.T) {
	ws := projectmux()
	good := liveSession(ws, "projectmux")
	gone := controller.LiveSession{
		ID:          "$2",
		Name:        "vanished",
		WorkspaceID: "2222222222222222222222222222222222222222222222222222222222222222",
		Slug:        "vanished",
		Worktree:    "/src/vanished",
	}
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.resolver.errs["/src/vanished"] = errors.New("worktree /src/vanished does not exist")
	h.observer.results = []controller.SessionObservation{observing(good)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{
			{Case: CaseRegister, Session: gone},
			{Case: CaseRegister, Session: good},
		},
	})

	if len(report.Registered) != 1 || report.Registered[0].Slug != "projectmux" {
		t.Fatalf("Registered = %+v, want only the resolvable workspace", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if report.Conflicts[0].Subject != "vanished" {
		t.Errorf("Subject = %q, want %q", report.Conflicts[0].Subject, "vanished")
	}
	if !strings.Contains(report.Conflicts[0].Reason, "/src/vanished") {
		t.Errorf("Reason = %q, want it to name the worktree", report.Conflicts[0].Reason)
	}
}

func TestApplyConfigFailureIsOneWorkspacesConflictNotTheBatchs(t *testing.T) {
	ws := projectmux()
	other := workspace(
		"3333333333333333333333333333333333333333333333333333333333333333",
		"other", "/src/other", "other", true)
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.know(other, "sha256:other")
	h.config.errs["other"] = errors.New(`other.yaml: unknown field "widnows"`)
	good := liveSession(ws, "projectmux")
	otherSess := liveSession(other, "other")
	// Both candidates are scripted, in candidate order. A failed digest is
	// carried to the lock rather than ending the candidate, so "other"
	// re-observes too and only then refuses; scripting "good" alone would
	// hand "other" the wrong session and leave "good" unscripted.
	h.observer.results = []controller.SessionObservation{observing(otherSess), observing(good)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{
			{Case: CaseRegister, Session: otherSess},
			{Case: CaseRegister, Session: good},
		},
	})

	if len(report.Registered) != 1 || report.Registered[0].Slug != "projectmux" {
		t.Fatalf("Registered = %+v, want only the workspace whose configuration loaded", report.Registered)
	}
	if len(report.Conflicts) != 1 || report.Conflicts[0].Subject != "other" {
		t.Fatalf("Conflicts = %+v, want one for %q", report.Conflicts, "other")
	}
	if !strings.Contains(report.Conflicts[0].Reason, "widnows") {
		t.Errorf("Reason = %q, want the underlying configuration error preserved", report.Conflicts[0].Reason)
	}
	if _, err := h.fakeStore.Workspace(other.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace %s err = %v, want ErrNotFound: a configuration failure writes nothing", other.ID, err)
	}
}

func TestApplyOrdersRegistrationsBySlugThenSessionAndPassesPlanConflictsThrough(t *testing.T) {
	alpha := workspace(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"alpha", "/src/alpha", "alpha", true)
	alphaWT := workspace(
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"alpha", "/src/alpha/.worktrees/wt", "alpha--wt", false)
	zulu := workspace(
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"zulu", "/src/zulu", "zulu", true)

	h := newHarness()
	h.know(alpha, "sha256:a")
	h.know(alphaWT, "sha256:a")
	h.know(zulu, "sha256:z")

	zuluSess := liveSession(zulu, "zulu")
	wtSess := liveSession(alphaWT, "alpha--wt")
	alphaSess := liveSession(alpha, "alpha")
	// The observer is scripted in candidate order, not report order.
	h.observer.results = []controller.SessionObservation{
		observing(zuluSess), observing(wtSess), observing(alphaSess),
	}

	planConflict := Conflict{Subject: "ghost", Reason: "two live sessions claim workspace dddd"}
	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{
			{Case: CaseRegister, Session: zuluSess},
			{Case: CaseRegister, Session: wtSess},
			{Case: CaseRegister, Session: alphaSess},
		},
		Conflicts: []Conflict{planConflict},
	})

	var sessions []string
	for _, r := range report.Registered {
		sessions = append(sessions, r.Session)
	}
	want := []string{"alpha", "alpha--wt", "zulu"}
	if !reflect.DeepEqual(sessions, want) {
		t.Errorf("registered sessions = %v, want %v (slug, then session name)", sessions, want)
	}
	if !reflect.DeepEqual(report.Conflicts, []Conflict{planConflict}) {
		t.Errorf("Conflicts = %+v, want the classification conflict passed through unchanged", report.Conflicts)
	}
}

func TestApplyRefusesASessionWhoseIdentityKeysContradictTheWorkspace(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	// The derived workspace ID still matches; the slug does not. Checking
	// the ID alone would register the workspace from resolved values that
	// silently disagree with the live keys.
	sess.Slug = "stale-slug"

	h := newHarness()
	h.know(ws, "sha256:desired")
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "stale-slug") {
		t.Errorf("Reason = %q, want it to name the contradictory key", report.Conflicts[0].Reason)
	}
	if len(h.locker.locked) != 0 {
		t.Errorf("locked = %v, want none: identity is verified before the lock", h.locker.locked)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound: a refused session writes nothing", err)
	}
}

func TestApplyDryRunMatchesTheRealRunAndWritesNothing(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	gone := controller.LiveSession{
		ID:          "$2",
		Name:        "vanished",
		WorkspaceID: "2222222222222222222222222222222222222222222222222222222222222222",
		Slug:        "vanished",
		Worktree:    "/src/vanished",
	}
	planConflict := Conflict{Subject: "ghost", Reason: "two live sessions claim workspace dddd"}
	newPlan := func() Plan {
		return Plan{
			Candidates: []Candidate{
				{Case: CaseRegister, Session: gone},
				{Case: CaseRegister, Session: sess},
			},
			Conflicts: []Conflict{planConflict},
		}
	}

	actual := newHarness()
	actual.know(ws, "sha256:desired")
	actual.resolver.errs["/src/vanished"] = errors.New("worktree /src/vanished does not exist")
	actual.observer.results = []controller.SessionObservation{observing(sess)}
	actualReport := actual.applier().Apply(context.Background(), newPlan())

	preview := newHarness()
	preview.know(ws, "sha256:desired")
	preview.resolver.errs["/src/vanished"] = errors.New("worktree /src/vanished does not exist")
	// Scripted identically to the real run: the preview observes too, so
	// that it can predict the refusals observation is what discovers.
	preview.observer.results = []controller.SessionObservation{observing(sess)}
	counting := &countingStore{Store: preview.fakeStore}
	preview.store = counting
	preview.dryRun = true
	previewReport := preview.applier().Apply(context.Background(), newPlan())

	if !previewReport.DryRun {
		t.Errorf("dry run DryRun = false, want true")
	}
	if actualReport.DryRun {
		t.Errorf("real run DryRun = true, want false")
	}
	// The verdict is the deliverable: a dry run that says "would register"
	// has established every fact registration depends on except the
	// outcome of the writes themselves.
	if !reflect.DeepEqual(previewReport.Registered, actualReport.Registered) {
		t.Errorf("dry Registered = %+v, real = %+v; they must be identical",
			previewReport.Registered, actualReport.Registered)
	}
	if !reflect.DeepEqual(previewReport.Conflicts, actualReport.Conflicts) {
		t.Errorf("dry Conflicts = %+v, real = %+v; they must be identical",
			previewReport.Conflicts, actualReport.Conflicts)
	}
	if counting.registers != 0 || counting.adopts != 0 {
		t.Errorf("dry run wrote: %d registers, %d adopts; want 0 and 0",
			counting.registers, counting.adopts)
	}
	if len(preview.locker.locked) != 0 {
		t.Errorf("dry run locked %v, want nothing", preview.locker.locked)
	}
	// Observing is read-only, so a dry run may do it — and must, to
	// predict the refusals it discovers. The lock and the writes are what
	// a preview withholds, and those are asserted above.
	if preview.observer.calls != 1 {
		t.Errorf("dry run called ObserveSession %d times, want 1", preview.observer.calls)
	}
	recs, err := preview.fakeStore.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("dry run left %d records, want 0", len(recs))
	}
}

// seedRecorded registers a row whose every overwritable field disagrees
// with what the resolver and the configuration loader would supply.
// RegisterWorkspace's conflict branch overwrites slug, worktree,
// is_primary, proposed_session, and desired_digest
// (internal/state/store.go:43-49), so any of these changing proves the
// applier re-registered a workspace it should only have adopted.
//
// Slug and worktree deliberately match: a row disagreeing on those is an
// identity mismatch, which classification refuses before it ever reaches
// application.
func seedRecorded(t *testing.T, store *fake.Store, ws resolve.Workspace) {
	t.Helper()
	recorded := workspace(ws.ID, ws.Slug, ws.Worktree, "recorded-proposed", false)
	if err := store.RegisterWorkspace(recorded, "sha256:recorded", testTime); err != nil {
		t.Fatalf("seeding the recorded row: %v", err)
	}
}

func assertRecordedFieldsUntouched(t *testing.T, rec state.Record, ws resolve.Workspace) {
	t.Helper()
	if rec.IsPrimary {
		t.Errorf("IsPrimary = true, want the recorded false: adoption must not re-register")
	}
	if rec.ProposedSession != "recorded-proposed" {
		t.Errorf("ProposedSession = %q, want %q", rec.ProposedSession, "recorded-proposed")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:recorded" {
		t.Errorf("DesiredDigest = %v, want %q", rec.DesiredDigest, "sha256:recorded")
	}
	if rec.Slug != ws.Slug {
		t.Errorf("Slug = %q, want %q", rec.Slug, ws.Slug)
	}
	if rec.Worktree != ws.Worktree {
		t.Errorf("Worktree = %q, want %q", rec.Worktree, ws.Worktree)
	}
}

func TestApplyAdoptsOnlyAndLeavesEveryRecordedFieldUntouched(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	// A broken workspace configuration must not block adoption: only
	// registration writes a digest.
	h.config.errs["projectmux"] = errors.New("projectmux.yaml is unreadable")
	seedRecorded(t, h.fakeStore, ws)
	h.observer.results = []controller.SessionObservation{observing(sess)}

	// Candidate.Record is left nil on purpose: the applier re-reads the
	// row under the lock and must never trust the first pass's copy.
	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseAdopt, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", report.Conflicts)
	}
	if len(report.Registered) != 1 {
		t.Fatalf("Registered = %+v, want one", report.Registered)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	assertRecordedFieldsUntouched(t, rec, ws)
}

func TestApplyReclassifiesARegisterCandidateWhoseRowAppearedBeforeTheLock(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	// Classification saw no row; another process registered the workspace
	// in the gap. The lock-time re-read must turn this into an adoption.
	seedRecorded(t, h.fakeStore, ws)
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", report.Conflicts)
	}
	if len(report.Registered) != 1 {
		t.Fatalf("Registered = %+v, want one", report.Registered)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	assertRecordedFieldsUntouched(t, rec, ws)

	if h.observer.calls != 1 {
		t.Fatalf("ObserveSession called %d times, want 1 (under the lock)", h.observer.calls)
	}
	wantQuery := controller.SessionQuery{
		WorkspaceID:    ws.ID,
		CandidateNames: []string{"projectmux"},
	}
	if !reflect.DeepEqual(h.observer.queries[0], wantQuery) {
		t.Errorf("query = %+v, want %+v", h.observer.queries[0], wantQuery)
	}
}

// The mirror image of the reclassification above, and the reason a digest
// failure is carried forward rather than returned where it happens.
// Classification saw no row, so a digest was loaded and the load failed —
// but by the time the lock was held the row existed, which makes this an
// adoption, and adoption writes no digest. Refusing at the load would
// turn a recoverable workspace into a conflict on the strength of a
// requirement it no longer has.
func TestApplyAdoptsARegisterCandidateWhoseDigestFailedButWhoseRowAppeared(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.config.errs["projectmux"] = errors.New("projectmux.yaml is unreadable")
	seedRecorded(t, h.fakeStore, ws)
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none: adoption does not need a digest", report.Conflicts)
	}
	if len(report.Registered) != 1 {
		t.Fatalf("Registered = %+v, want one", report.Registered)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	assertRecordedFieldsUntouched(t, rec, ws)
}

// Carrying the failure must not swallow it. A register that is still a
// register under the lock writes a row recording a desired digest, and
// there is no digest to record: inventing one would be a guess, so this
// is a conflict and nothing is written.
func TestApplyRegisterWithAnUnreadableConfigurationIsAConflict(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.config.errs["projectmux"] = errors.New("projectmux.yaml is unreadable")
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "projectmux.yaml is unreadable") {
		t.Errorf("Reason = %q, want it to carry the configuration error",
			report.Conflicts[0].Reason)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound: nothing may be written", err)
	}
}

func TestApplySessionThatVanishedBeforeTheLockIsAConflictWithNoWrite(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	// The session died between classification and the lock: the lock-time
	// observation finds nothing carrying the workspace's identity keys.
	h.observer.results = []controller.SessionObservation{{}}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "no longer live") {
		t.Errorf("Reason = %q, want it to say the session was no longer live",
			report.Conflicts[0].Reason)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound: a dead session writes nothing", err)
	}
	if h.locker.released != 1 {
		t.Errorf("lock released %d times, want 1 even on the conflict path", h.locker.released)
	}
}

func TestApplyReportsAnObservationFailureAsAConflict(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.observer.errs = []error{errors.New("no server running on /tmp/tmux-1000/default")}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 ||
		!strings.Contains(report.Conflicts[0].Reason, "no server running") {
		t.Fatalf("Conflicts = %+v, want one preserving the tmux error", report.Conflicts)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound", err)
	}
}

func TestApplyRegisterThenFailedAdoptNamesBothHalvesAndASecondRunCompletesIt(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.store = &adoptFailStore{
		Store: h.fakeStore,
		err:   &state.SessionNameConflictError{Name: "projectmux"},
	}
	h.observer.results = []controller.SessionObservation{observing(sess), observing(sess)}
	plan := Plan{Candidates: []Candidate{{Case: CaseRegister, Session: sess}}}

	first := h.applier().Apply(context.Background(), plan)

	if len(first.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none: only half the work landed", first.Registered)
	}
	if len(first.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", first.Conflicts)
	}
	// The operator must never be told the workspace was registered when
	// only half of it was, nor that nothing happened when a row now
	// exists. The reason names both halves.
	reason := first.Conflicts[0].Reason
	for _, want := range []string{
		"was registered",
		"adopting session name",
		"already recorded for another workspace",
		"later rebuild will complete it",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", reason, want)
		}
	}

	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("the row must exist after a successful register: %v", err)
	}
	if rec.ActualSession != nil {
		t.Fatalf("ActualSession = %q, want nil: the adoption failed", *rec.ActualSession)
	}

	// The half-written row is exactly the adopt case, so the next run
	// completes it rather than needing a new atomic primitive.
	second := h.applier().Apply(context.Background(), plan)

	if len(second.Conflicts) != 0 {
		t.Fatalf("second run Conflicts = %+v, want none", second.Conflicts)
	}
	if len(second.Registered) != 1 {
		t.Fatalf("second run Registered = %+v, want one", second.Registered)
	}
	rec, err = h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:desired" {
		t.Errorf("DesiredDigest = %v, want the digest written by the first run", rec.DesiredDigest)
	}
}

// The pre-lock gate validates the candidate's session, but the writes use
// the session the lock-held observation returned, and the observer matches
// on the workspace-ID tag alone (internal/tmux/decode.go:63-67). A session
// that acquired that tag with contradictory keys must be refused here too,
// exactly as planning re-checks ByIdentity (internal/controller/plan.go:87).
func TestApplyRefusesALockHeldSessionWhoseIdentityKeysContradictTheWorkspace(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	// This one passes the pre-lock gate. What comes back under the lock is
	// a different session carrying the same workspace ID and a stale slug.
	impostor := liveSession(ws, "impostor")
	impostor.Slug = "stale-slug"

	h := newHarness()
	h.know(ws, "sha256:desired")
	h.observer.results = []controller.SessionObservation{observing(impostor)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "stale-slug") {
		t.Errorf("Reason = %q, want it to name the contradictory key", report.Conflicts[0].Reason)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound: nothing may be written", err)
	}
	if h.locker.released != 1 {
		t.Errorf("lock released %d times, want 1 even on the conflict path", h.locker.released)
	}
}

// The race the lock-held re-classification exists to absorb: another
// rebuild, or an open, completed the workspace in the gap. The desired
// end state is confirmed to hold, so this is a no-op — not a refusal.
// Reporting a conflict here would map a verified ok to exit 6, which is
// the tri-state rule inverted.
func TestApplySettledUnderTheLockIsASilentNoOpNotAConflict(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	seedRecorded(t, h.fakeStore, ws)
	// Someone else adopted the very session this run was going to adopt.
	if err := h.fakeStore.AdoptSessionName(ws.ID, "projectmux", testTime); err != nil {
		t.Fatalf("seeding the settled row: %v", err)
	}
	counting := &countingStore{Store: h.fakeStore}
	h.store = counting
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none: the workspace is already in the desired state", report.Conflicts)
	}
	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none: this run wrote nothing", report.Registered)
	}
	if counting.registers != 0 || counting.adopts != 0 {
		t.Errorf("wrote %d registers, %d adopts; want 0 and 0", counting.registers, counting.adopts)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q left as it was", rec.ActualSession, "projectmux")
	}
	assertRecordedFieldsUntouched(t, rec, ws)
}

// The dry run must predict the real run's refusals, and a session that
// died between classification and the run is a refusal only the
// observation discovers. ObserveSession is read-only, so the preview can
// make it; only the lock is withheld.
func TestApplyDryRunPredictsTheRefusalForASessionThatDiedBeforeTheRun(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	newPlan := func() Plan {
		return Plan{Candidates: []Candidate{{Case: CaseRegister, Session: sess}}}
	}

	actual := newHarness()
	actual.know(ws, "sha256:desired")
	actual.observer.results = []controller.SessionObservation{{}}
	actualReport := actual.applier().Apply(context.Background(), newPlan())

	preview := newHarness()
	preview.know(ws, "sha256:desired")
	preview.observer.results = []controller.SessionObservation{{}}
	counting := &countingStore{Store: preview.fakeStore}
	preview.store = counting
	preview.dryRun = true
	previewReport := preview.applier().Apply(context.Background(), newPlan())

	if len(actualReport.Conflicts) != 1 {
		t.Fatalf("real run Conflicts = %+v, want exactly one", actualReport.Conflicts)
	}
	// Byte-identical, not merely both-refusals: the preview's whole job is
	// to be the verdict the operator would get, and a differently-worded
	// prediction is a different answer.
	if !reflect.DeepEqual(previewReport.Conflicts, actualReport.Conflicts) {
		t.Errorf("dry Conflicts = %+v, real = %+v; they must be identical",
			previewReport.Conflicts, actualReport.Conflicts)
	}
	if !reflect.DeepEqual(previewReport.Registered, actualReport.Registered) {
		t.Errorf("dry Registered = %+v, real = %+v; they must be identical",
			previewReport.Registered, actualReport.Registered)
	}
	if len(previewReport.Registered) != 0 {
		t.Errorf("dry Registered = %+v, want none: a dead session is not a recovery",
			previewReport.Registered)
	}
	if counting.registers != 0 || counting.adopts != 0 {
		t.Errorf("dry run wrote: %d registers, %d adopts; want 0 and 0",
			counting.registers, counting.adopts)
	}
	if len(preview.locker.locked) != 0 {
		t.Errorf("dry run locked %v, want nothing", preview.locker.locked)
	}
}
