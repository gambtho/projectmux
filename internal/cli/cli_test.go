package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// workspace creates a git repository with the given configuration files, points
// PROJECTMUX_CONFIG_ROOT at them, changes into the repository, and returns its
// slug. Configuration paths are relative to the configuration root.
func workspace(t *testing.T, files map[string]string) string {
	t.Helper()

	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	repo := filepath.Join(base, "slabledger")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, args := range [][]string{
		{"init", "-q", repo},
		{"commit", "-q", "--allow-empty", "-m", "init"},
	} {
		full := append([]string{
			"-c", "user.email=t@example.com", "-c", "user.name=t",
			"-c", "init.defaultBranch=main",
		}, args...)
		cmd := exec.Command("git", full...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}

	root := filepath.Join(base, "config")
	for name, body := range files {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	t.Setenv("PROJECTMUX_CONFIG_ROOT", root)
	t.Chdir(repo)
	return "slabledger"
}

// run invokes the CLI exactly as main does and returns its exit code and streams.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Main(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

const validConfig = `
version: 1
devcontainer:
  enabled: auto
  start_timeout: 5m
environment:
  CGO_ENABLED: "1"
windows:
  - name: agent-1
    agent: claude
    focus: true
  - name: shell
    shell: true
  - name: scratch
    shell: true
    location: host
`

func TestHelpIsSuccessfulAndListsImplementedCommands(t *testing.T) {
	for _, args := range [][]string{{}, {"help"}, {"-h"}, {"--help"}} {
		code, stdout, stderr := run(t, args...)
		if code != 0 {
			t.Errorf("%v: exit = %d, stderr = %q", args, code, stderr)
		}
		if !strings.Contains(stdout, "config") {
			t.Errorf("%v: help does not mention the config command: %q", args, stdout)
		}
	}
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	code, _, stderr := run(t, "frobnicate")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Errorf("stderr %q should name the unknown command", stderr)
	}
}

func TestConfigHelpIsSuccessful(t *testing.T) {
	code, stdout, _ := run(t, "config", "--help")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{"--json", "--compact", "<workspace>"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("config help %q does not mention %q", stdout, want)
		}
	}
}

func TestConfigUnknownFlagIsAUsageError(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})
	code, _, stderr := run(t, "config", "--nope")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d; stderr = %q", code, ExitUsage, stderr)
	}
}

func TestConfigTooManyArgumentsIsAUsageError(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})
	code, _, _ := run(t, "config", "one", "two")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
}

func TestConfigPrintsHumanOutputForTheCurrentDirectory(t *testing.T) {
	slug := workspace(t, map[string]string{"defaults.yaml": validConfig})

	code, stdout, stderr := run(t, "config")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	for _, want := range []string{slug, "agent-1", "shell", "scratch", "sha256:", "claude", "host"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("human output does not mention %q:\n%s", want, stdout)
		}
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Error("human output should not be JSON by default")
	}
}

func TestConfigResolvesByWorkspaceName(t *testing.T) {
	slug := workspace(t, map[string]string{"defaults.yaml": validConfig})
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Configure the parent of the repository as a repository root, then resolve
	// from somewhere else entirely.
	rootsYAML := validConfig + "repository_roots:\n  - " + filepath.Dir(repo) + "\n"
	if err := os.WriteFile(
		filepath.Join(os.Getenv("PROJECTMUX_CONFIG_ROOT"), "defaults.yaml"),
		[]byte(rootsYAML), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Chdir(t.TempDir())

	code, stdout, stderr := run(t, "config", "--json", slug)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	var env envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout)
	}
	if env.Workspace.Slug != slug {
		t.Errorf("slug = %q", env.Workspace.Slug)
	}
	if env.Workspace.Worktree != repo {
		t.Errorf("worktree = %q, want %q", env.Workspace.Worktree, repo)
	}
}

