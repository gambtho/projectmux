package bindpath

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// root returns a symlink-free temporary directory so expected paths compare
// equal to the canonical ones this package returns. The idea is copied from
// internal/resolve/resolve_test.go:51-58, where macOS's /var -> /private/var
// symlink makes the raw t.TempDir() path unequal to its canonical form.
func root(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return dir
}

func mkdir(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	return path
}

// TestContainsComparesPathComponents is the case spec §7 names explicitly: a
// string-prefix comparison would report that a bind of services/api contains a
// cwd of services/apixyz.
func TestContainsComparesPathComponents(t *testing.T) {
	api := filepath.Join("/r", "services", "api")
	for _, tc := range []struct {
		name string
		dir  string
		path string
		want bool
	}{
		{"a sibling sharing a name prefix", api, filepath.Join("/r", "services", "apixyz"), false},
		{"a descendant", api, filepath.Join("/r", "services", "api", "cmd"), true},
		{"the directory itself", api, api, true},
		{"an ancestor", api, filepath.Join("/r", "services"), false},
		{"an unrelated tree", api, filepath.Join("/other", "api"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Contains(tc.dir, tc.path); got != tc.want {
				t.Errorf("Contains(%q, %q) = %v, want %v", tc.dir, tc.path, got, tc.want)
			}
		})
	}
}

func TestResolveAcceptsASubdirectory(t *testing.T) {
	repo := root(t)
	want := mkdir(t, filepath.Join(repo, "services", "api"))

	got, err := Resolve(repo, "services/api")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != want {
		t.Errorf("Resolve = %q, want %q", got, want)
	}
}

func TestResolveAcceptsTheRepositoryRootItself(t *testing.T) {
	repo := root(t)
	got, err := Resolve(repo, ".")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got != repo {
		t.Errorf("Resolve = %q, want %q", got, repo)
	}
}

// TestResolveRejectsASymlinkOutOfTheRepository is the re-check spec §4 exists
// for: the stored path is an ordinary in-repository relative path, and only
// EvalSymlinks reveals that following it leaves the repository.
func TestResolveRejectsASymlinkOutOfTheRepository(t *testing.T) {
	base := root(t)
	repo := mkdir(t, filepath.Join(base, "repo"))
	outside := mkdir(t, filepath.Join(base, "outside"))
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	_, err := Resolve(repo, "escape")
	var escaped *EscapedError
	if !errors.As(err, &escaped) {
		t.Fatalf("Resolve = %v (%T), want *EscapedError", err, err)
	}
	if escaped.Rel != "escape" || escaped.Resolved != outside || escaped.RepoRoot != repo {
		t.Errorf("EscapedError = %+v, want Rel=escape Resolved=%s RepoRoot=%s",
			escaped, outside, repo)
	}
}

// TestResolveRejectsTraversalAndAbsolutePaths pins the two malformed shapes.
// The traversal target is created so the rejection cannot be an accident of
// the directory not existing.
func TestResolveRejectsTraversalAndAbsolutePaths(t *testing.T) {
	base := root(t)
	repo := mkdir(t, filepath.Join(base, "repo"))
	sibling := mkdir(t, filepath.Join(base, "escape"))

	_, err := Resolve(repo, "../escape")
	var escaped *EscapedError
	if !errors.As(err, &escaped) {
		t.Fatalf("Resolve(\"../escape\") = %v (%T), want *EscapedError", err, err)
	}

	if _, err := Resolve(repo, sibling); err == nil {
		t.Fatalf("Resolve(%q) succeeded, want an error: a bind is stored relative", sibling)
	}
}

func TestRelRoundTripsASubdirectory(t *testing.T) {
	repo := root(t)
	dir := mkdir(t, filepath.Join(repo, "services", "api"))

	rel, err := Rel(repo, dir)
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if rel != "services/api" {
		t.Fatalf("Rel = %q, want %q (slash-separated, repository-relative)", rel, "services/api")
	}
	back, err := Resolve(repo, rel)
	if err != nil {
		t.Fatalf("Resolve of the stored form: %v", err)
	}
	if back != dir {
		t.Errorf("round trip = %q, want %q", back, dir)
	}
}

