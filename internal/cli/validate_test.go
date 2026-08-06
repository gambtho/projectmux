package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configOnly points the CLI at a configuration root and nothing else.
// --validate deliberately performs no workspace resolution, so unlike the
// other commands it needs no git repository to run against.
func configOnly(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
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
	return root
}

const goodWorkspace = "windows:\n  - name: dev\n    shell: true\n"

func TestValidateCleanInstallationExitsZero(t *testing.T) {
	configOnly(t, map[string]string{
		"defaults.yaml":       "version: 1\n",
		"workspaces/api.yaml": goodWorkspace,
	})

	code, stdout, stderr := run(t, "config", "--validate")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\nstdout:\n%s\nstderr:\n%s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "api") {
		t.Errorf("stdout does not mention the workspace:\n%s", stdout)
	}
}

// The report is the output, so it goes to stdout even though the command
// fails, and the exit code stays the invalid-configuration one.
func TestValidateReportsProblemsOnStdoutAndExitsFive(t *testing.T) {
	configOnly(t, map[string]string{
		"defaults.yaml": "version: 1\ndevcontainer:\n  enabled: 'false'\n",
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    location: container\n",
	})

	code, stdout, stderr := run(t, "config", "--validate", "dev")
	if code != ExitInvalidConfig {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s", code, ExitInvalidConfig, stdout)
	}
	if !strings.Contains(stdout, "workspaces/dev.yaml:4") {
		t.Errorf("stdout lacks the offending position:\n%s", stdout)
	}
	if !strings.Contains(stdout, "devcontainer.enabled is false") {
		t.Errorf("stdout lacks the message:\n%s", stdout)
	}
	if strings.TrimSpace(stderr) == "" {
		t.Error("stderr carried no one-line summary")
	}
	if strings.Contains(stderr, "workspaces/dev.yaml:4") {
		t.Errorf("stderr duplicated the report instead of summarizing:\n%s", stderr)
	}
}

// Naming a workspace that has no configuration file is a caller mistake, not
// a resolver verdict: --validate never consults git, so exit 4 would imply a
// lookup that never happened.
func TestValidateUnknownSlugExitsTwoAndListsWhatExists(t *testing.T) {
	configOnly(t, map[string]string{
		"workspaces/api.yaml": goodWorkspace,
		"workspaces/web.yaml": goodWorkspace,
	})

	code, _, stderr := run(t, "config", "--validate", "nope")
	if code != ExitUsage {
		t.Fatalf("exit = %d, want %d\nstderr:\n%s", code, ExitUsage, stderr)
	}
	for _, want := range []string{"nope", "api", "web"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr does not mention %q:\n%s", want, stderr)
		}
	}
}

// The argument names a workspace file, never a path. Membership in the
// discovered set is what makes traversal unreachable.
func TestValidateRejectsAnArgumentThatIsNotASlug(t *testing.T) {
	configOnly(t, map[string]string{"workspaces/api.yaml": goodWorkspace})

	for _, arg := range []string{
		"../../etc/passwd",
		"nested/api",
		"api*",
		"..",
		"/etc/passwd",
	} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, _ := run(t, "config", "--validate", arg)
			if code != ExitUsage {
				t.Errorf("exit = %d for %q, want %d", code, arg, ExitUsage)
			}
			if strings.Contains(stdout, "root:") || strings.Contains(stdout, "passwd") {
				t.Errorf("output suggests a file outside the root was read:\n%s", stdout)
			}
		})
	}
}

// With no argument every discovered workspace is checked, so one bad file is
// found without knowing which to ask about.
func TestValidateWithNoArgumentChecksEveryWorkspace(t *testing.T) {
	configOnly(t, map[string]string{
		"defaults.yaml":        "version: 1\n",
		"workspaces/good.yaml": goodWorkspace,
		"workspaces/bad.yaml":  "windows:\n  - name: broken\n",
	})

	code, stdout, _ := run(t, "config", "--validate")
	if code != ExitInvalidConfig {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitInvalidConfig, stdout)
	}
	if !strings.Contains(stdout, "good") || !strings.Contains(stdout, "bad") {
		t.Errorf("stdout does not cover every workspace:\n%s", stdout)
	}
	if !strings.Contains(stdout, "exactly one of") {
		t.Errorf("stdout lacks the problem:\n%s", stdout)
	}
}

// Defaults read alone are legitimately incomplete, so problems found there
// are a warning and the command still succeeds.
func TestValidateWarningsOnlyExitsZero(t *testing.T) {
	configOnly(t, map[string]string{
		"defaults.yaml": "version: 9\n",
	})

	code, stdout, _ := run(t, "config", "--validate")
	if code != ExitOK {
		t.Fatalf("exit = %d, want 0\n%s", code, stdout)
	}
	if !strings.Contains(stdout, "warn") {
		t.Errorf("stdout does not mark the defaults warning:\n%s", stdout)
	}
}

