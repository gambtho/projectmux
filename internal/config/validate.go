package config

import (
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// windowNamePattern is the portable character set for window names.
//
// The restriction is load-bearing beyond tidiness: window names are
// interpolated into tmux hook commands, where a quote or a backslash would
// break out of the shell word the hook builds.
var windowNamePattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// numericWindowName rejects fully numeric names: tmux resolves a numeric
// token in a target as a window index before a window name, so a window
// literally named "3" can make focus land on the wrong window.
var numericWindowName = regexp.MustCompile(`^[0-9]+$`)

// ValidateDefaults reports every problem in the defaults layer read on
// its own. Validation otherwise lives only in Load, which needs a slug,
// so a defaults.yaml would go unchecked on an installation with no
// workspace files at all.
//
// Defaults are legitimately incomplete — a workspace layer supplies the
// rest — so an omitted version is not a problem here. Anything defaults
// does state is checked as stated; a later layer may still override it,
// which is why the caller reports these as a warning rather than a
// rejection.
func ValidateDefaults(src Source) []Problem {
	merged := mergeLayers(Merged{root: filepath.Dir(src.File)}, src)
	if merged.Layer.Version == nil {
		supported := SchemaVersion
		merged.Layer.Version = &supported
	}
	problems := merged.duplicateWindows(src)
	return append(problems, validate(merged, normalize(merged.Layer))...)
}

// validate returns every problem in the effective configuration. The merged
// layer is needed alongside the normalized config to tell an omitted version
// from an unsupported one, and to attribute each problem to its source.
func validate(m Merged, cfg Config) []Problem {
	var problems []Problem

	switch {
	case m.Layer.Version == nil:
		problems = append(problems, m.problem("version", fmt.Sprintf(
			"version is required and must be %d", SchemaVersion)))
	case cfg.Version != SchemaVersion:
		problems = append(problems, m.problem("version", fmt.Sprintf(
			"unsupported schema version %d; this build supports version %d",
			cfg.Version, SchemaVersion)))
	}

	if cfg.DevContainer.StartTimeout <= 0 {
		problems = append(problems, m.problem("devcontainer.start_timeout", fmt.Sprintf(
			"devcontainer.start_timeout must be positive, got %s",
			cfg.DevContainer.StartTimeout)))
	}
	if cfg.DevContainer.Config != nil {
		if msg := checkContained("devcontainer.config", *cfg.DevContainer.Config); msg != "" {
			problems = append(problems, m.problem("devcontainer.config", msg))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Environment)) {
		field := "environment." + name
		switch {
		case name == "":
			problems = append(problems, m.problem(field,
				"environment contains an empty variable name"))
		case strings.ContainsAny(name, "=\x00"):
			// Neither character can appear in a real environment variable name,
			// so the export in a later slice would fail; reject it at config time.
			problems = append(problems, m.problem(field, fmt.Sprintf(
				"environment variable name %q must not contain %q or a NUL byte", name, "=")))
		}
	}

	var focused []string
	var focusFields []string
	for _, w := range cfg.Windows {
		problems = append(problems, validateWindow(m, w)...)
		if cfg.DevContainer.Enabled == "false" && w.Location != nil && *w.Location == "container" {
			problems = append(problems, m.problem(windowField(w.Name, "location"), fmt.Sprintf(
				"window %q sets location: container but devcontainer.enabled is false", w.Name),
				"devcontainer.enabled"))
		}
		if w.Focus {
			focused = append(focused, w.Name)
			focusFields = append(focusFields, windowField(w.Name, "focus"))
		}
	}
	if len(focused) > 1 {
		// No single field owns this: every focused window contributes, so
		// the problem names them all rather than picking one arbitrarily.
		problems = append(problems, m.problem("", fmt.Sprintf(
			"more than one window sets focus: %s", strings.Join(focused, ", ")),
			focusFields...))
	}
	return problems
}

func validateWindow(m Merged, w Window) []Problem {
	var problems []Problem
	window := fmt.Sprintf("windows[%s]", w.Name)

	if !windowNamePattern.MatchString(w.Name) {
		problems = append(problems, m.problem(window, fmt.Sprintf(
			"window %q has an invalid name; use characters from [A-Za-z0-9._-]", w.Name)))
	}
	if numericWindowName.MatchString(w.Name) {
		problems = append(problems, m.problem(window, fmt.Sprintf(
			"window %q has a fully numeric name, which tmux treats as a window index; include a non-digit", w.Name)))
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
		// Attributed to the window, not to a key: when it sets none there is
		// no key to point at, and when it sets two neither one alone is the
		// answer. The window is what the reader has to look at either way.
		problems = append(problems, m.problem(window, fmt.Sprintf(
			"window %q must set exactly one of agent, command, or shell: true (%s)",
			w.Name, detail)))
	}
	if w.Agent != nil && strings.TrimSpace(*w.Agent) == "" {
		problems = append(problems, m.problem(windowField(w.Name, "agent"), fmt.Sprintf(
			"window %q has an empty agent", w.Name)))
	}
	if w.Command != nil && strings.TrimSpace(*w.Command) == "" {
		problems = append(problems, m.problem(windowField(w.Name, "command"), fmt.Sprintf(
			"window %q has an empty command", w.Name)))
	}

	if w.Location != nil && *w.Location != "host" && *w.Location != "container" {
		problems = append(problems, m.problem(windowField(w.Name, "location"), fmt.Sprintf(
			"window %q has an invalid location %q; use host or container", w.Name, *w.Location)))
	}
	if w.Cwd != nil {
		if msg := checkContained(fmt.Sprintf("window %q cwd", w.Name), *w.Cwd); msg != "" {
			problems = append(problems, m.problem(windowField(w.Name, "cwd"), msg))
		}
	}
	problems = append(problems, validatePanes(m, w)...)
	return problems
}

// validatePanes checks one window's additional panes. The rules mirror
// validateWindow where they carry over; there is no numeric-name rule
// because panes are never addressed by name in a tmux target, and no
// location rule because panes inherit the window's location by design.
//
// Duplicates are checked here rather than in duplicateWindows: panes
// merge as a unit, so the merged window's list comes verbatim from one
// layer and survives to the normalized config intact. A duplicated list
// in a layer that a later layer replaced is deliberately not reported —
// the whole list was overridden, exactly as an overridden bad agent in
// the mode trio goes unreported; defaults.yaml read on its own is still
// covered because ValidateDefaults normalizes and validates that single
// layer, which reaches this check.
func validatePanes(m Merged, w Window) []Problem {
	var problems []Problem
	seen := map[string]bool{}
	reported := map[string]bool{}
	var focused []string
	for _, p := range w.Panes {
		pane := fmt.Sprintf("windows[%s].panes[%s]", w.Name, p.Name)

		if !windowNamePattern.MatchString(p.Name) {
			problems = append(problems, m.problem(pane, fmt.Sprintf(
				"pane %q of window %q has an invalid name; use characters from [A-Za-z0-9._-]",
				p.Name, w.Name)))
		}
		switch {
		case !seen[p.Name]:
			seen[p.Name] = true
		case !reported[p.Name]:
			reported[p.Name] = true
			problems = append(problems, m.problem(pane, fmt.Sprintf(
				"pane %q of window %q is defined more than once", p.Name, w.Name)))
		}

		var modes []string
		if p.Agent != nil {
			modes = append(modes, "agent")
		}
		if p.Command != nil {
			modes = append(modes, "command")
		}
		if p.Shell {
			modes = append(modes, "shell")
		}
		if len(modes) != 1 {
			detail := "it sets none"
			if len(modes) > 1 {
				detail = "it sets " + strings.Join(modes, " and ")
			}
			problems = append(problems, m.problem(pane, fmt.Sprintf(
				"pane %q of window %q must set exactly one of agent, command, or shell: true (%s)",
				p.Name, w.Name, detail)))
		}
		if p.Agent != nil && strings.TrimSpace(*p.Agent) == "" {
			problems = append(problems, m.problem(pane+".agent", fmt.Sprintf(
				"pane %q of window %q has an empty agent", p.Name, w.Name)))
		}
		if p.Command != nil && strings.TrimSpace(*p.Command) == "" {
			problems = append(problems, m.problem(pane+".command", fmt.Sprintf(
				"pane %q of window %q has an empty command", p.Name, w.Name)))
		}
		if p.Cwd != nil {
			if msg := checkContained(fmt.Sprintf("pane %q of window %q cwd", p.Name, w.Name), *p.Cwd); msg != "" {
				problems = append(problems, m.problem(pane+".cwd", msg))
			}
		}
		if p.Focus {
			focused = append(focused, p.Name)
		}
	}
	if len(focused) > 1 {
		problems = append(problems, m.problem(fmt.Sprintf("windows[%s].panes", w.Name), fmt.Sprintf(
			"more than one pane of window %q sets focus: %s",
			w.Name, strings.Join(focused, ", "))))
	}
	return problems
}

// windowField builds the path of one field of one window.
func windowField(name, field string) string {
	return fmt.Sprintf("windows[%s].%s", name, field)
}

// checkContained reports why a path is unusable, or "" when it is fine. It
// rejects a path that is absolute or that climbs out of the repository root.
//
// The check is lexical only. The target need not exist when configuration is
// read, so a symlink pointing outside the repository is not detectable here;
// this rejects the paths a user can see are wrong, not every path that could
// resolve outside.
func checkContained(field, value string) string {
	if strings.TrimSpace(value) == "" {
		return fmt.Sprintf("%s must not be empty", field)
	}
	if filepath.IsAbs(value) {
		return fmt.Sprintf("%s must be relative to the repository root, got %q", field, value)
	}
	clean := filepath.Clean(value)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Sprintf("%s must not escape the repository root, got %q", field, value)
	}
	return ""
}
