package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// statusWorkspace builds the standard test repository and returns its
// resolved identity so store seeding and live-session fixtures agree
// with what buildStatus will resolve.
func statusWorkspace(t *testing.T) resolve.Workspace {
	t.Helper()
	workspace(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/slabledger.yaml": validConfig,
	})
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

func decodeStatus(t *testing.T, stdout string) statusEnvelope {
	t.Helper()
	var env statusEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding status JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestStatusLiveMatchingSession(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	actual, err := s.AllocateSessionName(ws.ID, cliTestTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	applied := "sha256:stale"
	if err := s.CommitReconciliation(ws.ID, state.ReconciliationResult{
		AppliedDigest: &applied,
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, cliTestTime); err != nil {
		t.Fatalf("commit: %v", err)
	}
	installFakeStore(t, s)
	// validConfig enables devcontainer: auto; a failing discovery makes
	// the live observation honest uncertainty.
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult: true,
		DiscoverErr:   errors.New("docker down"),
	})
	live := controller.LiveSession{
		Name: actual, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.RepoRoot,
	}
	installSessionObserver(t, controller.SessionObservation{
		ByIdentity: &live,
		ByName:     []controller.LiveSession{live},
	}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if !env.Registered || env.Stored == nil {
		t.Fatalf("registered = %t, stored = %+v", env.Registered, env.Stored)
	}
	if env.Session.State != "live" {
		t.Errorf("session.state = %q", env.Session.State)
	}
	if env.Session.Identity == nil || *env.Session.Identity != "match" {
		t.Errorf("session.identity = %v, want match", env.Session.Identity)
	}
	if env.Plan.Session != "none" {
		t.Errorf("plan.session = %q, want none", env.Plan.Session)
	}
	if !env.Config.Drifted {
		t.Error("config.drifted = false; the applied digest is stale")
	}
	if env.Config.AppliedDigest == nil || *env.Config.AppliedDigest != applied {
		t.Errorf("config.applied_digest = %v", env.Config.AppliedDigest)
	}
	if !env.Plan.Reapply {
		t.Error("plan.reapply = false, want true")
	}
	if env.LastOperation == nil || env.LastOperation.Operation != "open" {
		t.Errorf("last_operation = %+v", env.LastOperation)
	}
	if env.Container == nil {
		t.Fatal("container section missing while devcontainer is enabled")
	}
	if env.Container.Stored != nil {
		t.Errorf("container.stored = %+v, want none", env.Container.Stored)
	}
	if !env.Container.Observation.Attempted || env.Container.Observation.Error == "" {
		t.Errorf("observation = %+v, want an attempted observation carrying the failure",
			env.Container.Observation)
	}
	if env.Plan.Container != "probe-first" {
		t.Errorf("plan.container = %q, want probe-first", env.Plan.Container)
	}
}

func TestStatusUnknownSessionRefuses(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, errors.New("tmux exploded"))

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if env.Session.State != "unknown" {
		t.Errorf("session.state = %q, want unknown", env.Session.State)
	}
	if env.Session.Identity != nil {
		t.Errorf("session.identity = %v; an unobserved session has no verdict", env.Session.Identity)
	}
	if env.Plan.Session != "refuse" || env.Plan.Refusal == "" {
		t.Errorf("plan = %+v, want a refusal", env.Plan)
	}
	if env.Registered {
		t.Error("registered = true for an empty store")
	}
}

func TestStatusStoredBindingNeverRendersAsLive(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Health: state.HealthMissing,
	}, cliTestTime); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	installFakeStore(t, s)
	// A failing live probe: the stored binding must still never render
	// as live, and the observation must carry the failure.
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult: true,
		ProbeErr:      errors.New("docker down"),
	})
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStatus(t, stdout)
	if env.Container == nil || env.Container.Stored == nil {
		t.Fatalf("container = %+v, want a stored binding", env.Container)
	}
	if env.Container.Stored.Health != "missing" || env.Container.Stored.ContainerID != "c1" {
		t.Errorf("container.stored = %+v, want retained missing c1", env.Container.Stored)
	}
	if !env.Container.Observation.Attempted || env.Container.Observation.Error == "" {
		t.Errorf("observation = %+v, want an attempted observation carrying the failure",
			env.Container.Observation)
	}

	code, human, _ := run(t, "status")
	if code != 0 {
		t.Fatalf("human exit %d", code)
	}
	if !strings.Contains(human, "missing") || !strings.Contains(human, "observation failed") {
		t.Errorf("human output hides the missing/failed-observation truth: %s", human)
	}
}

func TestStatusForeignOccupantRefuses(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{
		ByName: []controller.LiveSession{{Name: ws.SessionName}},
	}, nil)

	code, stdout, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStatus(t, stdout)
	if env.Plan.Session != "refuse" {
		t.Errorf("plan.session = %q, want refuse", env.Plan.Session)
	}
	if !strings.Contains(env.Plan.Refusal, ws.SessionName) {
		t.Errorf("refusal %q does not name the occupant", env.Plan.Refusal)
	}
}

