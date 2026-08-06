package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

// originFixture is a complete layer whose line numbers are load-bearing.
// The trailing comments are the assertions' source of truth: edit a line
// here and the comment tells you which expectation moves.
const originFixture = `version: 1
devcontainer:
  enabled: false
  start_timeout: 5m
windows:
  - name: dev
    command: vim
    location: container
  - name: test
    shell: true
`

// Line numbers in originFixture, by 1-based position above.
const (
	lineVersion      = 1
	lineDevContainer = 2
	lineEnabled      = 3
	lineStartTimeout = 4
	lineWindowDev    = 6
	lineDevCommand   = 7
	lineDevLocation  = 8
	lineWindowTest   = 9
	lineTestShell    = 10
)

func parseFixture(t *testing.T, body string) *yaml.Node {
	t.Helper()
	var node yaml.Node
	if err := yaml.Unmarshal([]byte(body), &node); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}
	return &node
}

func TestLineOfResolvesFieldPaths(t *testing.T) {
	root := parseFixture(t, originFixture)

	cases := []struct {
		path string
		want int
	}{
		{"version", lineVersion},
		{"devcontainer", lineDevContainer},
		{"devcontainer.enabled", lineEnabled},
		{"devcontainer.start_timeout", lineStartTimeout},
		{"windows[dev]", lineWindowDev},
		{"windows[dev].command", lineDevCommand},
		{"windows[dev].location", lineDevLocation},
		{"windows[test]", lineWindowTest},
		{"windows[test].shell", lineTestShell},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			if got := lineOf(root, tc.path); got != tc.want {
				t.Errorf("lineOf(%q) = %d, want %d", tc.path, got, tc.want)
			}
		})
	}
}

// A path that names nothing must report 0 rather than guessing. Rendering
// treats 0 as "no position", so a wrong answer here would print a line
// number that points at unrelated configuration.
func TestLineOfReportsZeroForAnAbsentPath(t *testing.T) {
	root := parseFixture(t, originFixture)

	for _, path := range []string{
		"autostart",                 // key absent entirely
		"devcontainer.config",       // absent within a present mapping
		"windows[dev].cwd",          // absent within a present window
		"windows[missing]",          // no window carries this name
		"windows[missing].location", // nor any field of it
		"environment.FOO",           // whole subtree absent
		"",                          // empty path
	} {
		if got := lineOf(root, path); got != 0 {
			t.Errorf("lineOf(%q) = %d, want 0", path, got)
		}
	}
}

// Window names are matched whole. Substring matching would resolve "dev"
// to a window named "dev-server", reporting a line the user did not ask
// about — the same trap mergeWindows documents.
func TestLineOfMatchesWindowNamesWhole(t *testing.T) {
	root := parseFixture(t, "windows:\n  - name: dev-server\n    shell: true\n")

	if got := lineOf(root, "windows[dev]"); got != 0 {
		t.Errorf("lineOf(windows[dev]) = %d against a dev-server window, want 0", got)
	}
	if got := lineOf(root, "windows[dev-server]"); got != 2 {
		t.Errorf("lineOf(windows[dev-server]) = %d, want 2", got)
	}
}

// The portable window charset admits a dot, so a path separator can appear
// inside a window name. Splitting on every dot would read "windows[api.v2]"
// as two segments and lose the window entirely.
func TestLineOfResolvesAWindowNameContainingADot(t *testing.T) {
	root := parseFixture(t, "windows:\n  - name: api.v2\n    shell: true\n    location: host\n")

	if got := lineOf(root, "windows[api.v2]"); got != 2 {
		t.Errorf("lineOf(windows[api.v2]) = %d, want 2", got)
	}
	if got := lineOf(root, "windows[api.v2].location"); got != 4 {
		t.Errorf("lineOf(windows[api.v2].location) = %d, want 4", got)
	}
}

// An empty document has no positions to offer, and must not panic.
func TestLineOfHandlesAnEmptyDocument(t *testing.T) {
	var empty yaml.Node
	if got := lineOf(&empty, "version"); got != 0 {
		t.Errorf("lineOf on an empty node = %d, want 0", got)
	}
	if got := lineOf(nil, "version"); got != 0 {
		t.Errorf("lineOf on a nil node = %d, want 0", got)
	}
}
