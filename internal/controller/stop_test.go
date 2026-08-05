package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

func (r *ensureRig) stop(t *testing.T, d controller.Desired, stopContainer bool) (controller.StopResult, error) {
	t.Helper()
	return r.ctrl.Stop(context.Background(), d, stopContainer, r.lockDir, time.Second)
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

	res, err := r.stop(t, ensureDesired(), false)
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

	res, err := r.stop(t, ensureDesired(), false)
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

	_, err := r.stop(t, ensureDesired(), false)
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

	_, err := r.stop(t, ensureDesired(), false)
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

	res, err := r.stop(t, ensureDesired(), false)
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

	_, err := r.stop(t, ensureDesired(), false)
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

	_, err := r.stop(t, ensureDesired(), false)
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
	if err := r.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}

	res, err := r.stop(t, ensureDesired(), true)
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

	res, err := r.stop(t, ensureDesired(), true)
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
	if err := r.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	r.actuatorC.StopErr = errors.New("docker stop exploded")

	res, err := r.stop(t, ensureDesired(), true)
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

	res, err := r.stop(t, ensureDesired(), false)
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
