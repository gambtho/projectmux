package target

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
	_ "modernc.org/sqlite"
)

var testTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// The test environment has no gitconfig, so identity and the initial branch
// name are supplied explicitly (copied from internal/resolve/resolve_test.go).
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{
		"-c", "user.email=t@example.com",
		"-c", "user.name=t",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func makeRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	git(t, dir, "init", "-q", dir)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

// base returns a symlink-free temporary directory, so resolved paths compare
// equal to the canonical ones resolve and bindpath return.
func base(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// fixture is one repository under a searchable root, plus a state root.
type fixture struct {
	roots     []string
	repo      string
	stateRoot string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := base(t)
	return fixture{
		roots:     []string{dir},
		repo:      makeRepo(t, filepath.Join(dir, "slabledger")),
		stateRoot: t.TempDir(),
	}
}

// bindSession registers session on the fixture's repository and records its
// bind. It goes through the real store so the lookup reads exactly what the
// bind command will write.
func (f fixture) bindSession(t *testing.T, session, bind string) {
	t.Helper()
	ws, err := resolve.Resolve("", session, f.roots, f.repo)
	if err != nil {
		t.Fatalf("resolving %q: %v", session, err)
	}
	st, err := state.Open(f.stateRoot)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RegisterWorkspace(ws, "sha256:aaaa", testTime); err != nil {
		t.Fatalf("RegisterWorkspace(%q): %v", session, err)
	}
	if bind != "" {
		if err := st.SetBind(ws.ID, &bind, testTime); err != nil {
			t.Fatalf("SetBind(%q): %v", session, err)
		}
	}
}

func mustSelect(t *testing.T, ref Ref, f fixture, cwd string) resolve.Workspace {
	t.Helper()
	ws, err := Select(ref, f.roots, cwd, f.stateRoot)
	if err != nil {
		t.Fatalf("Select(%+v) from %s: %v", ref, cwd, err)
	}
	return ws
}

// TestSelectKeysOnTargetPresence is the pair spec §7 calls out. From inside a
// bound directory, a bare `<repo>` target must still address the default
// session; only the absence of a target lets the cwd choose.
func TestSelectKeysOnTargetPresence(t *testing.T) {
	f := newFixture(t)
	bound := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")

	explicit := mustSelect(t, Ref{Present: true, Name: "slabledger"}, f, bound)
	if explicit.Session != "" {
		t.Errorf("Session = %q from an explicit <repo> target, want the default session",
			explicit.Session)
	}

	implicit := mustSelect(t, Ref{}, f, bound)
	if implicit.Session != "api" {
		t.Errorf("Session = %q with no target inside the bound directory, want api",
			implicit.Session)
	}
	if implicit.SessionName != "slabledger--api" {
		t.Errorf("SessionName = %q, want slabledger--api", implicit.SessionName)
	}
	if implicit.RepositoryID != explicit.RepositoryID {
		t.Errorf("the two selections disagree on the repository: %s vs %s",
			implicit.RepositoryID, explicit.RepositoryID)
	}
	if implicit.ID == explicit.ID {
		t.Error("the bound session and the default session share a workspace ID")
	}
}

// TestSelectExplicitSessionIgnoresTheCwd is case 1: an explicit
// <repo>/<session> is final, whatever the cwd is bound to.
func TestSelectExplicitSessionIgnoresTheCwd(t *testing.T) {
	f := newFixture(t)
	bound := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")
	f.bindSession(t, "web", "services/web")

	ws := mustSelect(t, Ref{Present: true, Name: "slabledger", Session: "web", HasSession: true}, f, bound)
	if ws.Session != "web" {
		t.Errorf("Session = %q, want web", ws.Session)
	}
}

// TestSelectTakesTheLongestBind covers nested binds. services/api is deeper
// than services, so a cwd below both belongs to the api session.
func TestSelectTakesTheLongestBind(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api", "cmd"))
	f.bindSession(t, "svc", "services")
	f.bindSession(t, "api", "services/api")

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "api" {
		t.Errorf("Session = %q, want api: the deeper bind wins", ws.Session)
	}
}

// TestSelectAmbiguousBindsAreExit3 covers two sessions bound to one directory.
func TestSelectAmbiguousBindsAreExit3(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")
	f.bindSession(t, "other", "services/api")

	_, err := Select(Ref{}, f.roots, cwd, f.stateRoot)
	var ambiguous *resolve.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Select = %v (%T), want *resolve.AmbiguousError", err, err)
	}
	want := []string{"slabledger/api", "slabledger/other"}
	if len(ambiguous.Candidates) != len(want) {
		t.Fatalf("Candidates = %v, want %v", ambiguous.Candidates, want)
	}
	for i, c := range want {
		if ambiguous.Candidates[i] != c {
			t.Errorf("Candidates = %v, want %v", ambiguous.Candidates, want)
			break
		}
	}
}

// TestSelectDoesNotMatchASiblingNamePrefix is the component-wise comparison a
// string prefix would fail: services/apixyz is not inside services/api.
func TestSelectDoesNotMatchASiblingNamePrefix(t *testing.T) {
	f := newFixture(t)
	mkdir(t, filepath.Join(f.repo, "services", "api"))
	cwd := mkdir(t, filepath.Join(f.repo, "services", "apixyz"))
	f.bindSession(t, "api", "services/api")

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session: services/apixyz is "+
			"a sibling of the bind, not inside it", ws.Session)
	}
}