func TestStatusUnknownWorkspaceExitCode(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, _ := run(t, "status", "no-such-workspace")
	if code != ExitUnknownWorkspace {
		t.Errorf("exit %d, want %d", code, ExitUnknownWorkspace)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
}

func TestStatusRejectsExtraArguments(t *testing.T) {
	code, _, _ := run(t, "status", "one", "two")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}

func TestStatusLiveProbeContradictsStalePresent(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatal(err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult: true,
		ProbeResult:   controller.ContainerObservation{Health: state.HealthMissing},
	})

	code, stdout, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStatus(t, stdout)
	if env.Container.Stored == nil || env.Container.Stored.Health != "present" {
		t.Fatalf("stored = %+v (last-observed present)", env.Container.Stored)
	}
	if !env.Container.Observation.Attempted || env.Container.Observation.Health != "missing" {
		t.Errorf("observation = %+v, want an attempted live missing", env.Container.Observation)
	}
}

// A repository still recorded at a linked worktree is what migration 0002
// leaves behind, and only rebuild can correct it. Status is where an
// operator looks first, so status is where it has to say so.
func TestStatusReportsAStaleRepositoryRootAsNeedingRebuild(t *testing.T) {
	ws := statusWorkspace(t)
	worktree := linkedWorktree(t, "1529")
	s := fake.NewStore()
	if err := s.RegisterWorkspace(resolve.Workspace{
		ID:           "stale-workspace-id",
		RepositoryID: "stale-repository-id",
		Slug:         ws.Slug,
		RepoRoot:     worktree,
		SessionName:  ws.Slug + "--1529",
	}, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if !env.NeedsRebuild {
		t.Fatalf("needs_rebuild = false; %s is recorded as a repository root", worktree)
	}
	if !strings.Contains(env.NeedsRebuildReason, worktree) {
		t.Errorf("reason %q does not name the stale root", env.NeedsRebuildReason)
	}
}

// The state a rebuild leaves when its collapse succeeds and its retag
// fails (exit 6): no row is stale any more, but a live session still
// carries the identity keys a pre-change projectmux wrote, and `open`
// refuses it as a foreign occupant. Reporting on rows alone would call
// this installation clean.
func TestStatusReportsAStaleLiveSessionAsNeedingRebuild(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	// The collapse already ran: the only repository row is the real one.
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{
		ByName: []controller.LiveSession{{
			Name:        ws.SessionName,
			WorkspaceID: "pre-change-workspace-id",
			Slug:        ws.Slug,
			Worktree:    ws.RepoRoot,
		}},
	}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if !env.NeedsRebuild {
		t.Fatalf("needs_rebuild = false; the live session still carries stale keys")
	}
	if !strings.Contains(env.NeedsRebuildReason, ws.SessionName) {
		t.Errorf("reason %q does not name the stale session", env.NeedsRebuildReason)
	}
}

// A session of the same name belonging to a different repository is a
// genuine collision, not a migration leftover: rebuild will not retag it,
// so status must not send the operator there.
func TestStatusDoesNotBlameRebuildForAForeignSession(t *testing.T) {
	ws := statusWorkspace(t)
	// A real repository, not just a directory: resolving it has to
	// succeed and disagree, so the exclusion is the root comparison
	// rather than a resolver error.
	other := t.TempDir()
	if out, err := exec.Command("git", "-C", other, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	other, err := filepath.EvalSymlinks(other)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{
		ByName: []controller.LiveSession{{
			Name:        ws.SessionName,
			WorkspaceID: "some-other-workspace",
			Slug:        ws.Slug,
			Worktree:    other,
		}},
	}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if env := decodeStatus(t, stdout); env.NeedsRebuild {
		t.Errorf("needs_rebuild = true for a foreign session: %s", env.NeedsRebuildReason)
	}
}

// status reports the stored bind, and reports when that bind cannot be
// used. Only Ensure computes the latter today, so status has to reach
// the same check without ensuring anything (spec §5).
func TestStatusReportsTheBindAndAnUnusableBind(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	bind := "services/gone"
	if err := s.SetBind(ws.ID, &bind, cliTestTime); err != nil {
		t.Fatalf("set bind: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if env.Stored == nil || env.Stored.Bind == nil || *env.Stored.Bind != bind {
		t.Fatalf("stored.bind = %+v, want %q", env.Stored, bind)
	}
	if env.BindWarning == "" {
		t.Fatal("bind_warning is empty; an unusable bind is invisible")
	}
	if !strings.Contains(env.BindWarning, bind) {
		t.Errorf("bind_warning = %q, want it to name the bind", env.BindWarning)
	}
	if !strings.Contains(stdout, `"schema_version": 2`) {
		t.Errorf("status envelope is no longer schema 2:\n%s", stdout)
	}
}

func TestStatusIsQuietWhenNoBindIsRecorded(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if env.Stored == nil || env.Stored.Bind != nil {
		t.Errorf("stored.bind = %+v, want none", env.Stored)
	}
	if env.BindWarning != "" {
		t.Errorf("bind_warning = %q, want empty", env.BindWarning)
	}
	if strings.Contains(stdout, "bind") {
		t.Errorf("an unbound workspace emits a bind key:\n%s", stdout)
	}
}

func TestStatusIsQuietWhenNoRepositoryRootIsStale(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if env := decodeStatus(t, stdout); env.NeedsRebuild {
		t.Errorf("needs_rebuild = true on a migrated installation: %s", env.NeedsRebuildReason)
	}
}
