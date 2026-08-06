package config

import (
	"path/filepath"
	"testing"
)

func loadOne(t *testing.T, body string) Source {
	t.Helper()
	root := writeRoot(t, map[string]string{"defaults.yaml": body})
	src, err := loadLayer(filepath.Join(root, "defaults.yaml"))
	if err != nil {
		t.Fatalf("loadLayer: %v", err)
	}
	return src
}

// Strict decoding expands an aliased sequence element, so two entries that
// share an anchor are a real duplicate. Both definitions must be reported:
// the alias line is where the second one enters, and it is the line a reader
// has to delete.
func TestWindowLinesFindsAnAliasedElement(t *testing.T) {
	src := loadOne(t, "version: 1\n"+
		"windows:\n"+
		"  - &devwin\n"+
		"    name: dev\n"+
		"    shell: true\n"+
		"  - *devwin\n")

	if len(src.Layer.Windows) != 2 {
		t.Fatalf("decoded %d windows, want 2 — the alias did not expand", len(src.Layer.Windows))
	}
	lines := windowLines(src.root, "dev")
	if len(lines) != 2 {
		t.Fatalf("windowLines = %v, want both definitions", lines)
	}
	if lines[0] != 3 || lines[1] != 6 {
		t.Errorf("windowLines = %v, want [3 6]: the anchor and the alias that repeats it", lines)
	}
}

// The duplicate that alias produces is reported with both positions, like any
// other duplicate.
func TestDuplicateViaAliasReportsBothPositions(t *testing.T) {
	src := loadOne(t, "version: 1\n"+
		"windows:\n"+
		"  - &devwin\n"+
		"    name: dev\n"+
		"    shell: true\n"+
		"  - *devwin\n")

	merged := mergeLayers(Merged{root: filepath.Dir(src.File)}, src)
	problems := merged.duplicateWindows(src)
	if len(problems) != 1 {
		t.Fatalf("problems = %+v, want one duplicate", problems)
	}
	if len(problems[0].Origins) != 2 {
		t.Errorf("origins = %+v, want both definitions", problems[0].Origins)
	}
}

// A merge key expands into the decoded layer, so the field has a real value
// and should have a real position rather than degrading to file-only.
func TestLineOfResolvesAMergeKey(t *testing.T) {
	src := loadOne(t, "version: 1\n"+
		"windows:\n"+
		"  - name: dev\n"+
		"    shell: true\n"+
		"devcontainer:\n"+
		"  <<: {enabled: 'false'}\n")

	if dc := src.Layer.DevContainer; dc == nil || dc.Enabled == nil || *dc.Enabled != "false" {
		t.Fatalf("the merge key did not decode: %+v", src.Layer.DevContainer)
	}
	if got := lineOf(src.root, "devcontainer.enabled"); got != 6 {
		t.Errorf("lineOf = %d, want 6 — the line the merge key sits on", got)
	}
}

// An aliased mapping value resolves through to the anchored content.
func TestLineOfResolvesAnAliasedMapping(t *testing.T) {
	src := loadOne(t, "version: 1\n"+
		"devcontainer: &dc\n"+
		"  enabled: 'false'\n"+
		"windows:\n"+
		"  - name: dev\n"+
		"    shell: true\n")

	if got := lineOf(src.root, "devcontainer.enabled"); got != 3 {
		t.Errorf("lineOf = %d, want 3", got)
	}
	// The anchor itself must not confuse window lookup.
	if got := lineOf(src.root, "windows[dev]"); got != 5 {
		t.Errorf("lineOf(windows[dev]) = %d, want 5", got)
	}
}
