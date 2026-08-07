package cli

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
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
	origExec, origSwitch, origSocket := execAttach, switchClient, currentSocket
	t.Cleanup(func() { execAttach, switchClient, currentSocket = origExec, origSwitch, origSocket })
	execAttach = func(session string) error {
		execCalls = append(execCalls, session)
		return nil
	}
	switchClient = func(_ context.Context, session string) error {
		switchCalls = append(switchCalls, session)
		return nil
	}

	currentSocket = func() string { return "" }
	if err := attachTerminal(context.Background(), "slab"); err != nil {
		t.Fatalf("attachTerminal: %v", err)
	}
	currentSocket = func() string { return tmux.SocketPath("") }
	if err := attachTerminal(context.Background(), "slab"); err != nil {
		t.Fatalf("attachTerminal: %v", err)
	}
	if len(execCalls) != 1 || len(switchCalls) != 1 {
		t.Errorf("execCalls = %v, switchCalls = %v; want one each", execCalls, switchCalls)
	}
}

// TestAttachTerminalRefusesAcrossServers covers the case the socket
// override exists for: a terminal attached to one tmux server while
// projectmux drives another. tmux cannot move a client between servers,
// so the refusal must come before any tmux command runs.
//
// attached is a socket path, as $TMUX carries it, because that is what
// distinguishes two servers — the last case is a client on a -S socket
// whose base name collides with the configured -L name.
func TestAttachTerminalRefusesAcrossServers(t *testing.T) {
	cases := []struct {
		name, env, attached string
		wantRefusal         bool
	}{
		{name: "default both ways", attached: tmux.SocketPath("")},
		{name: "override matches", env: "pmxvalidate",
			attached: tmux.SocketPath("pmxvalidate")},
		{name: "override, attached to default", env: "pmxvalidate",
			attached: tmux.SocketPath(""), wantRefusal: true},
		{name: "no override, attached elsewhere",
			attached: tmux.SocketPath("pmxvalidate"), wantRefusal: true},
		{name: "same base name, different directory", env: "pmxvalidate",
			attached: "/tmp/pmx-elsewhere/pmxvalidate", wantRefusal: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var execCalls, switchCalls []string
			origExec, origSwitch, origSocket := execAttach, switchClient, currentSocket
			t.Cleanup(func() {
				execAttach, switchClient, currentSocket = origExec, origSwitch, origSocket
			})
			execAttach = func(s string) error { execCalls = append(execCalls, s); return nil }
			switchClient = func(_ context.Context, s string) error {
				switchCalls = append(switchCalls, s)
				return nil
			}
			currentSocket = func() string { return tc.attached }
			t.Setenv(tmux.SocketEnv, tc.env)

			err := attachTerminal(context.Background(), "slab")
			var refusal *controller.RefusalError
			if got := errors.As(err, &refusal); got != tc.wantRefusal {
				t.Fatalf("refusal = %v (err %v), want %v", got, err, tc.wantRefusal)
			}
			if tc.wantRefusal {
				if len(execCalls) != 0 || len(switchCalls) != 0 {
					t.Errorf("refusal ran tmux anyway: exec %v, switch %v",
						execCalls, switchCalls)
				}
				for _, want := range []string{tc.attached, tmux.SocketPath(tc.env)} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("refusal %q does not name %q", err, want)
					}
				}
			}
		})
	}
}

// TestTmuxArgvAppliesSocket pins the hand-built argv the attach seams
// use, which cannot go through tmux.Client.
func TestTmuxArgvAppliesSocket(t *testing.T) {
	t.Setenv(tmux.SocketEnv, "")
	if got := tmuxArgv("attach-session", "-t", "=slab"); !slices.Equal(got,
		[]string{"tmux", "attach-session", "-t", "=slab"}) {
		t.Errorf("default socket argv = %v", got)
	}
	t.Setenv(tmux.SocketEnv, "pmxvalidate")
	if got := tmuxArgv("attach-session", "-t", "=slab"); !slices.Equal(got,
		[]string{"tmux", "-L", "pmxvalidate", "attach-session", "-t", "=slab"}) {
		t.Errorf("override argv = %v", got)
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
		if !reflect.DeepEqual(intents[i], want[i]) {
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

func TestWindowIntentsCarryPanes(t *testing.T) {
	agent := "claude"
	cmd := "tail -f dev.log"
	cwd := "services/api"
	cfg := config.Config{Windows: []config.Window{{
		Name:  "dev",
		Agent: &agent,
		Panes: []config.Pane{
			{Name: "shell", Shell: true},
			{Name: "logs", Command: &cmd, Cwd: &cwd, Focus: true},
		},
	}}}
	intents := windowIntents(cfg)
	panes := intents[0].Panes
	if len(panes) != 2 {
		t.Fatalf("panes = %+v", panes)
	}
	if panes[0].Command != "" || panes[0].Name != "shell" {
		t.Errorf("shell pane should map to an empty command, got %+v", panes[0])
	}
	if panes[1].Command != cmd || panes[1].RelDir != cwd || !panes[1].Focus {
		t.Errorf("command pane not mapped, got %+v", panes[1])
	}
}

func TestWindowIntentsImplicitWindowGetsDefaultPane(t *testing.T) {
	// Spec §3 exception: the implicit window lives outside the digest, so
	// its default pane is supplied here, at derivation.
	intents := windowIntents(config.Config{})
	if len(intents) != 1 || intents[0].Name != "shell" {
		t.Fatalf("intents = %+v", intents)
	}
	panes := intents[0].Panes
	if len(panes) != 1 || panes[0].Name != "shell" || panes[0].Command != "" {
		t.Errorf("implicit window should carry the default shell pane, got %+v", panes)
	}
}
