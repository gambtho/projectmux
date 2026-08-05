package container

import (
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/gambtho/projectmux/internal/state"
)

// shellQuote renders s as one single-quoted POSIX shell word. This is
// the only place quoting happens (spec §3).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExecCommand renders the §5 container execution request as a tmux
// window command string: the pane's shell runs docker exec into the
// stored binding. The configured environment must be passed explicitly:
// tmux -e sets pane variables on the host side only, and docker exec
// forwards nothing into the container. Container-relative cwds join the
// binding's workdir with POSIX path semantics. ExecCommand never
// inspects live state; a pane whose container died shows docker's own
// error.
func (a *Adapter) ExecCommand(binding state.ContainerBinding, command, relDir string, env map[string]string) string {
	args := []string{"docker", "exec", "-i", "-t"}
	if binding.ContainerUser != "" {
		args = append(args, "-u", shellQuote(binding.ContainerUser))
	}
	args = append(args, "-w", shellQuote(path.Join(binding.Workdir, relDir)))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", shellQuote(k+"="+env[k]))
	}
	args = append(args, shellQuote(binding.ContainerID))
	if command == "" {
		args = append(args, "sh", "-l")
	} else {
		args = append(args, "sh", "-lc", shellQuote(command))
	}
	return strings.Join(args, " ")
}
