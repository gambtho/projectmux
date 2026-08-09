package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
)

// openTestStore lets open mutate a fake store (guardedStore is for
// observation commands only).
type openTestStore struct{ *fake.Store }

func (openTestStore) Close() error { return nil }

func installOpenStore(t *testing.T, s *fake.Store) {
	t.Helper()
	orig := openStore
	t.Cleanup(func() { openStore = orig })
	openStore = func() (stateStore, error) { return openTestStore{Store: s}, nil }
}

// scriptedCLISessions sequences ObserveSession results across Ensure's
// observation, squat check, and confirmation calls.
type scriptedCLISessions struct {
	steps []func(controller.SessionQuery) (controller.SessionObservation, error)
}

func (s *scriptedCLISessions) ObserveSession(_ context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	if len(s.steps) == 0 {
		return controller.SessionObservation{}, errors.New("scripted observer exhausted")
	}
	step := s.steps[0]
	s.steps = s.steps[1:]
	return step(q)
}

func installScriptedSessions(t *testing.T, steps ...func(controller.SessionQuery) (controller.SessionObservation, error)) {
	t.Helper()
	orig := newSessionObserver
	t.Cleanup(func() { newSessionObserver = orig })
	obs := &scriptedCLISessions{steps: steps}
	newSessionObserver = func() controller.SessionObserver { return obs }
}

func installFakeActuator(t *testing.T) *fake.SessionActuator {
	t.Helper()
	orig := newSessionActuator
	t.Cleanup(func() { newSessionActuator = orig })
	a := &fake.SessionActuator{}
	newSessionActuator = func() controller.SessionActuator { return a }
	return a
}

func installAttachSpies(t *testing.T) (execs, switches *[]string) {
	t.Helper()
	origExec, origSwitch, origSocket := execAttach, switchClient, currentSocket
	t.Cleanup(func() { execAttach, switchClient, currentSocket = origExec, origSwitch, origSocket })
	var e, s []string
	execAttach = func(session string) error { e = append(e, session); return nil }
	switchClient = func(_ context.Context, session string) error { s = append(s, session); return nil }
	currentSocket = func() string { return "" }
	return &e, &s
}

func cliAbsent() func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{}, nil
	}
}

func cliLive(s controller.LiveSession) func(controller.SessionQuery) (controller.SessionObservation, error) {
	return func(controller.SessionQuery) (controller.SessionObservation, error) {
		return controller.SessionObservation{ByIdentity: &s, ByName: []controller.LiveSession{s}}, nil
	}
}

