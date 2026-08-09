// Package resolve turns a workspace name or a working directory into the
// canonical repository root and the identity derived from it.
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

// Workspace is one session on one repository. A repository is the unit a
// container is keyed on, so every tree of a project — main or linked worktree —
// resolves to the same repository and shares that container.
type Workspace struct {
	// ID is the hex SHA-256 of RepoRoot, a NUL byte, and Session. It is stable
	// for that pair and is the key the state store records session state under.
	ID string
	// RepositoryID is the hex SHA-256 of RepoRoot. Sessions on one repository
	// share it, which is what makes one container per repository expressible.
	RepositoryID string
	// Slug names the repository.
	Slug string
	// RepoRoot is absolute, symlink-free, and always a main working tree.
	RepoRoot string
	// Session is the named session on the repository, empty for the default.
	Session string
	// SessionName is the proposed human-facing tmux session name.
	SessionName string
}

// AmbiguousError reports a name matching more than one repository.
type AmbiguousError struct {
	Name       string
	Candidates []string
}

func (e *AmbiguousError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "ambiguous workspace name %q; it matches more than one repository:", e.Name)
	for _, c := range e.Candidates {
		b.WriteString("\n  " + c)
	}
	b.WriteString("\ndisambiguate by changing into the intended repository and " +
		"running projectmux with no workspace argument, or by renaming one repository")
	return b.String()
}

// UnknownWorkspaceError reports a name matching no repository.
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
	}
	return b.String()
}

// Resolve finds the workspace for name, or for cwd when name is empty, under
// the given session component. An empty session is the repository's default
// session. Selecting the session is a policy decision that needs the state
// store, so it is made by the caller (internal/target) and this package stays
// pure.
func Resolve(name, session string, roots []string, cwd string) (Workspace, error) {
	dir := cwd
	if name != "" {
		found, err := byName(name, roots)
		if err != nil {
			return Workspace{}, err
		}
		dir = found
	}

	canonical, err := canonicalize(dir)
	if err != nil {
		return Workspace{}, err
	}
	repoRoot := mainWorktree(canonical)
	repositorySum := sha256.Sum256([]byte(repoRoot))

	// The session-bearing fields are derived in exactly one place, so a
	// workspace built here can never disagree with one WithSession rebuilds
	// from a live session's recorded session component.
	return WithSession(Workspace{
		RepositoryID: hex.EncodeToString(repositorySum[:]),
		Slug:         filepath.Base(repoRoot),
		RepoRoot:     repoRoot,
	}, session), nil
}

// WithSession re-derives ID, Session and SessionName for a different session
// component on the same repository. RepositoryID, Slug and RepoRoot are
// properties of the repository and are carried over untouched, so no git
// invocation and no filesystem access is needed: rebuild and status use it to
// reconstruct a live session's identity from its recorded session component.
func WithSession(ws Workspace, session string) Workspace {
	ws.Session = session
	ws.SessionName = ws.Slug
	if session != "" {
		ws.SessionName = ws.Slug + "--" + session
	}
	workspaceSum := sha256.Sum256([]byte(ws.RepoRoot + "\x00" + session))
	ws.ID = hex.EncodeToString(workspaceSum[:])
	return ws
}

// byName searches each configured root for a directly-named repository. A
// linked worktree is no longer findable by name: it is a separate working tree
// attached to a repository — wherever on the disk it sits — and its sessions
// belong to that repository.
func byName(name string, roots []string) (string, error) {
	// name is one literal directory component, not a pattern or a path.
	// Without this guard a separator escapes the configured roots, and a glob
	// metacharacter would be looked up as a directory that cannot exist,
	// reporting "unknown" for a name the user may have meant literally.
	//
	// "." and ".." pass filepath.Base unchanged, so they need naming: joined
	// onto a root they address the root itself and its parent, which would
	// resolve a name to a directory that is not a repository under that root
	// at all — and, for "..", to one outside every configured root.
	if name != filepath.Base(name) || name == "." || name == ".." ||
		strings.ContainsAny(name, `*?[\`) {
		return "", &UnknownWorkspaceError{Name: name, Roots: roots}
	}
	var candidates []string
	seen := map[string]bool{}

	for _, r := range roots {
		dir := filepath.Join(r, name)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			continue
		}
		// Deduplicate by canonical path: overlapping or repeated roots are a
		// configuration wart, not a genuine collision.
		canonical, err := canonicalize(dir)
		if err != nil || seen[canonical] {
			continue
		}
		seen[canonical] = true
		candidates = append(candidates, canonical)
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

// mainWorktree returns the repository's main working tree for path. The first
// entry of `git worktree list --porcelain` is the main tree, so a linked
// worktree, and any subdirectory of one, answer with the repository the user
// means rather than with a tree of their own. A directory outside git, or one
// whose main tree git names but the filesystem no longer has, is its own root.
func mainWorktree(path string) string {
	out, err := gitOutput(path, "worktree", "list", "--porcelain")
	if err != nil {
		return path
	}
	for _, line := range strings.Split(out, "\n") {
		main, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		canonical, err := canonicalize(main)
		if err != nil {
			break
		}
		return canonical
	}
	return path
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
