// The ensure tests live in the external test package: they need the
// exported fakes, and controller/fake imports controller, so an internal
// test package would form an import cycle.
package controller_test

import (
	"context"
	"errors"
	"strings"
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

// scriptedContainer returns one canned probe or discover result per call,
// so a probe-first retry (which re-observes the same kind that failed) can
// be scripted to succeed on the second call — fake.ContainerObserver
// returns the same canned result every call, which cannot express that.
type scriptedContainer struct {
	probeSteps    []func(state.ContainerBinding) (controller.ContainerObservation, error)
	discoverSteps []func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error)
}

func (s *scriptedContainer) Applies(context.Context, resolve.Workspace, config.Config) (bool, error) {
	return true, nil
}

func (s *scriptedContainer) ProbeContainer(_ context.Context, b state.ContainerBinding) (controller.ContainerObservation, error) {
	if len(s.probeSteps) == 0 {
		return controller.ContainerObservation{}, errors.New("scripted probe exhausted")
	}
	step := s.probeSteps[0]
	s.probeSteps = s.probeSteps[1:]
	return step(b)
}

func (s *scriptedContainer) DiscoverContainer(_ context.Context, ws resolve.Workspace, cfg config.Config) (*controller.ContainerObservation, error) {
	if len(s.discoverSteps) == 0 {
		return nil, errors.New("scripted discover exhausted")
	}
	step := s.discoverSteps[0]
	s.discoverSteps = s.discoverSteps[1:]
	return step(ws, cfg)
}

func probeErr(err error) func(state.ContainerBinding) (controller.ContainerObservation, error) {
	return func(state.ContainerBinding) (controller.ContainerObservation, error) {
		return controller.ContainerObservation{}, err
	}
}

func probeOK(obs controller.ContainerObservation) func(state.ContainerBinding) (controller.ContainerObservation, error) {
	return func(state.ContainerBinding) (controller.ContainerObservation, error) { return obs, nil }
}

func discoverErr(err error) func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error) {
	return func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error) { return nil, err }
}

func discoverOK(obs *controller.ContainerObservation) func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error) {
	return func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error) { return obs, nil }
}

func ensureWorkspace() resolve.Workspace {
	return resolve.Workspace{
		ID:           "w1",
		RepositoryID: "r1",
		Slug:         "slab",
		RepoRoot:     "/w/slab",
		SessionName:  "slab",
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
	store     *fake.Store
	sessions  *scriptedSessions
	actuator  *fake.SessionActuator
	actuatorC *fake.ContainerActuator
	ctrl      *controller.Controller
	lockDir   string
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
		Containers: &fake.ContainerObserver{AppliesResult: true},
		Clock:      &fake.Clock{Time: ensureTime},
		Actuator:   r.actuator,
	}
	return r
}

func (r *ensureRig) ensure(t *testing.T, d controller.Desired) (controller.EnsureResult, error) {
	t.Helper()
	intents := []controller.WindowIntent{{Name: "shell"}}
	return r.ctrl.Ensure(context.Background(), d, intents, r.lockDir, time.Second)
}

