package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
)

func decodeRebuild(t *testing.T, stdout string) rebuildEnvelope {
	t.Helper()
	var env rebuildEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding rebuild JSON: %v\n%s", err, stdout)
	}
	return env
}

// rebuildEnv wires every rebuild seam to a benign fake — a real git
// repository with valid configuration, an isolated state root, and a
// store the command may mutate — so a test can perturb exactly one of
// them. The scripted session observer errors if consulted rather than
// failing the test outright; a test relying on it not being reached must
// assert the conflict reason, since observeLive turns any observation
// error into a conflict (internal/rebuild/apply.go).
// It returns the resolved workspace the repository represents.
func rebuildEnv(t *testing.T, s *fake.Store, live []controller.LiveSession) resolve.Workspace {
	t.Helper()
	ws := openWorkspace(t)
	installOpenStore(t, s)
	installLiveSessions(t, live, nil)
	installScriptedSessions(t) // exhausts on any call
	return ws
}

// installRebuildDatabaseCheck substitutes the pre-flight classification
// so a command test can exercise the refusal without a corrupt file.
func installRebuildDatabaseCheck(t *testing.T, err error) {
	t.Helper()
	orig := rebuildDatabaseCheck
	t.Cleanup(func() { rebuildDatabaseCheck = orig })
	rebuildDatabaseCheck = func(string) error { return err }
}

// A fully recovered installation produces an empty report and exits 0.
// That is what makes applying by default safe: a needless run costs
// nothing and says so.
func TestRebuildReportsNothingWhenEverythingIsRecorded(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	env := decodeRebuild(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, OutputSchemaVersion)
	}
	if env.DryRun {
		t.Error("dry_run is set without --dry-run")
	}
	if len(env.Registered) != 0 || len(env.Conflicts) != 0 {
		t.Errorf("report = %+v, want empty", env)
	}
}

func TestRebuildRejectsArguments(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, stderr := run(t, "rebuild", "slabledger")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "slabledger") {
		t.Errorf("stderr %q should name the unexpected argument", stderr)
	}
}

// mismatchedSession builds the fixture for spec §4 case 3: a recorded
// workspace whose actual_session is "old-name" while a live session
// carrying its identity keys answers to "new-name". Fill-only means
// rebuild reports that and writes nothing.
func mismatchedSession(t *testing.T) (*fake.Store, resolve.Workspace) {
	t.Helper()
	s := fake.NewStore()
	ws := rebuildEnv(t, s, nil)
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.AdoptSessionName(ws.ID, "old-name", cliTestTime); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	installLiveSessions(t, []controller.LiveSession{{
		Name:        "new-name",
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.RepoRoot,
	}}, nil)
	return s, ws
}

// The report goes to stdout even when the command exits 6. Hiding the
// conflicts in exactly the case that needs reading is what reportedError
// exists to prevent.
func TestRebuildReportsConflictsOnStdoutAndExitsRefused(t *testing.T) {
	s, ws := mismatchedSession(t)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitRefused, stderr)
	}
	if stdout == "" {
		t.Fatal("the report did not reach stdout")
	}
	env := decodeRebuild(t, stdout)
	if len(env.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", env.Conflicts)
	}
	// The reason must be the session-mismatch finding, not an incidental
	// conflict from the scripted observer erroring when consulted (which
	// would produce a conflict for the wrong reason and pass this test
	// regardless).
	if !strings.Contains(env.Conflicts[0].Reason, "never overwrites") {
		t.Errorf("conflict reason %q is not the session-mismatch finding", env.Conflicts[0].Reason)
	}
	if len(env.Registered) != 0 {
		t.Errorf("registered = %+v, want nothing written on a conflict", env.Registered)
	}
	if stderr == "" {
		t.Error("stderr should carry the one-line summary")
	}

	// Fill-only: the recorded name is untouched.
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "old-name" {
		t.Errorf("actual_session = %v, want %q unchanged", rec.ActualSession, "old-name")
	}
}

