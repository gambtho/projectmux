package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// Slugs lists the tracked workspace layers. The machine-local files are not
// separate subjects: Load already merges each under its slug.
func TestSlugsListsTrackedWorkspaceLayers(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/api.yaml":        "",
		"workspaces/dev.yaml":        "",
		"workspaces/dev.local.yaml":  "",
		"workspaces/notes.md":        "",
		"workspaces/.hidden.yaml":    "",
		"workspaces/nested/api.yaml": "",
	})

	slugs, err := Slugs(root)
	if err != nil {
		t.Fatalf("Slugs: %v", err)
	}
	if want := []string{".hidden", "api", "dev"}; !slices.Equal(slugs, want) {
		t.Errorf("Slugs = %v, want %v", slugs, want)
	}
}

// An absent workspaces directory is a fresh installation: confirmed absence,
// so an empty list and no error.
func TestSlugsOnAnAbsentDirectoryIsEmpty(t *testing.T) {
	slugs, err := Slugs(writeRoot(t, nil))
	if err != nil {
		t.Fatalf("Slugs on an absent directory: %v", err)
	}
	if len(slugs) != 0 {
		t.Errorf("Slugs = %v, want none", slugs)
	}
}

// A directory that cannot be read is uncertainty, not emptiness. Reporting
// "no workspaces" here would be an affirmative answer over unexamined
// ground — the failure this rule exists to prevent.
func TestSlugsOnAnUnreadableDirectoryIsAnError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := writeRoot(t, map[string]string{"workspaces/dev.yaml": ""})
	dir := filepath.Join(root, "workspaces")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	if _, err := Slugs(root); err == nil {
		t.Error("Slugs reported no error for a directory it could not read")
	}
}

// Slugs are sorted, so every report built from them is stable run to run.
func TestSlugsAreSorted(t *testing.T) {
	root := writeRoot(t, map[string]string{
		"workspaces/zeta.yaml":  "",
		"workspaces/alpha.yaml": "",
		"workspaces/mid.yaml":   "",
	})

	slugs, err := Slugs(root)
	if err != nil {
		t.Fatalf("Slugs: %v", err)
	}
	if !slices.IsSorted(slugs) {
		t.Errorf("Slugs = %v, want sorted", slugs)
	}
}
