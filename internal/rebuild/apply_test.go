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
	h.observer.results = []controller.SessionObservation{observing(good)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{
			{Case: CaseRegister, Session: liveSession(other, "other")},
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
