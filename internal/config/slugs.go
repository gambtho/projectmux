package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Slugs lists the tracked workspace layers under root. The machine-local
// files are not separate subjects: Load already merges each under its slug.
//
// Reading the directory rather than globbing it is deliberate: Glob discards
// filesystem errors, so a workspaces/ directory that cannot be read would
// report as an installation with no workspaces — an affirmative answer over
// unexamined ground. Only an absent directory means "none"; anything else is
// returned as the error it is.
//
// The result is sorted so that every report built from it is stable.
func Slugs(root string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(root, "workspaces"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}

	var slugs []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		name = strings.TrimSuffix(name, ".yaml")
		if strings.HasSuffix(name, ".local") {
			continue
		}
		slugs = append(slugs, name)
	}
	slices.Sort(slugs)
	return slugs, nil
}
