package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeRoot builds a configuration root from a path -> contents map and returns
// it. Paths are relative to the root; parent directories are created.
func writeRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// load runs the whole defaults-then-workspace pipeline the CLI uses.
func load(t *testing.T, root, slug string) (Effective, error) {
	t.Helper()
	defaults, err := LoadDefaults(root)
	if err != nil {
		return Effective{}, err
	}
	return Load(root, defaults, slug)
}

func mustLoad(t *testing.T, root, slug string) Effective {
	t.Helper()
	eff, err := load(t, root, slug)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return eff
}

func windowNames(c Config) string {
	names := make([]string, 0, len(c.Windows))
	for _, w := range c.Windows {
		names = append(names, w.Name)
	}
	return strings.Join(names, ",")
}

func window(t *testing.T, c Config, name string) Window {
	t.Helper()
	for _, w := range c.Windows {
		if w.Name == name {
			return w
		}
	}
	t.Fatalf("no window named %q in %s", name, windowNames(c))
	return Window{}
}

// The four-window layout from the Phase 1 default fixture, restated under the
// design's "exactly one of agent, command, shell" rule.
const defaultsYAML = `
version: 1
autostart: false
devcontainer:
  enabled: auto
  start_timeout: 5m
windows:
  - name: agent-1
    agent: claude
    focus: true
  - name: agent-2
    agent: claude
  - name: shell
    shell: true
  - name: scratch
    shell: true
    location: host
`

func TestDefaultsAloneProducesOrderedWindowsAndSchemaDefaults(t *testing.T) {
	root := writeRoot(t, map[string]string{"defaults.yaml": defaultsYAML})
	eff := mustLoad(t, root, "slabledger")

	if got := windowNames(eff.Config); got != "agent-1,agent-2,shell,scratch" {
		t.Errorf("window order = %q", got)
	}
	if got := window(t, eff.Config, "agent-1").Agent; got == nil || *got != "claude" {
		t.Errorf("agent-1 agent = %v", got)
	}
	if !window(t, eff.Config, "shell").Shell {
		t.Error("shell window should have shell: true")
	}
	if got := window(t, eff.Config, "scratch").Location; got == nil || *got != "host" {
		t.Errorf("scratch location = %v", got)
	}
	if !window(t, eff.Config, "agent-1").Focus {
		t.Error("agent-1 should be focused")
	}
	if eff.Config.Version != 1 {
		t.Errorf("version = %d", eff.Config.Version)
	}
	if eff.Config.Autostart {
		t.Error("autostart should default false")
	}
	if eff.Config.DevContainer.Enabled != "auto" {
		t.Errorf("devcontainer.enabled = %q", eff.Config.DevContainer.Enabled)
	}
	if got := eff.Config.DevContainer.StartTimeout.String(); got != "5m0s" {
		t.Errorf("start_timeout = %q", got)
	}
	if eff.Config.Environment == nil {
		t.Error("environment should normalize to an empty map, not nil")
	}
}

func TestStartTimeoutDefaultsToFiveMinutes(t *testing.T) {
	root := writeRoot(t, map[string]string{"defaults.yaml": "version: 1\n"})
	eff := mustLoad(t, root, "slabledger")
	if got := eff.Config.DevContainer.StartTimeout.String(); got != "5m0s" {
		t.Errorf("start_timeout = %q, want 5m0s", got)
	}
	if eff.Config.DevContainer.Enabled != "auto" {
		t.Errorf("enabled = %q, want auto", eff.Config.DevContainer.Enabled)
	}
}

func TestWindowsMergeByNameNotPosition(t *testing.T) {
	// scratch is last in defaults and first (and only) in the overlay. A
	// positional merge would rewrite agent-1 instead.
	root := writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
		"workspaces/slabledger.yaml": `
version: 1
windows:
  - name: scratch
    command: make test
`,
	})
	eff := mustLoad(t, root, "slabledger")

	if got := window(t, eff.Config, "agent-1").Agent; got == nil || *got != "claude" {
		t.Errorf("agent-1 agent = %v, want claude", got)
	}
	if got := window(t, eff.Config, "agent-1").Command; got != nil {
		t.Errorf("agent-1 command = %v, want nil", *got)
	}
	if got := window(t, eff.Config, "scratch").Command; got == nil || *got != "make test" {
		t.Errorf("scratch command = %v", got)
	}
}

