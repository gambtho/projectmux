package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
)

// provisionRepositoryRoot configures the workspace's parent directory as a
// repository root, the same idiom as TestConfigResolvesByWorkspaceName
// (cli_test.go:159-166): bind's target always carries a repository name
// (a bare `bind <path>` cannot mean anything, since bind creates sessions
// rather than resolving one from the cwd), so every test here resolves the
// target by name and needs roots configured, unlike open's or status's
// no-argument form.
func provisionRepositoryRoot(t *testing.T, ws resolve.Workspace) {
	t.Helper()
	rootsYAML := "version: 1\nrepository_roots:\n  - " + filepath.Dir(ws.RepoRoot) + "\n"
	if err := os.WriteFile(
		filepath.Join(os.Getenv("PROJECTMUX_CONFIG_ROOT"), "defaults.yaml"),
		[]byte(rootsYAML), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestBindRecordsThePathRelativeToTheRepository(t *testing.T) {
	ws := openWorkspace(t)
	provisionRepositoryRoot(t, ws)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", "--json", ws.Slug, rel)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, OutputSchemaVersion)
	}
	if env.Bind == nil || *env.Bind != rel {
		t.Errorf("bind = %v, want %q", env.Bind, rel)
	}
	if !env.Created {
		t.Error("created = false; binding an unregistered session should create its record")
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("stored bind = %v, want %q", rec.Bind, rel)
	}
}

// Binding a session that does not exist yet is how a named session is
// declared (spec §4), so the record it creates is that session's, not the
// repository's default one.
func TestBindCreatesANamedSession(t *testing.T) {
	ws := openWorkspace(t)
	provisionRepositoryRoot(t, ws)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", "--json", ws.Slug+"/feature-a", rel)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.Workspace.Session != "feature-a" {
		t.Fatalf("session = %q, want feature-a", env.Workspace.Session)
	}
	if env.Workspace.ID == ws.ID {
		t.Error("the named session was recorded under the default session's ID")
	}
	if _, err := s.Workspace(ws.ID); err == nil {
		t.Error("binding a named session also created the default session's record")
	}
}

// The path defaults to the current directory, which is the repository
// root in these tests.
func TestBindDefaultsToTheCurrentDirectory(t *testing.T) {
	ws := openWorkspace(t)
	provisionRepositoryRoot(t, ws)
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", "--json", ws.Slug)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.Bind == nil || *env.Bind != "." {
		t.Errorf("bind = %v, want %q", env.Bind, ".")
	}
}

func TestBindClearRemovesTheBindAndKeepsTheRecord(t *testing.T) {
	ws := openWorkspace(t)
	provisionRepositoryRoot(t, ws)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)

	if code, _, stderr := run(t, "bind", "--json", ws.Slug, rel); code != 0 {
		t.Fatalf("seeding bind: exit %d, stderr: %s", code, stderr)
	}
	code, stdout, stderr := run(t, "bind", "--clear", "--json", ws.Slug)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.Bind != nil {
		t.Errorf("bind = %v, want null after --clear", env.Bind)
	}
	if env.Created {
		t.Error("created = true, want false: --clear must not re-create the record")
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("--clear removed the record: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("stored bind = %v, want nil", rec.Bind)
	}
}

// Spec §4: the directory must exist at bind time, and the message names
// the path so the typo is visible without re-running anything.
func TestBindRejectsAMissingPath(t *testing.T) {
	ws := openWorkspace(t)
	provisionRepositoryRoot(t, ws)
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", ws.Slug, "services/nope")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "services/nope") {
		t.Errorf("stderr = %q, should name the path", stderr)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if _, err := s.Workspace(ws.ID); err == nil {
		t.Error("a rejected bind created the record anyway")
	}
}

func TestBindRequiresATarget(t *testing.T) {
	openWorkspace(t)
	installOpenStore(t, fake.NewStore())

	code, _, _ := run(t, "bind")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}
