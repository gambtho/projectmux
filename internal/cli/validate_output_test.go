package cli

import (
	"strings"
	"testing"
)

// The whole report a user sees on a broken installation. Printed rather than
// only asserted piecemeal: passing structural assertions can coexist with
// output that reads badly, and the point of this slice is the reading.
func TestValidateWholeReportReads(t *testing.T) {
	configOnly(t, map[string]string{
		"defaults.yaml": "version: 1\n" +
			"devcontainer:\n" +
			"  enabled: 'false'\n",
		"workspaces/api.yaml": goodWorkspace,
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    location: container\n" +
			"  - name: logs\n" +
			"    cwd: ../escape\n",
	})

	code, stdout, stderr := run(t, "config", "--validate")
	t.Logf("exit %d\n--- stdout ---\n%s\n--- stderr ---\n%s", code, stdout, stderr)

	// Every subject appears, clean ones included: a report that lists only
	// failures cannot distinguish "checked and fine" from "not checked".
	for _, subject := range []string{"defaults.yaml", "api", "dev"} {
		if !strings.Contains(stdout, subject) {
			t.Errorf("report omits subject %q:\n%s", subject, stdout)
		}
	}
	// The two problems in dev are both reported, with positions.
	for _, want := range []string{
		"workspaces/dev.yaml:4",
		"workspaces/dev.yaml:5",
		"exactly one of",
		"escape the repository root",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("report omits %q:\n%s", want, stdout)
		}
	}
}
