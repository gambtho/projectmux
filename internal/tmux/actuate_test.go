package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
)

func actuateSpec() controller.SessionSpec {
	return controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    "/w/slab",
		Env:         map[string]string{"B_KEY": "2", "A_KEY": "1"},
		Windows: []controller.WindowSpec{
			{Name: "agent-1", Command: "claude", Dir: "/w/slab", Focus: true},
			{Name: "shell", Dir: "/w/slab/sub"},
		},
	}
}

func TestCreateArgvShape(t *testing.T) {
	argv := createArgv(actuateSpec())
	joined := strings.Join(argv, " ")

	for _, want := range []string{
		"new-session -d -s slab -n agent-1 -c /w/slab -e A_KEY=1 -e B_KEY=2 claude",
		"; set-option -t slab @dev_workspace_id w1",
		"; set-option -t slab @dev_slug proj",
		"; set-option -t slab @dev_worktree /w/slab",
		"; new-window -d -t slab -n shell -c /w/slab/sub -e A_KEY=1 -e B_KEY=2",
		"; select-window -t slab:agent-1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv %q\nmissing %q", joined, want)
		}
	}
	if strings.Count(joined, "select-window") != 1 {
		t.Errorf("select-window should appear exactly once: %q", joined)
	}
}

func TestCreateArgvNoFocusMeansNoSelect(t *testing.T) {
	spec := actuateSpec()
	spec.Windows[0].Focus = false
	argv := createArgv(spec)
	if slices.Contains(argv, "select-window") {
		t.Errorf("select-window emitted without an explicit focus: %v", argv)
	}
}

func TestCreateArgvShellWindowHasNoCommand(t *testing.T) {
	spec := actuateSpec()
	spec.Windows = []controller.WindowSpec{{Name: "shell", Dir: "/w/slab"}}
	spec.Env = nil
	argv := createArgv(spec)
	joined := strings.Join(argv, " ")
	if !strings.HasPrefix(joined, "new-session -d -s slab -n shell -c /w/slab ;") {
		t.Errorf("argv = %q", joined)
	}
}

func TestEscapeChainArg(t *testing.T) {
	cases := map[string]string{
		"plain":      "plain",
		"trailing;":  `trailing\;`,
		`trailing\;`: `trailing\\;`,
		`trailing\`:  `trailing\`,
		`trailing\\`: `trailing\\`,
		";":          `\;`,
		"mid;dle":    "mid;dle",
		"":           "",
	}
	for in, want := range cases {
		if got := escapeChainArg(in); got != want {
			t.Errorf("escapeChainArg(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCreateArgvEscapesTrailingSemicolons(t *testing.T) {
	spec := actuateSpec()
	spec.Env = map[string]string{"KEY": "value;"}
	spec.Windows[0].Command = "run;"
	spec.Windows[0].Dir = "/w/slab;"
	spec.Windows[0].Name = "agent1;"
	argv := createArgv(spec)

	want := []string{
		"-n", `agent1\;`,
		"-c", `/w/slab\;`,
	}
	joined := strings.Join(argv, " ")
	for i := 0; i < len(want); i += 2 {
		if !strings.Contains(joined, want[i]+" "+want[i+1]) {
			t.Errorf("argv %q missing %q %q", joined, want[i], want[i+1])
		}
	}
	if !strings.Contains(joined, `-e KEY=value\;`) {
		t.Errorf("argv %q missing escaped env value", joined)
	}
	if !strings.Contains(joined, `run\;`) {
		t.Errorf("argv %q missing escaped window command", joined)
	}
}

func TestCreateSessionRejectsZeroWindows(t *testing.T) {
	c := &Client{}
	err := c.CreateSession(context.Background(), controller.SessionSpec{Name: "x"})
	if err == nil {
		t.Fatal("a zero-window spec was accepted")
	}
}

func TestIntegrationCreateSession(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("projectmux-actuate-%d", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	dir := t.TempDir()
	spec := controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    dir,
		Env:         map[string]string{"PROJECTMUX_TEST_ENV": "visible"},
		Windows: []controller.WindowSpec{
			{Name: "first", Dir: dir, Focus: true},
			{Name: "second", Dir: dir},
		},
	}
	c := &Client{Socket: socket}
	if err := c.CreateSession(context.Background(), spec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	live, err := c.Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != "slab" || live[0].WorkspaceID != "w1" ||
		live[0].Slug != "proj" || live[0].Worktree != dir {
		t.Fatalf("observed = %+v", live)
	}

	out, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", "slab",
		"-F", "#{window_name} #{window_active}").Output()
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	windows := strings.TrimSpace(string(out))
	if !strings.Contains(windows, "first 1") || !strings.Contains(windows, "second 0") {
		t.Errorf("windows = %q, want first active and second inactive", windows)
	}

	env, err := exec.Command("tmux", "-L", socket, "show-environment", "-t", "slab").Output()
	if err == nil && strings.Contains(string(env), "PROJECTMUX_TEST_ENV=visible") {
		return // session-level env visible; done
	}
	// -e sets pane environment rather than the session table on some
	// versions; verify via a pane instead.
	if err := exec.Command("tmux", "-L", socket, "send-keys", "-t", "slab:first",
		"printf 'ENVRESULT=%s\\n' \"$PROJECTMUX_TEST_ENV\"", "Enter").Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	deadline := 50
	for i := 0; i < deadline; i++ {
		out, err := exec.Command("tmux", "-L", socket, "capture-pane", "-p",
			"-t", "slab:first").Output()
		if err == nil && strings.Contains(string(out), "ENVRESULT=visible") {
			return
		}
		exec.Command("sleep", "0.1").Run()
	}
	t.Error("PROJECTMUX_TEST_ENV did not reach the first window's pane")
}

// TestIntegrationCreateSessionEnvValueEndingInSemicolon proves the
// escapeChainArg fix on real tmux: an env value ending in ";" must reach
// the pane byte-for-byte rather than being silently truncated by tmux's
// chained-command parser (open/attach spec §4).
func TestIntegrationCreateSessionEnvValueEndingInSemicolon(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("projectmux-actuate-semi-%d", os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	dir := t.TempDir()
	spec := controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    dir,
		Env:         map[string]string{"PROJECTMUX_TEST_ENV": "visible;"},
		Windows:     []controller.WindowSpec{{Name: "first", Dir: dir, Focus: true}},
	}
	c := &Client{Socket: socket}
	if err := c.CreateSession(context.Background(), spec); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	if err := exec.Command("tmux", "-L", socket, "send-keys", "-t", "slab:first",
		"printf 'ENVRESULT=%s\\n' \"$PROJECTMUX_TEST_ENV\"", "Enter").Run(); err != nil {
		t.Fatalf("send-keys: %v", err)
	}
	deadline := 50
	for i := 0; i < deadline; i++ {
		out, err := exec.Command("tmux", "-L", socket, "capture-pane", "-p",
			"-t", "slab:first").Output()
		if err == nil && strings.Contains(string(out), "ENVRESULT=visible;") {
			return
		}
		exec.Command("sleep", "0.1").Run()
	}
	t.Error("env value ending in ';' did not reach the pane verbatim")
}
