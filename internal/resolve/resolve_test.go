package resolve

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The test environment has no gitconfig, so identity and the initial branch
// name are supplied explicitly on every invocation.
func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	full := append([]string{
		"-c", "user.email=t@example.com",
		"-c", "user.name=t",
		"-c", "init.defaultBranch=main",
	}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s in %s: %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func makeRepo(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	git(t, dir, "init", "-q", dir)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
	return dir
}

func addWorktree(t *testing.T, repo, path, branch string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	git(t, repo, "worktree", "add", "-q", path, "-b", branch)
	return path
}

// root returns a symlink-free temporary directory so expected paths compare
// equal to the canonical ones the resolver returns.
func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

func mustResolve(t *testing.T, name string, roots []string, cwd string) Workspace {
	t.Helper()
	ws, err := Resolve(name, roots, cwd)
	if err != nil {
		t.Fatalf("Resolve(%q): %v", name, err)
	}
	return ws
}

func TestWorkspaceIDIsStableAcrossTrailingSlashAndSymlink(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	link := filepath.Join(base, "link")
	if err := os.Symlink(repo, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	plain := mustResolve(t, "", nil, repo).ID
	slashed := mustResolve(t, "", nil, repo+string(filepath.Separator)).ID
	linked := mustResolve(t, "", nil, link).ID

	if plain != slashed {
		t.Errorf("trailing slash changed the ID: %q vs %q", plain, slashed)
	}
	if plain != linked {
		t.Errorf("symlinked path changed the ID: %q vs %q", plain, linked)
	}
}

func TestWorkspaceIDCombinesTheRepositoryRootAndTheSession(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	ws := mustResolve(t, "euro_trip", []string{base}, base)

	wantRepository := sha256.Sum256([]byte(repo))
	wantWorkspace := sha256.Sum256([]byte(repo + "\x00" + ws.Session))
	if ws.RepositoryID != hex.EncodeToString(wantRepository[:]) {
		t.Errorf("RepositoryID = %q, want the hash of %q", ws.RepositoryID, repo)
	}
	if ws.ID != hex.EncodeToString(wantWorkspace[:]) {
		t.Errorf("ID = %q, want the hash of the root and the session", ws.ID)
	}
}

func TestTheDefaultSessionIsNamedForTheRepository(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	ws := mustResolve(t, "euro_trip", []string{base}, base)

	if ws.Slug != "euro_trip" {
		t.Errorf("slug = %q", ws.Slug)
	}
	if ws.Session != "" {
		t.Errorf("session = %q, want the default session", ws.Session)
	}
	if ws.SessionName != "euro_trip" {
		t.Errorf("session name = %q", ws.SessionName)
	}
	if ws.RepoRoot != repo {
		t.Errorf("repo root = %q, want %q", ws.RepoRoot, repo)
	}
}

func TestASiblingWorktreeNamedDirectlyResolvesToItsRepository(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	addWorktree(t, repo, filepath.Join(base, "euro_trip-pr5"), "pr5")
	ws := mustResolve(t, "euro_trip-pr5", []string{base}, base)

	if ws.Slug != "euro_trip" || ws.RepoRoot != repo {
		t.Errorf("slug/root = %q/%q, want the parent repository %q", ws.Slug, ws.RepoRoot, repo)
	}
	if ws.SessionName != "euro_trip" {
		t.Errorf("session name = %q, want the repository's default session", ws.SessionName)
	}
}

// Two worktrees of one repository are one workspace. This is the defect in
// the design's §1 stated directly: the container is keyed on the resolved
// path, so a linked worktree that resolved to itself demanded a second
// container on the same project.
func TestEveryWorktreeOfARepositoryResolvesToOneWorkspace(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	linked := addWorktree(t, repo, filepath.Join(repo, ".worktrees", "1529"), "pr1529")
	sub := filepath.Join(linked, "deep")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	want := mustResolve(t, "", nil, repo)
	for _, cwd := range []string{linked, sub} {
		got := mustResolve(t, "", nil, cwd)
		if got.RepoRoot != repo {
			t.Errorf("from %s: RepoRoot = %q, want %q", cwd, got.RepoRoot, repo)
		}
		if got.ID != want.ID || got.RepositoryID != want.RepositoryID {
			t.Errorf("from %s: identity = %q/%q, want %q/%q",
				cwd, got.ID, got.RepositoryID, want.ID, want.RepositoryID)
		}
		if got.SessionName != "euro_trip" {
			t.Errorf("from %s: session name = %q", cwd, got.SessionName)
		}
	}
}

func TestNestedWorktreeDirectoriesAreNoLongerSearched(t *testing.T) {
	// A worktree is an ordinary directory inside a repository now. Finding one
	// by name would hand back a second identity for the same project, which is
	// the shape the design removes.
	for _, nest := range []string{".worktrees", ".claude/worktrees"} {
		t.Run(nest, func(t *testing.T) {
			base := root(t)
			repo := makeRepo(t, filepath.Join(base, "slabledger"))
			addWorktree(t, repo, filepath.Join(repo, nest, "review"), "review")

			_, err := Resolve("review", []string{base}, base)
			var unknown *UnknownWorkspaceError
			if !errors.As(err, &unknown) {
				t.Fatalf("error = %v, want *UnknownWorkspaceError", err)
			}
		})
	}
}

func TestAmbiguousNameIsAnErrorListingEveryCandidate(t *testing.T) {
	// Picking the first match is how a user ends up running an agent against
	// the wrong repository, so ambiguity is never resolved by guessing.
	a, b := root(t), root(t)
	first := makeRepo(t, filepath.Join(a, "slabledger"))
	second := makeRepo(t, filepath.Join(b, "slabledger"))

	_, err := Resolve("slabledger", []string{a, b}, a)
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v, want *AmbiguousError", err)
	}
	msg := err.Error()
	for _, want := range []string{first, second, "repository"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v", ambiguous.Candidates)
	}
}

func TestTheSameTreeReachedThroughOverlappingRootsIsNotAmbiguous(t *testing.T) {
	// Overlapping or repeated roots are a configuration wart, not a genuine
	// collision. Deduplicating by canonical path keeps them from blocking work.
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	roots := []string{base, base, filepath.Join(base, "sub", "..")}

	ws := mustResolve(t, "euro_trip", roots, base)
	if ws.RepoRoot != repo {
		t.Errorf("repo root = %q, want %q", ws.RepoRoot, repo)
	}
}

func TestUnknownNameNamesTheSearchedRoots(t *testing.T) {
	base := root(t)
	_, err := Resolve("nosuchproject", []string{base}, base)

	var unknown *UnknownWorkspaceError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want *UnknownWorkspaceError", err)
	}
	msg := err.Error()
	for _, want := range []string{"nosuchproject", base} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %q", msg, want)
		}
	}
}

