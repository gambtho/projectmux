package config

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// windowNamePattern is the portable character set for window names.
//
// The restriction is load-bearing beyond tidiness: window names are
// interpolated into tmux hook commands, where a quote or a backslash would
// break out of the shell word the hook builds.
var windowNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// validate returns every problem in the effective configuration. The layer is
// needed alongside the normalized config to tell an omitted version from an
// unsupported one.
func validate(l Layer, cfg Config) []string {
	var problems []string

	switch {
	case l.Version == nil:
		problems = append(problems, fmt.Sprintf(
			"version is required and must be %d", SchemaVersion))
	case cfg.Version != SchemaVersion:
		problems = append(problems, fmt.Sprintf(
			"unsupported schema version %d; this build supports version %d",
			cfg.Version, SchemaVersion))
	}

	if cfg.DevContainer.StartTimeout <= 0 {
		problems = append(problems, fmt.Sprintf(
			"devcontainer.start_timeout must be positive, got %s",
			cfg.DevContainer.StartTimeout))
	}
	if cfg.DevContainer.Config != nil {
		if err := checkContained("devcontainer.config", *cfg.DevContainer.Config); err != nil {
			problems = append(problems, err.Error())
		}
	}
	for key := range cfg.Environment {
		if key == "" {
			problems = append(problems, "environment contains an empty variable name")
		}
	}

	var focused []string
	for _, w := range cfg.Windows {
		problems = append(problems, validateWindow(w)...)
		if w.Focus {
			focused = append(focused, w.Name)
		}
	}
	if len(focused) > 1 {
		problems = append(problems, fmt.Sprintf(
			"more than one window sets focus: %s", strings.Join(focused, ", ")))
	}
	return problems
}

func validateWindow(w Window) []string {
	var problems []string

	if !windowNamePattern.MatchString(w.Name) {
		problems = append(problems, fmt.Sprintf(
			"window %q has an invalid name; use characters from [A-Za-z0-9._-]", w.Name))
	}

	var modes []string
	if w.Agent != nil {
		modes = append(modes, "agent")
	}
	if w.Command != nil {
		modes = append(modes, "command")
	}
	if w.Shell {
		modes = append(modes, "shell")
	}
	if len(modes) != 1 {
		detail := "it sets none"
		if len(modes) > 1 {
			detail = "it sets " + strings.Join(modes, " and ")
		}
		problems = append(problems, fmt.Sprintf(
			"window %q must set exactly one of agent, command, or shell: true (%s)",
			w.Name, detail))
	}
	if w.Agent != nil && strings.TrimSpace(*w.Agent) == "" {
		problems = append(problems, fmt.Sprintf("window %q has an empty agent", w.Name))
	}
	if w.Command != nil && strings.TrimSpace(*w.Command) == "" {
		problems = append(problems, fmt.Sprintf("window %q has an empty command", w.Name))
	}

	if w.Location != nil && *w.Location != "host" && *w.Location != "container" {
		problems = append(problems, fmt.Sprintf(
			"window %q has an invalid location %q; use host or container", w.Name, *w.Location))
	}
	if w.Cwd != nil {
		if err := checkContained(fmt.Sprintf("window %q cwd", w.Name), *w.Cwd); err != nil {
			problems = append(problems, err.Error())
		}
	}
	return problems
}

// checkContained rejects a path that is absolute or that climbs out of the
// worktree.
//
// The check is lexical only. The target need not exist when configuration is
// read, so a symlink pointing outside the worktree is not detectable here; this
// rejects the paths a user can see are wrong, not every path that could
// resolve outside.
func checkContained(field, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s must not be empty", field)
	}
	if filepath.IsAbs(value) {
		return fmt.Errorf("%s must be relative to the worktree, got %q", field, value)
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must not escape the worktree, got %q", field, value)
	}
	return nil
}
