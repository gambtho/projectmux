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
// repositories, and container seams. Repositories: "eligible" (autostart
// on, container applies, and carrying two sessions), "disabled"
// (autostart off), "gone" (autostart on, repository root deleted).
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

	mkRepo := func(slug string) string {
		t.Helper()
		dir := filepath.Join(base, "repos", slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	s := fake.NewStore()
	register := func(id, repoID, slug, repoRoot, session, sessionName string) {
		t.Helper()
		ws := resolve.Workspace{
			ID: id, RepositoryID: repoID, Slug: slug, RepoRoot: repoRoot,
			Session: session, SessionName: sessionName,
		}
		if err := s.RegisterWorkspace(ws, "sha256:"+id, cliTestTime); err != nil {
			t.Fatalf("register %s: %v", sessionName, err)
		}
	}
	eligible := mkRepo("eligible")
	register("w-eligible", "r-eligible", "eligible", eligible, "", "eligible")
	// A second session on the same repository. Autostart must still start
	// that repository's container exactly once: the guarantee is_primary
	// used to provide by filtering, now provided by the row count.
	register("w-eligible-2", "r-eligible", "eligible", eligible, "feature-a", "eligible--feature-a")
	register("w-disabled", "r-disabled", "disabled", mkRepo("disabled"), "", "disabled")
	register("w-gone", "r-gone", "gone", filepath.Join(base, "repos", "vanished"), "", "gone")

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
	for _, e := range env.Repositories {
		if e.Slug == slug {
			return e
		}
	}
	t.Fatalf("no entry for %s in %+v", slug, env.Repositories)
	return autostartEntry{}
}

func bindingFor(t *testing.T, s *fake.Store, repositoryID string) *state.ContainerBinding {
	t.Helper()
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, repo := range repos {
		if repo.ID == repositoryID {
			return repo.Container
		}
	}
	t.Fatalf("no repository %s in %+v", repositoryID, repos)
	return nil
}

func TestAutostartMatrix(t *testing.T) {
	s, actC := autostartFixture(t)

	code, stdout, stderr := run(t, "autostart", "--json")
	// The vanished repository root makes this a partial failure: exit 1
	// with the full report on stdout (the spec §5 contract amendment).
	if code != ExitError {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitError, stderr)
	}
	env := decodeAutostart(t, stdout)

	eligible := entryFor(t, env, "eligible")
	if eligible.ID != "r-eligible" || eligible.Outcome != "started" || eligible.ContainerID != "cid-1" {
		t.Errorf("eligible = %+v", eligible)
	}
	// One start for the repository, not one per session on it.
	if len(actC.Started) != 1 || actC.Started[0] != "r-eligible" {
		t.Errorf("Started = %v, want exactly one start for r-eligible", actC.Started)
	}
	seen := 0
	for _, e := range env.Repositories {
		if e.ID == "r-eligible" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("r-eligible appears %d times in the report, want once", seen)
	}

	if disabled := entryFor(t, env, "disabled"); disabled.Outcome != "skipped" {
		t.Errorf("disabled = %+v", disabled)
	}
	if gone := entryFor(t, env, "gone"); gone.Outcome != "failed" || gone.Reason == "" {
		t.Errorf("gone = %+v, want failed with a reason", gone)
	}

	// The binding lands on the repository, which is what owns it.
	if b := bindingFor(t, s, "r-eligible"); b == nil || b.ContainerID != "cid-1" {
		t.Errorf("binding = %+v, want cid-1 on the repository", b)
	}
	if !strings.Contains(stderr, "1 repository(ies)") {
		t.Errorf("stderr = %q, want the one-line summary", stderr)
	}
}

func TestAutostartAllHealthyExitsZero(t *testing.T) {
	s, _ := autostartFixture(t)
	// Recreate the vanished repository root so every repository succeeds.
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, repo := range repos {
		if repo.ID != "r-gone" {
			continue
		}
		if err := os.MkdirAll(repo.RepoRoot, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	code, stdout, _ := run(t, "autostart", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
}

// Migration 0002 promotes every stored path to a repository row, so a
// linked worktree that used to be its own workspace becomes a repository
// row of its own until `rebuild` collapses it. The systemd unit runs
// unattended and can fall in that window: without the skip it would start
// one container per worktree, which is exactly what making the repository
// the unit of a container was supposed to prevent.
//
// This uses a real repository and a real linked worktree rather than the
// path-only fixture above, because the predicate is a git question and a
// fake resolver would only re-assert the answer the test is checking.
func TestAutostartSkipsALinkedWorktreeRow(t *testing.T) {
	workspace(t, map[string]string{
		"defaults.yaml": "version: 1\n",
		"workspaces/slabledger.yaml": "version: 1\nautostart: true\n" +
			"devcontainer:\n  enabled: true\n",
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	main, err := resolve.Resolve("", "", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	worktree := linkedWorktree(t, "1529")

	s := fake.NewStore()
	for _, seed := range []resolve.Workspace{
		main,
		// What 0002 leaves behind: a second repository row whose recorded
		// root is a linked worktree of the first.
		{
			ID: "stale-workspace-id", RepositoryID: "stale-repository-id",
			Slug: main.Slug, RepoRoot: worktree, SessionName: main.Slug + "--1529",
		},
	} {
		if err := s.RegisterWorkspace(seed, "sha256:seed", cliTestTime); err != nil {
			t.Fatalf("register %s: %v", seed.RepoRoot, err)
		}
	}
	installOpenStore(t, s)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})
	installScriptedSessions(t)
	actC := installContainerActuator(t)

	code, stdout, stderr := run(t, "autostart", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if len(actC.Started) != 1 || actC.Started[0] != main.RepositoryID {
		t.Errorf("Started = %v, want exactly one start for the repository %s",
			actC.Started, main.RepositoryID)
	}

	env := decodeAutostart(t, stdout)
	var stale, started *autostartEntry
	for i := range env.Repositories {
		switch env.Repositories[i].ID {
		case "stale-repository-id":
			stale = &env.Repositories[i]
		case main.RepositoryID:
			started = &env.Repositories[i]
		}
	}
	if stale == nil || stale.Outcome != "skipped" {
		t.Fatalf("worktree row = %+v, want skipped", stale)
	}
	// The reason has to send the operator somewhere: a silent skip on the
	// first boot after an upgrade is indistinguishable from a broken unit.
	if !strings.Contains(stale.Reason, "rebuild") {
		t.Errorf("reason = %q, want it to name rebuild", stale.Reason)
	}
	if started == nil || started.Outcome != "started" {
		t.Errorf("repository row = %+v, want started", started)
	}
}

func TestAutostartRejectsArguments(t *testing.T) {
	code, _, _ := run(t, "autostart", "extra")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}
