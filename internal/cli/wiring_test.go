package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestUnprobedObserverAlwaysFails(t *testing.T) {
	var obs controller.ContainerObserver = unprobedObserver{}
	if _, err := obs.ProbeContainer(context.Background(), state.ContainerBinding{}); err == nil {
		t.Error("ProbeContainer pretended to probe")
	}
	if _, err := obs.DiscoverContainer(context.Background(), resolve.Workspace{},
		config.Config{Version: 1}); err == nil {
		t.Error("DiscoverContainer pretended to discover")
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

func TestWindowSpecsDerivation(t *testing.T) {
	cfg := config.Config{
		Windows: []config.Window{
			{Name: "agent-1", Agent: strPtr("claude"), Focus: true},
			{Name: "build", Command: strPtr("make watch"), Cwd: strPtr("sub")},
			{Name: "shell", Shell: true},
		},
	}
	specs, err := windowSpecs(cfg, "/w/slab")
	if err != nil {
		t.Fatalf("windowSpecs: %v", err)
	}
	want := []controller.WindowSpec{
		{Name: "agent-1", Command: "claude", Dir: "/w/slab", Focus: true},
		{Name: "build", Command: "make watch", Dir: "/w/slab/sub"},
		{Name: "shell", Dir: "/w/slab"},
	}
	if len(specs) != len(want) {
		t.Fatalf("specs = %+v", specs)
	}
	for i := range want {
		if specs[i] != want[i] {
			t.Errorf("spec %d = %+v, want %+v", i, specs[i], want[i])
		}
	}
}

func TestWindowSpecsImplicitShellWindow(t *testing.T) {
	specs, err := windowSpecs(config.Config{}, "/w/slab")
	if err != nil {
		t.Fatalf("windowSpecs: %v", err)
	}
	if len(specs) != 1 || specs[0].Name != "shell" || specs[0].Dir != "/w/slab" ||
		specs[0].Command != "" {
		t.Errorf("specs = %+v, want one implicit shell window", specs)
	}
}

func TestWindowSpecsRejectContainerLocation(t *testing.T) {
	cfg := config.Config{
		Windows: []config.Window{
			{Name: "agent-1", Agent: strPtr("claude"), Location: strPtr("container")},
		},
	}
	_, err := windowSpecs(cfg, "/w/slab")
	var cw *containerWindowError
	if !errors.As(err, &cw) {
		t.Fatalf("err = %v, want *containerWindowError", err)
	}
	if !strings.Contains(err.Error(), "agent-1") {
		t.Errorf("error %q does not name the window", err)
	}
}

func TestHostOnlyContainerObserver(t *testing.T) {
	observer := hostOnlyContainerObserver{}
	worktree := t.TempDir()
	ws := resolve.Workspace{ID: "w1", Worktree: worktree}
	auto := config.Config{DevContainer: config.DevContainer{Enabled: "auto"}}

	// auto with no devcontainer configuration: no container applies.
	obs, err := observer.DiscoverContainer(context.Background(), ws, auto)
	if err != nil || obs != nil {
		t.Errorf("auto/no-config = (%v, %v), want (nil, nil)", obs, err)
	}

	// auto with a devcontainer.json on disk: unsupported (error funnel).
	if err := os.MkdirAll(filepath.Join(worktree, ".devcontainer"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, ".devcontainer", "devcontainer.json"),
		[]byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := observer.DiscoverContainer(context.Background(), ws, auto); err == nil {
		t.Error("auto with a devcontainer config discovered nothing to refuse")
	}

	// enabled true is unsupported regardless of files.
	if _, err := observer.DiscoverContainer(context.Background(),
		resolve.Workspace{Worktree: t.TempDir()},
		config.Config{DevContainer: config.DevContainer{Enabled: "true"}}); err == nil {
		t.Error("enabled true discovered nothing to refuse")
	}

	// A stored binding cannot be probed in this build.
	if _, err := observer.ProbeContainer(context.Background(),
		state.ContainerBinding{ContainerID: "c1"}); err == nil {
		t.Error("ProbeContainer pretended to probe")
	}
}

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
