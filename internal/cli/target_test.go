package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller/fake"
)

// Every command that takes a workspace argument parses it with the same
// grammar. The bare form is included because `projectmux <target>` is the
// open shorthand (cli.go:157-163), which is where a tab-completed
// filename actually lands.
func TestMalformedTargetIsAUsageErrorForEveryCommand(t *testing.T) {
	for _, argv := range [][]string{
		{"open", "slabledger/"},
		{"attach", "slabledger/"},
		{"stop", "slabledger/"},
		{"status", "slabledger/"},
		{"config", "slabledger/"},
		{"docs/commands.md"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			openWorkspace(t)
			installOpenStore(t, fake.NewStore())

			code, stdout, stderr := run(t, argv...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("a failing command wrote to stdout: %q", stdout)
			}
		})
	}
}

// A well-formed target naming a session resolves to that session rather
// than to the repository's default one. config is the cheapest command to
// prove it with: it resolves and reports identity without touching tmux.
func TestNamedSessionTargetResolvesToThatSession(t *testing.T) {
	slug := openWorkspace(t).Slug

	// openWorkspace's defaults.yaml carries no repository_roots, so a
	// Present-with-Name target — this test's whole point — cannot resolve
	// through it: resolve.byName (frozen, resolve.go:131-144) requires a
	// configured root even for a name matching the repository already sitting
	// under cwd. Provision one root here, the same way cli_test.go:166-173
	// does for TestConfigResolvesByWorkspaceName, rather than widening the
	// shared openWorkspace helper for every other test that calls it.
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rootsYAML := "version: 1\nrepository_roots:\n  - " + filepath.Dir(repo) + "\n"
	if err := os.WriteFile(
		filepath.Join(os.Getenv("PROJECTMUX_CONFIG_ROOT"), "defaults.yaml"),
		[]byte(rootsYAML), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	installOpenStore(t, fake.NewStore())

	code, stdout, stderr := run(t, "config", "--json", slug+"/feature-a")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	var env envelope
	decodeJSON(t, stdout, &env)
	if env.Workspace.Session != "feature-a" {
		t.Errorf("session = %q, want %q", env.Workspace.Session, "feature-a")
	}
	if env.Workspace.SessionName != slug+"--feature-a" {
		t.Errorf("session_name = %q, want %q", env.Workspace.SessionName, slug+"--feature-a")
	}
}

func decodeJSON(t *testing.T, stdout string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(stdout), into); err != nil {
		t.Fatalf("decoding JSON: %v\n%s", err, stdout)
	}
}