func (r *ensureRig) withContainerActuator() *ensureRig {
	r.actuatorC = &fake.ContainerActuator{
		StartResult: controller.ContainerObservation{
			Kind: "devcontainer", ContainerID: "cid-1",
			ContainerUser: "vscode", Workdir: "/workspaces/slab",
			Health: state.HealthPresent,
		},
	}
	r.ctrl.ContainerAct = r.actuatorC
	return r
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
		ID: "w2", Slug: "other", RepoRoot: "/w/other", SessionName: "other",
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
	// A persistently unobservable container (probe-first, then the retry
	// discover also fails) fails Ensure before any actuation — with or
	// without a container actuator wired.
	r := newEnsureRig(t, absentStep())
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		DiscoverErr:   errors.New("no adapter"),
	}
	d := ensureDesired()
	d.Config.DevContainer.Enabled = "auto"

	_, err := r.ensure(t, d)
	if err == nil || !strings.Contains(err.Error(), "re-observing the container") {
		t.Fatalf("err = %v, want the container re-observation failure", err)
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

func TestEnsureAcceptsPartialSessionAfterFailedCreation(t *testing.T) {
	// A failed chained creation aborts mid-chain but leaves the session
	// alive with its identity keys set (they come right after
	// new-session). The next ensure must accept it as already-running
	// with the drift flag set — never repair or recreate (spec §4).
	r := newEnsureRig(t, absentStep(), absentStep())
	r.actuator.Err = errors.New("tmux new-session exited 1: boom")

	if _, err := r.ensure(t, ensureDesired()); err == nil {
		t.Fatal("first ensure should fail with the actuator error")
	}
	rec, _ := r.store.Workspace("w1")
	if rec.AppliedDigest != nil {
		t.Error("a failed creation must not commit an applied digest")
	}

	// The chain died after identity was set: simulate the surviving
	// partial session in the observer.
	r.actuator.Err = nil
	r.sessions.steps = []func(controller.SessionQuery) (controller.SessionObservation, error){
		liveStep(ownSession("slab")),
	}

	res, err := r.ensure(t, ensureDesired())
	if err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning {
		t.Errorf("action = %v, want already-running (accepted, not repaired)", res.Action)
	}
	if !res.Drifted {
		t.Error("a partial session with no committed digest must report drift")
	}
	if calls := len(r.actuator.Created); calls != 1 {
		t.Errorf("actuator calls across both runs = %d, want 1 (the failed attempt only; no repair/recreate)", calls)
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
	defer func() { _ = held.Release() }()

	_, err = r.ctrl.Ensure(context.Background(), ensureDesired(),
		[]controller.WindowIntent{{Name: "shell"}},
		r.lockDir, 200*time.Millisecond)
	var lockErr *lock.ErrLockHeld
	if !errors.As(err, &lockErr) {
		t.Fatalf("err = %v, want *lock.ErrLockHeld", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a locked-out Ensure reached the actuator")
	}
}

func containerDesired() controller.Desired {
	d := ensureDesired()
	d.Config.DevContainer.Enabled = "true"
	d.Config.Environment = map[string]string{"FOO": "bar"}
	return d
}

func TestEnsureStartsContainerAndRendersContainerWindows(t *testing.T) {
	r := newEnsureRig(t,
		absentStep(), absentStep(), liveStep(ownSession("slab")),
	).withContainerActuator()
	// enabled true, no binding: Discover reports bare missing.
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}

	d := containerDesired()
	intents := []controller.WindowIntent{
		{Name: "agent-1", Command: "claude", Focus: true}, // auto => container
		{Name: "logs", Command: "tail -f log", RelDir: "sub", Location: controller.WindowContainer},
		{Name: "host-shell", Location: controller.WindowHost},
	}
	res, err := r.ctrl.Ensure(context.Background(), d, intents, r.lockDir, time.Second)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(r.actuatorC.Started) != 1 {
		t.Fatalf("StartContainer calls = %d, want 1", len(r.actuatorC.Started))
	}
	if res.Container == nil || res.Container.ContainerID != "cid-1" {
		t.Errorf("result container = %+v", res.Container)
	}
	spec := r.actuator.Created[0]
	if got := spec.Windows[0].Command; got != `fake-exec cid-1 /workspaces/slab "claude" env=1` {
		t.Errorf("auto window command = %q", got)
	}
	if got := spec.Windows[1].Command; got != `fake-exec cid-1 /workspaces/slab/sub "tail -f log" env=1` {
		t.Errorf("container window command = %q", got)
	}
	if got := spec.Windows[2].Command; got != "" {
		t.Errorf("host window command = %q, want empty (shell)", got)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.Container == nil || rec.Container.ContainerID != "cid-1" ||
		rec.Container.ContainerUser != "vscode" {
		t.Errorf("committed binding = %+v", rec.Container)
	}
	if res.ContainerWindowsStale {
		t.Error("a fresh creation is never stale")
	}
}

func TestEnsureAcquireRunsIdempotentStart(t *testing.T) {
	r := newEnsureRig(t,
		absentStep(), absentStep(), liveStep(ownSession("slab")),
	).withContainerActuator()
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		DiscoverResult: &controller.ContainerObservation{
			Health: state.HealthPresent, Kind: "devcontainer", ContainerID: "cid-1",
			// No Workdir: the acquire shape.
		},
	}
	res, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(r.actuatorC.Started) != 1 {
		t.Errorf("acquire did not run the idempotent start (calls = %d)", len(r.actuatorC.Started))
	}
	if res.ContainerWindowsStale {
		t.Error("acquire onto an already-running container must not report stale container windows")
	}
}

func TestEnsureStartFailurePersistsExitStatus(t *testing.T) {
	r := newEnsureRig(t, absentStep()).withContainerActuator()
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}
	r.actuatorC.StartErr = &controller.ContainerStartError{
		ExitCode: 47, Stderr: "build exploded", Reason: "devcontainer up exited 47",
	}

	_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
	if err == nil {
		t.Fatal("Ensure succeeded despite a failing start")
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a failed container start reached the session actuator")
	}
	op := lastOp(t, r.store, "w1")
	if op == nil || op.Outcome != state.OutcomeFailed {
		t.Fatalf("last operation = %+v", op)
	}
	if op.ExitStatus == nil || *op.ExitStatus != 47 {
		t.Errorf("ExitStatus = %v, want 47 (design §9)", op.ExitStatus)
	}
	if !strings.Contains(op.ErrorSummary, "build exploded") {
		t.Errorf("summary %q lacks the stderr", op.ErrorSummary)
	}
}

func TestEnsureProbeFirstRetriesBoundAndUnbound(t *testing.T) {
	t.Run("bound retries probe", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		if err := r.store.RegisterWorkspace(containerDesired().Workspace, "sha256:x", ensureTime); err != nil {
			t.Fatal(err)
		}
		if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
			t.Fatal(err)
		}
		if err := r.store.RecordContainerObservation("w1", state.ContainerObservation{
			Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
			Workdir: "/workspaces/slab", Health: state.HealthPresent,
		}, ensureTime); err != nil {
			t.Fatal(err)
		}
		// First probe (inside Observe) errors -> probe-first; fake.ContainerObserver
		// returns the same canned ProbeErr on every call, so the retry fails
		// again too. TestEnsureProbeFirstRetrySucceeds covers a retry that
		// resolves, using scriptedContainer to vary the result per call.
		obs := &fake.ContainerObserver{
			AppliesResult: true,
			ProbeErr:      errors.New("docker hiccup"),
		}
		r.ctrl.Containers = obs

		_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
		if err == nil {
			t.Fatal("Ensure succeeded with an unobservable container")
		}
		if len(r.actuator.Created) != 0 {
			t.Error("uncertainty reached the session actuator")
		}
		if got := len(obs.Probed); got != 2 {
			t.Errorf("probe calls = %d, want 2 (observe + one retry)", got)
		}
	})

	t.Run("unbound retries discover", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		obs := &fake.ContainerObserver{
			AppliesResult: true,
			DiscoverErr:   errors.New("docker hiccup"),
		}
		r.ctrl.Containers = obs
		_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
		if err == nil {
			t.Fatal("Ensure succeeded with an unobservable container")
		}
		if got := len(obs.Discovered); got != 2 {
			t.Errorf("discover calls = %d, want 2 (observe + one retry)", got)
		}
	})
}

