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
func linkedWorktree(t *testing.T, name string) string {
	t.Helper()
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	path := filepath.Join(repo, ".worktrees", name)
	cmd := exec.Command("git", "worktree", "add", "-b", name, path)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
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