func TestNameIsALiteralDirectoryNameNotAPattern(t *testing.T) {
	base := root(t)
	makeRepo(t, filepath.Join(base, "euro_trip"))
	outside := makeRepo(t, filepath.Join(root(t), "elsewhere"))

	for _, name := range []string{
		"*",         // would match euro_trip instead of being unknown
		"euro[",     // would abort the search as a malformed pattern
		"euro?trip", // wildcard
		`euro\trip`, // glob escape character
		"../" + filepath.Base(filepath.Dir(outside)) + "/elsewhere", // traversal out of the root
		"a/b", // more than one path component
	} {
		_, err := Resolve(name, []string{base}, base)
		var unknown *UnknownWorkspaceError
		if !errors.As(err, &unknown) {
			t.Errorf("Resolve(%q): error = %v, want *UnknownWorkspaceError", name, err)
		}
	}
}

func TestResolvingByNameWithoutConfiguredRootsSaysSo(t *testing.T) {
	// The Bash implementation hardcoded $HOME/workspace. That is personal
	// installation policy, so an unconfigured application says what to
	// configure instead of searching a guessed directory.
	base := root(t)
	_, err := Resolve("anything", nil, base)

	var unknown *UnknownWorkspaceError
	if !errors.As(err, &unknown) {
		t.Fatalf("error = %v, want *UnknownWorkspaceError", err)
	}
	if !strings.Contains(err.Error(), "repository_roots") {
		t.Errorf("error %q should point at repository_roots", err.Error())
	}
}

func TestCwdResolutionOutsideGitFallsBackToTheDirectory(t *testing.T) {
	base := root(t)
	dir := filepath.Join(base, "notgit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ws := mustResolve(t, "", nil, dir)
	if ws.RepoRoot != dir {
		t.Errorf("repo root = %q, want %q", ws.RepoRoot, dir)
	}
	if ws.Slug != "notgit" {
		t.Errorf("slug = %q", ws.Slug)
	}
}

func TestResolvingAMissingDirectoryFails(t *testing.T) {
	base := root(t)
	if _, err := Resolve("", nil, filepath.Join(base, "gone")); err == nil {
		t.Error("expected an error for a missing directory")
	}
}