func TestOverlayOnlyWindowsAppendInOverlayOrder(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
		"workspaces/slabledger.yaml": `
version: 1
windows:
  - name: docs
    command: mkdocs serve
  - name: logs
    command: tail -f log
`,
	})
	eff := mustLoad(t, root, "slabledger")
	if got := windowNames(eff.Config); got != "agent-1,agent-2,shell,scratch,docs,logs" {
		t.Errorf("window order = %q", got)
	}
	if got := window(t, eff.Config, "docs").Location; got != nil {
		t.Errorf("docs location = %v, want unresolved", *got)
	}
}

func TestSubstringWindowNameIsAppendedNotMerged(t *testing.T) {
	// "age" is a substring of "agent-1"; substring matching would swallow it.
	root := writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
		"workspaces/slabledger.yaml": `
version: 1
windows:
  - name: age
    command: watch-age
`,
	})
	eff := mustLoad(t, root, "slabledger")
	if got := windowNames(eff.Config); got != "agent-1,agent-2,shell,scratch,age" {
		t.Errorf("window order = %q", got)
	}
	if got := window(t, eff.Config, "age").Command; got == nil || *got != "watch-age" {
		t.Errorf("age command = %v", got)
	}
}

func TestUnsetLocationStaysUnresolved(t *testing.T) {
	// The design's default is "container when one exists", which is conditional
	// on the workspace having a container. Normalization has no record in scope,
	// so it must not decide; collapsing unset to "container" here is what made a
	// plain repository unopenable in Phase 1.
	root := writeRoot(t, map[string]string{"defaults.yaml": defaultsYAML})
	eff := mustLoad(t, root, "slabledger")
	for _, name := range []string{"agent-1", "agent-2", "shell"} {
		if got := window(t, eff.Config, name).Location; got != nil {
			t.Errorf("%s location = %q, want unresolved", name, *got)
		}
	}
	if got := window(t, eff.Config, "scratch").Location; got == nil || *got != "host" {
		t.Errorf("scratch location = %v, want host", got)
	}
}

func TestLocalLayerBeatsWorkspaceLayer(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
		"workspaces/slabledger.yaml": `
version: 1
environment:
  DATABASE_URL: tracked
  CGO_ENABLED: "1"
windows:
  - name: shell
    command: tracked-cmd
    location: container
`,
		"workspaces/slabledger.local.yaml": `
version: 1
environment:
  DATABASE_URL: local
windows:
  - name: shell
    command: local-cmd
`,
	})
	eff := mustLoad(t, root, "slabledger")

	if got := eff.Config.Environment["DATABASE_URL"]; got != "local" {
		t.Errorf("DATABASE_URL = %q", got)
	}
	if got := eff.Config.Environment["CGO_ENABLED"]; got != "1" {
		t.Errorf("CGO_ENABLED = %q, should survive from the tracked layer", got)
	}
	shell := window(t, eff.Config, "shell")
	if shell.Command == nil || *shell.Command != "local-cmd" {
		t.Errorf("shell command = %v", shell.Command)
	}
	if shell.Location == nil || *shell.Location != "container" {
		t.Errorf("shell location = %v, should survive from the tracked layer", shell.Location)
	}
}

func TestSettingOneModeFieldReplacesTheInheritedMode(t *testing.T) {
	// Mode is a unit. Field-wise merge would leave agent-2 with both agent and
	// command set, which "exactly one" then rejects, making a mode override
	// impossible to express.
	root := writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
		"workspaces/slabledger.yaml": `
version: 1
windows:
  - name: agent-2
    command: make test
  - name: shell
    agent: codex
`,
	})
	eff := mustLoad(t, root, "slabledger")

	agent2 := window(t, eff.Config, "agent-2")
	if agent2.Agent != nil {
		t.Errorf("agent-2 agent = %q, should be replaced by command", *agent2.Agent)
	}
	if agent2.Command == nil || *agent2.Command != "make test" {
		t.Errorf("agent-2 command = %v", agent2.Command)
	}
	shell := window(t, eff.Config, "shell")
	if shell.Shell {
		t.Error("shell window's shell:true should be replaced by agent")
	}
	if shell.Agent == nil || *shell.Agent != "codex" {
		t.Errorf("shell agent = %v", shell.Agent)
	}
}

