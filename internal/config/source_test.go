package config

import (
	"path/filepath"
	"testing"
)

// A loaded layer carries the position of what it set, so a problem found
// after merging can name the file and line the reader must edit.
func TestLoadDefaultsCarriesItsSource(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml": originFixture,
	})

	src, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}

	if src.File != filepath.Join(root, "defaults.yaml") {
		t.Errorf("File = %q, want the defaults path under %q", src.File, root)
	}
	if got := lineOf(src.root, "windows[dev].location"); got != lineDevLocation {
		t.Errorf("windows[dev].location resolved to line %d, want %d", got, lineDevLocation)
	}
	if src.Layer.Version == nil || *src.Layer.Version != 1 {
		t.Errorf("Layer did not decode: %+v", src.Layer)
	}
}

// An absent layer is legal and empty, and must not carry a stale node that
// would attribute a later problem to a file that does not exist.
func TestLoadDefaultsOnAnAbsentFileHasNoPositions(t *testing.T) {
	src, err := LoadDefaults(writeRoot(t, nil))
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}

	if got := lineOf(src.root, "version"); got != 0 {
		t.Errorf("an absent defaults.yaml reported line %d for version, want 0", got)
	}
	if src.Layer.Version != nil {
		t.Errorf("an absent defaults.yaml decoded a version: %v", *src.Layer.Version)
	}
}

// A comment-only document decodes as empty, and likewise offers nothing.
func TestLoadDefaultsOnACommentOnlyFileHasNoPositions(t *testing.T) {
	root := writeRoot(t, map[string]string{"defaults.yaml": "# nothing here\n"})

	src, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	if got := lineOf(src.root, "version"); got != 0 {
		t.Errorf("a comment-only defaults.yaml reported line %d, want 0", got)
	}
}

// Load takes the defaults Source through unchanged, so the pipeline the CLI
// runs still produces a validated configuration.
func TestLoadAcceptsADefaultsSource(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml":             "version: 1\n",
		"workspaces/api.yaml":       "windows:\n  - name: dev\n    shell: true\n",
		"workspaces/api.local.yaml": "autostart: true\n",
	})

	defaults, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	effective, err := Load(root, defaults, "api")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !effective.Config.Autostart {
		t.Error("the local layer did not merge: autostart is false")
	}
	if len(effective.Config.Windows) != 1 || effective.Config.Windows[0].Name != "dev" {
		t.Errorf("windows did not merge: %+v", effective.Config.Windows)
	}
}