// A directory that cannot be read is uncertainty, and must not read as a
// clean installation. It is an I/O failure, not invalid configuration.
func TestValidateUnreadableWorkspacesDirectoryExitsOne(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := configOnly(t, map[string]string{"workspaces/api.yaml": goodWorkspace})
	dir := filepath.Join(root, "workspaces")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	code, stdout, stderr := run(t, "config", "--validate")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d\nstdout:\n%s\nstderr:\n%s", code, ExitError, stdout, stderr)
	}
}

// A confirmed defect outranks unexamined ground: both are reported, and the
// exit code names the one that is actionable.
func TestValidateInvalidOutranksUnknown(t *testing.T) {
	configOnly(t, map[string]string{
		"defaults.yaml":       "version: 1\n",
		"workspaces/bad.yaml": "windows:\n  - name: broken\n",
	})

	code, stdout, _ := run(t, "config", "--validate")
	if code != ExitInvalidConfig {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitInvalidConfig, stdout)
	}
}

// The summary counts the invalid findings, not every problem in the report.
// Defaults warnings are report content and are not part of what makes the
// command fail, so counting them would state a number matching neither the
// failure nor any section above it.
func TestValidateSummaryCountsOnlyInvalidProblems(t *testing.T) {
	configOnly(t, map[string]string{
		// One warning: defaults read alone has an unsupported version.
		"defaults.yaml": "version: 9\n",
		// One genuine failure in a workspace.
		"workspaces/dev.yaml": "windows:\n  - name: broken\n",
	})

	code, stdout, stderr := run(t, "config", "--validate")
	if code != ExitInvalidConfig {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitInvalidConfig, stdout)
	}
	// The workspace contributes the only invalid problems; the defaults
	// warning must not inflate the count.
	if strings.Contains(stderr, "(3 problems)") {
		t.Errorf("summary counted the defaults warning:\n%s", stderr)
	}
	if !strings.Contains(stderr, "(2 problems)") {
		t.Errorf("summary = %q, want the two invalid problems", strings.TrimSpace(stderr))
	}
}

func TestValidateJSONShape(t *testing.T) {
	configOnly(t, map[string]string{
		"defaults.yaml": "version: 1\ndevcontainer:\n  enabled: 'false'\n",
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    location: container\n",
	})

	code, stdout, _ := run(t, "config", "--validate", "--json", "dev")
	if code != ExitInvalidConfig {
		t.Fatalf("exit = %d, want %d\n%s", code, ExitInvalidConfig, stdout)
	}

	var got struct {
		SchemaVersion int    `json:"schema_version"`
		ConfigRoot    string `json:"config_root"`
		Results       []struct {
			Subject  string `json:"subject"`
			Status   string `json:"status"`
			Problems []struct {
				Field   string `json:"field"`
				Message string `json:"message"`
				Origins []struct {
					File string `json:"file"`
					Line int    `json:"line"`
				} `json:"origins"`
			} `json:"problems"`
		} `json:"results"`
		Summary struct {
			Subjects     int `json:"subjects"`
			WithProblems int `json:"with_problems"`
			Problems     int `json:"problems"`
		} `json:"summary"`
	}
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("decoding %q: %v", stdout, err)
	}

	if got.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d, want %d", got.SchemaVersion, OutputSchemaVersion)
	}
	if got.Summary.Problems != 1 || got.Summary.WithProblems != 1 {
		t.Errorf("summary = %+v, want one problem in one subject", got.Summary)
	}

	var dev *struct {
		Subject  string `json:"subject"`
		Status   string `json:"status"`
		Problems []struct {
			Field   string `json:"field"`
			Message string `json:"message"`
			Origins []struct {
				File string `json:"file"`
				Line int    `json:"line"`
			} `json:"origins"`
		} `json:"problems"`
	}
	for i := range got.Results {
		if got.Results[i].Subject == "dev" {
			dev = &got.Results[i]
		}
	}
	if dev == nil {
		t.Fatalf("no result for dev: %+v", got.Results)
	}
	if dev.Status != "invalid" {
		t.Errorf("status = %q, want invalid", dev.Status)
	}
	if len(dev.Problems) != 1 {
		t.Fatalf("problems = %+v, want one", dev.Problems)
	}
	p := dev.Problems[0]
	if p.Field != "windows[dev].location" {
		t.Errorf("field = %q, want windows[dev].location", p.Field)
	}
	// Both sides of the conflict, primary first.
	if len(p.Origins) != 2 {
		t.Fatalf("origins = %+v, want two", p.Origins)
	}
	if p.Origins[0].File != "workspaces/dev.yaml" || p.Origins[0].Line != 4 {
		t.Errorf("primary origin = %+v, want workspaces/dev.yaml:4", p.Origins[0])
	}
	if p.Origins[1].File != "defaults.yaml" || p.Origins[1].Line != 3 {
		t.Errorf("secondary origin = %+v, want defaults.yaml:3", p.Origins[1])
	}
}
