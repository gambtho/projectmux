package cli

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/doctor"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// fakeProbe answers every dependency probe from a table keyed by the
// tool name; an unlisted tool is reported installed and healthy.
type fakeProbe struct {
	results map[string]doctor.ProbeResult
	errs    map[string]error
}

func (p fakeProbe) Probe(_ context.Context, argv ...string) (doctor.ProbeResult, error) {
	tool := argv[0]
	if err := p.errs[tool]; err != nil {
		return doctor.ProbeResult{}, err
	}
	if res, ok := p.results[tool]; ok {
		return res, nil
	}
	return doctor.ProbeResult{Stdout: tool + " 1.0", Found: true}, nil
}

func installVersionRunner(t *testing.T, p fakeProbe) {
	t.Helper()
	orig := newVersionRunner
	t.Cleanup(func() { newVersionRunner = orig })
	newVersionRunner = func() doctor.VersionRunner { return p }
}

// installDoctorDatabase substitutes doctor's read-only database view.
func installDoctorDatabase(t *testing.T, db doctor.Database, store doctor.Store) {
	t.Helper()
	orig := inspectDatabase
	t.Cleanup(func() { inspectDatabase = orig })
	inspectDatabase = func(string) (doctor.Database, doctor.Store, func()) {
		return db, store, func() {}
	}
}

// healthyDoctorEnv wires every doctor seam to a benign fake so a test can
// perturb exactly one of them.
func healthyDoctorEnv(t *testing.T) {
	t.Helper()
	workspace(t, map[string]string{"defaults.yaml": validConfig})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())
	installVersionRunner(t, fakeProbe{})
	installDoctorDatabase(t, doctor.Database{
		Path: "/state/state.db", Version: state.SchemaVersion, Supported: state.SchemaVersion,
	}, fake.NewStore())
	installLiveSessions(t, nil, nil)
	installContainerObserver(t, &fake.ContainerObserver{})
	// Doctor must never take the mutating store path: state.Open creates
	// and migrates the database.
	orig := openStore
	t.Cleanup(func() { openStore = orig })
	openStore = func() (stateStore, error) {
		t.Error("doctor opened the mutating store")
		return nil, nil
	}
}

