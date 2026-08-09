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

func listWorkspace(id, slug string) resolve.Workspace {
	return resolve.Workspace{
		ID:           id,
		RepositoryID: "r-" + id,
		Slug:         slug,
		RepoRoot:     "/w/" + slug,
		SessionName:  slug,
	}
}

func seededListStore(t *testing.T) *fake.Store {
	t.Helper()
	s := fake.NewStore()
	if err := s.RegisterWorkspace(listWorkspace("w1", "alpha"), "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.AllocateSessionName("w1", cliTestTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := s.RecordContainerObservation("r-w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := s.RecordContainerObservation("r-w1", state.ContainerObservation{
		Health: state.HealthMissing,
	}, cliTestTime); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	if err := s.RegisterWorkspace(listWorkspace("w2", "beta"), "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	return s
}

func decodeList(t *testing.T, stdout string) listEnvelope {
	t.Helper()
	var env listEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding list JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestListUnionsStoredAndLiveSessions(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, []controller.LiveSession{
		{Name: "alpha", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/alpha"},
		{Name: "rogue", WorkspaceID: "w9", Slug: "elsewhere", Worktree: "/w/elsewhere"},
	}, nil)

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeList(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d", env.SchemaVersion)
	}
	if !env.TmuxObserved {
		t.Error("tmux_observed = false, want true")
	}
	if len(env.Workspaces) != 3 {
		t.Fatalf("%d rows, want 3: %+v", len(env.Workspaces), env.Workspaces)
	}

	alpha := env.Workspaces[0]
	if alpha.Slug != "alpha" || !alpha.Recorded || alpha.SessionState != "live" {
		t.Errorf("alpha row = %+v", alpha)
	}
	if alpha.LiveSession == nil || *alpha.LiveSession != "alpha" {
		t.Errorf("alpha live_session = %v, want alpha", alpha.LiveSession)
	}
	if alpha.Container == nil || alpha.Container.Health != "missing" {
		t.Errorf("alpha container = %+v, want retained missing binding", alpha.Container)
	}
	if alpha.IdentityConflict {
		t.Error("alpha identity_conflict = true, want false")
	}

	beta := env.Workspaces[1]
	if beta.Slug != "beta" || beta.SessionState != "absent" || beta.Container != nil {
		t.Errorf("beta row = %+v", beta)
	}
	if beta.ActualSession != nil {
		t.Errorf("beta actual_session = %v, want unassigned", beta.ActualSession)
	}

	rogue := env.Workspaces[2]
	if rogue.Recorded || rogue.SessionState != "live" || rogue.ID != "w9" {
		t.Errorf("rogue row = %+v", rogue)
	}
	if rogue.LiveSession == nil || *rogue.LiveSession != "rogue" {
		t.Errorf("rogue live_session = %v", rogue.LiveSession)
	}
	if rogue.IdentityConflict {
		t.Error("rogue identity_conflict = true, want false")
	}
}

func TestListIdentityConflictOnContradictoryKeys(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, []controller.LiveSession{
		{Name: "alpha", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/other"},
	}, nil)

	code, stdout, _ := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeList(t, stdout)
	if !env.Workspaces[0].IdentityConflict {
		t.Errorf("contradictory worktree key did not set identity_conflict: %+v", env.Workspaces[0])
	}
}

// TestListReportsAnIdentityConflictOnTheSessionKey is the list-side half of
// the same rule: what rebuild refuses to adopt, list must not report as a
// clean live session.
func TestListReportsAnIdentityConflictOnTheSessionKey(t *testing.T) {
	s := fake.NewStore()
	ws := listWorkspace("w1", "proj")
	ws.Session = "feature-a"
	if err := s.RegisterWorkspace(ws, "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.AllocateSessionName("w1", cliTestTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	installFakeStore(t, s)
	installLiveSessions(t, []controller.LiveSession{
		{Name: "proj--feature-a", WorkspaceID: "w1", Slug: "proj", Worktree: "/w/proj", Session: "feature-b"},
	}, nil)

	code, stdout, _ := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeList(t, stdout)
	if len(env.Workspaces) != 1 {
		t.Fatalf("Workspaces = %+v, want one", env.Workspaces)
	}
	if !env.Workspaces[0].IdentityConflict {
		t.Error("IdentityConflict = false, want true: @dev_session contradicts the record")
	}
}

func TestListDuplicateClaimsReportUncertainty(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, []controller.LiveSession{
		{Name: "one", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/alpha"},
		{Name: "two", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/alpha"},
	}, nil)

	code, stdout, _ := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeList(t, stdout)
	if len(env.Workspaces) != 4 {
		t.Fatalf("%d rows, want 4 (2 stored + 2 claimants): %+v", len(env.Workspaces), env.Workspaces)
	}
	alpha := env.Workspaces[0]
	if alpha.SessionState != "unknown" || !alpha.IdentityConflict || alpha.LiveSession != nil {
		t.Errorf("duplicate-claimed stored row = %+v, want unknown/conflict/no live_session", alpha)
	}
	for _, row := range env.Workspaces[2:] {
		if row.Recorded || !row.IdentityConflict || row.SessionState != "live" {
			t.Errorf("claimant row = %+v, want unrecorded live conflict", row)
		}
	}
}

func TestListTmuxFailureIsUncertainNotFatal(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, nil, errors.New("tmux exploded"))

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeList(t, stdout)
	if env.TmuxObserved {
		t.Error("tmux_observed = true after a failed observation")
	}
	for _, row := range env.Workspaces {
		if row.SessionState != "unknown" {
			t.Errorf("row %s session_state = %q, want unknown", row.Slug, row.SessionState)
		}
	}
}

func TestListHumanNeverRendersRetainedBindingAsLive(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, nil, nil)

	code, stdout, _ := run(t, "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "missing (as of 2026-08-05T12:00:00Z)") {
		t.Errorf("human output does not label the retained binding: %s", stdout)
	}
	if strings.Contains(stdout, "alpha (unassigned)") {
		t.Errorf("alpha has an assigned session yet renders unassigned: %s", stdout)
	}
	if !strings.Contains(stdout, "beta (unassigned)") {
		t.Errorf("beta has no assigned session yet renders without the marker: %s", stdout)
	}
}

func TestListEmpty(t *testing.T) {
	installFakeStore(t, fake.NewStore())
	installLiveSessions(t, nil, nil)

	code, stdout, _ := run(t, "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "no workspaces") {
		t.Errorf("empty list output: %q", stdout)
	}
}

func TestListRejectsArguments(t *testing.T) {
	code, _, _ := run(t, "list", "extra")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}

// boundListStore seeds a workspace carrying both a named session and a
// bind beside a plain default-session one, so rendering can be checked
// on each. It is separate from seededListStore because that fixture's
// row count and order are asserted positionally by three tests.
func boundListStore(t *testing.T) *fake.Store {
	t.Helper()
	s := fake.NewStore()
	named := listWorkspace("w1", "alpha")
	named.Session = "feature-a"
	named.SessionName = "alpha--feature-a"
	if err := s.RegisterWorkspace(named, "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	bind := "services/api"
	if err := s.SetBind("w1", &bind, cliTestTime); err != nil {
		t.Fatalf("set bind: %v", err)
	}
	if err := s.RegisterWorkspace(listWorkspace("w2", "beta"), "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	return s
}

// The workspace column renders the target a user would type: slug/session
// for a named session, bare slug for the repository's default one. A
// default session must never render with a trailing slash, which is what
// the "beta/" check rules out.
func TestListHumanRendersTheBindAndTheSessionTarget(t *testing.T) {
	installFakeStore(t, boundListStore(t))
	installLiveSessions(t, nil, nil)

	code, stdout, stderr := run(t, "list")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "BIND") {
		t.Errorf("table has no BIND column:\n%s", stdout)
	}
	if !strings.Contains(stdout, "alpha/feature-a") {
		t.Errorf("named session does not render as slug/session:\n%s", stdout)
	}
	if !strings.Contains(stdout, "services/api") {
		t.Errorf("bind is not rendered:\n%s", stdout)
	}
	if strings.Contains(stdout, "beta/") {
		t.Errorf("the default session renders with a slash:\n%s", stdout)
	}
	// The five pre-existing columns survive the insertion.
	for _, column := range []string{"WORKSPACE", "SESSION", "TMUX", "CONTAINER", "NOTES"} {
		if !strings.Contains(stdout, column) {
			t.Errorf("table lost the %s column:\n%s", column, stdout)
		}
	}
}

func TestListUnrecordedRowCarriesItsSessionComponent(t *testing.T) {
	installFakeStore(t, fake.NewStore())
	installLiveSessions(t, []controller.LiveSession{{
		Name:        "gamma--feature-b",
		WorkspaceID: "w9",
		Slug:        "gamma",
		Worktree:    "/w/gamma",
		Session:     "feature-b",
	}}, nil)

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeList(t, stdout)
	if len(env.Workspaces) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(env.Workspaces), env.Workspaces)
	}
	if env.Workspaces[0].Session != "feature-b" {
		t.Errorf("unrecorded row session = %q, want feature-b",
			env.Workspaces[0].Session)
	}
}

func TestListCompactImpliesJSON(t *testing.T) {
	installFakeStore(t, fake.NewStore())
	installLiveSessions(t, nil, nil)

	code, stdout, _ := run(t, "list", "--compact")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Errorf("compact output is not one line: %q", stdout)
	}
	decodeList(t, stdout)
}
