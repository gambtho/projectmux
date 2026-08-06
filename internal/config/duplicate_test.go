package config

import (
	"strings"
	"testing"
)

// A duplicate in defaults.yaml is invisible on an installation with no
// workspace files, because detection used to live in the merge and nothing
// merges. This is the regression: it fails against the pre-slice code.
func TestValidateDefaultsRejectsADuplicateWindow(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml": "version: 1\n" +
			"windows:\n" +
			"  - name: dev\n" +
			"    shell: true\n" +
			"  - name: dev\n" +
			"    command: vim\n",
	})
	src, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}

	problems := ValidateDefaults(src)
	var found *Problem
	for i, p := range problems {
		if strings.Contains(p.Message, "defined more than once") {
			found = &problems[i]
		}
	}
	if found == nil {
		t.Fatalf("a duplicated window in defaults.yaml was not reported: %+v", problems)
	}
	// Both definitions are named: either line looks correct alone, and the
	// duplicate is only visible as a pair.
	if len(found.Origins) != 2 {
		t.Fatalf("Origins = %+v, want both definitions", found.Origins)
	}
	if found.Origins[0].Line != 3 || found.Origins[1].Line != 5 {
		t.Errorf("origins = %+v, want lines 3 and 5 in that order", found.Origins)
	}
	if found.Origins[0].File != "defaults.yaml" {
		t.Errorf("origin file = %q, want defaults.yaml", found.Origins[0].File)
	}
}

// Reading defaults alone must stay a warning path: the file parsed, so every
// other workspace is still diagnosable. LoadDefaults therefore keeps
// succeeding, and the duplicate surfaces through validation instead.
func TestLoadDefaultsSucceedsDespiteADuplicateWindow(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml": "windows:\n  - name: dev\n    shell: true\n  - name: dev\n    shell: true\n",
	})

	if _, err := LoadDefaults(root); err != nil {
		t.Fatalf("LoadDefaults turned a duplicate into a load failure: %v", err)
	}
}

// Load still rejects a duplicate, in whichever layer it appears.
func TestLoadRejectsADuplicateWindow(t *testing.T) {
	cases := map[string]map[string]string{
		"in defaults": {
			"defaults.yaml":       "version: 1\nwindows:\n  - name: dev\n    shell: true\n  - name: dev\n    shell: true\n",
			"workspaces/dev.yaml": "",
		},
		"in the workspace layer": {
			"defaults.yaml":       "version: 1\n",
			"workspaces/dev.yaml": "windows:\n  - name: dev\n    shell: true\n  - name: dev\n    shell: true\n",
		},
	}
	for name, files := range cases {
		t.Run(name, func(t *testing.T) {
			problems := problemsFor(t, files)
			if p := find(t, problems, "defined more than once"); p.Field != "windows[dev]" {
				t.Errorf("Field = %q, want windows[dev]", p.Field)
			}
		})
	}
}

// The same name in two different files is the merge working as intended: a
// workspace adjusting a default window, not a mistake.
func TestLoadAcceptsTheSameWindowNameAcrossLayers(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml":       "version: 1\nwindows:\n  - name: dev\n    shell: true\n",
		"workspaces/dev.yaml": "windows:\n  - name: dev\n    command: vim\n",
	})
	defaults, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	if _, err := Load(root, defaults, "dev"); err != nil {
		t.Fatalf("Load rejected a legitimate cross-layer override: %v", err)
	}
}

// Error ordering is unchanged: a layer that will not decode is reported
// before a duplicate elsewhere, exactly as when the check lived in the merge.
func TestLoadReportsADecodeFailureBeforeADuplicate(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml":       "version: 1\nwindows:\n  - name: dev\n    shell: true\n  - name: dev\n    shell: true\n",
		"workspaces/dev.yaml": "windows:\n  - name: dev\n    nonsense: true\n",
	})
	defaults, err := LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}

	_, err = Load(root, defaults, "dev")
	if err == nil {
		t.Fatal("Load accepted an undecodable overlay")
	}
	if !strings.Contains(err.Error(), "nonsense") {
		t.Errorf("error = %q, want the decode failure reported first", err)
	}
	if strings.Contains(err.Error(), "defined more than once") {
		t.Errorf("error = %q, want the decode failure alone", err)
	}
}