// TestSelectUnbindableBindIsTreatedAsMissing covers design §4's fallback: a
// stored path replaced by a symlink out of the repository does not claim the
// cwd, and does not fail the command either.
func TestSelectUnbindableBindIsTreatedAsMissing(t *testing.T) {
	f := newFixture(t)
	outside := mkdir(t, filepath.Join(base(t), "outside"))
	if err := os.Symlink(outside, filepath.Join(f.repo, "escaped")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	f.bindSession(t, "api", "escaped")

	ws := mustSelect(t, Ref{}, f, f.repo)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session: a bind that escapes "+
			"the repository is treated as missing", ws.Session)
	}
}

func TestSelectNoBindsFallsBackToTheDefaultSession(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "")

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session", ws.Session)
	}
}

// TestSelectMissingDatabaseFallsBack is the fresh-installation path: nothing
// is registered, so nothing can be bound.
func TestSelectMissingDatabaseFallsBack(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	if _, err := os.Stat(state.DBPath(f.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fixture already has a database; this test proves nothing")
	}

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session", ws.Session)
	}
	if _, err := os.Stat(state.DBPath(f.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the lookup created the state database")
	}
}

// TestSelectPendingMigrationFallsBack covers the other silent fallback. The
// schema version is rolled back through a raw connection because migrating
// forwards is the only thing state.Open will do.
func TestSelectPendingMigrationFallsBack(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")
	setUserVersion(t, state.DBPath(f.stateRoot), state.SchemaVersion-1)

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session: a schema this build "+
			"does not read must not be interpreted", ws.Session)
	}
}

// TestSelectIntegrityFailurePropagates is the case that must not fall back.
// Falling back would silently open the wrong workspace on a corrupt database.
// The corruption is staged the way internal/state/readonly_test.go stages it
// (TestOpenReadOnlyCorruptDatabaseReportsIntegrity): keep the SQLite header so
// the file is still recognizably a database, and scribble over the pages
// behind it.
func TestSelectIntegrityFailurePropagates(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")

	path := state.DBPath(f.stateRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the database: %v", err)
	}
	for i := 100; i < len(raw); i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the corrupt database: %v", err)
	}

	ws, err := Select(Ref{}, f.roots, cwd, f.stateRoot)
	if err == nil {
		t.Fatalf("a corrupt state database selected %q instead of failing", ws.Session)
	}
}

// setUserVersion rewrites PRAGMA user_version through a raw connection.
// state.Open would migrate the database forward again, so the version is set
// on a connection that does not go through it.
func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening the database directly: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
}
