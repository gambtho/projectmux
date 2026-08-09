// Package bindpath holds the path rules a session's bind obeys.
//
// A bind is stored relative to the repository root and must lie inside the
// repository after symlinks are resolved. That check is re-run at every use,
// not only when the bind is recorded: a stored in-repository path can later be
// replaced by a symlink pointing outside, and window creation would then join
// onto the escaped path (design §4).
//
// The rules live here rather than in either caller because two packages need
// them — internal/target for the bind lookup and internal/controller for
// window rendering — and a second copy is a second chance to get containment
// wrong.
package bindpath

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// EscapedError reports a bind that no longer canonicalizes to a path inside
// the repository.
//
// Suggestion is set only by Rel, and only when the argument escaped under the
// working-directory reading but would have resolved inside the repository under
// the repository-relative one. It is the directory that reading would have
// found. Empty means there is nothing to suggest.
type EscapedError struct {
	Rel        string
	Resolved   string
	RepoRoot   string
	Suggestion string
}

func (e *EscapedError) Error() string {
	msg := fmt.Sprintf(
		"the bind %q resolves to %s, which is outside the repository at %s",
		e.Rel, e.Resolved, e.RepoRoot)
	if e.Suggestion != "" {
		msg += fmt.Sprintf("; did you mean %s?", e.Suggestion)
	}
	return msg
}

// Resolve canonicalizes a repository-relative bind against repoRoot and
// verifies it still lies inside the repository. It returns the absolute
// canonical path.
func Resolve(repoRoot, rel string) (string, error) {
	if filepath.IsAbs(rel) {
		return "", fmt.Errorf(
			"the bind %q is absolute; a bind is stored relative to the repository root", rel)
	}
	root, err := canonicalize(repoRoot)
	if err != nil {
		return "", err
	}

	// The traversal check is lexical and runs before the filesystem is
	// touched, so "../escape" reports what is wrong with it whether or not
	// the directory it names happens to exist.
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", &EscapedError{
			Rel:      rel,
			Resolved: filepath.Join(root, clean),
			RepoRoot: root,
		}
	}

	resolved, err := canonicalize(filepath.Join(root, clean))
	if err != nil {
		return "", err
	}
	if !Contains(root, resolved) {
		return "", &EscapedError{Rel: rel, Resolved: resolved, RepoRoot: root}
	}
	return resolved, nil
}

// Rel converts a user-typed path argument to the repository-relative,
// slash-separated form that is stored. It canonicalizes both sides, requires
// the directory to exist, and requires the result to lie inside the repository.
//
// A relative argument is taken against the process's working directory, which
// is what filepath.Abs does and what every other CLI does with a path argument.
// That is deliberately *not* how the stored form is read — Resolve takes that
// against the repository root — because only this rule makes `--cwd .` and
// shell tab-completion mean what the user typed (design §4).
//
// The two rules disagree for the same string, so when an argument escapes the
// repository this way, Rel checks whether the repository-relative reading would
// have landed inside and, if so, names that directory in the error. `list`
// prints the stored form, so pasting it back from elsewhere is the expected
// mistake, not an exotic one.
func Rel(repoRoot, path string) (string, error) {
	root, err := canonicalize(repoRoot)
	if err != nil {
		return "", err
	}
	resolved, err := canonicalize(path)
	if err != nil {
		return "", err
	}
	if !Contains(root, resolved) {
		return "", &EscapedError{
			Rel:        path,
			Resolved:   resolved,
			RepoRoot:   root,
			Suggestion: suggest(root, path),
		}
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return "", fmt.Errorf("relating %s to %s: %w", resolved, root, err)
	}
	return filepath.ToSlash(rel), nil
}

// suggest returns the directory the repository-relative reading of arg would
// have found, or "" when that reading is unavailable or lands nowhere. It
// reuses Resolve so the suggestion is only ever a path a bind could actually
// hold — a suggestion the very next command would reject is worse than none.
func suggest(root, arg string) string {
	if filepath.IsAbs(arg) {
		return ""
	}
	resolved, err := Resolve(root, arg)
	if err != nil {
		return ""
	}
	return resolved
}

// Contains reports whether path lies at or below dir, comparing path
// components rather than string prefixes: a dir of "services/api" does not
// contain "services/apixyz".
//
// filepath.Rel is the component-wise comparison — it answers "../apixyz" for
// the sibling and "cmd" for the descendant — so the test is whether the
// relative form has to climb out of dir to reach path.
func Contains(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonicalize makes a path absolute, symlink-free, and confirmed to be a
// directory. It follows internal/resolve/resolve.go:167-183 deliberately: a
// bind and a repository root have to canonicalize the same way, or a bind
// recorded through one spelling of a path would not be found through another.
func canonicalize(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", path, err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("no such directory: %s", path)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("not a directory: %s", path)
	}
	return resolved, nil
}
