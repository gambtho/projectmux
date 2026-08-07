package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

func TestIntegrationCreateSessionWithPanes(t *testing.T) {
	socket := isolatedSocket(t, "panes")
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}

	spec := controller.SessionSpec{
		Name:        "twopane",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    dir,
		Env:         map[string]string{"PANE_PROBE": "yes"},
		Windows: []controller.WindowSpec{
			{Name: "dev", Dir: dir, Focus: true, Panes: []controller.PaneSpec{
				{Name: "shell", Dir: sub, Focus: true},
			}},
			{Name: "solo", Dir: dir},
		},
	}
	if err := (&Client{Socket: socket}).CreateSession(context.Background(), spec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	panes := func(window string) []string {
		out, err := exec.Command("tmux", "-L", socket, "list-panes", "-t",
			"twopane:"+window, "-F",
			"#{pane_current_path}|#{pane_active}").CombinedOutput()
		if err != nil {
			t.Fatalf("list-panes %s: %v\n%s", window, err, out)
		}
		return strings.Split(strings.TrimSpace(string(out)), "\n")
	}

	dev := panes("dev")
	if len(dev) != 2 {
		t.Fatalf("dev panes = %v, want 2", dev)
	}
	if dev[0] != dir+"|0" {
		t.Errorf("primary pane = %q, want %q (unfocused, worktree cwd)", dev[0], dir+"|0")
	}
	if dev[1] != sub+"|1" {
		t.Errorf("split pane = %q, want %q (focused, sub cwd)", dev[1], sub+"|1")
	}
	if solo := panes("solo"); len(solo) != 1 {
		t.Errorf("solo panes = %v, want 1 (empty pane list)", solo)
	}

	// The -h split must yield two side-by-side panes of (near-)equal
	// width; a one-column difference absorbs an odd total width.
	widthsOut, err := exec.Command("tmux", "-L", socket, "list-panes", "-t",
		"twopane:dev", "-F", "#{pane_width}").CombinedOutput()
	if err != nil {
		t.Fatalf("list-panes widths: %v\n%s", err, widthsOut)
	}
	widths := strings.Fields(strings.TrimSpace(string(widthsOut)))
	if len(widths) != 2 {
		t.Fatalf("widths = %v", widths)
	}
	w0, _ := strconv.Atoi(widths[0])
	w1, _ := strconv.Atoi(widths[1])
	if diff := w0 - w1; diff < -1 || diff > 1 {
		t.Errorf("pane widths %d/%d are not an even -h split", w0, w1)
	}

	// The configured environment must be visible INSIDE the split pane:
	// the session table is populated by new-session -e alone, so probing
	// it would not prove split-window -e worked (the existing env
	// integration test documents the same limitation and uses this
	// send-keys probe). The split pane is dev's active pane (Focus: true),
	// so the bare window target addresses it without a pane index.
	if err := exec.Command("tmux", "-L", socket, "send-keys", "-t",
		"twopane:dev",
		"printf 'PROBE=%s\\n' \"$PANE_PROBE\"", "Enter").Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	found := false
	for i := 0; i < 50 && !found; i++ {
		out, err := exec.Command("tmux", "-L", socket, "capture-pane", "-p",
			"-t", "twopane:dev").Output()
		if err == nil && strings.Contains(string(out), "PROBE=yes") {
			found = true
			break
		}
		exec.Command("sleep", "0.1").Run()
	}
	if !found {
		t.Error("PANE_PROBE never appeared inside the split pane; split-window -e did not deliver the environment")
	}
}

// TestIntegrationSocketPathMatchesTmux grounds SocketPath in tmux's own
// behavior rather than in a restatement of the rule: it asks a real
// server where its socket is. The refusal in internal/cli compares this
// path against $TMUX, so a divergence here would silently turn a
// cross-server refusal into a mismatch that never fires.
func TestIntegrationSocketPathMatchesTmux(t *testing.T) {
	socket := isolatedSocket(t, "socketpath")
	tmuxOK(t, socket, "new-session", "-d", "-s", "alpha")

	out, err := exec.Command("tmux", "-L", socket,
		"display-message", "-p", "#{socket_path}").Output()
	if err != nil {
		t.Fatalf("asking tmux for its socket path: %v", err)
	}
	reported := strings.TrimSpace(string(out))
	if got := SocketPath(socket); got != reported {
		t.Errorf("SocketPath(%q) = %q, tmux reports %q", socket, got, reported)
	}
}
