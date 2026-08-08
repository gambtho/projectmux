package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
)

// linkedWorktree adds a linked worktree to the repository the test is
// running in and returns its canonical path.
//
// The name is derived from the test rather than taken as a fixed value:
// `git worktree add -b` fails on a branch that already exists, so two
// tests sharing a literal name collide, and so does a rerun after a test
// that died before its cleanup. Both the worktree and its branch are
// removed afterwards — this writes into the real repository the suite is
// running in, not a temporary directory.
func linkedWorktree(t *testing.T, suffix string) string {
	t.Helper()
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	name := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()) + "-" + suffix
	path := filepath.Join(repo, ".worktrees", name)
	cmd := exec.Command("git", "worktree", "add", "-b", name, path)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		remove := exec.Command("git", "worktree", "remove", "--force", path)
		remove.Dir = repo
		_ = remove.Run()
		branch := exec.Command("git", "branch", "-D", name)
		branch.Dir = repo
		_ = branch.Run()
	})
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return canonical
}

// TestRebuildCollapsesAMigratedLinkedWorktreeRow drives the second half
// of the documented upgrade against a real repository, a real linked
// worktree, and the real SQLite store. Migration 0002 moves rows verbatim
// and cannot tell the two apart; this is the run that corrects it.
func TestRebuildCollapsesAMigratedLinkedWorktreeRow(t *testing.T) {
	ws := openWorkspace(t)
	worktree := linkedWorktree(t, "1529")

	root, err := state.Root()
	if err != nil {
		t.Fatalf("state.Root: %v", err)
	}
	st, err := state.Open(root)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	// Exactly what 0002 leaves behind: a repository row whose recorded
	// root is a linked worktree. It is registered through the ordinary
	// primitive with a hand-built identity, because no resolver will
	// produce one after Task 1.
	stale := resolve.Workspace{
		ID:           "stale-workspace-id",
		RepositoryID: "stale-repository-id",
		Slug:         ws.Slug,
		RepoRoot:     worktree,
		SessionName:  ws.Slug + "--1529",
	}
	if err := st.RegisterWorkspace(stale, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	installLiveSessions(t, nil, nil)
	installScriptedSessions(t)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"collapsed"`) {
		t.Errorf("report does not record the collapse:\n%s", stdout)
	}

	st, err = state.Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()
	repos, err := st.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("repositories = %+v, want one", repos)
	}
	if repos[0].RepoRoot != ws.RepoRoot {
		t.Errorf("repo_root = %q, want the main worktree %q", repos[0].RepoRoot, ws.RepoRoot)
	}
}

// TestRebuildDiscardsTheCollapsedRowsContainerBinding pins the cascade
// the collapse relies on, against the real schema. Both rows carry a
// binding, which is what 0002 leaves when a worktree and its parent were
// each opened with --container. The stale row's binding goes with the row
// (container_bindings.repository_id is ON DELETE CASCADE), the parent's
// survives, and the container the stale row named keeps running with
// nothing referring to it — so the report has to name it.
func TestRebuildDiscardsTheCollapsedRowsContainerBinding(t *testing.T) {
	ws := openWorkspace(t)
	worktree := linkedWorktree(t, "1529")

	root, err := state.Root()
	if err != nil {
		t.Fatalf("state.Root: %v", err)
	}
	st, err := state.Open(root)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	stale := resolve.Workspace{
		ID:           "stale-workspace-id",
		RepositoryID: "stale-repository-id",
		Slug:         ws.Slug,
		RepoRoot:     worktree,
		SessionName:  ws.Slug + "--1529",
	}
	for _, seed := range []struct {
		ws          resolve.Workspace
		containerID string
	}{{ws, "cid-parent"}, {stale, "cid-stale"}} {
		if err := st.RegisterWorkspace(seed.ws, "sha256:seed", cliTestTime); err != nil {
			t.Fatalf("register %s: %v", seed.ws.RepoRoot, err)
		}
		obs := state.ContainerObservation{
			Kind: "devcontainer", ContainerID: seed.containerID,
			ContainerUser: "vscode", Workdir: "/workspaces/x",
			Health: state.HealthPresent,
		}
		if err := st.RecordContainerObservation(seed.ws.RepositoryID, obs, cliTestTime); err != nil {
			t.Fatalf("record container for %s: %v", seed.ws.RepoRoot, err)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	installLiveSessions(t, nil, nil)
	installScriptedSessions(t)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"binding-discarded"`) ||
		!strings.Contains(stdout, `"cid-stale"`) {
		t.Errorf("report does not name the discarded binding:\n%s", stdout)
	}
	// cid-parent must not be reported as discarded: naming the surviving
	// binding would send the operator after the wrong container.
	if strings.Contains(stdout, `"cid-parent"`) {
		t.Errorf("report names the surviving binding as discarded:\n%s", stdout)
	}

	st, err = state.Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()
	repos, err := st.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("repositories = %+v, want one", repos)
	}
	if repos[0].Container == nil || repos[0].Container.ContainerID != "cid-parent" {
		t.Errorf("binding = %+v, want the parent's cid-parent to survive", repos[0].Container)
	}
}

// TestRebuildRetagsALiveSessionOntoItsRepository drives the retag against
// a real tmux server on its own socket, the isolation the override exists
// for (socket_integration_test.go). The session carries the keys a
// pre-change projectmux wrote: an ID derived from the linked worktree,
// and @dev_worktree pointing at it.
func TestRebuildRetagsALiveSessionOntoItsRepository(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	ws := openWorkspace(t)
	worktree := linkedWorktree(t, "1529")

	socket := "projectmux-migrate-" + t.Name()
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	tmuxRun := func(args ...string) {
		t.Helper()
		full := append([]string{"-L", socket}, args...)
		if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}
	name := ws.Slug + "--1529"
	tmuxRun("new-session", "-d", "-s", name)
	tmuxRun("set-option", "-t", name, controller.KeyWorkspaceID, "stale-workspace-id")
	tmuxRun("set-option", "-t", name, controller.KeySlug, ws.Slug)
	tmuxRun("set-option", "-t", name, controller.KeyWorktree, worktree)
	t.Setenv(tmux.SocketEnv, socket)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	got := func(key string) string {
		t.Helper()
		out, err := exec.Command("tmux", "-L", socket,
			"show-options", "-v", "-t", name, key).Output()
		if err != nil {
			t.Fatalf("show-options %s: %v", key, err)
		}
		return strings.TrimSpace(string(out))
	}
	if got(controller.KeyWorkspaceID) != ws.ID {
		t.Errorf("@dev_workspace_id = %q, want the repository's ID %q",
			got(controller.KeyWorkspaceID), ws.ID)
	}
	if got(controller.KeyWorktree) != ws.RepoRoot {
		t.Errorf("@dev_worktree = %q, want the repository root %q",
			got(controller.KeyWorktree), ws.RepoRoot)
	}
	if !strings.Contains(stdout, `"retagged"`) {
		t.Errorf("report does not record the retag:\n%s", stdout)
	}
}
