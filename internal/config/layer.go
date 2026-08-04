package config

import (
	"fmt"
	"time"

	"gopkg.in/yaml.v3"
)

// Layer is one decoded YAML file. Every field is a pointer or slice so that an
// omitted key is distinguishable from an explicit zero value: without that,
// an overlay could never turn a default off.
//
// Every field must stay concretely typed. yaml.v3's KnownFields check does not
// police anything decoded into `any`, so a single `any` field would silently
// reopen the unknown-field hole that keeps a misspelled key from turning into
// default behavior.
type Layer struct {
	Version *int `yaml:"version"`
	// RepositoryRoots is accepted only in defaults.yaml; Load rejects it
	// elsewhere. It is resolver infrastructure, not workspace desired state.
	RepositoryRoots []string           `yaml:"repository_roots"`
	Autostart       *bool              `yaml:"autostart"`
	DevContainer    *DevContainerLayer `yaml:"devcontainer"`
	Environment     map[string]string  `yaml:"environment"`
	Windows         []WindowLayer      `yaml:"windows"`
}

// DevContainerLayer is the undecided form of DevContainer.
type DevContainerLayer struct {
	Enabled      *Enabled  `yaml:"enabled"`
	Config       *string   `yaml:"config"`
	StartTimeout *Duration `yaml:"start_timeout"`
}

// WindowLayer is the undecided form of Window.
type WindowLayer struct {
	Name     string  `yaml:"name"`
	Agent    *string `yaml:"agent"`
	Command  *string `yaml:"command"`
	Shell    *bool   `yaml:"shell"`
	Cwd      *string `yaml:"cwd"`
	Location *string `yaml:"location"`
	Focus    *bool   `yaml:"focus"`
}

// setsMode reports whether this layer expresses how the window runs. Mode is
// merged as a unit: a later layer that names any one of agent, command, or
// shell replaces all three. Merging the three field-wise would leave a window
// with both an inherited agent and an overriding command, which "exactly one"
// then rejects — making a mode override impossible to express.
func (w WindowLayer) setsMode() bool {
	return w.Agent != nil || w.Command != nil || w.Shell != nil
}

// Enabled is the devcontainer.enabled tri-state. YAML spells two of its values
// as booleans and one as a string, so it accepts both node kinds and stores the
// canonical string.
type Enabled string

// UnmarshalYAML accepts `auto`, `true`, and `false` in either YAML spelling.
func (e *Enabled) UnmarshalYAML(node *yaml.Node) error {
	switch node.ShortTag() {
	case "!!bool", "!!str":
	default:
		return fmt.Errorf("devcontainer.enabled must be auto, true, or false, got %q", node.Value)
	}
	switch node.Value {
	case "auto", "true", "false":
		*e = Enabled(node.Value)
		return nil
	default:
		return fmt.Errorf("devcontainer.enabled must be auto, true, or false, got %q", node.Value)
	}
}

// UnmarshalYAML parses a duration string. A bare number gets a targeted message
// rather than a unit guess, because the Bash implementation spelled this field
// as integer seconds and a pasted `300` is the likely mistake.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	if node.ShortTag() == "!!int" || node.ShortTag() == "!!float" {
		return fmt.Errorf(
			"devcontainer.start_timeout must be a duration with units, such as %q, not the bare number %s",
			node.Value+"s", node.Value)
	}
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("devcontainer.start_timeout is not a valid duration: %q", node.Value)
	}
	*d = Duration(parsed)
	return nil
}
