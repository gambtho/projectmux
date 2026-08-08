package controller_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

func (r *ensureRig) stop(t *testing.T, d controller.Desired, opts controller.StopOptions) (controller.StopResult, error) {
	t.Helper()
	return r.ctrl.Stop(context.Background(), d, opts, r.lockDir, time.Second)
}

func registerStopFixture(t *testing.T, r *ensureRig) {
	t.Helper()
	if err := r.store.RegisterWorkspace(ensureWorkspace(), "sha256:x", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}
}

func TestStopKillsLiveSession(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab")))
	registerStopFixture(t, r)

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.SessionStopped || res.SessionName != "slab" {
		t.Errorf("result = %+v", res)
	}
	if len(r.actuator.Killed) != 1 || r.actuator.Killed[0] != "slab" {
		t.Errorf("Killed = %v", r.actuator.Killed)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Name != "stop" || op.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want stop/ok", op)
	}
}

func TestStopAbsentSessionIsIdempotentSuccess(t *testing.T) {
	r := newEnsureRig(t, absentStep())
	registerStopFixture(t, r)

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.SessionStopped {
		t.Errorf("result = %+v, want nothing stopped", res)
	}
	if len(r.actuator.Killed) != 0 {
		t.Errorf("Killed = %v, want none", r.actuator.Killed)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Name != "stop" || op.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want stop/ok", op)
	}
}

func TestStopRefusesOnUnknownSessionState(t *testing.T) {
	r := newEnsureRig(t, errorStep(errors.New("tmux exploded")))
	registerStopFixture(t, r)

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuator.Killed) != 0 {
		t.Error("uncertainty reached the kill actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Name != "stop" || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want stop/failed", op)
	}
}

func TestStopUnregisteredWritesNothing(t *testing.T) {
	r := newEnsureRig(t, errorStep(errors.New("tmux exploded")))

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if _, err := r.store.Workspace("w1"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("an unregistered stop wrote a record: %v", err)
	}
}

func TestStopUnregisteredStillKillsIdentitySession(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab")))

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.SessionStopped || len(r.actuator.Killed) != 1 {
		t.Errorf("result = %+v, Killed = %v", res, r.actuator.Killed)
	}
	if _, err := r.store.Workspace("w1"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("an unregistered stop wrote a record: %v", err)
	}
}

func TestStopRefusesContradictoryIdentity(t *testing.T) {
	foreign := controller.LiveSession{
		Name: "slab", WorkspaceID: "w1", Slug: "slab", Worktree: "/w/OTHER",
	}
	r := newEnsureRig(t, liveStep(foreign))
	registerStopFixture(t, r)

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuator.Killed) != 0 {
		t.Error("a contradictory session was killed")
	}
}

func TestStopKillFailureIsRecorded(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab")))
	registerStopFixture(t, r)
	r.actuator.KillErr = errors.New("kill exploded")

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	if err == nil {
		t.Fatal("a kill failure was swallowed")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want stop/failed", op)
	}
}

func TestStopContainerStopsBoundContainer(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)
	if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.ContainerStopped || res.ContainerID != "cid-1" {
		t.Errorf("result = %+v", res)
	}
	if len(r.actuatorC.Stopped) != 1 || r.actuatorC.Stopped[0] != "cid-1" {
		t.Errorf("Stopped = %v", r.actuatorC.Stopped)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.Container == nil || rec.Container.Health != state.HealthMissing ||
		rec.Container.ContainerID != "cid-1" {
		t.Errorf("binding = %+v, want retained id with missing health", rec.Container)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Name != "stop" || op.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want stop/ok", op)
	}
}

func TestStopContainerWithoutBindingIsSuccess(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if res.ContainerStopped || len(r.actuatorC.Stopped) != 0 {
		t.Errorf("result = %+v, Stopped = %v", res, r.actuatorC.Stopped)
	}
}

func TestStopContainerFailureReportsPartial(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)
	if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	r.actuatorC.StopErr = errors.New("docker stop exploded")

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	if err == nil {
		t.Fatal("a container-stop failure was swallowed")
	}
	if !res.SessionStopped {
		t.Error("the partial result must report the session kill that already happened")
	}
	if res.ContainerStopped {
		t.Error("the failed container stop was reported as stopped")
	}
	rec, _ := r.store.Workspace("w1")
	if rec.Container == nil || rec.Container.Health != state.HealthPresent {
		t.Errorf("binding = %+v; a failed stop must not record missing", rec.Container)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want stop/failed", op)
	}
}

