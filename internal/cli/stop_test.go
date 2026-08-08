package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

func decodeStop(t *testing.T, stdout string) stopEnvelope {
	t.Helper()
	var env stopEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding stop JSON: %v\n%s", err, stdout)
	}
	return env
}

// openWorkspaceIdentity is openWorkspace under a clearer name for stop
// tests, which need only the identity.
func openWorkspaceIdentity(t *testing.T) resolve.Workspace {
	t.Helper()
	return openWorkspace(t)
}

func stopFixtureFor(t *testing.T, ws resolve.Workspace) (*fake.Store, *fake.SessionActuator) {
	t.Helper()
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.AllocateSessionName(ws.ID, cliTestTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	installOpenStore(t, s)
	return s, installFakeActuator(t)
}

func TestStopKillsLiveSessionJSON(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	s, actuator := stopFixtureFor(t, ws)
	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStop(t, stdout)
	if !env.Session.Stopped || env.Session.Name != ws.SessionName {
		t.Errorf("session = %+v", env.Session)
	}
	if env.Container != nil {
		t.Errorf("container = %+v, want absent without --container", env.Container)
	}
	if len(actuator.Killed) != 1 || actuator.Killed[0] != ws.SessionName {
		t.Errorf("Killed = %v", actuator.Killed)
	}
	rec, _ := s.Workspace(ws.ID)
	if rec.LastOperation == nil || rec.LastOperation.Name != "stop" ||
		rec.LastOperation.Outcome != state.OutcomeOK {
		t.Errorf("last operation = %+v, want stop/ok", rec.LastOperation)
	}
}

func TestStopAbsentIsIdempotentSuccess(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	_, actuator := stopFixtureFor(t, ws)
	installScriptedSessions(t, cliAbsent())

	code, stdout, _ := run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStop(t, stdout)
	if env.Session.Stopped {
		t.Errorf("session = %+v, want not stopped", env.Session)
	}
	if len(actuator.Killed) != 0 {
		t.Errorf("Killed = %v", actuator.Killed)
	}
}

func TestStopUnknownRefusesExitSix(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	stopFixtureFor(t, ws)
	installScriptedSessions(t,
		func(controller.SessionQuery) (controller.SessionObservation, error) {
			return controller.SessionObservation{}, errors.New("tmux exploded")
		})

	code, stdout, _ := run(t, "stop", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on refusal (nothing was done)", stdout)
	}
}

func TestStopContainerStopsBinding(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	s, _ := stopFixtureFor(t, ws)
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	actC := installContainerActuator(t)
	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, _ := run(t, "stop", "--container", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStop(t, stdout)
	if env.Container == nil || !env.Container.Stopped || env.Container.ContainerID != "cid-1" {
		t.Errorf("container = %+v", env.Container)
	}
	if len(actC.Stopped) != 1 || actC.Stopped[0] != "cid-1" {
		t.Errorf("Stopped = %v", actC.Stopped)
	}
	rec, _ := s.Workspace(ws.ID)
	if rec.Container == nil || rec.Container.Health != state.HealthMissing {
		t.Errorf("binding = %+v, want retained with missing health", rec.Container)
	}
}

func TestStopContainerPartialFailureReportsOnStdout(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	s, _ := stopFixtureFor(t, ws)
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	actC := installContainerActuator(t)
	actC.StopErr = errors.New("docker stop exploded")
	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "stop", "--container", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	// The deliberate contract amendment (spec §5): the report IS the
	// output, on stdout, even though the command exits nonzero.
	env := decodeStop(t, stdout)
	if !env.Session.Stopped {
		t.Errorf("session = %+v; the partial report must show the kill", env.Session)
	}
	if env.Container == nil || env.Container.Stopped {
		t.Errorf("container = %+v, want reported unstopped", env.Container)
	}
	if env.Error == "" || !strings.Contains(env.Error, "docker stop exploded") {
		t.Errorf("error field = %q, want the failure detail", env.Error)
	}
	if !strings.Contains(stderr, "projectmux:") {
		t.Errorf("stderr = %q, want the one-line summary", stderr)
	}
}

func TestStopUnregisteredIsQuietSuccess(t *testing.T) {
	openWorkspaceIdentity(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	installScriptedSessions(t, cliAbsent())

	code, stdout, _ := run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStop(t, stdout)
	if env.Session.Stopped {
		t.Errorf("session = %+v", env.Session)
	}
}

// registerCLISibling adds a second session on the workspace's repository
// directly through the store: the resolver has no argument form for one
// yet, but rebuild produces them from pre-existing worktree sessions.
func registerCLISibling(t *testing.T, s *fake.Store, ws resolve.Workspace) controller.LiveSession {
	t.Helper()
	sib := resolve.Workspace{
		ID:           ws.ID + "-2",
		RepositoryID: ws.RepositoryID,
		Slug:         ws.Slug,
		RepoRoot:     ws.RepoRoot,
		Session:      "feature-a",
		SessionName:  ws.SessionName + "--feature-a",
	}
	if err := s.RegisterWorkspace(sib, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register sibling: %v", err)
	}
	if _, err := s.AllocateSessionName(sib.ID, cliTestTime); err != nil {
		t.Fatalf("allocate sibling: %v", err)
	}
	return controller.LiveSession{
		ID: "$9", Name: sib.SessionName, WorkspaceID: sib.ID,
		Slug: sib.Slug, Worktree: sib.RepoRoot,
	}
}

func TestStopContainerRefusesWithLiveSiblingExitsSix(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	s, _ := stopFixtureFor(t, ws)
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	sib := registerCLISibling(t, s, ws)
	actC := installContainerActuator(t)
	installScriptedSessions(t, cliLive(sib))

	code, stdout, stderr := run(t, "stop", "--container", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitRefused, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on refusal (nothing was done)", stdout)
	}
	if !strings.Contains(stderr, sib.Name) {
		t.Errorf("stderr = %q, want the live sibling named", stderr)
	}
	if len(actC.Stopped) != 0 {
		t.Errorf("Stopped = %v; the shared container was killed anyway", actC.Stopped)
	}
}

func TestStopContainerForceStopsSharedContainer(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	s, actuator := stopFixtureFor(t, ws)
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	registerCLISibling(t, s, ws)
	actC := installContainerActuator(t)
	// Force never observes the siblings, so the only scripted step is the
	// workspace's own session.
	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "stop", "--container", "--force", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStop(t, stdout)
	if env.Container == nil || !env.Container.Stopped || env.Container.ContainerID != "cid-1" {
		t.Errorf("container = %+v", env.Container)
	}
	if len(actC.Stopped) != 1 || len(actuator.Killed) != 1 {
		t.Errorf("Stopped = %v, Killed = %v", actC.Stopped, actuator.Killed)
	}
}