func TestConfigJSONEnvelopeIsVersionedAndComplete(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})

	code, stdout, stderr := run(t, "config", "--json")
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &top); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout)
	}
	for _, key := range []string{"schema_version", "workspace", "repository_roots", "digest", "config"} {
		if _, ok := top[key]; !ok {
			t.Errorf("envelope is missing %q: %s", key, stdout)
		}
	}
	if len(top) != 5 {
		t.Errorf("envelope has %d keys, want 5: %s", len(top), stdout)
	}

	var env envelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, OutputSchemaVersion)
	}
	if !strings.HasPrefix(env.Digest, "sha256:") {
		t.Errorf("digest = %q", env.Digest)
	}
	if len(env.Workspace.ID) != 64 {
		t.Errorf("workspace id = %q", env.Workspace.ID)
	}
	if !env.Workspace.IsPrimary {
		t.Error("the repository is its own primary tree")
	}
	if len(env.Config.Windows) != 3 {
		t.Errorf("windows = %d, want 3", len(env.Config.Windows))
	}
	if env.Config.Environment["CGO_ENABLED"] != "1" {
		t.Errorf("environment = %v", env.Config.Environment)
	}

	// Default JSON is indented for reading; --compact is one line.
	if !strings.Contains(stdout, "\n  ") {
		t.Errorf("JSON output should be indented by default:\n%s", stdout)
	}
	_, compact, _ := run(t, "config", "--json", "--compact")
	if strings.Count(strings.TrimSpace(compact), "\n") != 0 {
		t.Errorf("--compact should emit one line:\n%s", compact)
	}
}

func TestConfigDigestIsStableAcrossInvocations(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})

	_, first, _ := run(t, "config", "--json", "--compact")
	_, second, _ := run(t, "config", "--json", "--compact")
	if first != second {
		t.Errorf("output not stable:\n%s\n%s", first, second)
	}
}

func TestInvalidConfigurationExitsFiveWithADiagnosticOnStderr(t *testing.T) {
	cases := map[string]struct {
		files map[string]string
		want  string
	}{
		"unknown field": {
			files: map[string]string{"defaults.yaml": "version: 1\nautostrt: true\n"},
			want:  "autostrt",
		},
		"two modes": {
			files: map[string]string{
				"defaults.yaml": "version: 1\nwindows:\n  - name: w\n    agent: a\n    command: c\n",
			},
			want: "exactly one of",
		},
		"unsupported version": {
			files: map[string]string{"defaults.yaml": "version: 9\n"},
			want:  "version",
		},
		"unparsable yaml": {
			files: map[string]string{"defaults.yaml": "version: 1\n  windows: [\n"},
			want:  "defaults.yaml",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			workspace(t, tc.files)
			code, stdout, stderr := run(t, "config")
			if code != ExitInvalidConfig {
				t.Errorf("exit = %d, want %d", code, ExitInvalidConfig)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not mention %q", stderr, tc.want)
			}
			if stdout != "" {
				t.Errorf("invalid configuration should print nothing to stdout, got %q", stdout)
			}
		})
	}
}

func TestUnknownWorkspaceExitsFour(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})
	code, _, stderr := run(t, "config", "nosuchproject")
	if code != ExitUnknownWorkspace {
		t.Errorf("exit = %d, want %d", code, ExitUnknownWorkspace)
	}
	if !strings.Contains(stderr, "nosuchproject") {
		t.Errorf("stderr %q should name the workspace", stderr)
	}
}

func TestAmbiguousWorkspaceExitsThree(t *testing.T) {
	slug := workspace(t, map[string]string{"defaults.yaml": validConfig})
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	base := filepath.Dir(repo)
	// A second root containing a directory of the same name.
	other := filepath.Join(base, "other")
	if err := os.MkdirAll(filepath.Join(other, slug), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := validConfig + "repository_roots:\n  - " + base + "\n  - " + other + "\n"
	if err := os.WriteFile(
		filepath.Join(os.Getenv("PROJECTMUX_CONFIG_ROOT"), "defaults.yaml"),
		[]byte(body), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	code, _, stderr := run(t, "config", slug)
	if code != ExitAmbiguous {
		t.Errorf("exit = %d, want %d; stderr = %q", code, ExitAmbiguous, stderr)
	}
	if !strings.Contains(stderr, "ambiguous") {
		t.Errorf("stderr %q should explain the ambiguity", stderr)
	}
}

func TestVersionIsReported(t *testing.T) {
	code, stdout, _ := run(t, "version")
	if code != 0 {
		t.Fatalf("exit = %d", code)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("version should print something")
	}
}
