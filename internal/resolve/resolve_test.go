package resolve

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func TestWorkspaceIDIsTheSHA256OfTheCanonicalPath(t *testing.T) {
	base := root(t)
	makeRepo(t, filepath.Join(base, "euro_trip"))
	ws := mustResolve(t, "euro_trip", []string{base}, base)

	if !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(ws.ID) {
		t.Errorf("ID = %q, want 64 hex characters", ws.ID)
	}
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

func TestPrimaryTreeAndNonGitDirectoryAreBothPrimary(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	plain := filepath.Join(base, "plain")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if !mustResolve(t, "", nil, repo).IsPrimary {
		t.Error("a primary working tree should be primary")
	}
	if !mustResolve(t, "", nil, plain).IsPrimary {
		t.Error("a non-git directory should count as primary")
	}
}

func TestLinkedWorktreeIsNotPrimaryIncludingFromASubdirectory(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	linked := addWorktree(t, repo, filepath.Join(base, "euro_trip-pr5"), "pr5")
	sub := filepath.Join(linked, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if mustResolve(t, "", nil, linked).IsPrimary {
		t.Error("a linked worktree should not be primary")
	}
	if mustResolve(t, "", nil, sub).IsPrimary {
		t.Error("a linked worktree should not be primary from a subdirectory")
	}
}

func TestPrimaryTreeUsesTheBareSlugAsItsSessionName(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	ws := mustResolve(t, "euro_trip", []string{base}, base)

	if ws.Slug != "euro_trip" {
		t.Errorf("slug = %q", ws.Slug)
	}
	if ws.SessionName != "euro_trip" {
		t.Errorf("session name = %q", ws.SessionName)
	}
	if ws.Worktree != repo {
		t.Errorf("worktree = %q, want %q", ws.Worktree, repo)
	}
	if !ws.IsPrimary {
		t.Error("should be primary")
	}
}

func TestSiblingWorktreeInheritsTheParentSlug(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	addWorktree(t, repo, filepath.Join(base, "euro_trip-pr5"), "pr5")
	ws := mustResolve(t, "euro_trip-pr5", []string{base}, base)

	if ws.Slug != "euro_trip" {
		t.Errorf("slug = %q, want the parent repository's name", ws.Slug)
	}
	if ws.SessionName != "euro_trip--euro_trip-pr5" {
		t.Errorf("session name = %q", ws.SessionName)
	}
	if ws.IsPrimary {
		t.Error("should not be primary")
	}
}

func TestNestedWorktreeDirectoriesAreSearched(t *testing.T) {
	for _, nest := range []string{".worktrees", ".claude/worktrees"} {
		t.Run(nest, func(t *testing.T) {
			base := root(t)
			repo := makeRepo(t, filepath.Join(base, "slabledger"))
			want := addWorktree(t, repo, filepath.Join(repo, nest, "review"), "review")
			ws := mustResolve(t, "review", []string{base}, base)

			if ws.Slug != "slabledger" {
				t.Errorf("slug = %q", ws.Slug)
			}
			if ws.SessionName != "slabledger--review" {
				t.Errorf("session name = %q", ws.SessionName)
			}
			if ws.Worktree != want {
				t.Errorf("worktree = %q, want %q", ws.Worktree, want)
			}
		})
	}
}

func TestAmbiguousNameIsAnErrorListingEveryCandidate(t *testing.T) {
	// Picking the first match is how a user ends up running an agent against
	// the wrong branch, so ambiguity is never resolved by guessing.
	base := root(t)
	slab := makeRepo(t, filepath.Join(base, "slabledger"))
	euro := makeRepo(t, filepath.Join(base, "euro_trip"))
	addWorktree(t, slab, filepath.Join(slab, ".worktrees", "review"), "review")
	addWorktree(t, euro, filepath.Join(euro, ".claude", "worktrees", "review"), "review")

	_, err := Resolve("review", []string{base}, base)
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v, want *AmbiguousError", err)
	}
	msg := err.Error()
	for _, want := range []string{
		filepath.Join("slabledger", ".worktrees", "review"),
		filepath.Join("euro_trip", ".claude", "worktrees", "review"),
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not list %q", msg, want)
		}
	}
	if len(ambiguous.Candidates) != 2 {
		t.Errorf("candidates = %v", ambiguous.Candidates)
	}
}

func TestAmbiguityAcrossRootsIsReported(t *testing.T) {
	a, b := root(t), root(t)
	makeRepo(t, filepath.Join(a, "shared"))
	makeRepo(t, filepath.Join(b, "shared"))

	_, err := Resolve("shared", []string{a, b}, a)
	var ambiguous *AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("error = %v, want *AmbiguousError", err)
	}
}

func TestTheSameTreeReachedThroughOverlappingRootsIsNotAmbiguous(t *testing.T) {
	// Overlapping or repeated roots are a configuration wart, not a genuine
	// collision. Deduplicating by canonical path keeps them from blocking work.
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	roots := []string{base, base, filepath.Join(base, "sub", "..")}

	ws := mustResolve(t, "euro_trip", roots, base)
	if ws.Worktree != repo {
		t.Errorf("worktree = %q, want %q", ws.Worktree, repo)
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
	for _, want := range []string{"nosuchproject", base, ".worktrees", ".claude/worktrees"} {
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

func TestCwdResolutionFromASubdirectoryFindsTheWorktreeRoot(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	linked := addWorktree(t, repo, filepath.Join(base, "euro_trip-pr5"), "pr5")
	deep := filepath.Join(linked, "deep", "nested")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ws := mustResolve(t, "", nil, deep)
	if ws.Worktree != linked {
		t.Errorf("worktree = %q, want %q", ws.Worktree, linked)
	}
	if ws.SessionName != "euro_trip--euro_trip-pr5" {
		t.Errorf("session name = %q", ws.SessionName)
	}
}

func TestCwdResolutionOutsideGitFallsBackToTheDirectory(t *testing.T) {
	base := root(t)
	dir := filepath.Join(base, "notgit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	ws := mustResolve(t, "", nil, dir)
	if ws.Worktree != dir {
		t.Errorf("worktree = %q, want %q", ws.Worktree, dir)
	}
	if ws.Slug != "notgit" {
		t.Errorf("slug = %q", ws.Slug)
	}
	if !ws.IsPrimary {
		t.Error("a non-git directory should count as primary")
	}
}

func TestResolvingAMissingDirectoryFails(t *testing.T) {
	base := root(t)
	if _, err := Resolve("", nil, filepath.Join(base, "gone")); err == nil {
		t.Error("expected an error for a missing directory")
	}
}