func TestMissingAndEmptyLayersBehaveAsEmptyDocuments(t *testing.T) {
	for name, body := range map[string]string{
		"absent":       "",
		"empty":        "",
		"commentOnly":  "# nothing here\n",
		"documentOnly": "---\n",
		"explicitNull": "null\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := map[string]string{"defaults.yaml": defaultsYAML}
			if name != "absent" {
				files["workspaces/slabledger.yaml"] = body
			}
			eff := mustLoad(t, writeRoot(t, files), "slabledger")
			if got := windowNames(eff.Config); got != "agent-1,agent-2,shell,scratch" {
				t.Errorf("window order = %q", got)
			}
		})
	}
}

func TestDigestIsPrefixedAndStable(t *testing.T) {
	root := writeRoot(t, map[string]string{"defaults.yaml": defaultsYAML})
	first := mustLoad(t, root, "slabledger").Digest
	second := mustLoad(t, root, "slabledger").Digest

	if first != second {
		t.Errorf("digest not stable: %q vs %q", first, second)
	}
	if !strings.HasPrefix(first, "sha256:") || len(first) != len("sha256:")+64 {
		t.Errorf("digest = %q, want sha256:<64 hex>", first)
	}
}

func TestDigestChangesWhenAnyLayerChanges(t *testing.T) {
	base := mustLoad(t, writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
	}), "slabledger").Digest

	tracked := mustLoad(t, writeRoot(t, map[string]string{
		"defaults.yaml":              defaultsYAML,
		"workspaces/slabledger.yaml": "version: 1\nenvironment:\n  CGO_ENABLED: \"1\"\n",
	}), "slabledger").Digest

	localised := mustLoad(t, writeRoot(t, map[string]string{
		"defaults.yaml":                    defaultsYAML,
		"workspaces/slabledger.yaml":       "version: 1\nenvironment:\n  CGO_ENABLED: \"1\"\n",
		"workspaces/slabledger.local.yaml": "version: 1\nenvironment:\n  DATABASE_URL: postgres://x\n",
	}), "slabledger").Digest

	if base == tracked {
		t.Error("digest ignored the workspace layer")
	}
	if tracked == localised {
		t.Error("digest ignored the local layer")
	}
}

func TestDigestIgnoresCosmeticYAMLEdits(t *testing.T) {
	ordered := mustLoad(t, writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
		"workspaces/slabledger.yaml": `
version: 1
environment:
  CGO_ENABLED: "1"
  DATABASE_URL: postgres://x
windows:
  - name: scratch
    command: make test
    location: container
`,
	}), "slabledger").Digest

	reordered := mustLoad(t, writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML,
		"workspaces/slabledger.yaml": `

# a comment the digest must ignore
windows:
  - location: container
    command:   make test
    name: scratch

environment:
  DATABASE_URL: postgres://x
  CGO_ENABLED: "1"
version: 1
`,
	}), "slabledger").Digest

	if ordered != reordered {
		t.Errorf("digest changed on a cosmetic edit: %q vs %q", ordered, reordered)
	}
}

func TestDigestExcludesRepositoryRoots(t *testing.T) {
	// Repository roots are resolver infrastructure, not workspace desired state.
	// Including them would read as config drift in the controller slice whenever
	// an unrelated root is added.
	withOne := mustLoad(t, writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML + "repository_roots: [/tmp/a]\n",
	}), "slabledger")
	withTwo := mustLoad(t, writeRoot(t, map[string]string{
		"defaults.yaml": defaultsYAML + "repository_roots: [/tmp/a, /tmp/b]\n",
	}), "slabledger")

	if withOne.Digest != withTwo.Digest {
		t.Error("repository_roots changed the digest")
	}
}

func TestRepositoryRootsAreReadFromDefaults(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml": "version: 1\nrepository_roots:\n  - /tmp/a\n  - /tmp/b\n",
	})
	defaults, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	if got := strings.Join(defaults.RepositoryRoots, ","); got != "/tmp/a,/tmp/b" {
		t.Errorf("repository_roots = %q", got)
	}
}

func TestRepositoryRootsAreRejectedOutsideDefaults(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml":              defaultsYAML,
		"workspaces/slabledger.yaml": "version: 1\nrepository_roots: [/tmp/a]\n",
	})
	_, err := load(t, root, "slabledger")
	assertInvalid(t, err, "repository_roots", "defaults.yaml")
}

