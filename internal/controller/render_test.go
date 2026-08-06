package controller

import (
	"context"
	"path"
	"testing"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// renderPaneActuator is a minimal ContainerActuator for renderWindows
// tests: it renders a deterministic exec marker so tests can assert the
// pane's command without real docker argv.
type renderPaneActuator struct{}

func (renderPaneActuator) StartContainer(context.Context, resolve.Workspace, config.Config) (ContainerObservation, error) {
	return ContainerObservation{}, nil
}

func (renderPaneActuator) ExecCommand(b state.ContainerBinding, command, relDir string, env map[string]string) string {
	return "fake-exec " + b.ContainerID + " " + path.Join(b.Workdir, relDir) + " " + command
}

func (renderPaneActuator) StopContainer(context.Context, string) error {
	return nil
}

func TestRenderWindowsHostPanes(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{Worktree: "/w/slab"}}
	intents := []WindowIntent{{
		Name:    "dev",
		Command: "claude",
		Panes: []PaneIntent{
			{Name: "shell"},
			{Name: "logs", Command: "tail -f dev.log", RelDir: "services/api", Focus: true},
		},
	}}
	specs, err := renderWindows(intents, d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	panes := specs[0].Panes
	if len(panes) != 2 {
		t.Fatalf("panes = %+v", panes)
	}
	if panes[0].Dir != "/w/slab" || panes[0].Command != "" {
		t.Errorf("default pane should be a shell in the window's dir, got %+v", panes[0])
	}
	if panes[1].Dir != "/w/slab/services/api" || !panes[1].Focus {
		t.Errorf("explicit pane cwd/focus not rendered, got %+v", panes[1])
	}
}

func TestRenderWindowsPaneInheritsWindowDir(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{Worktree: "/w/slab"}}
	intents := []WindowIntent{{
		Name:   "api",
		RelDir: "services/api",
		Panes:  []PaneIntent{{Name: "shell"}},
	}}
	specs, err := renderWindows(intents, d, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := specs[0].Panes[0].Dir; got != "/w/slab/services/api" {
		t.Errorf("a pane without cwd must inherit the window's directory, got %q", got)
	}
}

func TestRenderWindowsContainerPanes(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{Worktree: "/w/slab"},
		Config: config.Config{Environment: map[string]string{"K": "v"}}}
	obs := &ContainerObservation{Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab"}
	act := renderPaneActuator{}
	intents := []WindowIntent{{
		Name:     "dev",
		Command:  "claude",
		Location: WindowContainer,
		Panes:    []PaneIntent{{Name: "shell", RelDir: "sub"}},
	}}
	specs, err := renderWindows(intents, d, obs, act)
	if err != nil {
		t.Fatal(err)
	}
	pane := specs[0].Panes[0]
	if pane.Dir != "/w/slab" {
		t.Errorf("container pane host dir should be the worktree, got %q", pane.Dir)
	}
	want := act.ExecCommand(state.ContainerBinding{
		Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab",
	}, "", "sub", d.Config.Environment)
	if pane.Command != want {
		t.Errorf("container pane command = %q, want the exec rendering %q", pane.Command, want)
	}
}
