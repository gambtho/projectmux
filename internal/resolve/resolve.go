// Package resolve turns a workspace name or a working directory into the
// canonical worktree and the identity derived from it.
//
// It owns every git invocation in the application. No other package shells out
// to git, and this package neither reads configuration files nor mutates any
// resource.
package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// nestedWorktreeDirs are the conventional locations for linked worktrees kept
// inside their parent repository.
var nestedWorktreeDirs = []string{".worktrees", ".claude/worktrees"}

// Workspace is the identity derived from a canonical worktree path.
type Workspace struct {
	// ID is the hex SHA-256 of Worktree. It is stable for that path and is the
	// key later slices use for stored operational state.
	ID string
	// Slug names the repository. A linked worktree inherits its parent
	// repository's slug so that every tree of one project shares configuration.
	Slug string
	// Worktree is absolute and symlink-free.
	Worktree string
	// SessionName is the proposed human-facing tmux session name.
	SessionName string
	// IsPrimary reports whether this is the repository's main working tree. A
	// directory that is not a git repository at all counts as primary.
	IsPrimary bool
}

// AmbiguousError reports a name matching more than one worktree.
type AmbiguousError struct {
	Name       string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous workspace name %q; it matches:", e.Name)
	for _, c := range e.Candidates {
		b.WriteString("\n  " + c)
	}
	b.WriteString("\ndisambiguate by changing into the intended tree and " +
		"running projectmux with no workspace argument, or by renaming one tree")
	return b.String()
}

// UnknownWorkspaceError reports a name matching no worktree.
type UnknownWorkspaceError struct {
	Name  string
	Roots []string
}

func (e *UnknownWorkspaceError) Error() string {
	if len(e.Roots) == 0 {
		return fmt.Sprintf(
			"unknown workspace %q: no repository_roots are configured, "+
				"so a workspace cannot be looked up by name; set repository_roots "+
				"in defaults.yaml, or change into the intended tree and run "+
				"projectmux with no workspace argument", e.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "unknown workspace %q; searched under:", e.Name)
	for _, r := range e.Roots {
		fmt.Fprintf(&b, "\n  %s", r)
		for _, nest := range nestedWorktreeDirs {
			fmt.Fprintf(&b, "\n  %s", filepath.Join(r, "*", nest))
		}
	}
	return b.String()
}

// Resolve finds the workspace for name, or for cwd when name is empty.
func Resolve(name string, roots []string, cwd string) (Workspace, error) {
	var worktree string
	if name == "" {
		worktree = fromDirectory(cwd)
	} else {
		found, err := byName(name, roots)
		if err != nil {
			return Workspace{}, err
		}
		worktree = found
	}

	canonical, err := canonicalize(worktree)
	if err != nil {
		return Workspace{}, err
	}

	primary := isPrimary(canonical)
	slug := slugFor(canonical)
	session := slug
	if !primary {
		session = slug + "--" + filepath.Base(canonical)
	}
	sum := sha256.Sum256([]byte(canonical))

	return Workspace{
		ID:          hex.EncodeToString(sum[:]),
		Slug:        slug,
		Worktree:    canonical,
		SessionName: session,
		IsPrimary:   primary,
	}, nil
}

// fromDirectory prefers the enclosing worktree root so that running from a
// subdirectory resolves the same workspace. A directory outside git resolves to
// itself.
func fromDirectory(cwd string) string {
	if top, err := gitOutput(cwd, "rev-parse", "--show-toplevel"); err == nil && top != "" {
		return top
	}
	return cwd
}

// byName searches each configured root for a directly-named repository and for
// linked worktrees kept in the conventional nested directories.
func byName(name string, roots []string) (string, error) {
	// name is one literal directory component, not a pattern or a path. Without
	// this guard, a separator escapes the configured roots, and a glob
	// metacharacter either matches unrelated directories ("*") or aborts the
	// search with a pattern-syntax error ("foo["). The only wildcard the search
	// owns is the repository level of the nested-worktree patterns below.
	if name != filepath.Base(name) || strings.ContainsAny(name, `*?[\`) {
		return "", &UnknownWorkspaceError{Name: name, Roots: roots}
	}
	var candidates []string
	seen := map[string]bool{}

	for _, r := range roots {
		var patterns []string
		patterns = append(patterns, filepath.Join(r, name))
		for _, nest := range nestedWorktreeDirs {
			patterns = append(patterns, filepath.Join(r, "*", nest, name))
		}
		for _, pattern := range patterns {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				// The only possible error is a malformed pattern, which would
				// come from a root the user typed. Report it rather than
				// silently searching nothing.
				return "", fmt.Errorf("searching %s: %w", pattern, err)
			}
			for _, m := range matches {
				info, err := os.Stat(m)
				if err != nil || !info.IsDir() {
					continue
				}
				// Deduplicate by canonical path: overlapping or repeated roots
				// are a configuration wart, not a genuine collision.
				canonical, err := canonicalize(m)
				if err != nil || seen[canonical] {
					continue
				}
				seen[canonical] = true
				candidates = append(candidates, canonical)
			}
		}
	}

	switch len(candidates) {
	case 0:
		return "", &UnknownWorkspaceError{Name: name, Roots: roots}
	case 1:
		return candidates[0], nil
	default:
		slices.Sort(candidates)
		return "", &AmbiguousError{Name: name, Candidates: candidates}
	}
}

// canonicalize makes a path absolute and symlink-free so that identity derived
// from it is stable across trailing slashes and symlinked access.
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

// isPrimary reports whether path is a repository's main working tree. git may
// report either directory relative to where it ran, so both are canonicalized
// before comparison. A directory outside git counts as primary.
func isPrimary(path string) bool {
	gitDir, err := gitOutput(path, "rev-parse", "--git-dir")
	if err != nil {
		return true
	}
	commonDir, err := gitOutput(path, "rev-parse", "--git-common-dir")
	if err != nil {
		return true
	}
	canonicalDir := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(path, p)
		}
		if canonical, err := filepath.EvalSymlinks(p); err == nil {
			return canonical
		}
		return p
	}
	return canonicalDir(gitDir) == canonicalDir(commonDir)
}

// slugFor names the repository. The first entry of `git worktree list` is the
// main working tree, so every linked worktree inherits the parent repository's
// name and therefore its configuration.
func slugFor(path string) string {
	out, err := gitOutput(path, "worktree", "list", "--porcelain")
	if err == nil {
		for _, line := range strings.Split(out, "\n") {
			main, ok := strings.CutPrefix(line, "worktree ")
			if !ok {
				continue
			}
			if info, err := os.Stat(main); err == nil && info.IsDir() {
				return filepath.Base(main)
			}
			break
		}
	}
	return filepath.Base(path)
}

func gitOutput(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	// Stderr is left unset so Output captures it into the returned ExitError
	// rather than leaking git's diagnostics onto the process's own stderr.
	// Every caller treats a git failure as "not a repository" and has a defined
	// fallback, so the message is available but never printed.
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