// assertInvalid requires err to be an *InvalidConfigError mentioning every
// wanted substring.
func assertInvalid(t *testing.T, err error, wants ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	var invalid *InvalidConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("error %v is not an *InvalidConfigError", err)
	}
	msg := err.Error()
	for _, want := range wants {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestValidationRejects(t *testing.T) {
	cases := []struct {
		name      string
		defaults  string
		workspace string
		wants     []string
	}{
		{
			name:      "window name outside the portable charset",
			workspace: "version: 1\nwindows:\n  - name: \"my agent's window\"\n    command: t\n",
			// Names are interpolated into tmux hook commands, where a quote would
			// break out of the word the hook builds.
			wants: []string{"my agent's window", "invalid name"},
		},
		{
			name:      "duplicate window names",
			workspace: "version: 1\nwindows:\n  - name: docs\n    command: one\n  - name: docs\n    command: two\n",
			wants:     []string{"docs", "more than once"},
		},
		{
			name:      "a window with no mode",
			workspace: "version: 1\nwindows:\n  - name: docs\n",
			wants:     []string{"docs", "exactly one of"},
		},
		{
			name:      "a window with two modes",
			workspace: "version: 1\nwindows:\n  - name: docs\n    agent: claude\n    command: make\n",
			wants:     []string{"docs", "exactly one of"},
		},
		{
			name:      "shell set to false is not a mode",
			workspace: "version: 1\nwindows:\n  - name: docs\n    shell: false\n",
			wants:     []string{"docs", "exactly one of"},
		},
		{
			name:      "a second focused window",
			defaults:  defaultsYAML,
			workspace: "version: 1\nwindows:\n  - name: shell\n    focus: true\n",
			wants:     []string{"focus", "agent-1", "shell"},
		},
		{
			name:      "an unknown location",
			workspace: "version: 1\nwindows:\n  - name: shell\n    shell: true\n    location: kubernetes\n",
			wants:     []string{"shell", "kubernetes", "location"},
		},
		{
			name:      "an unsupported schema version",
			workspace: "version: 2\n",
			wants:     []string{"version", "2"},
		},
		{
			name:  "a missing schema version",
			wants: []string{"version", "required"},
		},
		{
			name:      "an unknown top-level field",
			workspace: "version: 1\nautostrt: true\n",
			wants:     []string{"autostrt"},
		},
		{
			name:      "an unknown window field",
			workspace: "version: 1\nwindows:\n  - name: docs\n    shell: true\n    focuss: true\n",
			wants:     []string{"focuss"},
		},
		{
			name:      "an unknown devcontainer field",
			workspace: "version: 1\ndevcontainer:\n  enabld: true\n",
			wants:     []string{"enabld"},
		},
		{
			name:      "an unparsable document",
			workspace: "version: 1\n  windows: [\n",
			wants:     []string{"workspaces/slabledger.yaml"},
		},
		{
			name:      "an unknown devcontainer.enabled value",
			workspace: "version: 1\ndevcontainer:\n  enabled: maybe\n",
			wants:     []string{"enabled", "maybe"},
		},
		{
			name:      "a bare-integer start_timeout",
			workspace: "version: 1\ndevcontainer:\n  start_timeout: 300\n",
			wants:     []string{"start_timeout", "300s"},
		},
		{
			name:      "an unparsable start_timeout",
			workspace: "version: 1\ndevcontainer:\n  start_timeout: soon\n",
			wants:     []string{"start_timeout", "soon"},
		},
		{
			name:      "a non-positive start_timeout",
			workspace: "version: 1\ndevcontainer:\n  start_timeout: 0s\n",
			wants:     []string{"start_timeout", "positive"},
		},
		{
			name:      "an absolute window cwd",
			workspace: "version: 1\nwindows:\n  - name: docs\n    shell: true\n    cwd: /etc\n",
			wants:     []string{"docs", "cwd", "relative"},
		},
		{
			name:      "a window cwd escaping the worktree",
			workspace: "version: 1\nwindows:\n  - name: docs\n    shell: true\n    cwd: ../../etc\n",
			wants:     []string{"docs", "cwd", "escape"},
		},
		{
			name:      "a devcontainer config escaping the worktree",
			workspace: "version: 1\ndevcontainer:\n  config: ../other/devcontainer.json\n",
			wants:     []string{"devcontainer.config", "escape"},
		},
		{
			name:      "an absolute devcontainer config",
			workspace: "version: 1\ndevcontainer:\n  config: /etc/devcontainer.json\n",
			wants:     []string{"devcontainer.config", "relative"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{}
			if tc.defaults != "" {
				files["defaults.yaml"] = tc.defaults
			}
			if tc.workspace != "" {
				files["workspaces/slabledger.yaml"] = tc.workspace
			}
			_, err := load(t, writeRoot(t, files), "slabledger")
			assertInvalid(t, err, tc.wants...)
		})
	}
}

func TestValidConfigurationsAreAccepted(t *testing.T) {
	cases := map[string]string{
		"nested relative cwd":     "version: 1\nwindows:\n  - name: docs\n    shell: true\n    cwd: sub/dir\n",
		"dot-relative cwd":        "version: 1\nwindows:\n  - name: docs\n    shell: true\n    cwd: ./sub\n",
		"descend then return":     "version: 1\nwindows:\n  - name: docs\n    shell: true\n    cwd: sub/../other\n",
		"devcontainer enabled on": "version: 1\ndevcontainer:\n  enabled: true\n",
		"devcontainer disabled":   "version: 1\ndevcontainer:\n  enabled: false\n",
		"nested devcontainer cfg": "version: 1\ndevcontainer:\n  config: .devcontainer/devcontainer.json\n",
		"no windows at all":       "version: 1\n",
		"portable name charset":   "version: 1\nwindows:\n  - name: a.b_c-1\n    shell: true\n",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeRoot(t, map[string]string{"workspaces/slabledger.yaml": body})
			if _, err := load(t, root, "slabledger"); err != nil {
				t.Errorf("expected valid, got %v", err)
			}
		})
	}
}

