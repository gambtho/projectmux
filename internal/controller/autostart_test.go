package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/state"
)

func (r *ensureRig) startContainer(t *testing.T, d controller.Desired) (controller.ContainerStartOutcome, *controller.ContainerObservation, error) {
	t.Helper()
	return r.ctrl.StartWorkspaceContainer(context.Background(), d, r.lockDir, time.Second)
}

func TestStartWorkspaceContainerStarts(t *testing.T) {
	r := newEnsureRig(t).withContainerActuator()
	registerStopFixture(t, r)
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}

	outcome, obs, err := r.startContainer(t, containerDesired())
	if err != nil {
		t.Fatalf("StartWorkspaceContainer: %v", err)
	}
	if outcome != controller.ContainerStarted || obs == nil || obs.ContainerID != "cid-1" {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 1 {
		t.Errorf("Started = %v", r.actuatorC.Started)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.Container == nil || rec.Container.ContainerID != "cid-1" {
		t.Errorf("binding = %+v", rec.Container)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Name != "autostart" || op.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want autostart/ok", op)
	}
	// The whole pass is container-only: tmux must never be consulted.
	if len(r.sessions.queries) != 0 {
		t.Errorf("session observer was consulted: %v", r.sessions.queries)
	}
}

func TestStartWorkspaceContainerAlreadyRunning(t *testing.T) {
	r := newEnsureRig(t).withContainerActuator()
	registerStopFixture(t, r)
	if err := r.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		ProbeResult: controller.ContainerObservation{
			Health: state.HealthPresent, Kind: "devcontainer", ContainerID: "cid-1",
			ContainerUser: "vscode", Workdir: "/workspaces/slab",
		},
	}

	outcome, obs, err := r.startContainer(t, containerDesired())
	if err != nil {
		t.Fatalf("StartWorkspaceContainer: %v", err)
	}
	if outcome != controller.ContainerAlreadyRunning || obs == nil {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 0 {
		t.Errorf("an already-running container was started again: %v", r.actuatorC.Started)
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Name != "autostart" || op.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want autostart/ok", op)
	}
}

func TestStartWorkspaceContainerNoneApplies(t *testing.T) {
	r := newEnsureRig(t).withContainerActuator()
	registerStopFixture(t, r)
	r.ctrl.Containers = &fake.ContainerObserver{AppliesResult: false}
	d := containerDesired()
	d.Config.DevContainer.Enabled = "auto"

	outcome, obs, err := r.startContainer(t, d)
	if err != nil {
		t.Fatalf("StartWorkspaceContainer: %v", err)
	}
	if outcome != controller.ContainerNoneApplies || obs != nil {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 0 {
		t.Errorf("Started = %v, want none", r.actuatorC.Started)
	}
}

func TestStartWorkspaceContainerFailureRecordsAutostartOp(t *testing.T) {
	r := newEnsureRig(t).withContainerActuator()
	registerStopFixture(t, r)
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}
	r.actuatorC.StartErr = &controller.ContainerStartError{
		ExitCode: 7, Stderr: "boot boom", Reason: "devcontainer up exited 7",
	}

	_, _, err := r.startContainer(t, containerDesired())
	if err == nil {
		t.Fatal("a failing start was swallowed")
	}
	op := lastOp(t, r.store, "w1")
	if op == nil || op.Name != "autostart" || op.Outcome != state.OutcomeFailed {
		t.Fatalf("last operation = %+v, want autostart/failed", op)
	}
	if op.ExitStatus == nil || *op.ExitStatus != 7 {
		t.Errorf("ExitStatus = %v, want 7", op.ExitStatus)
	}
}

func TestStartWorkspaceContainerUnobservableFails(t *testing.T) {
	r := newEnsureRig(t).withContainerActuator()
	registerStopFixture(t, r)
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		DiscoverErr:   errors.New("docker down"),
	}

	_, _, err := r.startContainer(t, containerDesired())
	if err == nil {
		t.Fatal("an unobservable container start was swallowed")
	}
	if len(r.actuatorC.Started) != 0 {
		t.Error("uncertainty reached the container actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Name != "autostart" || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want autostart/failed", op)
	}
}
