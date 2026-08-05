// Package config loads, merges, normalizes, validates, and digests the layered
// YAML that describes a workspace's desired state.
//
// The package performs no git, tmux, or container work. It reads files and
// returns values, so every behavior here is testable without a live
// environment.
package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// SchemaVersion is the only configuration schema version this build supports.
const SchemaVersion = 1

// DefaultStartTimeout is used when devcontainer.start_timeout is omitted.
const DefaultStartTimeout = 5 * time.Minute

// Config is the normalized, validated configuration for one workspace. Its
// JSON encoding is the digested document and the `config` member of the
// versioned CLI envelope, so field order and names are a public contract.
type Config struct {
	Version      int               `json:"version"`
	Autostart    bool              `json:"autostart"`
	DevContainer DevContainer      `json:"devcontainer"`
	Environment  map[string]string `json:"environment"`
	Windows      []Window          `json:"windows"`
}

// DevContainer holds the normalized Dev Container policy.
type DevContainer struct {
	// Enabled is "auto", "true", or "false" — a string rather than a bool
	// because "auto" is a real third state, not an absent one.
	Enabled string `json:"enabled"`
	// Config is an optional worktree-relative path to devcontainer.json.
	Config       *string  `json:"config"`
	StartTimeout Duration `json:"start_timeout"`
}

// Window is one normalized tmux window.
type Window struct {
	Name string `json:"name"`
	// Exactly one of Agent, Command, or Shell describes how the window runs.
	Agent   *string `json:"agent"`
	Command *string `json:"command"`
	Shell   bool    `json:"shell"`
	// Cwd is an optional worktree-relative working directory.
	Cwd *string `json:"cwd"`
	// Location is "host", "container", or nil. It stays nil when omitted: the
	// design's default is "container when one exists", which is conditional on
	// the workspace having a container, and no container binding is in scope
	// here. Collapsing nil to "container" during normalization is what made a
	// plain repository unopenable in the Bash implementation.
	Location *string `json:"location"`
	Focus    bool    `json:"focus"`
}

// Effective is the result of loading every layer for one workspace.
type Effective struct {
	Config Config
	// Digest is "sha256:" followed by the hex digest of Config's canonical JSON
	// encoding. It covers workspace desired state only; repository roots are
	// resolver infrastructure and are deliberately excluded so that editing a
	// root does not read as workspace configuration drift.
	Digest string
}

// Duration wraps time.Duration so it survives a YAML round trip as a duration
// string ("5m") and marshals to JSON as a canonical one ("5m0s").
type Duration time.Duration

// String returns the canonical Go duration form.
func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON encodes the duration as a string so the JSON envelope carries
// units rather than an ambiguous number.
func (d Duration) MarshalJSON() ([]byte, error) { return json.Marshal(d.String()) }

// UnmarshalJSON accepts the same string form MarshalJSON produces.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}

// InvalidConfigError reports configuration that must not reach any workspace
// mutation. It carries every problem found rather than only the first, because
// fixing configuration one error per run is needlessly slow.
type InvalidConfigError struct {
	Problems []string
}

func (e *InvalidConfigError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid configuration: " + e.Problems[0]
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p)
	}
	return b.String()
}

// invalid builds a single-problem InvalidConfigError. Load-time failures abort
// at the first problem because a file that will not decode has no further
// problems to report; validate collects many and builds the error itself.
func invalid(problem string) error {
	return &InvalidConfigError{Problems: []string{problem}}
}