// TestEnsureProbeFirstRetrySucceeds covers the spec §4 retry outcomes when
// the retried observation succeeds, as opposed to
// TestEnsureProbeFirstRetriesBoundAndUnbound, which only covers a retry
// that fails again. Each case's initial observation errors (forcing
// probe-first) and the retry then resolves per the case; the session is
// already registered and allocated under its live name so the session
// action is none and the container phase is isolated.
func TestEnsureProbeFirstRetrySucceeds(t *testing.T) {
	registerLive := func(t *testing.T, r *ensureRig) {
		t.Helper()
		if err := r.store.RegisterWorkspace(containerDesired().Workspace, "sha256:x", ensureTime); err != nil {
			t.Fatal(err)
		}
		if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("bound retry probe returns present with full binding", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		registerLive(t, r)
		if err := r.store.RecordContainerObservation("w1", state.ContainerObservation{
			Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
			Workdir: "/workspaces/slab", Health: state.HealthPresent,
		}, ensureTime); err != nil {
			t.Fatal(err)
		}
		sc := &scriptedContainer{probeSteps: []func(state.ContainerBinding) (controller.ContainerObservation, error){
			probeErr(errors.New("docker hiccup")),
			probeOK(controller.ContainerObservation{
				Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
				Workdir: "/workspaces/slab", Health: state.HealthPresent,
			}),
		}}
		r.ctrl.Containers = sc

		res, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if len(r.actuatorC.Started) != 0 {
			t.Errorf("StartContainer calls = %d, want 0 (already present and bound)", len(r.actuatorC.Started))
		}
		if res.Container == nil || res.Container.ContainerID != "cid-1" || res.Container.Workdir != "/workspaces/slab" {
			t.Errorf("result container = %+v", res.Container)
		}
		rec, _ := r.store.Workspace("w1")
		if rec.Container == nil || rec.Container.ContainerID != "cid-1" || rec.Container.Workdir != "/workspaces/slab" {
			t.Errorf("committed binding = %+v", rec.Container)
		}
	})

	t.Run("unbound retry discover returns present incomplete", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		registerLive(t, r)
		sc := &scriptedContainer{discoverSteps: []func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error){
			discoverErr(errors.New("docker hiccup")),
			discoverOK(&controller.ContainerObservation{
				Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
				// No Workdir: the discovery-acquire shape.
			}),
		}}
		r.ctrl.Containers = sc

		if _, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if len(r.actuatorC.Started) != 1 {
			t.Errorf("StartContainer calls = %d, want 1 (acquire)", len(r.actuatorC.Started))
		}
	})

	t.Run("unbound retry discover returns missing", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		registerLive(t, r)
		sc := &scriptedContainer{discoverSteps: []func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error){
			discoverErr(errors.New("docker hiccup")),
			discoverOK(&controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"}),
		}}
		r.ctrl.Containers = sc

		if _, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if len(r.actuatorC.Started) != 1 {
			t.Errorf("StartContainer calls = %d, want 1 (start)", len(r.actuatorC.Started))
		}
	})

	t.Run("unbound retry discover returns none", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		registerLive(t, r)
		sc := &scriptedContainer{discoverSteps: []func(resolve.Workspace, config.Config) (*controller.ContainerObservation, error){
			discoverErr(errors.New("docker hiccup")),
			discoverOK(nil),
		}}
		r.ctrl.Containers = sc

		res, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
		if err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		if len(r.actuatorC.Started) != 0 {
			t.Errorf("StartContainer calls = %d, want 0 (no container applies)", len(r.actuatorC.Started))
		}
		if res.Container != nil {
			t.Errorf("result container = %+v, want nil", res.Container)
		}
	})
}

