package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
)

// isolatedSocket names a per-test tmux server and kills it on cleanup.
func isolatedSocket(t *testing.T, label string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("projectmux-%s-%d", label, os.Getpid())
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

func TestIntegrationSessionsRoundTrip(t *testing.T) {
	socket := isolatedSocket(t, "roundtrip")
	tmuxOK(t, socket, "new-session", "-d", "-s", "alpha")
	tmuxOK(t, socket, "set-option", "-t", "alpha", controller.KeyWorkspaceID, "w1")
	tmuxOK(t, socket, "set-option", "-t", "alpha", controller.KeySlug, "proj")
	tmuxOK(t, socket, "set-option", "-t", "alpha", controller.KeyWorktree, "/w/evil\n$999\npath")
	tmuxOK(t, socket, "new-session", "-d", "-s", "beta")

	live, err := (&Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	byName := map[string]controller.LiveSession{}
	for _, s := range live {
		byName[s.Name] = s
	}
	if len(byName) != 2 {
		t.Fatalf("observed %d sessions, want 2: %+v", len(byName), live)
	}
	alpha := byName["alpha"]
	if alpha.WorkspaceID != "w1" || alpha.Slug != "proj" {
		t.Errorf("alpha identity = %+v, want w1/proj", alpha)
	}
	if alpha.Worktree != "/w/evil\n$999\npath" {
		t.Errorf("alpha worktree = %q; embedded newline or anchor-shaped content did not round-trip",
			alpha.Worktree)
	}
	beta := byName["beta"]
	if beta.WorkspaceID != "" || beta.Slug != "" || beta.Worktree != "" {
		t.Errorf("beta should carry no identity keys: %+v", beta)
	}
}

func TestIntegrationObserveSessionMatches(t *testing.T) {
	socket := isolatedSocket(t, "observe")
	tmuxOK(t, socket, "new-session", "-d", "-s", "mine")
	tmuxOK(t, socket, "set-option", "-t", "mine", controller.KeyWorkspaceID, "w1")
	tmuxOK(t, socket, "new-session", "-d", "-s", "squatter")

	obs, err := (&Client{Socket: socket}).ObserveSession(context.Background(),
		controller.SessionQuery{WorkspaceID: "w1", CandidateNames: []string{"squatter"}})
	if err != nil {
		t.Fatalf("ObserveSession: %v", err)
	}
	if obs.ByIdentity == nil || obs.ByIdentity.Name != "mine" {
		t.Errorf("ByIdentity = %+v, want mine", obs.ByIdentity)
	}
	if len(obs.ByName) != 1 || obs.ByName[0].Name != "squatter" {
		t.Errorf("ByName = %+v, want squatter", obs.ByName)
	}
}

func TestIntegrationNoServerIsAbsence(t *testing.T) {
	socket := isolatedSocket(t, "noserver")
	live, err := (&Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions on a never-started server: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("Sessions = %+v, want none", live)
	}
}
