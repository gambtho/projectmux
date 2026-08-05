package tmux

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

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
	// target is used as every mid-chain -t value (set-option, new-window);
	// it must be escaped exactly like -s, since those are ordinary argv
	// elements subject to the same trailing-";" chain-parsing rule.
	target := escapeChainArg(spec.Name)
	first := spec.Windows[0]

	argv := []string{"new-session", "-d", "-s", target, "-n", escapeChainArg(first.Name),
		"-c", escapeChainArg(windowDir(first, spec))}
	argv = append(argv, envArgs(spec.Env)...)
	if first.Command != "" {
		argv = append(argv, escapeChainArg(first.Command))
	}

	argv = append(argv,
		";", "set-option", "-t", target, controller.KeyWorkspaceID, escapeChainArg(spec.WorkspaceID),
		";", "set-option", "-t", target, controller.KeySlug, escapeChainArg(spec.Slug),
		";", "set-option", "-t", target, controller.KeyWorktree, escapeChainArg(spec.Worktree),
	)

	for _, w := range spec.Windows[1:] {
		// -d keeps the first window active unless a focus is selected
		// below (open/attach spec §4).
		argv = append(argv, ";", "new-window", "-d", "-t", target,
			"-n", escapeChainArg(w.Name), "-c", escapeChainArg(windowDir(w, spec)))
		argv = append(argv, envArgs(spec.Env)...)
		if w.Command != "" {
			argv = append(argv, escapeChainArg(w.Command))
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
		args = append(args, "-e", escapeChainArg(k+"="+env[k]))
	}
	return args
}

// escapeChainArg guards a single argv element against tmux's chained-command
// parser. Verified empirically on tmux 3.4 with an isolated -L socket: when
// tmux walks a command sequence passed as argv (new-session ... ; set-option
// ...), it inspects the LAST character of every argument. An argument ending
// in a bare, unescaped ";" is treated as if it were followed by a separate
// ";" argument — the trailing semicolon is stripped and the value is cut
// short there, silently truncating it and, in flag positions like -n, often
// corrupting the whole invocation. A single backslash immediately before
// that trailing ";" escapes it: tmux consumes exactly one backslash and
// keeps the semicolon as a literal character (confirmed for 0, 1, and 2
// pre-existing trailing backslashes: "x;"->"x", "x\;"->"x;", "x\\;"->"x\;").
// A trailing backslash with no following ";" is left untouched regardless of
// how many there are, so only the ";" case needs handling: prepending one
// more backslash before a trailing ";" always reproduces the original value
// byte-for-byte once tmux strips its one backslash.
func escapeChainArg(s string) string {
	if strings.HasSuffix(s, ";") {
		return s[:len(s)-1] + `\;`
	}
	return s
}

func windowDir(w controller.WindowSpec, spec controller.SessionSpec) string {
	if w.Dir != "" {
		return w.Dir
	}
	return spec.Worktree
}
