package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// autostartFixture builds a config root, a fake store with a mix of
// workspaces, and container seams. Workspaces: "eligible" (primary,
// autostart on, container applies), "disabled" (primary, autostart
// off), "secondary" (non-primary), "gone" (primary, autostart on,
// worktree deleted).
func autostartFixture(t *testing.T) (*fake.Store, *fake.ContainerActuator) {
	t.Helper()
	base := t.TempDir()
	configRoot := filepath.Join(base, "config")
	if err := os.MkdirAll(filepath.Join(configRoot, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(configRoot, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("defaults.yaml", "version: 1\n")
	write("workspaces/eligible.yaml",
		"version: 1\nautostart: true\ndevcontainer:\n  enabled: true\n")
	write("workspaces/disabled.yaml", "version: 1\n")
	write("workspaces/gone.yaml",
		"version: 1\nautostart: true\ndevcontainer:\n  enabled: true\n")
	t.Setenv("PROJECTMUX_CONFIG_ROOT", configRoot)
	t.Setenv("PROJECTMUX_STATE_ROOT", filepath.Join(base, "state"))

	mkWorktree := func(slug string) string {
		t.Helper()
		dir := filepath.Join(base, "trees", slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	s := fake.NewStore()
	register := func(id, slug, worktree string, primary bool) {
		t.Helper()
		ws := resolve.Workspace{
			ID: id, Slug: slug, Worktree: worktree,
			SessionName: slug, IsPrimary: primary,
		}
		if err := s.RegisterWorkspace(ws, "sha256:"+id, cliTestTime); err != nil {
			t.Fatalf("register %s: %v", slug, err)
		}
	}
	register("w-eligible", "eligible", mkWorktree("eligible"), true)
	register("w-disabled", "disabled", mkWorktree("disabled"), true)
	register("w-secondary", "eligible", mkWorktree("eligible-x2"), false)
	register("w-gone", "gone", filepath.Join(base, "trees", "vanished"), true)

	installOpenStore(t, s)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})
	installScriptedSessions(t) // exhausts on any call: tmux must never be consulted
	return s, installContainerActuator(t)
}

func decodeAutostart(t *testing.T, stdout string) autostartEnvelope {
	t.Helper()
	var env autostartEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding autostart JSON: %v\n%s", err, stdout)
	}
	return env
}

func entryFor(t *testing.T, env autostartEnvelope, slug string) autostartEntry {
	t.Helper()
	for _, e := range env.Workspaces {
		if e.Slug == slug {
			return e
		}
	}
	t.Fatalf("no entry for %s in %+v", slug, env.Workspaces)
	return autostartEntry{}
}

func TestAutostartMatrix(t *testing.T) {
	s, actC := autostartFixture(t)

	code, stdout, stderr := run(t, "autostart", "--json")
	// The vanished worktree makes this a partial failure: exit 1 with
	// the full report on stdout (the spec §5 contract amendment).
	if code != ExitError {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitError, stderr)
	}
	env := decodeAutostart(t, stdout)

	eligible := entryFor(t, env, "eligible")
	if eligible.Outcome != "started" || eligible.ContainerID != "cid-1" {
		t.Errorf("eligible = %+v", eligible)
	}
	if len(actC.Started) != 1 || actC.Started[0] != "w-eligible" {
		t.Errorf("Started = %v", actC.Started)
	}

	disabled := entryFor(t, env, "disabled")
	if disabled.Outcome != "skipped" {
		t.Errorf("disabled = %+v", disabled)
	}

	gone := entryFor(t, env, "gone")
	if gone.Outcome != "failed" || gone.Reason == "" {
		t.Errorf("gone = %+v, want failed with a reason", gone)
	}

	// The non-primary workspace never appears.
	for _, e := range env.Workspaces {
		if e.ID == "w-secondary" {
			t.Errorf("non-primary workspace reported: %+v", e)
		}
	}

	// Autostart recorded operations under its own name.
	rec, err := s.Workspace("w-eligible")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.LastOperation == nil || rec.LastOperation.Name != "autostart" {
		t.Errorf("last operation = %+v, want autostart", rec.LastOperation)
	}
	if !strings.Contains(stderr, "1 workspace(s)") {
		t.Errorf("stderr = %q, want the one-line summary", stderr)
	}
}

func TestAutostartAllHealthyExitsZero(t *testing.T) {
	s, _ := autostartFixture(t)
	// Recreate the vanished worktree so every workspace succeeds.
	rec, err := s.Workspace("w-gone")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if err := os.MkdirAll(rec.Worktree, 0o755); err != nil {
		t.Fatal(err)
	}

	code, stdout, _ := run(t, "autostart", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
}

func TestAutostartRejectsArguments(t *testing.T) {
	code, _, _ := run(t, "autostart", "extra")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}
