package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/tmux"
)

// TestIntegrationSocketOverrideIsolatesObservation is the property the
// PROJECTMUX_TMUX_SOCKET override exists for (design §13 step 6): a
// session on another server stays invisible even though it carries the
// exact three identity keys projectmux keys on. Without the override,
// side-by-side validation would observe — and could mutate — the
// sessions of the installation it is meant to leave alone.
//
// It exercises the production seam, not a test-supplied client: the
// only thing selecting the server here is the environment variable.
func TestIntegrationSocketOverrideIsolatesObservation(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	other := socketWithIdentitySession(t, "other", "neighbor")
	ours := socketWithIdentitySession(t, "ours", "mine")

	names := func(socket string) []string {
		t.Helper()
		t.Setenv(tmux.SocketEnv, socket)
		live, err := liveSessions(context.Background())
		if err != nil {
			t.Fatalf("liveSessions on %q: %v", socket, err)
		}
		var out []string
		for _, s := range live {
			out = append(out, s.Name)
		}
		return out
	}

	if got := names(ours); len(got) != 1 || got[0] != "mine" {
		t.Errorf("sessions on our socket = %v, want [mine] only", got)
	}
	if got := names(other); len(got) != 1 || got[0] != "neighbor" {
		t.Errorf("sessions on the other socket = %v, want [neighbor] only", got)
	}
}

// socketWithIdentitySession starts an isolated tmux server holding one
// session tagged with all three identity keys, and kills it on cleanup.
func socketWithIdentitySession(t *testing.T, label, session string) string {
	t.Helper()
	socket := fmt.Sprintf("projectmux-sock-%s-%d", label, os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	run := func(args ...string) {
		t.Helper()
		full := append([]string{"-L", socket}, args...)
		if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}
	run("new-session", "-d", "-s", session)
	run("set-option", "-t", session, controller.KeyWorkspaceID, "w-"+label)
	run("set-option", "-t", session, controller.KeySlug, "slabledger")
	run("set-option", "-t", session, controller.KeyWorktree, "/w/"+label)
	return socket
}
