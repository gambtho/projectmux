// Package config loads, merges, normalizes, validates, and digests the layered
// YAML that describes a workspace's desired state.
//
// The package performs no git, tmux, or container work. It reads files and
// returns values, so every behavior here is testable without a live
// environment.
package config

import (
	"encoding/json"
	"errors"
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

// Problem is one validation failure together with every layer position that
// contributed to it.
type Problem struct {
	// Field is the dotted path the problem is primarily about, such as
	// "devcontainer.start_timeout" or "windows[dev].location". It is empty
	// for a problem no single field owns, such as a duplicated focus.
	Field   string
	Message string
	// Origins are the positions the reader may have to edit, primary first.
	// It is empty when no layer set the field: an absent required key has no
	// file to point at, and inventing one would be a lie.
	Origins []Origin
}

// String renders the problem with its primary position, or bare when there
// is none. A problem with no origin prints no prefix at all rather than a
// ":0" that asserts a position which does not exist.
//
// Contributing positions follow the message. For a cross-field conflict the
// primary line is often correct in isolation — a window asking for a
// container is only wrong because another file disabled them — so a report
// naming one position sends the reader somewhere with nothing to fix.
func (p Problem) String() string {
	if len(p.Origins) == 0 {
		return p.Message
	}
	out := p.Origins[0].String() + ": " + p.Message
	if len(p.Origins) > 1 {
		rest := make([]string, 0, len(p.Origins)-1)
		for _, o := range p.Origins[1:] {
			rest = append(rest, o.String())
		}
		out += " (also " + strings.Join(rest, ", ") + ")"
	}
	return out
}

// String renders "file:line", dropping the line when it is unknown.
func (o Origin) String() string {
	if o.Line == 0 {
		return o.File
	}
	return fmt.Sprintf("%s:%d", o.File, o.Line)
}

// InvalidConfigError reports configuration that must not reach any workspace
// mutation. It carries every problem found rather than only the first, because
// fixing configuration one error per run is needlessly slow.
type InvalidConfigError struct {
	Problems []Problem
}

func (e *InvalidConfigError) Error() string {
	if len(e.Problems) == 1 {
		return "invalid configuration: " + e.Problems[0].String()
	}
	var b strings.Builder
	fmt.Fprintf(&b, "invalid configuration (%d problems):", len(e.Problems))
	for _, p := range e.Problems {
		b.WriteString("\n  - ")
		b.WriteString(p.String())
	}
	return b.String()
}

// ProblemsOf returns the structured problems an error carries, or the error
// rendered as a single problem when it is not an invalid-configuration error.
//
// It exists so a reporting caller never has to choose between the structured
// detail and the ordinary error text: every failure becomes problems.
func ProblemsOf(err error) []Problem {
	if err == nil {
		return nil
	}
	var bad *InvalidConfigError
	if errors.As(err, &bad) {
		return bad.Problems
	}
	return []Problem{{Message: err.Error()}}
}

// invalid builds a single-problem InvalidConfigError. Load-time failures abort
// at the first problem because a file that will not decode has no further
// problems to report; validate collects many and builds the error itself.
func invalid(problem string) error {
	return &InvalidConfigError{Problems: []Problem{{Message: problem}}}
}