func TestEnsureContainerWindowWithoutContainerFails(t *testing.T) {
	r := newEnsureRig(t, absentStep()).withContainerActuator()
	// auto that resolves to none.
	r.ctrl.Containers = &fake.ContainerObserver{AppliesResult: false}
	d := containerDesired()
	d.Config.DevContainer.Enabled = "auto"

	_, err := r.ctrl.Ensure(context.Background(), d,
		[]controller.WindowIntent{{Name: "agent-1", Command: "claude", Location: controller.WindowContainer}},
		r.lockDir, time.Second)
	var cw *controller.ContainerWindowError
	if !errors.As(err, &cw) {
		t.Fatalf("err = %v, want *ContainerWindowError", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("the failing window demand reached the session actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureReplacementIntoLiveSessionIsStale(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	if err := r.store.RegisterWorkspace(containerDesired().Workspace, "sha256:x", ensureTime); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
		t.Fatal(err)
	}
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}

	res, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "agent-1", Command: "claude"}}, r.lockDir, time.Second)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning {
		t.Fatalf("action = %v", res.Action)
	}
	if !res.ContainerWindowsStale {
		t.Error("a container started into a live session must report stale container windows")
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a live session was re-created")
	}
}

func TestEnsureNilActuatorStillRefusesContainerActions(t *testing.T) {
	r := newEnsureRig(t, absentStep())
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}
	_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
	if !errors.Is(err, controller.ErrContainerActionUnsupported) {
		t.Fatalf("err = %v, want ErrContainerActionUnsupported", err)
	}
}
