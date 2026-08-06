package config

import (
	"path/filepath"
	"testing"
)

// mergeFixture builds the three-layer stack the CLI loads and merges it in
// the same order, returning the result and the root the files live under.
func mergeFixture(t *testing.T, defaults, workspace, local string) (Merged, string) {
	t.Helper()
	root := writeRoot(t, map[string]string{
		"defaults.yaml":             defaults,
		"workspaces/dev.yaml":       workspace,
		"workspaces/dev.local.yaml": local,
	})

	merged := Merged{}
	for _, name := range []string{"defaults.yaml", "workspaces/dev.yaml", "workspaces/dev.local.yaml"} {
		src, err := loadLayer(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("loadLayer %s: %v", name, err)
		}
		merged = mergeLayers(merged, src)
	}
	return merged, root
}

// wantOrigin asserts which file and line a field path is credited to.
func wantOrigin(t *testing.T, m Merged, root, path, file string, line int) {
	t.Helper()
	got := m.origins[path]
	if want := filepath.Join(root, file); got.File != want {
		t.Errorf("origin of %q is %q, want %q", path, got.File, want)
	}
	if got.Line != line {
		t.Errorf("origin of %q is line %d, want %d", path, got.Line, line)
	}
}

// The layer that last set a field owns it: that is the file the reader has
// to edit, regardless of how many earlier layers also mentioned it.
func TestMergeCreditsTheLayerThatWon(t *testing.T) {
	merged, root := mergeFixture(t,
		"version: 1\nautostart: false\ndevcontainer:\n  enabled: auto\n",
		"autostart: true\ndevcontainer:\n  enabled: 'true'\n",
		"devcontainer:\n  enabled: 'false'\n",
	)

	// Set by defaults only.
	wantOrigin(t, merged, root, "version", "defaults.yaml", 1)
	// Set by defaults and overridden once.
	wantOrigin(t, merged, root, "autostart", "workspaces/dev.yaml", 1)
	// Set by all three: the last one wins.
	wantOrigin(t, merged, root, "devcontainer.enabled", "workspaces/dev.local.yaml", 2)
}

// A window's fields are credited independently, so overriding one does not
// move the blame for the others.
func TestMergeCreditsWindowFieldsIndependently(t *testing.T) {
	merged, root := mergeFixture(t,
		"windows:\n  - name: dev\n    command: vim\n    cwd: src\n",
		"windows:\n  - name: dev\n    location: container\n",
		"",
	)

	wantOrigin(t, merged, root, "windows[dev].cwd", "defaults.yaml", 4)
	wantOrigin(t, merged, root, "windows[dev].location", "workspaces/dev.yaml", 3)
	// The window itself is credited where it first appeared, which is what
	// the fallback ladder reports for a problem owning no single field.
	wantOrigin(t, merged, root, "windows[dev]", "defaults.yaml", 2)
}

// Mode merges as a unit: a layer naming any of agent, command, or shell
// replaces all three, so all three must be credited to that layer. Crediting
// them field-wise would blame defaults for a command the overlay removed.
func TestMergeCreditsModeAsAUnit(t *testing.T) {
	merged, root := mergeFixture(t,
		"windows:\n  - name: dev\n    command: vim\n",
		"windows:\n  - name: dev\n    shell: true\n",
		"",
	)

	for _, path := range []string{
		"windows[dev].agent",
		"windows[dev].command",
		"windows[dev].shell",
	} {
		wantOrigin(t, merged, root, path, "workspaces/dev.yaml", 3)
	}
}

// A window only the overlay names is credited entirely to the overlay.
func TestMergeCreditsAWindowIntroducedByAnOverlay(t *testing.T) {
	merged, root := mergeFixture(t,
		"windows:\n  - name: dev\n    shell: true\n",
		"windows:\n  - name: logs\n    command: tail\n",
		"",
	)

	wantOrigin(t, merged, root, "windows[logs]", "workspaces/dev.yaml", 2)
	wantOrigin(t, merged, root, "windows[logs].command", "workspaces/dev.yaml", 3)
	wantOrigin(t, merged, root, "windows[dev]", "defaults.yaml", 2)
}

// Environment entries are credited per variable, so one overridden value
// does not reattribute the rest.
func TestMergeCreditsEnvironmentPerVariable(t *testing.T) {
	merged, root := mergeFixture(t,
		"environment:\n  EDITOR: vi\n  PAGER: less\n",
		"environment:\n  EDITOR: nvim\n",
		"",
	)

	wantOrigin(t, merged, root, "environment.PAGER", "defaults.yaml", 3)
	wantOrigin(t, merged, root, "environment.EDITOR", "workspaces/dev.yaml", 2)
}

// Merging returns a new value and leaves its input alone.
//
// mergeLayers copies the struct, but a map copied by value is still the same
// map. Without an explicit clone, two branches from one base would write into
// each other's attributions and report the wrong file — silently, since every
// lookup still succeeds.
func TestMergeDoesNotMutateItsBase(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml":             "autostart: false\n",
		"workspaces/dev.yaml":       "autostart: true\n",
		"workspaces/dev.local.yaml": "autostart: false\n",
	})
	load := func(name string) Source {
		t.Helper()
		src, err := loadLayer(filepath.Join(root, name))
		if err != nil {
			t.Fatalf("loadLayer %s: %v", name, err)
		}
		return src
	}

	base := mergeLayers(Merged{root: root}, load("defaults.yaml"))
	baseOrigin := base.origins["autostart"]

	// Two independent branches from the same base.
	left := mergeLayers(base, load("workspaces/dev.yaml"))
	right := mergeLayers(base, load("workspaces/dev.local.yaml"))

	if got := base.origins["autostart"]; got != baseOrigin {
		t.Errorf("merging rewrote its base: autostart moved from %+v to %+v", baseOrigin, got)
	}
	if left.origins["autostart"] == right.origins["autostart"] {
		t.Errorf("branches share attribution: both report %+v", left.origins["autostart"])
	}
	if want := filepath.Join(root, "workspaces/dev.yaml"); left.origins["autostart"].File != want {
		t.Errorf("left credits %q, want %q", left.origins["autostart"].File, want)
	}
	if want := filepath.Join(root, "workspaces/dev.local.yaml"); right.origins["autostart"].File != want {
		t.Errorf("right credits %q, want %q", right.origins["autostart"].File, want)
	}
}

// A field no layer set has no origin at all. Zero is the honest answer;
// attributing it to the last file loaded would point at innocent lines.
func TestMergeLeavesUnsetFieldsUnattributed(t *testing.T) {
	merged, _ := mergeFixture(t, "version: 1\n", "", "")

	if got, ok := merged.origins["autostart"]; ok {
		t.Errorf("an unset autostart was credited to %+v", got)
	}
	if got, ok := merged.origins["windows[dev].location"]; ok {
		t.Errorf("a field of an absent window was credited to %+v", got)
	}
}
