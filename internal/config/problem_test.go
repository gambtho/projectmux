package config

import (
	"errors"
	"strings"
	"testing"
)

// problemsFor runs the real pipeline and returns the problems it rejected.
func problemsFor(t *testing.T, files map[string]string) []Problem {
	t.Helper()
	root := writeRoot(t, files)
	defaults, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	_, err = Load(root, defaults, "dev")
	if err == nil {
		t.Fatal("Load accepted a configuration the test expected it to reject")
	}
	var invalid *InvalidConfigError
	if !errors.As(err, &invalid) {
		t.Fatalf("Load returned %T, want *InvalidConfigError: %v", err, err)
	}
	return invalid.Problems
}

// find returns the one problem whose message mentions want.
func find(t *testing.T, problems []Problem, want string) Problem {
	t.Helper()
	var found []Problem
	for _, p := range problems {
		if strings.Contains(p.Message, want) {
			found = append(found, p)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want exactly one problem mentioning %q, got %d: %+v", want, len(found), problems)
	}
	return found[0]
}

// A problem names the field it is about and the position that set it, so a
// reader can go straight to the line rather than searching three files.
func TestProblemCarriesTheFieldAndPositionThatSetIt(t *testing.T) {
	problems := problemsFor(t, map[string]string{
		"defaults.yaml": "version: 1\n",
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    cwd: ../outside\n",
	})

	p := find(t, problems, "escape the repository root")
	if p.Field != "windows[dev].cwd" {
		t.Errorf("Field = %q, want windows[dev].cwd", p.Field)
	}
	if len(p.Origins) != 1 {
		t.Fatalf("Origins = %+v, want exactly one", p.Origins)
	}
	if p.Origins[0].File != "workspaces/dev.yaml" {
		t.Errorf("origin file = %q, want workspaces/dev.yaml", p.Origins[0].File)
	}
	if p.Origins[0].Line != 4 {
		t.Errorf("origin line = %d, want 4", p.Origins[0].Line)
	}
}

// A failure owned jointly by two fields reports both positions, primary
// first. Naming only one would send the reader to a line that is correct on
// its own — the conflict lives between the two.
func TestProblemReportsBothSidesOfACrossFieldConflict(t *testing.T) {
	problems := problemsFor(t, map[string]string{
		"defaults.yaml": "version: 1\ndevcontainer:\n  enabled: 'false'\n",
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    location: container\n",
	})

	p := find(t, problems, "devcontainer.enabled is false")
	if p.Field != "windows[dev].location" {
		t.Errorf("Field = %q, want windows[dev].location", p.Field)
	}
	if len(p.Origins) != 2 {
		t.Fatalf("Origins = %+v, want two", p.Origins)
	}
	// Primary first: the field the problem is about.
	if p.Origins[0].File != "workspaces/dev.yaml" || p.Origins[0].Line != 4 {
		t.Errorf("primary origin = %+v, want workspaces/dev.yaml:4", p.Origins[0])
	}
	if p.Origins[1].File != "defaults.yaml" || p.Origins[1].Line != 3 {
		t.Errorf("secondary origin = %+v, want defaults.yaml:3", p.Origins[1])
	}
}

// Duplicated focus is owned by no single field, so it names every focused
// window, ordered by layer and then by line.
func TestProblemReportsEveryFocusedWindowInLayerOrder(t *testing.T) {
	problems := problemsFor(t, map[string]string{
		"defaults.yaml": "version: 1\n" +
			"windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    focus: true\n",
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: logs\n" +
			"    command: tail\n" +
			"    focus: true\n",
	})

	p := find(t, problems, "more than one window sets focus")
	if p.Field != "" {
		t.Errorf("Field = %q, want empty for a problem owning no single field", p.Field)
	}
	if len(p.Origins) != 2 {
		t.Fatalf("Origins = %+v, want two", p.Origins)
	}
	if p.Origins[0].File != "defaults.yaml" || p.Origins[0].Line != 5 {
		t.Errorf("first origin = %+v, want defaults.yaml:5", p.Origins[0])
	}
	if p.Origins[1].File != "workspaces/dev.yaml" || p.Origins[1].Line != 4 {
		t.Errorf("second origin = %+v, want workspaces/dev.yaml:4", p.Origins[1])
	}
}

// A window that names no mode has no offending key, but the window itself is
// written down — so the problem attributes to the window, not to nothing.
func TestProblemAboutAnAbsentKeyAttributesToItsWindow(t *testing.T) {
	problems := problemsFor(t, map[string]string{
		"defaults.yaml":       "version: 1\n",
		"workspaces/dev.yaml": "windows:\n  - name: docs\n",
	})

	p := find(t, problems, "exactly one of")
	if len(p.Origins) != 1 {
		t.Fatalf("Origins = %+v, want the window's own position", p.Origins)
	}
	if p.Origins[0].File != "workspaces/dev.yaml" || p.Origins[0].Line != 2 {
		t.Errorf("origin = %+v, want workspaces/dev.yaml:2", p.Origins[0])
	}
}

// A required key no layer mentions has nothing to point at anywhere, and
// must report no position rather than blaming an arbitrary file.
func TestProblemAboutAKeyNoLayerSetHasNoOrigin(t *testing.T) {
	problems := problemsFor(t, map[string]string{
		"workspaces/dev.yaml": "windows:\n  - name: dev\n    shell: true\n",
	})

	p := find(t, problems, "version is required")
	if len(p.Origins) != 0 {
		t.Errorf("Origins = %+v, want none for a key no layer set", p.Origins)
	}
}

// The whole report a user actually sees: every problem, each prefixed by the
// file and line to edit, with positions relative to the configuration root so
// the text reads the same on any machine.
func TestInvalidConfigErrorRendersAWholeReport(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml": "version: 1\n" +
			"devcontainer:\n" +
			"  enabled: 'false'\n",
		"workspaces/dev.yaml": "windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"    location: container\n" +
			"  - name: logs\n" +
			"    command: tail\n" +
			"    cwd: ../escape\n",
	})
	defaults, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	if _, err = Load(root, defaults, "dev"); err == nil {
		t.Fatal("Load accepted an invalid configuration")
	}

	got := err.Error()
	t.Logf("rendered report:\n%s", got)

	for _, want := range []string{
		"invalid configuration (2 problems):",
		"workspaces/dev.yaml:4: window \"dev\" sets location: container",
		// The other half of the conflict lives in a different file, and the
		// line it names is correct in isolation — so the report has to say
		// where the contradiction comes from, not just where it was noticed.
		"(also defaults.yaml:3)",
		"workspaces/dev.yaml:7: window \"logs\" cwd must not escape",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report is missing %q:\n%s", want, got)
		}
	}
	// Positions are relative to the root, never absolute machine paths.
	if strings.Contains(got, root) {
		t.Errorf("report leaked the absolute root %q:\n%s", root, got)
	}
}

// Rendering puts the position in front of the message, and omits it when
// there is none — never printing a bare ":0".
func TestInvalidConfigErrorRendersPositions(t *testing.T) {
	withOrigin := &InvalidConfigError{Problems: []Problem{{
		Field:   "windows[dev].cwd",
		Message: `window "dev" cwd must not escape the repository root`,
		Origins: []Origin{{File: "workspaces/dev.yaml", Line: 4}},
	}}}
	if got := withOrigin.Error(); !strings.Contains(got, "workspaces/dev.yaml:4:") {
		t.Errorf("Error() = %q, want it to carry the position", got)
	}

	withoutOrigin := &InvalidConfigError{Problems: []Problem{{
		Message: "version is required and must be 1",
	}}}
	got := withoutOrigin.Error()
	if strings.Contains(got, ":0") {
		t.Errorf("Error() = %q, want no fabricated position", got)
	}
	if !strings.Contains(got, "version is required") {
		t.Errorf("Error() = %q, want the message", got)
	}
}