// A tmux outage is uncertainty, not an empty installation. It refuses
// with nothing on stdout: there is no report to write.
func TestRebuildRefusesWhenTmuxIsUnobservable(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)
	installLiveSessions(t, nil, errors.New("tmux exploded"))

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d", code, ExitRefused)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "tmux") {
		t.Errorf("stderr %q does not explain the outage", stderr)
	}
}

// An unloadable defaults layer makes the digest underivable for every
// workspace, so it is fatal rather than one workspace's conflict.
func TestRebuildExitsFiveWhenDefaultsWillNotLoad(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": "version: 1\nautostrt: true\n"})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitInvalidConfig {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitInvalidConfig, stderr)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
}

// A corrupt database is exit 1, not 6: exit 6 denotes uncertainty about
// the world, and a corrupt file is a diagnosed, definite condition. The
// value is in the message.
func TestRebuildExitsOneOnACorruptDatabase(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)
	installRebuildDatabaseCheck(t, errors.New(
		"the state database at /state/state.db cannot be read: malformed"))

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "state.db") {
		t.Errorf("stderr %q does not name the database", stderr)
	}
}

// Both arrays are always present and never null, matching doctor's
// always-full checks: a consumer never branches on absence.
func TestRebuildEnvelopeAlwaysCarriesBothArrays(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, _ := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var raw struct {
		SchemaVersion *int               `json:"schema_version"`
		DryRun        *bool              `json:"dry_run"`
		Registered    *[]json.RawMessage `json:"registered"`
		Conflicts     *[]json.RawMessage `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout)
	}
	if raw.SchemaVersion == nil || raw.DryRun == nil {
		t.Errorf("envelope is missing schema_version or dry_run: %s", stdout)
	}
	if raw.Registered == nil {
		t.Errorf("registered is null rather than an empty array: %s", stdout)
	}
	if raw.Conflicts == nil {
		t.Errorf("conflicts is null rather than an empty array: %s", stdout)
	}
}

func TestRebuildCompactImpliesJSON(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, stderr := run(t, "rebuild", "--compact")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Errorf("--compact should emit one line:\n%s", stdout)
	}
	decodeRebuild(t, stdout)
}

// A dry run is a preview, not a partial pass: it reports the conflicts
// the real run would report and exits on them the same way, because the
// exit code describes the state of the world rather than whether
// anything was written.
func TestRebuildDryRunReportsTheSameConflictsAndCode(t *testing.T) {
	s, ws := mismatchedSession(t)

	code, stdout, stderr := run(t, "rebuild", "--dry-run", "--json")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitRefused, stderr)
	}
	env := decodeRebuild(t, stdout)
	if !env.DryRun {
		t.Error("dry_run is false in a --dry-run report")
	}
	if len(env.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want the same conflict the real run reports", env.Conflicts)
	}
	if !strings.Contains(env.Conflicts[0].Reason, "never overwrites") {
		t.Errorf("conflict reason %q is not the session-mismatch finding", env.Conflicts[0].Reason)
	}
	if len(env.Registered) != 0 {
		t.Errorf("registered = %+v, want nothing", env.Registered)
	}

	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "old-name" {
		t.Errorf("actual_session = %v, want %q unchanged", rec.ActualSession, "old-name")
	}
}

// The name overpromises relative to what ships, so the help text is the
// mitigation and is pinned by a test.
func TestRebuildHelpStatesWhatItDoesNotDo(t *testing.T) {
	code, stdout, _ := run(t, "rebuild", "--help")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"usage: projectmux rebuild", "--dry-run", "repository_roots", "container bindings",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("rebuild help does not mention %q:\n%s", want, stdout)
		}
	}
}

// Human output is the default and must not be JSON.
func TestRebuildHumanOutputNamesEachRow(t *testing.T) {
	mismatchedSession(t)

	code, stdout, _ := run(t, "rebuild")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d", code, ExitRefused)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Error("human output should not be JSON by default")
	}
	if !strings.Contains(stdout, "conflict") {
		t.Errorf("human output does not name the conflict:\n%s", stdout)
	}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line ends in whitespace: %q", line)
		}
	}
}