func decodeDoctor(t *testing.T, stdout string) doctorEnvelope {
	t.Helper()
	var env doctorEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding doctor JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestDoctorEnvelopeIsVersionedAndReportsEveryCheck(t *testing.T) {
	healthyDoctorEnv(t)

	code, stdout, stderr := run(t, "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	env := decodeDoctor(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, OutputSchemaVersion)
	}
	want := []string{
		"dependencies", "configuration", "database",
		"orphaned-sessions", "stale-bindings",
	}
	if len(env.Checks) != len(want) {
		t.Fatalf("%d checks, want %d: %+v", len(env.Checks), len(want), env.Checks)
	}
	for i, name := range want {
		if env.Checks[i].Name != name {
			t.Errorf("check %d = %q, want %q", i, env.Checks[i].Name, name)
		}
		if env.Checks[i].Status == "" {
			t.Errorf("check %q has no status", name)
		}
	}

	// Items is always an array so consumers never branch on null.
	var raw struct {
		Checks []struct {
			Items *[]json.RawMessage `json:"items"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for i, check := range raw.Checks {
		if check.Items == nil {
			t.Errorf("check %d has a null items array", i)
		}
	}
}

func TestDoctorExitsZeroWithFindings(t *testing.T) {
	healthyDoctorEnv(t)
	// tmux absent is doctor's most severe finding, and it still reports.
	installVersionRunner(t, fakeProbe{
		results: map[string]doctor.ProbeResult{"tmux": {}},
	})

	code, stdout, stderr := run(t, "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0; stderr = %q", code, stderr)
	}
	deps := decodeDoctor(t, stdout).Checks[0]
	if deps.Status != "fail" {
		t.Errorf("dependencies status = %q, want fail: %+v", deps.Status, deps)
	}
	if !strings.Contains(stdout, "not installed") {
		t.Errorf("report does not explain the absence:\n%s", stdout)
	}
}

func TestDoctorHumanOutputNamesEveryCheck(t *testing.T) {
	healthyDoctorEnv(t)

	code, stdout, stderr := run(t, "doctor")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Error("human output should not be JSON by default")
	}
	for _, want := range []string{"dependencies", "tmux", "configuration", "database"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output does not mention %q:\n%s", want, stdout)
		}
	}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line ends in whitespace: %q", line)
		}
	}
}

func TestDoctorCompactImpliesJSON(t *testing.T) {
	healthyDoctorEnv(t)

	code, stdout, stderr := run(t, "doctor", "--compact")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Errorf("--compact should emit one line:\n%s", stdout)
	}
	decodeDoctor(t, stdout)
}

func TestDoctorRejectsArguments(t *testing.T) {
	healthyDoctorEnv(t)

	code, stdout, stderr := run(t, "doctor", "slabledger")
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

func TestDoctorHelpIsSuccessful(t *testing.T) {
	code, stdout, _ := run(t, "doctor", "--help")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	if !strings.Contains(stdout, "usage: projectmux doctor") {
		t.Errorf("doctor help = %q", stdout)
	}
}

// TestDoctorNeverTouchesTheStateDatabase is the slice's central promise:
// a diagnosis reports that state is missing or stale without creating or
// migrating anything. It deliberately runs the real inspectDatabase.
func TestDoctorNeverTouchesTheStateDatabase(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})
	stateRoot := t.TempDir()
	t.Setenv("PROJECTMUX_STATE_ROOT", stateRoot)
	installVersionRunner(t, fakeProbe{})
	installLiveSessions(t, nil, nil)
	installContainerObserver(t, &fake.ContainerObserver{})

	code, stdout, stderr := run(t, "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if db := decodeDoctor(t, stdout).Checks[2]; db.Status != "ok" {
		t.Errorf("database check = %+v, want ok for a fresh installation", db)
	}

	if _, err := os.Stat(state.DBPath(stateRoot)); !os.IsNotExist(err) {
		t.Fatalf("doctor created the state database: %v", err)
	}
	if names := dirNames(t, stateRoot); len(names) != 0 {
		t.Errorf("doctor wrote into the state root: %v", names)
	}

	// The same holds for a database this build would otherwise migrate.
	db, err := state.Open(stateRoot)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	path := state.DBPath(stateRoot)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	beforeNames := dirNames(t, stateRoot)

	if code, _, stderr := run(t, "doctor", "--json"); code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Error("doctor modified the state database file")
	}
	// Not even the -shm/-wal sidecars a WAL reader would ordinarily
	// materialize: doctor leaves the state root exactly as it found it.
	if got := dirNames(t, stateRoot); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("state root = %v, want %v unchanged", got, beforeNames)
	}
}

// TestDoctorLeavesAnUnrecoveredWALAlone covers the one state where
// reading the database at all would write to the state root: a -wal with
// no -shm beside it, left by a writer that did not shut down cleanly.
// Recovering it is the next mutating command's job, so doctor reports
// the database as unknown rather than repairing it in passing.
func TestDoctorLeavesAnUnrecoveredWALAlone(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})
	stateRoot := t.TempDir()
	t.Setenv("PROJECTMUX_STATE_ROOT", stateRoot)
	installVersionRunner(t, fakeProbe{})
	installLiveSessions(t, nil, nil)
	installContainerObserver(t, &fake.ContainerObserver{})

	// Stage the crash: copy the -wal aside while a writer holds it, then
	// restore it (with the pre-checkpoint database) after that writer's
	// clean close has removed both sidecars.
	path := state.DBPath(stateRoot)
	st, err := state.Open(stateRoot)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-1", RepositoryID: "repo-1", Slug: "slab", RepoRoot: "/w/slab", SessionName: "slab",
	}, "sha256:abc", time.Now()); err != nil {
		t.Fatalf("register: %v", err)
	}
	dbBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	walBytes, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.WriteFile(path, dbBytes, 0o600); err != nil {
		t.Fatalf("restore db: %v", err)
	}
	if err := os.WriteFile(path+"-wal", walBytes, 0o600); err != nil {
		t.Fatalf("restore wal: %v", err)
	}
	beforeNames := dirNames(t, stateRoot)

	code, stdout, stderr := run(t, "doctor", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if db := decodeDoctor(t, stdout).Checks[2]; db.Status != "unknown" {
		t.Errorf("database check = %+v, want unknown for an unrecovered log", db)
	}
	if got := dirNames(t, stateRoot); strings.Join(got, ",") != strings.Join(beforeNames, ",") {
		t.Errorf("state root = %v, want %v unchanged: doctor recovered the log", got, beforeNames)
	}
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}
