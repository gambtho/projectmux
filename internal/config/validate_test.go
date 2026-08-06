package config

import (
	"strings"
	"testing"
)

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
