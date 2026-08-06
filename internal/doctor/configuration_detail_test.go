package doctor

import (
	"strings"
	"testing"
)

// A failing workspace reports how many problems it has and where the first
// one is, then points at the command that lists the rest.
//
// Doctor's altitude is the whole installation, so it summarizes rather than
// reproducing every problem — but a summary with no position is what sent
// readers hunting through three files in the first place.
func TestConfigurationFailingWorkspaceCarriesPositionAndPointer(t *testing.T) {
	root, defaults := writeConfig(t, map[string]string{
		"defaults.yaml": "version: 1\ndevcontainer:\n  enabled: 'false'\n",
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    location: container\n" +
			"  - name: logs\n" +
			"    cwd: ../escape\n",
	})
	r := &Runner{ConfigRoot: root, Defaults: defaults}

	item := findItem(t, r.configuration(), "dev")
	if item.Status != StatusFail {
		t.Fatalf("status = %q, want fail", item.Status)
	}
	for _, want := range []string{
		"3 problems",
		"workspaces/dev.yaml:4",
		"projectmux config --validate dev",
	} {
		if !strings.Contains(item.Detail, want) {
			t.Errorf("detail does not mention %q:\n%s", want, item.Detail)
		}
	}
	// The full list belongs to the focused command, not to doctor.
	if strings.Contains(item.Detail, "escape the worktree") {
		t.Errorf("detail reproduced every problem instead of summarizing:\n%s", item.Detail)
	}
}

// A single problem is described as one, not as "1 problems".
func TestConfigurationSingleProblemReadsNaturally(t *testing.T) {
	root, defaults := writeConfig(t, map[string]string{
		"defaults.yaml":       "version: 1\n",
		"workspaces/dev.yaml": "windows:\n  - name: docs\n",
	})
	r := &Runner{ConfigRoot: root, Defaults: defaults}

	item := findItem(t, r.configuration(), "dev")
	if !strings.Contains(item.Detail, "1 problem,") {
		t.Errorf("detail = %q, want it to read as one problem", item.Detail)
	}
}

// A duplicated window name in defaults.yaml warns on the defaults item and
// does not abort the check: every workspace is still examined.
//
// Those workspaces do fail, and that is accurate rather than noise — defaults
// is part of each one's configuration, so opening any of them would refuse.
// What matters for the reader is that the position they are sent to is the
// defaults file, so one edit fixes all of them.
func TestConfigurationDuplicateInDefaultsWarnsAndKeepsDiagnosing(t *testing.T) {
	root, defaults := writeConfig(t, map[string]string{
		"defaults.yaml": "version: 1\n" +
			"windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"  - name: dev\n" +
			"    shell: true\n",
		"workspaces/api.yaml": "windows:\n  - name: api\n    shell: true\n",
	})
	r := &Runner{ConfigRoot: root, Defaults: defaults}
	check := r.configuration()

	defaultsItem := findItem(t, check, "defaults")
	if defaultsItem.Status != StatusWarn {
		t.Errorf("defaults status = %q, want warn", defaultsItem.Status)
	}
	if !strings.Contains(defaultsItem.Detail, "defined more than once") {
		t.Errorf("defaults detail does not report the duplicate:\n%s", defaultsItem.Detail)
	}
	if !strings.Contains(defaultsItem.Detail, "defaults.yaml:3") {
		t.Errorf("defaults detail lacks a position:\n%s", defaultsItem.Detail)
	}
	// The other workspace was still examined rather than skipped, and the
	// position it names is the defaults file — one edit, not one per
	// workspace.
	api := findItem(t, check, "api")
	if api.Status != StatusFail {
		t.Errorf("api status = %q, want fail: a broken defaults layer is part of api's configuration", api.Status)
	}
	if !strings.Contains(api.Detail, "defaults.yaml:") {
		t.Errorf("api detail does not point at the defaults file:\n%s", api.Detail)
	}
}