func TestRelRejectsOutsideAndMissingDirectories(t *testing.T) {
	base := root(t)
	repo := mkdir(t, filepath.Join(base, "repo"))
	outside := mkdir(t, filepath.Join(base, "outside"))

	_, err := Rel(repo, outside)
	var escaped *EscapedError
	if !errors.As(err, &escaped) {
		t.Fatalf("Rel(outside) = %v (%T), want *EscapedError", err, err)
	}

	if _, err := Rel(repo, filepath.Join(repo, "gone")); err == nil {
		t.Fatal("Rel of a directory that does not exist succeeded, want an error")
	}
}

// TestRelTakesARelativeArgumentAgainstTheWorkingDirectory pins the rule that
// separates Rel from Resolve. Resolve reads the *stored* form, which is
// repository-relative; Rel reads a *typed* argument, which follows the ordinary
// CLI convention. Without this, `--cwd .` would bind the repository root
// instead of the directory the user is standing in.
func TestRelTakesARelativeArgumentAgainstTheWorkingDirectory(t *testing.T) {
	repo := root(t)
	mkdir(t, filepath.Join(repo, "services", "api"))
	t.Chdir(filepath.Join(repo, "services"))

	for _, tc := range []struct{ arg, want string }{
		{"api", "services/api"},
		{".", "services"},
	} {
		got, err := Rel(repo, tc.arg)
		if err != nil {
			t.Fatalf("Rel(%q): %v", tc.arg, err)
		}
		if got != tc.want {
			t.Errorf("Rel(%q) = %q, want %q", tc.arg, got, tc.want)
		}
	}
}

// TestRelSuggestsTheRepositoryRelativeReadingWhenAnArgumentEscapes covers the
// copy-from-list case: `list` prints the stored, repository-relative form, and
// pasting it back from outside the repository resolves somewhere else entirely.
// The bare containment error would not explain that, so Rel names the directory
// the repository-relative reading would have found.
func TestRelSuggestsTheRepositoryRelativeReadingWhenAnArgumentEscapes(t *testing.T) {
	base := root(t)
	repo := mkdir(t, filepath.Join(base, "repo"))
	want := mkdir(t, filepath.Join(repo, "services", "api"))
	mkdir(t, filepath.Join(base, "services", "api")) // the cwd-relative reading
	t.Chdir(base)

	_, err := Rel(repo, "services/api")
	var escaped *EscapedError
	if !errors.As(err, &escaped) {
		t.Fatalf("Rel = %v (%T), want *EscapedError", err, err)
	}
	if escaped.Suggestion != want {
		t.Errorf("Suggestion = %q, want %q", escaped.Suggestion, want)
	}
	if !strings.Contains(err.Error(), "did you mean") {
		t.Errorf("Error() = %q, want it to carry the suggestion", err)
	}
}

// TestRelOmitsTheSuggestionWhenThereIsNothingToSuggest keeps the hint honest:
// an argument that escapes and has no in-repository reading gets the plain
// error, not a pointer at a directory that does not exist.
func TestRelOmitsTheSuggestionWhenThereIsNothingToSuggest(t *testing.T) {
	base := root(t)
	repo := mkdir(t, filepath.Join(base, "repo"))
	outside := mkdir(t, filepath.Join(base, "outside"))

	_, err := Rel(repo, outside)
	var escaped *EscapedError
	if !errors.As(err, &escaped) {
		t.Fatalf("Rel = %v (%T), want *EscapedError", err, err)
	}
	if escaped.Suggestion != "" {
		t.Errorf("Suggestion = %q, want empty", escaped.Suggestion)
	}
	if strings.Contains(err.Error(), "did you mean") {
		t.Errorf("Error() = %q, want no suggestion", err)
	}
}
