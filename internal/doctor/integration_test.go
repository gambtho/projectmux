package doctor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/tmux"
)

// isolatedSocket names a per-test tmux server and kills it on cleanup.
func isolatedSocket(t *testing.T, label string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("projectmux-doctor-%s-%d", label, os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	return socket
}

func tmuxOK(t *testing.T, socket string, args ...string) {
	t.Helper()
	full := append([]string{"-L", socket}, args...)
	if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
		t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
}

// startIdentitySession creates a detached session carrying the identity
// keys projectmux stamps onto the sessions it opens.
func startIdentitySession(t *testing.T, socket, name, workspaceID, slug, worktree string) {
	t.Helper()
	tmuxOK(t, socket, "new-session", "-d", "-s", name)
	tmuxOK(t, socket, "set-option", "-t", name, controller.KeyWorkspaceID, workspaceID)
	tmuxOK(t, socket, "set-option", "-t", name, controller.KeySlug, slug)
	tmuxOK(t, socket, "set-option", "-t", name, controller.KeyWorktree, worktree)
}

// TestIntegrationOrphanedSessionsAgainstRealTmux exercises the orphan
// check end to end: the sessions come from a real tmux server rather than
// a script, so the identity round-trip is part of the assertion.
func TestIntegrationOrphanedSessionsAgainstRealTmux(t *testing.T) {
	socket := isolatedSocket(t, "orphans")

	// A registered workspace whose worktree still exists.
	healthy := t.TempDir()
	store := seedStore(t, "healthy", healthy)
	startIdentitySession(t, socket, "healthy", "id-healthy", "healthy", healthy)

	// A registered workspace whose worktree has since been deleted.
	gone := filepath.Join(t.TempDir(), "deleted")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	register(t, store, "gone", gone)
	startIdentitySession(t, socket, "gone", "id-gone", "gone", gone)
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove: %v", err)
	}

	// A session claiming a workspace the database never registered.
	startIdentitySession(t, socket, "rogue", "id-rogue", "rogue", healthy)

	// A session with no identity keys at all is somebody else's.
	tmuxOK(t, socket, "new-session", "-d", "-s", "bystander")

	r := &Runner{
		Store:    store,
		Sessions: &tmux.Client{Socket: socket},
	}
	check := r.orphanedSessions(context.Background())
	if check.Status != StatusWarn {
		t.Errorf("check status = %q, want warn: %+v", check.Status, check)
	}
	if len(check.Items) != 3 {
		t.Fatalf("%d items, want 3 (healthy, gone, rogue): %+v", len(check.Items), check.Items)
	}

	if item := findItem(t, check, "healthy"); item.Status != StatusOK {
		t.Errorf("healthy item = %+v", item)
	}
	if item := findItem(t, check, "gone"); item.Status != StatusWarn ||
		!strings.Contains(item.Detail, "worktree no longer exists") {
		t.Errorf("gone item = %+v", item)
	}
	if item := findItem(t, check, "rogue"); item.Status != StatusWarn ||
		!strings.Contains(item.Detail, "not registered") {
		t.Errorf("rogue item = %+v", item)
	}
}

func TestIntegrationOrphanedSessionsCleanInstallation(t *testing.T) {
	socket := isolatedSocket(t, "clean")
	worktree := t.TempDir()
	store := seedStore(t, "healthy", worktree)
	startIdentitySession(t, socket, "healthy", "id-healthy", "healthy", worktree)

	r := &Runner{Store: store, Sessions: &tmux.Client{Socket: socket}}
	if check := r.orphanedSessions(context.Background()); check.Status != StatusOK {
		t.Errorf("check = %+v, want ok", check)
	}
}
