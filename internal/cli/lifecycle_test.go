package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/tmux"
)

// lifecycleRig points the CLI seams at a real tmux server on an
// isolated socket and the real SQLite store in a temp state root, then
// runs commands through Main exactly as a user would.
func lifecycleRig(t *testing.T, label string) (resolve.Workspace, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	workspace(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/slabledger.yaml": validConfig,
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())

	socket := fmt.Sprintf("projectmux-life-%s-%d", label, os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	origObs, origAct := newSessionObserver, newSessionActuator
	t.Cleanup(func() { newSessionObserver, newSessionActuator = origObs, origAct })
	newSessionObserver = func() controller.SessionObserver {
		return &tmux.Client{Socket: socket}
	}
	newSessionActuator = func() controller.SessionActuator {
		return &tmux.Client{Socket: socket}
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	ws, err := resolve.Resolve("", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ws, socket
}

func openJSON(t *testing.T) openEnvelope {
	t.Helper()
	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("open exit %d, stderr: %s", code, stderr)
	}
	var env openEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding: %v\n%s", err, stdout)
	}
	return env
}

func TestLifecycleOpenReopenAttach(t *testing.T) {
	ws, socket := lifecycleRig(t, "openreopen")

	created := openJSON(t)
	if created.Action != "created" || created.Session != ws.SessionName {
		t.Fatalf("first open = %+v", created)
	}

	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].WorkspaceID != ws.ID || live[0].Worktree != ws.Worktree {
		t.Fatalf("live = %+v", live)
	}

	reopened := openJSON(t)
	if reopened.Action != "already-running" || reopened.Session != ws.SessionName {
		t.Errorf("reopen = %+v, want already-running (idempotent reopen)", reopened)
	}
	if reopened.Drifted {
		t.Errorf("reopen drifted; creation should have recorded the applied digest")
	}

	code, stdout, stderr := run(t, "attach", "--json")
	if code != 0 {
		t.Fatalf("attach exit %d, stderr: %s", code, stderr)
	}
	var att attachEnvelope
	if err := json.Unmarshal([]byte(stdout), &att); err != nil {
		t.Fatalf("decoding attach: %v", err)
	}
	if att.Session.State != "live" || att.Session.Identity == nil ||
		*att.Session.Identity != "match" {
		t.Errorf("attach = %+v", att.Session)
	}
}

// TestLifecycleAdoptsPhaseOneSession is design §13 step 7: a live
// session created outside projectmux but carrying the three identity
// keys is adopted — never recreated, renamed, or wrongly attached.
func TestLifecycleAdoptsPhaseOneSession(t *testing.T) {
	ws, socket := lifecycleRig(t, "adopt")

	for _, args := range [][]string{
		{"new-session", "-d", "-s", "bash-era", "-c", ws.Worktree},
		{"set-option", "-t", "bash-era", controller.KeyWorkspaceID, ws.ID},
		{"set-option", "-t", "bash-era", controller.KeySlug, ws.Slug},
		{"set-option", "-t", "bash-era", controller.KeyWorktree, ws.Worktree},
	} {
		full := append([]string{"-L", socket}, args...)
		if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}

	env := openJSON(t)
	if env.Action != "adopted" || env.Session != "bash-era" {
		t.Fatalf("open = %+v, want adoption of bash-era", env)
	}
	if !env.Drifted {
		t.Error("adoption cleared drift without applying configuration")
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != "bash-era" {
		t.Errorf("live = %+v; adoption created or renamed sessions", live)
	}
}

func TestLifecycleForeignSquatterRefuses(t *testing.T) {
	ws, socket := lifecycleRig(t, "squat")

	full := []string{"-L", socket, "new-session", "-d", "-s", ws.SessionName}
	if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
		t.Fatalf("seeding the squatter: %v\n%s", err, out)
	}

	code, stdout, stderr := run(t, "open", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d (stdout %q, stderr %q)", code, ExitRefused, stdout, stderr)
	}

	code, _, _ = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
}

func TestLifecycleConcurrentOpensCreateExactlyOnce(t *testing.T) {
	ws, socket := lifecycleRig(t, "race")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, _ := run(t, "open", "--json")
			codes[i] = code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != 0 {
			t.Errorf("open %d exited %d", i, code)
		}
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].WorkspaceID != ws.ID {
		t.Errorf("live = %+v, want exactly one session", live)
	}
}