func TestDevContainerEnabledAcceptsBooleansAndAuto(t *testing.T) {
	for body, want := range map[string]string{
		"version: 1\ndevcontainer:\n  enabled: auto\n":  "auto",
		"version: 1\ndevcontainer:\n  enabled: true\n":  "true",
		"version: 1\ndevcontainer:\n  enabled: false\n": "false",
		"version: 1\n": "auto",
	} {
		root := writeRoot(t, map[string]string{"workspaces/slabledger.yaml": body})
		eff := mustLoad(t, root, "slabledger")
		if eff.Config.DevContainer.Enabled != want {
			t.Errorf("%q -> enabled = %q, want %q", body, eff.Config.DevContainer.Enabled, want)
		}
	}
}

func TestConfigMarshalsToTheDocumentedJSONShape(t *testing.T) {
	root := writeRoot(t, map[string]string{"defaults.yaml": defaultsYAML})
	eff := mustLoad(t, root, "slabledger")

	raw, err := json.Marshal(eff.Config)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"version", "autostart", "devcontainer", "environment", "windows"} {
		if _, ok := got[key]; !ok {
			t.Errorf("config JSON is missing %q", key)
		}
	}
	if len(got) != 5 {
		t.Errorf("config JSON has %d keys, want 5: %s", len(got), raw)
	}
	if !strings.Contains(string(raw), `"start_timeout":"5m0s"`) {
		t.Errorf("start_timeout should marshal as a duration string: %s", raw)
	}
	if !strings.Contains(string(raw), `"location":null`) {
		t.Errorf("unresolved location should marshal as null: %s", raw)
	}
}

func TestRootPrefersExplicitOverrideThenXDG(t *testing.T) {
	t.Setenv("PROJECTMUX_CONFIG_ROOT", "/tmp/override")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	got, err := Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != "/tmp/override" {
		t.Errorf("Root = %q, want the explicit override", got)
	}

	t.Setenv("PROJECTMUX_CONFIG_ROOT", "")
	got, err = Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != filepath.Join("/tmp/xdg", "projectmux") {
		t.Errorf("Root = %q, want the XDG location", got)
	}

	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "/tmp/home")
	got, err = Root()
	if err != nil {
		t.Fatalf("Root: %v", err)
	}
	if got != filepath.Join("/tmp/home", ".config", "projectmux") {
		t.Errorf("Root = %q, want the ~/.config fallback", got)
	}
}
