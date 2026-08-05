// The ensure tests live in the external test package: they need the
// exported fakes, and controller/fake imports controller, so an internal
// test package would form an import cycle.
package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var ensureTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

// scriptedSessions returns one canned step per ObserveSession call, so a
// single Ensure can see different observations across its initial
// observation, squat check, and post-create confirmation.
type scriptedSessions struct {
	steps   []func(controller.SessionQuery) (controller.SessionObservation, error)
	queries []controller.SessionQuery
}

func (s *scriptedSessions) ObserveSession(_ context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	s.queries = append(s.queries, q)
	if len(s.steps) == 0 {
		return controller.SessionObservation{}, errors.New("scripted observer exhausted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step(q)
}

func absentStep() func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{}, nil
	}
}

func liveStep(s controller.LiveSession) func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{
			ByIdentity: &s, ByName: []controller.LiveSession{s},
		}, nil
	}
}

func errorStep(err error) func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{}, err
	}
}

func ensureWorkspace() resolve.Workspace {
	return resolve.Workspace{
		ID:          "w1",
		Slug:        "slab",
		Worktree:    "/w/slab",
		SessionName: "slab",
		IsPrimary:   true,
	}
}

func ensureDesired() controller.Desired {
	return controller.Desired{
		Workspace: ensureWorkspace(),
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: "false"},
			Environment:  map[string]string{"FOO": "bar"},
		},
		Digest: "sha256:desired",
	}
}

func ownSession(name string) controller.LiveSession {
	return controller.LiveSession{
		Name: name, WorkspaceID: "w1", Slug: "slab", Worktree: "/w/slab",
	}
}

type ensureRig struct {
	store    *fake.Store
	sessions *scriptedSessions
	actuator *fake.SessionActuator
	ctrl     *controller.Controller
	lockDir  string
}

func newEnsureRig(t *testing.T, steps ...func(controller.SessionQuery) (controller.SessionObservation, error)) *ensureRig {
	t.Helper()
	r := &ensureRig{
		store:    fake.NewStore(),
		sessions: &scriptedSessions{steps: steps},
		actuator: &fake.SessionActuator{},
		lockDir:  t.TempDir(),
	}
	r.ctrl = &controller.Controller{
		Store:      r.store,
		Sessions:   r.sessions,
		Containers: &fake.ContainerObserver{},
		Clock:      &fake.Clock{Time: ensureTime},
		Actuator:   r.actuator,
	}
	return r
}

func (r *ensureRig) ensure(t *testing.T, d controller.Desired) (controller.EnsureResult, error) {
	t.Helper()
	windows := []controller.WindowSpec{{Name: "shell", Dir: d.Workspace.Worktree}}
	return r.ctrl.Ensure(context.Background(), d, windows, r.lockDir, time.Second)
}

func lastOp(t *testing.T, s *fake.Store, id string) *state.Operation {
	t.Helper()
	rec, err := s.Workspace(id)
	if err != nil {
		t.Fatalf("Workspace(%s): %v", id, err)
	}
	return rec.LastOperation
}