// openWorkspace builds the standard repo, points the state root at a
// temp dir (the lock directory derives from it), and returns the
// resolved workspace.
func openWorkspace(t *testing.T) resolve.Workspace {
	t.Helper()
	workspace(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/slabledger.yaml": validConfig,
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())
	// Container-free by default; container tests install their own
	// observer explicitly.
	installContainerObserver(t, &fake.ContainerObserver{})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	ws, err := resolve.Resolve("", "", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ws
}

func ownLive(ws resolve.Workspace, name string) controller.LiveSession {
	return controller.LiveSession{
		Name: name, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.RepoRoot,
	}
}

func decodeOpen(t *testing.T, stdout string) openEnvelope {
	t.Helper()
	var env openEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding open JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestOpenCreatesAndReportsJSON(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	actuator := installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeOpen(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion || env.Action != "created" ||
		env.Session != ws.SessionName || env.Drifted {
		t.Errorf("envelope = %+v", env)
	}
	if len(actuator.Created) != 1 {
		t.Fatalf("actuator calls = %d, want 1", len(actuator.Created))
	}
	spec := actuator.Created[0]
	// validConfig: agent-1 (agent claude, focus), shell, scratch.
	if len(spec.Windows) != 3 || spec.Windows[0].Name != "agent-1" ||
		spec.Windows[0].Command != "claude" || !spec.Windows[0].Focus {
		t.Errorf("windows = %+v", spec.Windows)
	}
	if spec.Env["CGO_ENABLED"] != "1" {
		t.Errorf("env = %+v; validConfig's environment was dropped", spec.Env)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.AppliedDigest == nil {
		t.Error("creation did not record the applied digest")
	}
}

func TestOpenAttachesByDefaultAndHonorsNoAttach(t *testing.T) {
	ws := openWorkspace(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	execs, switches := installAttachSpies(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, _, stderr := run(t, "open")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if len(*execs) != 1 || (*execs)[0] != ws.SessionName {
		t.Errorf("execAttach calls = %v", *execs)
	}
	if len(*switches) != 0 {
		t.Errorf("switchClient calls = %v", *switches)
	}

	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))
	code, _, _ = run(t, "open", "--no-attach")
	if code != 0 {
		t.Fatalf("no-attach exit %d", code)
	}
	if len(*execs) != 1 {
		t.Errorf("--no-attach attached anyway: %v", *execs)
	}
}

func TestOpenSwitchesClientInsideTmux(t *testing.T) {
	ws := openWorkspace(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	execs, switches := installAttachSpies(t)
	currentSocket = func() string { return tmux.SocketPath("") }
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	if code, _, stderr := run(t, "open"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if len(*switches) != 1 || len(*execs) != 0 {
		t.Errorf("switch = %v, exec = %v; want switch-client inside tmux", *switches, *execs)
	}
}

func TestOpenReportsAlreadyRunningWithDrift(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	actual, err := s.AllocateSessionName(ws.ID, cliTestTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	installOpenStore(t, s)
	actuator := installFakeActuator(t)
	installScriptedSessions(t, cliLive(ownLive(ws, actual)))

	code, stdout, _ := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeOpen(t, stdout)
	if env.Action != "already-running" || !env.Drifted {
		t.Errorf("envelope = %+v, want already-running and drifted", env)
	}
	if len(actuator.Created) != 0 {
		t.Error("already-running called the actuator")
	}
}

func TestOpenRefusalExitsSix(t *testing.T) {
	openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	installScriptedSessions(t,
		func(controller.SessionQuery) (controller.SessionObservation, error) {
			return controller.SessionObservation{}, errors.New("tmux exploded")
		})

	code, stdout, stderr := run(t, "open", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitRefused, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
}

// TestOpenContainerWindowWithoutContainerFails: an explicit container
// window on a workspace where no container applies (auto resolved to
// none) fails inside the locked loop with a failed op recorded — the
// session actuator never runs.
func TestOpenContainerWindowWithoutContainerFails(t *testing.T) {
	workspace(t, map[string]string{
		"defaults.yaml": "version: 1\n",
		"workspaces/slabledger.yaml": "version: 1\nwindows:\n" +
			"  - name: agent-1\n    agent: claude\n    location: container\n",
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())
	installContainerObserver(t, &fake.ContainerObserver{})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	ws, err := resolve.Resolve("", "", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	s := fake.NewStore()
	installOpenStore(t, s)
	actuator := installFakeActuator(t)
	installScriptedSessions(t, cliAbsent())

	code, _, stderr := run(t, "open", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitError, stderr)
	}
	if !strings.Contains(stderr, "requires a container") {
		t.Errorf("stderr = %q", stderr)
	}
	if len(actuator.Created) != 0 {
		t.Error("a container-located window reached the actuator")
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", rec.LastOperation)
	}
}

func TestBareWorkspaceDispatchesToOpen(t *testing.T) {
	openWorkspace(t)
	installOpenStore(t, fake.NewStore())

	// An unknown name proves the fallback reached workspace resolution
	// (exit 4), not the unknown-command usage path (exit 2).
	code, _, stderr := run(t, "no-such-workspace-name")
	if code != ExitUnknownWorkspace {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitUnknownWorkspace, stderr)
	}
}

func TestOpenStartsContainerAndReportsIt(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	actC := installContainerActuator(t)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeOpen(t, stdout)
	if env.Container == nil || env.Container.ContainerID != "cid-1" ||
		env.Container.Health != "present" {
		t.Errorf("container block = %+v", env.Container)
	}
	if env.ContainerWindowsStale {
		t.Error("fresh creation reported stale windows")
	}
	if len(actC.Started) != 1 {
		t.Errorf("StartContainer calls = %d, want 1", len(actC.Started))
	}
	rec, _ := s.Workspace(ws.ID)
	if rec.Container == nil || rec.Container.ContainerUser != "vscode" {
		t.Errorf("committed binding = %+v", rec.Container)
	}
}

func TestOpenReplacementReportsStaleWindows(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatal(err)
	}
	actual, err := s.AllocateSessionName(ws.ID, cliTestTime)
	if err != nil {
		t.Fatal(err)
	}
	installOpenStore(t, s)
	installFakeActuator(t)
	installContainerActuator(t)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})
	installScriptedSessions(t, cliLive(ownLive(ws, actual)))

	code, stdout, _ := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeOpen(t, stdout)
	if env.Action != "already-running" || !env.ContainerWindowsStale {
		t.Errorf("envelope = %+v, want already-running with stale container windows", env)
	}
}

// mkSubdir creates a directory inside the test repository and returns its
// repository-relative form, which is what a bind is stored as.
func mkSubdir(t *testing.T, rel string) string {
	t.Helper()
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	return rel
}

func TestOpenCwdRecordsTheBind(t *testing.T) {
	ws := openWorkspace(t)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, _, stderr := run(t, "open", "--cwd", rel, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("bind = %v, want %q", rec.Bind, rel)
	}
}

// A bind is a declaration about the session, not a side effect of a
// successful open (spec §4). Ensure registers and persists before it
// plans, so a refusal afterwards leaves the bind in place and retrying
// keeps it.
func TestOpenCwdBindSurvivesAFailedOpen(t *testing.T) {
	ws := openWorkspace(t)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	installScriptedSessions(t,
		func(controller.SessionQuery) (controller.SessionObservation, error) {
			return controller.SessionObservation{}, errors.New("tmux exploded")
		})

	code, _, _ := run(t, "open", "--cwd", rel, "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("bind = %v, want %q kept after the failed open", rec.Bind, rel)
	}
}

// A --cwd outside the repository is a caller mistake (spec §6: exit 2),
// and nothing is registered on the way to reporting it.
func TestOpenCwdOutsideTheRepositoryExitsTwo(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)

	code, stdout, stderr := run(t, "open", "--cwd", t.TempDir(), "--json")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if _, err := s.Workspace(ws.ID); err == nil {
		t.Error("a rejected --cwd registered the workspace anyway")
	}
}