func TestStopKillsByObservedSessionID(t *testing.T) {
	live := ownSession("slab")
	live.ID = "$7"
	r := newEnsureRig(t, liveStep(live))
	registerStopFixture(t, r)

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.SessionStopped || res.SessionName != "slab" {
		t.Errorf("result = %+v", res)
	}
	// The kill must bind to the observed session, not its reusable
	// name: a replacement session taking the name between observation
	// and kill has a different ID and survives.
	if len(r.actuator.Killed) != 1 || r.actuator.Killed[0] != "$7" {
		t.Errorf("Killed = %v, want the session ID", r.actuator.Killed)
	}
}

// registerSibling adds a second session on the same repository. The
// resolver cannot produce one yet, but the schema permits it and rebuild
// creates them, so the store is where a sibling comes from.
func registerSibling(t *testing.T, r *ensureRig) {
	t.Helper()
	sib := resolve.Workspace{
		ID: "w2", RepositoryID: "r1", Slug: "slab", RepoRoot: "/w/slab",
		Session: "feature-a", SessionName: "slab--feature-a",
	}
	if err := r.store.RegisterWorkspace(sib, "sha256:x", ensureTime); err != nil {
		t.Fatalf("register sibling: %v", err)
	}
	if _, err := r.store.AllocateSessionName("w2", ensureTime); err != nil {
		t.Fatalf("allocate sibling: %v", err)
	}
}

func siblingSession() controller.LiveSession {
	return controller.LiveSession{
		ID: "$9", Name: "slab--feature-a", WorkspaceID: "w2",
		Slug: "slab", Worktree: "/w/slab",
	}
}

func bindStopContainer(t *testing.T, r *ensureRig) {
	t.Helper()
	if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func TestStopContainerRefusesWhileSiblingIsLive(t *testing.T) {
	// One step only: the refusal must land before the workspace's own
	// session is ever observed, so nothing is destroyed.
	r := newEnsureRig(t, liveStep(siblingSession())).withContainerActuator()
	registerStopFixture(t, r)
	registerSibling(t, r)
	bindStopContainer(t, r)

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if !strings.Contains(refusal.Reason, "slab--feature-a") {
		t.Errorf("reason = %q, want the live sibling named", refusal.Reason)
	}
	if len(r.actuator.Killed) != 0 {
		t.Errorf("Killed = %v; a refusal must destroy nothing", r.actuator.Killed)
	}
	if len(r.actuatorC.Stopped) != 0 {
		t.Errorf("Stopped = %v; the shared container was killed anyway", r.actuatorC.Stopped)
	}
}

func TestStopContainerForceOverridesLiveSibling(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)
	registerSibling(t, r)
	bindStopContainer(t, r)

	// Force skips the sibling observation entirely: the user has already
	// answered the question it asks.
	res, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true, Force: true})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.ContainerStopped || res.ContainerID != "cid-1" {
		t.Errorf("result = %+v, want the container stopped", res)
	}
	if len(r.actuatorC.Stopped) != 1 || r.actuatorC.Stopped[0] != "cid-1" {
		t.Errorf("Stopped = %v", r.actuatorC.Stopped)
	}
}

func TestStopContainerRefusesWhenSiblingsCannotBeObserved(t *testing.T) {
	r := newEnsureRig(t, errorStep(errors.New("tmux exploded"))).withContainerActuator()
	registerStopFixture(t, r)
	registerSibling(t, r)
	bindStopContainer(t, r)

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuatorC.Stopped) != 0 {
		t.Error("uncertainty reached the container actuator")
	}
}

// lockProbingStopper tries to take the repository lock from inside
// StopContainer. Stop must still hold it there: if the sibling check
// released the lock before the stop, this acquire would succeed, and a
// sibling opening in that window would land in a container about to die.
type lockProbingStopper struct {
	*fake.ContainerActuator
	lockDir  string
	repoID   string
	probeErr error
}

func (a *lockProbingStopper) StopContainer(ctx context.Context, containerID string) error {
	l, err := lock.Acquire(ctx, a.lockDir, a.repoID, 10*time.Millisecond)
	if l != nil {
		_ = l.Release()
	}
	a.probeErr = err
	return a.ContainerActuator.StopContainer(ctx, containerID)
}

func TestStopHoldsTheRepositoryLockThroughTheContainerStop(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)
	bindStopContainer(t, r)
	probe := &lockProbingStopper{
		ContainerActuator: r.actuatorC, lockDir: r.lockDir, repoID: "r1",
	}
	r.ctrl.ContainerAct = probe

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.ContainerStopped {
		t.Fatalf("result = %+v, want the container stopped", res)
	}
	var held *lock.ErrLockHeld
	if !errors.As(probe.probeErr, &held) {
		t.Fatalf("acquiring the repository lock during the stop = %v, want *lock.ErrLockHeld; "+
			"the check and the stop must run under one continuous hold", probe.probeErr)
	}
	if held.Key != "r1" {
		t.Errorf("held key = %q, want the repository ID", held.Key)
	}
}