func TestEnsureCreates(t *testing.T) {
	r := newEnsureRig(t,
		absentStep(),                 // initial observation
		absentStep(),                 // allocated-name squat check
		liveStep(ownSession("slab")), // post-create confirmation
	)
	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureCreated || res.Session != "slab" || res.Drifted {
		t.Errorf("result = %+v", res)
	}
	if len(r.actuator.Created) != 1 {
		t.Fatalf("actuator calls = %d, want 1", len(r.actuator.Created))
	}
	spec := r.actuator.Created[0]
	if spec.Name != "slab" || spec.WorkspaceID != "w1" || spec.Env["FOO"] != "bar" {
		t.Errorf("actuated spec = %+v", spec)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.AppliedDigest == nil || *rec.AppliedDigest != "sha256:desired" {
		t.Errorf("AppliedDigest = %v, want the desired digest", rec.AppliedDigest)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want open/ok", op)
	}
}

func TestEnsureAlreadyRunningReportsDrift(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab")))
	if err := r.store.RegisterWorkspace(ensureWorkspace(), "sha256:desired", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}

	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning || res.Session != "slab" {
		t.Errorf("result = %+v", res)
	}
	if !res.Drifted {
		t.Error("Drifted = false; no applied digest exists")
	}
	if len(r.actuator.Created) != 0 {
		t.Errorf("actuator was called on an already-running session")
	}
}

func TestEnsureAdoptsAndRecordsTheLiveName(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab--phase1")))
	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureAdopted || res.Session != "slab--phase1" {
		t.Errorf("result = %+v", res)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.ActualSession == nil || *rec.ActualSession != "slab--phase1" {
		t.Errorf("ActualSession = %v", rec.ActualSession)
	}
	if rec.AppliedDigest != nil {
		t.Error("adoption recorded an applied digest; drift must stay honest")
	}
	if len(r.actuator.Created) != 0 {
		t.Error("actuator was called during adoption")
	}
}

func TestEnsureAdoptConflictRefuses(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab")))
	other := resolve.Workspace{
		ID: "w2", Slug: "other", Worktree: "/w/other", SessionName: "other",
	}
	if err := r.store.RegisterWorkspace(other, "sha256:x", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := r.store.AdoptSessionName("w2", "slab", ensureTime); err != nil {
		t.Fatalf("seed conflict: %v", err)
	}

	_, err := r.ensure(t, ensureDesired())
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureRefusesOnUnknownSessionState(t *testing.T) {
	r := newEnsureRig(t, errorStep(errors.New("tmux exploded")))
	_, err := r.ensure(t, ensureDesired())
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a refusing plan reached the actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureContainerGateFiresBeforeActuation(t *testing.T) {
	r := newEnsureRig(t, absentStep())
	r.ctrl.Containers = &fake.ContainerObserver{DiscoverErr: errors.New("no adapter")}
	d := ensureDesired()
	d.Config.DevContainer.Enabled = "auto"

	_, err := r.ensure(t, d)
	if !errors.Is(err, controller.ErrContainerActionUnsupported) {
		t.Fatalf("err = %v, want ErrContainerActionUnsupported", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("the container gate did not fire before actuation")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureSquatOnAllocatedNameRefuses(t *testing.T) {
	foreign := controller.LiveSession{
		Name: "slab", WorkspaceID: "w9", Slug: "elsewhere", Worktree: "/w/x",
	}
	r := newEnsureRig(t,
		absentStep(),
		func(controller.SessionQuery) (controller.SessionObservation, error) {
			return controller.SessionObservation{ByName: []controller.LiveSession{foreign}}, nil
		},
	)
	_, err := r.ensure(t, ensureDesired())
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("creation proceeded over a squatted allocation")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureCreationFailureIsRecorded(t *testing.T) {
	r := newEnsureRig(t, absentStep(), absentStep())
	r.actuator.Err = errors.New("tmux said no")
	_, err := r.ensure(t, ensureDesired())
	if err == nil {
		t.Fatal("Ensure succeeded despite a failing actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsurePostCreateConfirmationRejects(t *testing.T) {
	mismatched := controller.LiveSession{
		Name: "slab", WorkspaceID: "w1", Slug: "slab", Worktree: "/w/OTHER",
	}
	wrongName := ownSession("slab-2")
	cases := map[string]func(controller.SessionQuery) (controller.SessionObservation, error){
		"unknown after create": errorStep(errors.New("tmux vanished")),
		"absent after create":  absentStep(),
		"contradictory keys":   liveStep(mismatched),
		"wrong name":           liveStep(wrongName),
	}
	for label, confirm := range cases {
		t.Run(label, func(t *testing.T) {
			r := newEnsureRig(t, absentStep(), absentStep(), confirm)
			_, err := r.ensure(t, ensureDesired())
			if err == nil {
				t.Fatal("unconfirmed creation was committed")
			}
			rec, _ := r.store.Workspace("w1")
			if rec.AppliedDigest != nil {
				t.Error("an unconfirmed creation recorded the applied digest")
			}
			if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
				t.Errorf("last operation = %+v, want open/failed", op)
			}
		})
	}
}

// workspaceFailsOnCall wraps a *fake.Store and makes exactly its Nth
// Workspace call fail with a non-ErrNotFound error, so Observe's own
// "reading stored state" error return (as opposed to session/container
// uncertainty) can be exercised deterministically: the post-create
// confirmation Observe is the store's second Workspace call in an
// Ensure that creates.
type workspaceFailsOnCall struct {
	*fake.Store
	failOnCall int
	calls      int
}

func (s *workspaceFailsOnCall) Workspace(id string) (state.Record, error) {
	s.calls++
	if s.calls == s.failOnCall {
		return state.Record{}, errors.New("store read failed")
	}
	return s.Store.Workspace(id)
}

func TestEnsurePostCreateConfirmationObserveErrorIsRecorded(t *testing.T) {
	r := newEnsureRig(t, absentStep(), absentStep())
	store := &workspaceFailsOnCall{Store: r.store, failOnCall: 2}
	r.ctrl.Store = store

	_, err := r.ensure(t, ensureDesired())
	if err == nil {
		t.Fatal("Ensure succeeded despite a failing confirmation Observe")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureConvergesAfterCrashBetweenCreateAndCommit(t *testing.T) {
	r := newEnsureRig(t, absentStep(), absentStep(), absentStep())
	if _, err := r.ensure(t, ensureDesired()); err == nil {
		t.Fatal("expected the unconfirmed first Ensure to fail")
	}
	// The allocation persisted; the session exists now (the crash was
	// after create). The next Ensure converges without recreating.
	r.sessions.steps = []func(controller.SessionQuery) (controller.SessionObservation, error){
		liveStep(ownSession("slab")),
	}
	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("recovery Ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning || res.Session != "slab" {
		t.Errorf("recovery result = %+v", res)
	}
	if !res.Drifted {
		t.Error("recovery cleared drift without applying configuration")
	}
	if calls := len(r.actuator.Created); calls != 1 {
		t.Errorf("actuator calls across both runs = %d, want 1", calls)
	}
}

func TestEnsureRespectsTheWorkspaceLock(t *testing.T) {
	r := newEnsureRig(t, absentStep(), absentStep(), liveStep(ownSession("slab")))
	held, err := lock.Acquire(context.Background(), r.lockDir, "w1", time.Second)
	if err != nil {
		t.Fatalf("pre-acquiring the lock: %v", err)
	}
	defer held.Release()

	_, err = r.ctrl.Ensure(context.Background(), ensureDesired(),
		[]controller.WindowSpec{{Name: "shell", Dir: "/w/slab"}},
		r.lockDir, 200*time.Millisecond)
	var lockErr *lock.ErrLockHeld
	if !errors.As(err, &lockErr) {
		t.Fatalf("err = %v, want *lock.ErrLockHeld", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a locked-out Ensure reached the actuator")
	}
}
