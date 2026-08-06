package config

import (
	"strings"
	"testing"
)

// validConfig returns the minimal Config that validate accepts with no
// problems: a supported version and a positive start timeout. Tests that
// exercise one facet build on this rather than repeating the boilerplate.
func validConfig() Config {
	return Config{
		Version:      SchemaVersion,
		DevContainer: DevContainer{StartTimeout: Duration(DefaultStartTimeout)},
	}
}

// validateDefaults loads a defaults document and validates it standalone,
// the way doctor examines an installation with no workspace files.
func validateDefaults(t *testing.T, body string) []string {
	t.Helper()
	root := writeRoot(t, map[string]string{"defaults.yaml": body})
	src, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	var rendered []string
	for _, p := range ValidateDefaults(src) {
		rendered = append(rendered, p.String())
	}
	return rendered
}

func TestValidateDefaultsAcceptsAnIncompleteLayer(t *testing.T) {
	// Defaults may legitimately omit anything a workspace layer supplies,
	// including the version, so neither an empty nor a version-only
	// document is a problem.
	for _, body := range []string{"", "version: 1\n", defaultsYAML} {
		if problems := validateDefaults(t, body); len(problems) != 0 {
			t.Errorf("defaults %q reported %v", body, problems)
		}
	}
}

func TestValidateDefaultsReportsProblemsInWhatItStates(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		wants []string
	}{
		{
			name:  "unsupported schema version",
			body:  "version: 9\n",
			wants: []string{"unsupported schema version 9"},
		},
		{
			name:  "window name outside the portable charset",
			body:  "windows:\n  - name: \"my window\"\n    command: t\n",
			wants: []string{"my window", "invalid name"},
		},
		{
			name:  "a window with no mode",
			body:  "windows:\n  - name: docs\n",
			wants: []string{"docs", "exactly one of"},
		},
		{
			name:  "a non-positive container start timeout",
			body:  "devcontainer:\n  start_timeout: 0s\n",
			wants: []string{"start_timeout", "positive"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			problems := validateDefaults(t, tc.body)
			joined := strings.Join(problems, "\n")
			for _, want := range tc.wants {
				if !strings.Contains(joined, want) {
					t.Errorf("problems %v do not mention %q", problems, want)
				}
			}
			// An omitted version is supplied, never reported.
			if strings.Contains(joined, "version is required") {
				t.Errorf("an omitted version was reported: %v", problems)
			}
		})
	}
}

func TestValidatePanes(t *testing.T) {
	str := func(s string) *string { return &s }

	cases := []struct {
		name  string
		panes []Pane
		want  string // substring of the expected problem message; "" = valid
	}{
		{"default is valid", []Pane{{Name: "shell", Shell: true}}, ""},
		{"empty is valid", []Pane{}, ""},
		{"no mode", []Pane{{Name: "p1"}},
			`pane "p1" of window "dev" must set exactly one of agent, command, or shell: true`},
		{"two modes", []Pane{{Name: "p1", Agent: str("claude"), Shell: true}},
			`pane "p1" of window "dev" must set exactly one of agent, command, or shell: true`},
		{"empty agent", []Pane{{Name: "p1", Agent: str("  ")}},
			`pane "p1" of window "dev" has an empty agent`},
		{"empty command", []Pane{{Name: "p1", Command: str("")}},
			`pane "p1" of window "dev" has an empty command`},
		{"bad name", []Pane{{Name: "sp ace", Shell: true}},
			`invalid name`},
		{"absolute cwd", []Pane{{Name: "p1", Shell: true, Cwd: str("/etc")}},
			`must be relative to the worktree`},
		{"escaping cwd", []Pane{{Name: "p1", Shell: true, Cwd: str("../out")}},
			`must not escape the worktree`},
		{"duplicate names", []Pane{{Name: "p1", Shell: true}, {Name: "p1", Shell: true}},
			`pane "p1" of window "dev" is defined more than once`},
		{"two focused", []Pane{
			{Name: "p1", Shell: true, Focus: true},
			{Name: "p2", Shell: true, Focus: true}},
			`more than one pane of window "dev" sets focus`},
	}

	// validate's version check reads m.Layer.Version, not cfg.Version, so an
	// empty Merged always reports "version is required" regardless of cfg;
	// set it here the way a real caller (ValidateDefaults, Load) does.
	version := SchemaVersion
	m := Merged{Layer: Layer{Version: &version}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Windows = []Window{{Name: "dev", Shell: true, Panes: tc.panes}}
			problems := validate(m, cfg)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Fatalf("expected valid, got %v", problems)
				}
				return
			}
			found := false
			for _, p := range problems {
				if strings.Contains(p.Message, tc.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("problems %v\nmissing %q", problems, tc.want)
			}
		})
	}
}
