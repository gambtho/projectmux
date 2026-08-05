package tmux

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/gambtho/projectmux/internal/controller"
)

var _ controller.SessionActuator = (*Client)(nil)

// CreateSession creates the workspace session in one chained tmux
// invocation (verified on tmux 3.4): new-session with the first window,
// the three identity keys via set-option, remaining windows detached,
// and an explicit focus selection when configured. One subprocess makes
// creation-with-identity near-atomic (open/attach spec §4).
func (c *Client) CreateSession(ctx context.Context, spec controller.SessionSpec) error {
	if len(spec.Windows) == 0 {
		return fmt.Errorf("creating session %q: the spec carries no windows", spec.Name)
	}
	res, err := c.exec(ctx, createArgv(spec)...)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("tmux new-session exited %d: %s",
			res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	return nil
}

// createArgv renders the chained command list. Targets are the plain
// session name: set-option, new-window, and select-window reject the
// "=" exact-match prefix (their -t is not a target-session — verified
// on tmux 3.4), and tmux prefers an exact name match over a prefix, so
// targeting the just-created name is unambiguous. Window commands are
// tmux shell-command arguments: tmux runs them in the pane's default
// shell; nothing here interpolates into a shell (design §11).
func createArgv(spec controller.SessionSpec) []string {
	target := spec.Name
	first := spec.Windows[0]

	argv := []string{"new-session", "-d", "-s", spec.Name, "-n", first.Name,
		"-c", windowDir(first, spec)}
	argv = append(argv, envArgs(spec.Env)...)
	if first.Command != "" {
		argv = append(argv, first.Command)
	}

	argv = append(argv,
		";", "set-option", "-t", target, controller.KeyWorkspaceID, spec.WorkspaceID,
		";", "set-option", "-t", target, controller.KeySlug, spec.Slug,
		";", "set-option", "-t", target, controller.KeyWorktree, spec.Worktree,
	)

	for _, w := range spec.Windows[1:] {
		// -d keeps the first window active unless a focus is selected
		// below (open/attach spec §4).
		argv = append(argv, ";", "new-window", "-d", "-t", target,
			"-n", w.Name, "-c", windowDir(w, spec))
		argv = append(argv, envArgs(spec.Env)...)
		if w.Command != "" {
			argv = append(argv, w.Command)
		}
	}

	for _, w := range spec.Windows {
		if w.Focus {
			argv = append(argv, ";", "select-window", "-t", target+":"+w.Name)
		}
	}
	return argv
}

// envArgs renders -e KEY=VALUE pairs in sorted key order (deterministic
// argv). The environment is part of the digested desired configuration
// and must reach every window's panes; -e on new-session and new-window
// is the only mechanism covering the first window, which new-session
// itself creates (open/attach spec §4, verified on tmux 3.4).
func envArgs(env map[string]string) []string {
	var args []string
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", k+"="+env[k])
	}
	return args
}

func windowDir(w controller.WindowSpec, spec controller.SessionSpec) string {
	if w.Dir != "" {
		return w.Dir
	}
	return spec.Worktree
}
