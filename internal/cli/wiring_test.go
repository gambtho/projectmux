package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var cliTestTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

// guardedStore fails the test if an observation command mutates the
// store: design §8/§12 — list and status must not mutate workspaces.
type guardedStore struct {
	*fake.Store
	t *testing.T
}

func (g guardedStore) Close() error { return nil }

func (g guardedStore) forbidden(method string) error {
	g.t.Errorf("observation command called %s", method)
	return errors.New("observation commands must not mutate the store")
}

func (g guardedStore) RegisterWorkspace(resolve.Workspace, string, time.Time) error {
	return g.forbidden("RegisterWorkspace")
}

func (g guardedStore) AllocateSessionName(string, time.Time) (string, error) {
	return "", g.forbidden("AllocateSessionName")
}

func (g guardedStore) RecordContainerObservation(string, state.ContainerObservation, time.Time) error {
	return g.forbidden("RecordContainerObservation")
}

func (g guardedStore) RecordOperation(string, state.Operation, time.Time) error {
	return g.forbidden("RecordOperation")
}

func (g guardedStore) CommitReconciliation(string, state.ReconciliationResult, time.Time) error {
	return g.forbidden("CommitReconciliation")
}

func (g guardedStore) AdoptSessionName(string, string, time.Time) error {
	return g.forbidden("AdoptSessionName")
}

func installFakeStore(t *testing.T, s *fake.Store) {
	t.Helper()
	orig := openStore
	t.Cleanup(func() { openStore = orig })
	openStore = func() (stateStore, error) {
		return guardedStore{Store: s, t: t}, nil
	}
}

func installLiveSessions(t *testing.T, sessions []controller.LiveSession, err error) {
	t.Helper()
	orig := liveSessions
	t.Cleanup(func() { liveSessions = orig })
	liveSessions = func(context.Context) ([]controller.LiveSession, error) {
		return sessions, err
	}
}

func installSessionObserver(t *testing.T, obs controller.SessionObservation, err error) {
	t.Helper()
	orig := newSessionObserver
	t.Cleanup(func() { newSessionObserver = orig })
	newSessionObserver = func() controller.SessionObserver {
		return &fake.SessionObserver{Observation: obs, Err: err}
	}
}

func TestStoredContainerRendersTimestampsUTC(t *testing.T) {
	info := storedContainer(&state.ContainerBinding{
		Kind:        "devcontainer",
		ContainerID: "c1",
		Health:      state.HealthMissing,
		ObservedAt:  cliTestTime,
	})
	if info.Health != "missing" {
		t.Errorf("Health = %q, want missing", info.Health)
	}
	if info.ObservedAt != "2026-08-05T12:00:00Z" {
		t.Errorf("ObservedAt = %q", info.ObservedAt)
	}
	if storedContainer(nil) != nil {
		t.Error("storedContainer(nil) should be nil")
	}
}

func strPtr(s string) *string { return &s }

func TestAttachTerminalChoosesByTmuxEnv(t *testing.T) {
	var execCalls, switchCalls []string
	origExec, origSwitch, origInside := execAttach, switchClient, insideTmux
	t.Cleanup(func() { execAttach, switchClient, insideTmux = origExec, origSwitch, origInside })
	execAttach = func(session string) error {
		execCalls = append(execCalls, session)
		return nil
	}
	switchClient = func(_ context.Context, session string) error {
		switchCalls = append(switchCalls, session)
		return nil
	}

	insideTmux = func() bool { return false }
	if err := attachTerminal(context.Background(), "slab"); err != nil {
		t.Fatalf("attachTerminal: %v", err)
	}
	insideTmux = func() bool { return true }
	if err := attachTerminal(context.Background(), "slab"); err != nil {
		t.Fatalf("attachTerminal: %v", err)
	}
	if len(execCalls) != 1 || len(switchCalls) != 1 {
		t.Errorf("execCalls = %v, switchCalls = %v; want one each", execCalls, switchCalls)
	}
}

func TestRefusalErrorMapsToExitRefused(t *testing.T) {
	if got := exitCode(&controller.RefusalError{Reason: "nope"}); got != ExitRefused {
		t.Errorf("exitCode(RefusalError) = %d, want %d", got, ExitRefused)
	}
	if ExitRefused != 6 {
		t.Errorf("ExitRefused = %d, want 6", ExitRefused)
	}
}

func installContainerObserver(t *testing.T, o controller.ContainerObserver) {
	t.Helper()
	orig := newContainerObserver
	t.Cleanup(func() { newContainerObserver = orig })
	newContainerObserver = func() controller.ContainerObserver { return o }
}

func installContainerActuator(t *testing.T) *fake.ContainerActuator {
	t.Helper()
	orig := newContainerActuator
	t.Cleanup(func() { newContainerActuator = orig })
	a := &fake.ContainerActuator{
		StartResult: controller.ContainerObservation{
			Kind: "devcontainer", ContainerID: "cid-1",
			ContainerUser: "vscode", Workdir: "/workspaces/slabledger",
			Health: state.HealthPresent,
		},
	}
	newContainerActuator = func() controller.ContainerActuator { return a }
	return a
}

func TestWindowIntentsDerivation(t *testing.T) {
	cfg := config.Config{
		Windows: []config.Window{
			{Name: "agent-1", Agent: strPtr("claude"), Focus: true},
			{Name: "build", Command: strPtr("make watch"), Cwd: strPtr("sub")},
			{Name: "shell", Shell: true, Location: strPtr("container")},
		},
	}
	intents := windowIntents(cfg)
	want := []controller.WindowIntent{
		{Name: "agent-1", Command: "claude", Focus: true},
		{Name: "build", Command: "make watch", RelDir: "sub"},
		{Name: "shell", Location: controller.WindowContainer},
	}
	if len(intents) != len(want) {
		t.Fatalf("intents = %+v", intents)
	}
	for i := range want {
		if intents[i] != want[i] {
			t.Errorf("intent %d = %+v, want %+v", i, intents[i], want[i])
		}
	}
}

func TestWindowIntentsImplicitShellWindow(t *testing.T) {
	intents := windowIntents(config.Config{})
	if len(intents) != 1 || intents[0].Name != "shell" || intents[0].Command != "" ||
		intents[0].Location != controller.WindowAuto {
		t.Errorf("intents = %+v, want one implicit auto shell window", intents)
	}
}
