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
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"}}
	intents := []WindowIntent{{
		Name:    "dev",
		Command: "claude",
		Panes: []PaneIntent{
			{Name: "shell"},
			{Name: "logs", Command: "tail -f dev.log", RelDir: "services/api", Focus: true},
		},
	}}
	specs, err := renderWindows(intents, d, bindBase{Host: "/w/slab"}, nil, nil)
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
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"}}
	intents := []WindowIntent{{
		Name:   "api",
		RelDir: "services/api",
		Panes:  []PaneIntent{{Name: "shell"}},
	}}
	specs, err := renderWindows(intents, d, bindBase{Host: "/w/slab"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := specs[0].Panes[0].Dir; got != "/w/slab/services/api" {
		t.Errorf("a pane without cwd must inherit the window's directory, got %q", got)
	}
}

func TestRenderWindowsContainerPanes(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"},
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
	specs, err := renderWindows(intents, d, bindBase{Host: "/w/slab"}, obs, act)
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

// TestRenderWindowsHostTakesTheBase pins the two host sites: the window dir
// and the pane dir. The pane sets its own RelDir, which replaces the
// window's rather than nesting under it — so it must take the base too, or
// a bound session's pane escapes to the repository root.
func TestRenderWindowsHostTakesTheBase(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"}}
	base := bindBase{Host: "/w/slab/services/api", Rel: "services/api"}
	intents := []WindowIntent{{
		Name:   "dev",
		RelDir: "cmd",
		Panes: []PaneIntent{
			{Name: "shell"},
			{Name: "logs", RelDir: "internal"},
		},
	}}
	specs, err := renderWindows(intents, d, base, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Dir != "/w/slab/services/api/cmd" {
		t.Errorf("window dir = %q, want the window cwd under the base", specs[0].Dir)
	}
	if got := specs[0].Panes[0].Dir; got != "/w/slab/services/api/cmd" {
		t.Errorf("inheriting pane dir = %q, want the window's directory", got)
	}
	if got := specs[0].Panes[1].Dir; got != "/w/slab/services/api/internal" {
		t.Errorf("pane dir = %q, want the pane cwd under the base", got)
	}
}

// TestRenderWindowsHostWithoutABindIsUnchanged pins the no-bind case as a
// no-op: base.Host is the repository root and base.Rel is empty.
func TestRenderWindowsHostWithoutABindIsUnchanged(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"}}
	intents := []WindowIntent{{Name: "dev", Panes: []PaneIntent{{Name: "shell"}}}}
	specs, err := renderWindows(intents, d, bindBase{Host: "/w/slab"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Dir != "/w/slab" || specs[0].Panes[0].Dir != "/w/slab" {
		t.Errorf("unbound rendering changed: %+v", specs[0])
	}
}

// TestRenderWindowsContainerTakesTheBase pins the three container sites: the
// window's exec relDir, the pane's exec relDir, and the host-side -c both
// carry. The relDir is prefixed on this side precisely so
// container/exec.go's path.Join(Workdir, relDir) needs no change.
func TestRenderWindowsContainerTakesTheBase(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"},
		Config: config.Config{Environment: map[string]string{"K": "v"}}}
	base := bindBase{Host: "/w/slab/services/api", Rel: "services/api"}
	obs := &ContainerObservation{Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab"}
	act := renderPaneActuator{}
	binding := state.ContainerBinding{Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab"}
	intents := []WindowIntent{{
		Name:     "dev",
		Command:  "claude",
		RelDir:   "cmd",
		Location: WindowContainer,
		Panes:    []PaneIntent{{Name: "logs", RelDir: "internal"}},
	}}
	specs, err := renderWindows(intents, d, base, obs, act)
	if err != nil {
		t.Fatal(err)
	}
	wantWindow := act.ExecCommand(binding, "claude", "services/api/cmd", d.Config.Environment)
	if specs[0].Command != wantWindow {
		t.Errorf("window command = %q, want %q", specs[0].Command, wantWindow)
	}
	wantPane := act.ExecCommand(binding, "", "services/api/internal", d.Config.Environment)
	if specs[0].Panes[0].Command != wantPane {
		t.Errorf("pane command = %q, want %q", specs[0].Panes[0].Command, wantPane)
	}
	if specs[0].Dir != "/w/slab/services/api" || specs[0].Panes[0].Dir != "/w/slab/services/api" {
		t.Errorf("container host -c = %q/%q, want the base on both",
			specs[0].Dir, specs[0].Panes[0].Dir)
	}
}
