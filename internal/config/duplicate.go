package config

import "fmt"

// duplicateWindows reports each window name repeated within one layer.
//
// The check is per-layer and deliberately separate from the merge. It used to
// live inside mergeWindows, which meant it ran only when something merged: on
// an installation with no workspace files, a defaults.yaml naming one window
// twice validated clean and doctor called it healthy, and the defect surfaced
// later attributed to whichever workspace first forced a merge.
//
// The distinction it encodes is unchanged. Repetition within one file is
// always a mistake; repetition across files is the merge working as intended,
// letting a workspace adjust a default window without restating the layout.
func (m Merged) duplicateWindows(src Source) []Problem {
	seen := map[string]bool{}
	reported := map[string]bool{}
	var names []string
	for _, w := range src.Layer.Windows {
		switch {
		case !seen[w.Name]:
			seen[w.Name] = true
		case !reported[w.Name]:
			reported[w.Name] = true
			names = append(names, w.Name)
		}
	}

	problems := make([]Problem, 0, len(names))
	for _, name := range names {
		var origins []Origin
		for _, line := range windowLines(src.root, name) {
			origins = append(origins, Origin{File: src.File, Line: line})
		}
		problems = append(problems, Problem{
			Field:   fmt.Sprintf("windows[%s]", name),
			Message: fmt.Sprintf("window %q is defined more than once", name),
			Origins: m.relative(origins),
		})
	}
	return problems
}
