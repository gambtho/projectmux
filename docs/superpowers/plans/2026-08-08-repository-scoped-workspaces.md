# Repository-Scoped Workspaces Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the repository — not the git worktree — the unit a projectmux workspace and its container are keyed on, so N worktrees of one project share one container instead of demanding N.

**Architecture:** Resolution collapses every tree of a project onto its main worktree, and the state schema splits into `repositories` (one row per project, owning the container binding) and `workspaces` (one row per session on a project). Locking follows the same split: container work locks the repository, session work locks the workspace, repository first. The migration is deliberately pure SQL that over-counts — every stored path becomes a repository — and `rebuild`, which can ask git, is what collapses the linked-worktree rows afterward.

**Tech Stack:** Go 1.x (stdlib `testing`, no assertion library), SQLite via `modernc.org/sqlite` with `PRAGMA user_version` migrations, `syscall.Flock`, tmux, the Dev Containers CLI, and Bats for the dotfiles companion.

## Global Constraints

- Linux/WSL only. This is alpha software (v0.4.0) with no compatibility promise to preserve across the change.
- JSON envelopes are versioned. All carriers share one `cli.OutputSchemaVersion`; this plan bumps it 1 → 2 exactly once.
- Exit codes are fixed: 0 success, 1 I/O, 2 usage, 3 ambiguous, 4 unknown, 5 invalid config, 6 refused/conflict.
- `internal/resolve` owns every git invocation. No other package shells out to git.
- Migrations are pure SQL executed in one immediate transaction, with no Go hook. A migration must succeed with the filesystem entirely absent.
- The state database is rebuildable (`docs/design.md:253`). `rebuild` is the recovery path for anything a migration cannot classify.
- Tests run with `go test ./...`. The formatting gate is `gofmt -l .` producing no output.
- Comments explain intent and tradeoffs in full prose sentences, matching the existing codebase.
- On the authoring machine, `make` is broken under zsh — use `/usr/bin/make`; `rm` silently no-ops — use `rm -f`; restore tracked files with `git checkout --`, never `cp`; use `/usr/bin/diff`.

## Scope

**In scope (this plan):** spec §1, §2, §4, §5, §6.1, §6.2, §6.3, §8, §9 — the model change that fixes the reported defect.

**Deliberately excluded (a second plan, after this one lands):** spec §3's `<repo>/<session>` argument form, §7's `bind` verb and its `bound_path` column, and the Claude Code `PostToolUse` hook. `Session` is therefore always `""` throughout this plan. It exists in the schema and in the ID derivation *now* — not later — because `UNIQUE (repository_id, session)` is what makes the model representable, and adding the session as a hash input afterward would rewrite every stored ID a second time. Spec §12's two `bind` verification cases belong to that plan.

## Task Ordering and Interactions

Tasks 1–9 are sequential: each consumes types the previous one produces. Task 10 (dotfiles) is independent and may land at any point. Task 11 depends on 3 and 4 and is written last because it asserts their composition.

One property Task 11 relies on was checked against the code rather than assumed: `Ensure` takes its lock as the first statement of the pass (`internal/controller/ensure.go:81`) and releases it in a `defer`, so registration, observation, the container phase, and the terminal commit all run inside one continuous hold. Task 3 substitutes `lockPhases` at that same position and returns a single release closure, so the repository lock spans the whole pass too. **The observe phase therefore runs inside the repository lock**, and no lock-ordering change is needed to make concurrent opens deduplicate. Task 11's remaining risk is the other half of the composition — whether a binding written under a repository ID is readable through a *sibling* session's record — and its Step 3 is written to resolve that by running the test rather than by predicting the answer.

The tree does not build between Task 1 and Task 5a. Task 1 removes `resolve.Workspace.Worktree` and `.IsPrimary`, which six packages read, and Task 2 does the same to `state.Record`. Every remaining reader is converted by the first task that gates the package it lives in, which is what makes each task's verification runnable: `internal/controller` and its fake convert in Task 3, whose very first test run is `go test ./internal/controller/`; `internal/container`'s docker-gated fixture converts in Task 4; and `internal/cli` — whose test binary links `internal/container`, `internal/controller`, `internal/doctor`, `internal/rebuild` and `internal/tmux` (`go list -deps -test ./internal/cli/...`) — converts across Task 5a and Task 5b, along with the `internal/doctor` and `internal/rebuild` readers it drags in. `go build ./...` is therefore green again from the end of Task 5a. Do not treat an unrelated package's build failure before that point as a regression.

Task 5a and Task 5b were one task. They are split at the `go build ./...` gate because that is the only consistent point between Task 4 and Task 6: Task 5a converts source files only and ends with the whole tree compiling; Task 5b rewrites `internal/controller/autostart_test.go` in its second step and does not restore the build until its thirteenth, so no cut inside 5b leaves a committable tree. Task 5b is the largest task in the plan and cannot honestly be made smaller.

`go test ./...` goes green one task later than `go build ./...`. `internal/doctor`'s own tests are the last unconverted readers, and no task before Task 9 gates that package, so they convert there and Task 9 is where the full suite becomes a meaningful gate again. Tasks 3 through 8 each run a gate scoped to the packages they own, and every one of those gates is achievable at the point it appears: a package a task tests is a package that task (or an earlier one) has already converted. Where a task's Files block names a file only to make a package compile, the step says so.

One deliberate overlap: **Task 3 edits `internal/controller/autostart.go` to use `lockPhases`, and Task 5b then replaces that function entirely with `StartRepositoryContainer`, which takes the repository lock alone.** That is not a contradiction. Task 3 must leave the tree consistent at its own commit, and Task 5b's replacement has no session to lock — autostart starts a container without opening one.

---

### Task 1: Resolve to the repository, not the worktree

**Files:**
- Modify: `internal/resolve/resolve.go:1-7` (package doc), `:20-22` (`nestedWorktreeDirs`), `:24-39` (`Workspace`), `:47-56` (`AmbiguousError.Error`), `:64-81` (`UnknownWorkspaceError.Error`), `:83-116` (`Resolve`), `:118-126` (`fromDirectory`), `:128-182` (`byName`), `:202-244` (`isPrimary`, `slugFor`)
- Test: `internal/resolve/resolve_test.go:8` (the `regexp` import), `:68-76`, `:98-112`, `:114-129`, `:131-148`, `:150-165` (identity and primacy tests, replaced in Step 1), `:167-186`, `:188-214`, `:216-226` (name-lookup and ambiguity tests, replaced in Step 3), `:228-239` (`TestTheSameTreeReachedThroughOverlappingRootsIsNotAmbiguous`, whose assertion reads the dropped `Worktree` field — converted in Step 4), `:250` (the searched-roots expectation), `:294-310` (deleted in Step 3), `:312-329` (fallback assertions rewritten in Step 3)

**Interfaces:**
- Consumes: nothing
- Produces:
  ```go
  type Workspace struct {
      ID           string
      RepositoryID string
      Slug         string
      RepoRoot     string
      Session      string
      SessionName  string
  }
  func Resolve(name string, roots []string, cwd string) (Workspace, error)
  ```
  and the unexported `mainWorktree(path string) string`, which replaces both `isPrimary` and `slugFor`.

Note on scope: this task deliberately breaks compilation of `internal/cli`, `internal/controller`, `internal/container`, `internal/rebuild`, `internal/tmux`, and `internal/doctor`, which read `ws.Worktree` and `ws.IsPrimary` (`internal/container/adapter.go:56,95,150,152`, `internal/controller/ensure.go:265-286,392,441`, `internal/cli/config.go:136-166`, `internal/cli/autostart.go:97-142`, `internal/rebuild/apply.go:330-331`). Verification here is scoped to `./internal/resolve/...`; the callers are converted in later tasks, and `go build ./...` is green again only at the end of Task 5a.

- [ ] **Step 1: Rewrite the identity tests in `resolve_test.go` against the repository**

Replace `TestWorkspaceIDIsTheSHA256OfTheCanonicalPath` (`resolve_test.go:68-76`), `TestPrimaryTreeAndNonGitDirectoryAreBothPrimary` (`:98-112`), `TestLinkedWorktreeIsNotPrimaryIncludingFromASubdirectory` (`:114-129`), `TestPrimaryTreeUsesTheBareSlugAsItsSessionName` (`:131-148`) and `TestSiblingWorktreeInheritsTheParentSlug` (`:150-165`) with:

```go
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
```

Swap the `"regexp"` import (`resolve_test.go:8`), whose only use was the deleted hex assertion, for `"crypto/sha256"` and `"encoding/hex"`.

- [ ] **Step 2: Add the §1 defect as a test**

Append to `resolve_test.go`:

```go
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
```

- [ ] **Step 3: Invert the name-lookup tests**

Replace `TestNestedWorktreeDirectoriesAreSearched` (`resolve_test.go:167-186`) and `TestAmbiguousNameIsAnErrorListingEveryCandidate` (`:188-214`) with the following, and delete `TestAmbiguityAcrossRootsIsReported` (`:216-226`) — the rewritten ambiguity test is now exactly that case — and `TestCwdResolutionFromASubdirectoryFindsTheWorktreeRoot` (`:294-310`), covered by Step 2:

```go
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
```

In `TestUnknownNameNamesTheSearchedRoots` (`:250`) drop the nested directories from the expectation:

```go
	for _, want := range []string{"nosuchproject", base} {
```

In `TestCwdResolutionOutsideGitFallsBackToTheDirectory` (`:312-329`) replace the body's assertions:

```go
	ws := mustResolve(t, "", nil, dir)
	if ws.RepoRoot != dir {
		t.Errorf("repo root = %q, want %q", ws.RepoRoot, dir)
	}
	if ws.Slug != "notgit" {
		t.Errorf("slug = %q", ws.Slug)
	}
```

- [ ] **Step 4: Move the overlapping-roots test onto the repository root**

`TestTheSameTreeReachedThroughOverlappingRootsIsNotAmbiguous` (`resolve_test.go:228-239`) is about
root deduplication in `byName`, not about primacy, so it survives the rewrite — but its single
assertion reads `ws.Worktree`, the field this task deletes. Without this conversion the package does
not compile and Step 11 cannot pass. Replace the assertion at `:236-238`.

Before:

```go
	ws := mustResolve(t, "euro_trip", roots, base)
	if ws.Worktree != repo {
		t.Errorf("worktree = %q, want %q", ws.Worktree, repo)
	}
```

After:

```go
	ws := mustResolve(t, "euro_trip", roots, base)
	if ws.RepoRoot != repo {
		t.Errorf("repo root = %q, want %q", ws.RepoRoot, repo)
	}
```

That is the last `Worktree` or `IsPrimary` read in the file. The others are accounted for by the
preceding steps: Step 1 deletes the five identity and primacy tests that hold `:106`, `:109`,
`:123`, `:126`, `:142-145` and `:162`; Step 3 replaces `TestNestedWorktreeDirectoriesAreSearched`
(`:181-182`), deletes `TestCwdResolutionFromASubdirectoryFindsTheWorktreeRoot` (`:304-305`), and
rewrites the fallback assertions in `TestCwdResolutionOutsideGitFallsBackToTheDirectory`
(`:320-326`), whose `IsPrimary` check disappears because every resolved workspace is a main tree by
construction.

- [ ] **Step 5: Run the tests to verify they fail**

Run: `go test ./internal/resolve/... -v`
Expected: FAIL to build — `ws.RepoRoot undefined (type Workspace has no field or method RepoRoot)`, plus the same for `RepositoryID` and `Session`.

- [ ] **Step 6: Restate `Workspace` and the package doc**

Replace `resolve.go:1-7` and `:24-39`:

```go
// Package resolve turns a workspace name or a working directory into the
// canonical repository root and the identity derived from it.
//
// It owns every git invocation in the application. No other package shells out
// to git, and this package neither reads configuration files nor mutates any
// resource.
package resolve
```

```go
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
```

- [ ] **Step 7: Rewrite `Resolve` against the repository root**

Replace `resolve.go:83-126` (`Resolve` and `fromDirectory`):

```go
// Resolve finds the workspace for name, or for cwd when name is empty.
func Resolve(name string, roots []string, cwd string) (Workspace, error) {
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

	// The default session is the only one this slice can produce; the
	// <repo>/<session> argument form arrives with target parsing. The
	// derivation takes the session as an input now because adding it later
	// would rewrite every stored ID a second time.
	session := ""
	slug := filepath.Base(repoRoot)
	sessionName := slug
	if session != "" {
		sessionName = slug + "--" + session
	}
	repositorySum := sha256.Sum256([]byte(repoRoot))
	workspaceSum := sha256.Sum256([]byte(repoRoot + "\x00" + session))

	return Workspace{
		ID:           hex.EncodeToString(workspaceSum[:]),
		RepositoryID: hex.EncodeToString(repositorySum[:]),
		Slug:         slug,
		RepoRoot:     repoRoot,
		Session:      session,
		SessionName:  sessionName,
	}, nil
}
```

- [ ] **Step 8: Replace `isPrimary` and `slugFor` with `mainWorktree`**

Delete `resolve.go:202-244` and put in their place:

```go
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
```

- [ ] **Step 9: Narrow `byName` to directly-named repositories**

Delete `nestedWorktreeDirs` (`resolve.go:20-22`) and replace `byName` (`:128-182`) with:

```go
// byName searches each configured root for a directly-named repository. A
// linked worktree is no longer findable by name: it is a directory inside a
// repository, and its sessions belong to that repository.
func byName(name string, roots []string) (string, error) {
	// name is one literal directory component, not a pattern or a path.
	// Without this guard a separator escapes the configured roots, and a glob
	// metacharacter would be looked up as a directory that cannot exist,
	// reporting "unknown" for a name the user may have meant literally.
	if name != filepath.Base(name) || strings.ContainsAny(name, `*?[\`) {
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
```

- [ ] **Step 10: Say "repository" in both error messages**

Replace `resolve.go:41-56` and the loop body at `:72-79`:

```go
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
```

```go
	var b strings.Builder
	fmt.Fprintf(&b, "unknown workspace %q; searched under:", e.Name)
	for _, r := range e.Roots {
		fmt.Fprintf(&b, "\n  %s", r)
	}
	return b.String()
```

- [ ] **Step 11: Run the resolve tests**

Run: `go test ./internal/resolve/... -v`
Expected: PASS, including `TestEveryWorktreeOfARepositoryResolvesToOneWorkspace` and `TestNestedWorktreeDirectoriesAreNoLongerSearched`.

- [ ] **Step 12: Format and confirm the package builds**

Run: `gofmt -l internal/resolve && go build ./internal/resolve/...`
Expected: no output from either. `go build ./...` still fails in the packages listed under Interfaces above; those are later tasks.

- [ ] **Step 13: Commit**

Run:
```
git add internal/resolve && git commit -m "resolve: land every tree of a project on its repository

fromDirectory returned the enclosing worktree, so two worktrees of one
repository produced two identities and therefore two containers. Both it
and slugFor now go through mainWorktree, the first entry of git worktree
list. byName stops searching .worktrees and .claude/worktrees, IsPrimary
is gone (every resolved workspace is a main tree by construction), and
identity is derived from the repository root and the session name."
```

---

### Task 2: Schema 0002 — split repositories from sessions

**Files:**
- Create: `internal/state/migrations/0002_repositories.sql`
- Modify: `internal/state/migrate.go:23`, `internal/state/types.go:32-47`, `internal/state/store.go:26-56` (`RegisterWorkspace`), `:58-67` (`selectRecord`), `:74` and `:91-110` (`queryWorkspaces`), `:116-183` (`scanRecord`), `:266-327` (`RecordContainerObservation`, `recordContainer`, `requireWorkspace`), `:383-414` (`CommitReconciliation`)
- Test: `internal/state/store_test.go`, `internal/state/migrate_test.go`, `internal/state/readonly_test.go:24-29,191-196,241-246`

**Interfaces:**
- Consumes (Task 1): `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}`
- Produces:
  ```go
  type Repository struct {
      ID           string
      Slug         string
      RepoRoot     string
      RegisteredAt time.Time
      UpdatedAt    time.Time
      Container    *ContainerBinding
  }
  type Record struct {
      ID              string
      RepositoryID    string
      Slug            string
      RepoRoot        string
      Session         string
      ProposedSession string
      ActualSession   *string
      DesiredDigest   *string
      AppliedDigest   *string
      RegisteredAt    time.Time
      UpdatedAt       time.Time
      Container       *ContainerBinding // the repository's binding, read-only
      LastOperation   *Operation
  }
  func (s *Store) Repositories() ([]Repository, error)
  func (s *Store) RecordContainerObservation(repositoryID string, obs ContainerObservation, now time.Time) error
  const SchemaVersion = 2
  ```
  `CommitReconciliation(workspaceID string, ...)` keeps its signature and routes a container observation to the workspace's repository.

  `Record.Container` survives the split, but its meaning changes: it is the *repository's* binding projected onto every session that shares it, filled by a `LEFT JOIN container_bindings ON repository_id`, and nil when no binding has been recorded. It is read-only — there is no per-session binding to write, and every write goes through `RecordContainerObservation`, which takes a repository ID. Tasks 5b, 6, and 11 read it, and reading it is how a session learns about a container a *sibling* session started.

- [ ] **Step 1: Point the test fixtures at the new resolve shape**

`store_test.go:26-34`:

```go
func testWorkspace(id string) resolve.Workspace {
	return resolve.Workspace{
		ID:           id,
		RepositoryID: "repo-" + id,
		Slug:         "slabledger",
		RepoRoot:     "/home/u/workspace/slabledger-" + id,
		SessionName:  "slabledger",
	}
}
```

`readonly_test.go:24-29` and, with `id-2`/`slab2`/`/w/slab2`, `:191-196` and `:241-246`:

```go
	ws := resolve.Workspace{
		ID:           "id-1",
		RepositoryID: "repo-1",
		Slug:         "slab",
		RepoRoot:     "/w/slab",
		SessionName:  "slab",
	}
```

- [ ] **Step 2: Move the container assertions in `store_test.go` onto the repository**

Add the lookup helper after `mustRegister` (`store_test.go:41`):

```go
// repositoryOf returns the stored repository with id. A container binding is
// the repository's now, so a test that seeded one through a workspace reads it
// back from here.
func repositoryOf(t *testing.T, s *Store, id string) Repository {
	t.Helper()
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, r := range repos {
		if r.ID == id {
			return r
		}
	}
	t.Fatalf("repository %s is not stored", id)
	return Repository{}
}
```

Then rewrite the four container tests (`store_test.go:211-311`) and the container assertion inside `TestCommitReconciliationAppliesEverythingAtomically` (`:376-378`):

```go
func TestContainerObservationRoundTrips(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	if err := s.RecordContainerObservation("repo-w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("RecordContainerObservation: %v", err)
	}
	b := repositoryOf(t, s, "repo-w1").Container
	if b == nil || b.ContainerID != "c-1" || b.Health != HealthPresent ||
		b.Kind != "devcontainer" || b.ContainerUser != "vscode" ||
		!b.ObservedAt.Equal(testTime) {
		t.Errorf("binding = %+v", b)
	}
}

// TestMissingAndUnknownRetainTheBinding is the design-§7 tri-state gate:
// neither confirmed absence nor a failed probe erases the identity needed
// for repair.
func TestMissingAndUnknownRetainTheBinding(t *testing.T) {
	for _, health := range []Health{HealthMissing, HealthUnknown} {
		t.Run(string(health), func(t *testing.T) {
			s := openTestStore(t)
			mustRegister(t, s, testWorkspace("w1"))
			if err := s.RecordContainerObservation("repo-w1", presentObservation("c-1"), testTime); err != nil {
				t.Fatalf("seed: %v", err)
			}

			later := testTime.Add(time.Hour)
			err := s.RecordContainerObservation("repo-w1", ContainerObservation{Health: health}, later)
			if err != nil {
				t.Fatalf("record %s: %v", health, err)
			}
			b := repositoryOf(t, s, "repo-w1").Container
			if b == nil || b.ContainerID != "c-1" || b.Kind != "devcontainer" {
				t.Fatalf("identity was not retained: %+v", b)
			}
			if b.Health != health || !b.ObservedAt.Equal(later) {
				t.Errorf("health/freshness not updated: %+v", b)
			}
		})
	}
}

func TestReplacementOverwritesTheBinding(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))
	if err := s.RecordContainerObservation("repo-w1", presentObservation("c-1"), testTime); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := s.RecordContainerObservation("repo-w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing: %v", err)
	}

	if err := s.RecordContainerObservation("repo-w1", presentObservation("c-2"), testTime.Add(time.Hour)); err != nil {
		t.Fatalf("replacement: %v", err)
	}
	b := repositoryOf(t, s, "repo-w1").Container
	if b == nil || b.ContainerID != "c-2" || b.Health != HealthPresent {
		t.Errorf("binding = %+v, want the replacement c-2", b)
	}
}

func TestObservationsForNeverBoundAndUnknownRepositories(t *testing.T) {
	s := openTestStore(t)
	mustRegister(t, s, testWorkspace("w1"))

	// missing/unknown with no existing binding record nothing: there is no
	// identity to retain and none to invent.
	if err := s.RecordContainerObservation("repo-w1", ContainerObservation{Health: HealthMissing}, testTime); err != nil {
		t.Fatalf("missing on never-bound: %v", err)
	}
	if b := repositoryOf(t, s, "repo-w1").Container; b != nil {
		t.Errorf("never-bound repository grew a binding: %+v", b)
	}

	err := s.RecordContainerObservation("absent", presentObservation("c-1"), testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown repository error = %v, want ErrNotFound", err)
	}

	err = s.RecordContainerObservation("repo-w1", ContainerObservation{Health: HealthPresent}, testTime)
	if err == nil {
		t.Error("present without a container ID should be rejected")
	}
}
```

```go
	if b := repositoryOf(t, s, "repo-w1").Container; b == nil || b.ContainerID != "c-1" {
		t.Errorf("container = %+v", b)
	}
```

- [ ] **Step 3: Add the two-sessions and stale-repository tests**

Append to `store_test.go`:

```go
// TestTwoSessionsOnOneRepositoryCoexist is the design-§5.2 finding as a test:
// the 0001 schema made workspaces.worktree UNIQUE, so this pair could not be
// represented at all and a column rename would not have helped.
func TestTwoSessionsOnOneRepositoryCoexist(t *testing.T) {
	s := openTestStore(t)
	a := testWorkspace("w1")
	b := testWorkspace("w2")
	b.RepositoryID = a.RepositoryID
	b.RepoRoot = a.RepoRoot
	b.Session = "feature-a"
	b.SessionName = "slabledger--feature-a"
	mustRegister(t, s, a)
	mustRegister(t, s, b)

	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 || repos[0].RepoRoot != a.RepoRoot {
		t.Fatalf("repositories = %+v, want the one both sessions share", repos)
	}
	all, err := s.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("%d workspaces, want 2", len(all))
	}
	if all[0].Session != "" || all[1].Session != "feature-a" {
		t.Errorf("sessions = %q, %q; want the default first", all[0].Session, all[1].Session)
	}
	for _, rec := range all {
		if rec.RepositoryID != a.RepositoryID || rec.RepoRoot != a.RepoRoot {
			t.Errorf("record %s = %+v, want the shared repository", rec.ID, rec)
		}
	}
}

// TestRegisteringReplacesAStaleRepositoryForTheSamePath covers the state 0002
// leaves behind: repository IDs were carried over from the old workspace rows
// rather than recomputed, so the first registration after an upgrade brings a
// new ID for a repo_root that is UNIQUE. Registration re-keys the row instead
// of failing on the constraint.
func TestRegisteringReplacesAStaleRepositoryForTheSamePath(t *testing.T) {
	s := openTestStore(t)
	stale := testWorkspace("w1")
	mustRegister(t, s, stale)

	fresh := stale
	fresh.ID = "w1-rekeyed"
	fresh.RepositoryID = "repo-w1-rekeyed"
	mustRegister(t, s, fresh)

	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 || repos[0].ID != fresh.RepositoryID {
		t.Fatalf("repositories = %+v, want only the re-keyed row", repos)
	}
	if _, err := s.Workspace("w1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("stale workspace error = %v, want ErrNotFound: it should have cascaded", err)
	}
	if _, err := s.Workspace("w1-rekeyed"); err != nil {
		t.Errorf("re-keyed workspace: %v", err)
	}
}
```

- [ ] **Step 4: Add the migration test**

Append to `migrate_test.go`, and add `"repositories"` to the table list at `migrate_test.go:29`. The two tests below need `crypto/sha256`, `encoding/hex`, `fmt`, `time`, and `github.com/gambtho/projectmux/internal/resolve` in the import block — add whichever the file does not already have.

```go
// seedSchema1 writes a database in the pre-0002 shape using migration 0001's
// own SQL, so the fixture cannot drift from the schema it stands for. The
// recorded worktree path deliberately does not exist: 0002 must succeed with
// the filesystem entirely absent (design §9).
func seedSchema1(t *testing.T) string {
	t.Helper()
	return seedSchema1As(t, "w1", "/gone/slabledger")
}

// seedSchema1As is seedSchema1 with the stored workspace ID and worktree path
// chosen by the caller. The re-key test needs the seeded ID to be the *real*
// pre-0002 derivation — SHA-256 of the canonical path — because that is what
// makes the migrated row collide with the ID the new derivation produces.
// Passing a placeholder like "w1" would let a broken re-key pass.
func seedSchema1As(t *testing.T, workspaceID, worktree string) string {
	t.Helper()
	root := t.TempDir()
	db, err := sql.Open("sqlite", "file:"+uriPath(DBPath(root))+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatalf("opening the seed database: %v", err)
	}
	defer func() { _ = db.Close() }()

	body, err := migrations.ReadFile("migrations/0001_initial.sql")
	if err != nil {
		t.Fatalf("reading migration 1: %v", err)
	}
	stmts := []string{
		string(body),
		fmt.Sprintf(`INSERT INTO workspaces
			(id, slug, worktree, is_primary, proposed_session, actual_session,
			 desired_digest, applied_digest, registered_at, updated_at)
		 VALUES (%q, 'slabledger', %q, 1, 'slabledger', 'slabledger',
			 'sha256:aaaa', 'sha256:bbbb', '2026-08-05T12:00:00Z', '2026-08-05T12:00:00Z')`,
			workspaceID, worktree),
		fmt.Sprintf(`INSERT INTO container_bindings
			(workspace_id, kind, container_id, container_user, workdir, health, observed_at)
		 VALUES (%q, 'devcontainer', 'c-1', 'vscode', '/workspaces/slabledger',
			 'present', '2026-08-05T12:00:00Z')`, workspaceID),
		fmt.Sprintf(`INSERT INTO last_operations
			(workspace_id, operation, outcome, exit_status, error_summary, finished_at)
		 VALUES (%q, 'open', 'ok', 0, '', '2026-08-05T12:00:00Z')`, workspaceID),
		"PRAGMA user_version = 1",
	}
	for _, stmt := range stmts {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seeding: %v\n%s", err, stmt)
		}
	}
	return root
}

func TestMigration0002MovesEveryWorkspaceIntoARepository(t *testing.T) {
	root := seedSchema1(t)
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("%d repositories, want 1", len(repos))
	}
	r := repos[0]
	if r.Slug != "slabledger" || r.RepoRoot != "/gone/slabledger" {
		t.Errorf("repository = %+v, want the stored path verbatim", r)
	}
	if r.Container == nil || r.Container.ContainerID != "c-1" ||
		r.Container.Health != HealthPresent {
		t.Errorf("container binding = %+v, want the re-keyed binding", r.Container)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.RepositoryID != r.ID || rec.Session != "" ||
		rec.Slug != "slabledger" || rec.RepoRoot != "/gone/slabledger" {
		t.Errorf("record = %+v, want the default session on the migrated repository", rec)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slabledger" ||
		rec.AppliedDigest == nil || *rec.AppliedDigest != "sha256:bbbb" {
		t.Errorf("record = %+v, want the assignment and digests preserved", rec)
	}
	if rec.LastOperation == nil || rec.LastOperation.Name != "open" {
		t.Errorf("last operation = %+v, want the row preserved", rec.LastOperation)
	}
}

// TestRegisterAfterMigrationRekeysTheMigratedSession pins the half of the
// upgrade that migration 0002 cannot do by itself.
//
// Before 0002 a workspace ID was SHA-256 of the canonical path
// (internal/resolve/resolve.go:107). After 0002 the *repository* ID is that
// same hash, and the *workspace* ID hashes the session alongside the path. So a
// migrated row arrives with an ID that is byte-identical to its own repository
// ID and unequal to the ID the resolver now derives for it. The stale-row
// cleanup in RegisterWorkspace deletes by repo_root and skips the incoming ID,
// which matches nothing here — the row it would have to delete is the one being
// re-keyed, and deleting it would throw away the running session's assignment.
// Without the re-key branch the insert then violates UNIQUE (repository_id,
// session) and every first `open` after an upgrade fails with exit 1.
//
// The assertion that matters most is the last one: actual_session and
// applied_digest have to survive under the new ID, because that is what lets
// the next reconciliation adopt the tmux session that is still running rather
// than treat it as a foreign occupant of the name.
func TestRegisterAfterMigrationRekeysTheMigratedSession(t *testing.T) {
	const repoRoot = "/gone/slabledger"
	// The pre-0002 workspace ID and the post-0002 repository ID are the same
	// expression — SHA-256 of the canonical path — which is exactly why the
	// collision exists. Computing it once and using it for both roles keeps
	// that identity visible instead of asserting it.
	sum := sha256.Sum256([]byte(repoRoot))
	legacyID := hex.EncodeToString(sum[:])
	workspaceSum := sha256.Sum256([]byte(repoRoot + "\x00" + ""))
	registerTime := time.Date(2026, 8, 8, 9, 0, 0, 0, time.UTC)

	root := seedSchema1As(t, legacyID, repoRoot)
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	ws := resolve.Workspace{
		ID:           hex.EncodeToString(workspaceSum[:]),
		RepositoryID: legacyID,
		Slug:         "slabledger",
		RepoRoot:     repoRoot,
		Session:      "",
		SessionName:  "slabledger",
	}
	if ws.ID == legacyID {
		t.Fatalf("workspace ID still equals the repository ID; the session is " +
			"not being hashed in and lockPhases would deadlock on one path")
	}

	if err := s.RegisterWorkspace(ws, "sha256:cccc", registerTime); err != nil {
		t.Fatalf("RegisterWorkspace over a migrated row: %v", err)
	}

	recs, err := s.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("%d workspaces, want 1 — the migrated row was duplicated "+
			"rather than re-keyed", len(recs))
	}
	rec := recs[0]
	if rec.ID != ws.ID {
		t.Errorf("workspace ID = %s, want %s", rec.ID, ws.ID)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slabledger" {
		t.Errorf("actual_session = %v, want the running assignment carried over",
			rec.ActualSession)
	}
	if rec.AppliedDigest == nil || *rec.AppliedDigest != "sha256:bbbb" {
		t.Errorf("applied_digest = %v, want the applied digest carried over",
			rec.AppliedDigest)
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:cccc" {
		t.Errorf("desired_digest = %v, want the digest this call supplied",
			rec.DesiredDigest)
	}
	if rec.LastOperation == nil || rec.LastOperation.Name != "open" {
		t.Errorf("last operation = %+v, want the row re-keyed alongside the "+
			"workspace rather than orphaned", rec.LastOperation)
	}
	if rec.Container == nil || rec.Container.ContainerID != "c-1" {
		t.Errorf("container = %+v, want the repository's binding still "+
			"projected onto the re-keyed session", rec.Container)
	}
}
```

- [ ] **Step 5: Run the state tests to verify they fail**

Run: `go test ./internal/state/... -v`
Expected: FAIL to build — `s.Repositories undefined (type *Store has no field or method Repositories)`, `unknown field RepositoryID in struct literal of type resolve.Workspace` is gone after Task 1 but `rec.RepoRoot undefined (type Record has no field or method RepoRoot)` remains.

- [ ] **Step 6: Write migration 0002**

Create `internal/state/migrations/0002_repositories.sql`:

```sql
-- Schema version 2: the repository, not the worktree, is what a container is
-- keyed on, and a repository has many sessions (design §5.2).
--
-- This migration is pure SQL by design (design §9). Telling a main worktree
-- from a linked one requires asking git, and a schema migration that depended
-- on the filesystem and on git's exit status would abort an upgrade because a
-- directory was deleted between runs. Every stored worktree therefore becomes
-- a repository verbatim; `rebuild` collapses the rows whose path is really a
-- linked worktree and drops the rows whose path is gone. The intermediate
-- state is over-counted, never wrong.
--
-- IDs are carried over rather than recomputed: the new derivation is a SHA-256
-- SQLite has no function for. They are stale, not invalid, and the first
-- registration of a repository re-keys its row (see RegisterWorkspace).

-- The rebuild goes through unconstrained copies rather than the usual
-- create-and-rename. Dropping a table that foreign keys point at runs an
-- implicit DELETE, so dropping `workspaces` while any child still references
-- it would cascade the children away — and PRAGMA foreign_keys cannot be
-- turned off here, because it is a no-op inside a transaction and migrations
-- run inside one (migrate.go:57).
CREATE TABLE m0002_workspaces AS SELECT * FROM workspaces;
CREATE TABLE m0002_container_bindings AS SELECT * FROM container_bindings;
CREATE TABLE m0002_last_operations AS SELECT * FROM last_operations;

DROP TABLE container_bindings;
DROP TABLE last_operations;
DROP TABLE workspaces;

CREATE TABLE repositories (
    id            TEXT PRIMARY KEY,
    slug          TEXT NOT NULL,
    repo_root     TEXT NOT NULL UNIQUE,
    registered_at TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- slug lives on the repository because it is a property of the repository;
-- storing it per session would let two sessions on one repository disagree.
CREATE TABLE workspaces (
    id               TEXT PRIMARY KEY,
    repository_id    TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    session          TEXT NOT NULL DEFAULT '',
    proposed_session TEXT NOT NULL,
    actual_session   TEXT UNIQUE,
    desired_digest   TEXT,
    applied_digest   TEXT,
    registered_at    TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    UNIQUE (repository_id, session)
);

-- A row exists only once a binding has been recorded: no row means no
-- binding has ever existed, and health is non-null whenever one does.
-- health=missing or health=unknown never clears the identity columns;
-- only a successful replacement overwrites them.
CREATE TABLE container_bindings (
    repository_id  TEXT PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    container_id   TEXT NOT NULL,
    container_user TEXT,
    workdir        TEXT,
    health         TEXT NOT NULL CHECK (health IN ('present', 'missing', 'unknown')),
    observed_at    TEXT NOT NULL
);

-- last_operations stays keyed on workspace_id: an operation is performed by a
-- session, not by a repository.
--
-- ON UPDATE CASCADE is what carries this row along when step 9 re-keys a
-- migrated workspace. Foreign keys are enforced here: Open puts
-- _pragma=foreign_keys(1) on the DSN so every pooled connection has them on
-- (internal/state/state.go:65), and migrate_test.go:152-156 asserts it per
-- connection. The declaration is therefore load-bearing, not documentation.
--
-- Step 9 still writes its cleanup out statement by statement rather than
-- leaning on ON DELETE CASCADE. That is a readability choice, not a
-- correctness one: a reader of RegisterWorkspace should be able to see which
-- rows a stale repository takes with it without reconstructing the schema
-- from memory, and the explicit order is the same order the cascade would
-- have used.
CREATE TABLE last_operations (
    workspace_id  TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE ON UPDATE CASCADE,
    operation     TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN ('ok', 'failed')),
    exit_status   INTEGER,
    error_summary TEXT,
    finished_at   TEXT NOT NULL
);

INSERT INTO repositories (id, slug, repo_root, registered_at, updated_at)
SELECT id, slug, worktree, registered_at, updated_at FROM m0002_workspaces;

-- Every migrated row becomes the default session of its repository. is_primary
-- is not carried: every repository row is a main worktree by construction, so
-- the flag would always be true.
INSERT INTO workspaces
    (id, repository_id, session, proposed_session, actual_session,
     desired_digest, applied_digest, registered_at, updated_at)
SELECT id, id, '', proposed_session, actual_session,
       desired_digest, applied_digest, registered_at, updated_at
FROM m0002_workspaces;

-- One binding per repository, keeping the most recently observed one. The
-- grouping is defensive today — 0001 made workspaces.worktree UNIQUE, so the
-- mapping is one-to-one — but the tie-break is stated rather than left to scan
-- order, because `rebuild` collapses siblings onto one repository next. The
-- non-aggregated columns come from the MAX row: SQLite defines bare columns
-- that way when the query uses exactly one min/max aggregate.
INSERT INTO container_bindings
    (repository_id, kind, container_id, container_user, workdir, health, observed_at)
SELECT w.repository_id, b.kind, b.container_id, b.container_user, b.workdir,
       b.health, MAX(b.observed_at)
FROM m0002_container_bindings b
JOIN workspaces w ON w.id = b.workspace_id
GROUP BY w.repository_id;

INSERT INTO last_operations
    (workspace_id, operation, outcome, exit_status, error_summary, finished_at)
SELECT workspace_id, operation, outcome, exit_status, error_summary, finished_at
FROM m0002_last_operations;

DROP TABLE m0002_container_bindings;
DROP TABLE m0002_last_operations;
DROP TABLE m0002_workspaces;
```

- [ ] **Step 7: Bump the schema version**

`migrate.go:22-23`:

```go
// SchemaVersion is the newest schema this build understands.
const SchemaVersion = 2
```

- [ ] **Step 8: Split `Record` and add `Repository` in `types.go`**

Replace `types.go:32-47`:

```go
// Record is the joined current state of one session on one repository. Slug
// and RepoRoot are the repository's, read through the join; LastOperation is
// nil when never recorded.
//
// Container is the *repository's* binding, projected onto every session that
// shares it, and is nil when no binding has been recorded. It is a read-only
// projection: writes go through RecordContainerObservation, which takes a
// repository ID, and there is no per-session binding to write. Keeping it on
// the record is what lets a session observe the container a sibling session
// started (design §5.2, §6.1) without every caller having to carry a second
// identifier into a result it assembles per session.
type Record struct {
	ID              string
	RepositoryID    string
	Slug            string
	RepoRoot        string
	Session         string
	ProposedSession string
	ActualSession   *string
	DesiredDigest   *string
	AppliedDigest   *string
	RegisteredAt    time.Time
	UpdatedAt       time.Time
	Container       *ContainerBinding
	LastOperation   *Operation
}

// Repository is one project and the container every session on it shares.
// Container is nil when no binding has ever been recorded.
type Repository struct {
	ID           string
	Slug         string
	RepoRoot     string
	RegisteredAt time.Time
	UpdatedAt    time.Time
	Container    *ContainerBinding
}
```

- [ ] **Step 9: Register the repository and the session together**

Replace `store.go:26-56`:

```go
// RegisterWorkspace upserts the repository and the session on it.
// Re-registration refreshes everything derivable from resolution and
// configuration while preserving registered_at, the assigned session name,
// the applied digest, and any binding — rebuilding the database is simply
// re-running registration (design §7).
func (s *Store) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Foreign keys are enforced on this connection (internal/state/state.go:65),
	// so deleting the stale repository alone would cascade the rest away. The
	// cleanup is written out statement by statement anyway, in dependency
	// order, so that a reader can see what a stale repository takes with it
	// without reconstructing the schema from memory. Each statement is a no-op
	// when the cascade would have covered it, because the cascade has not run
	// yet — the repository row is deleted last.
	//
	// repo_root is UNIQUE, so a row recorded under a different ID for this
	// same path would fail the insert rather than be refreshed by it. This is
	// *not* the migrated case: the pre-0002 workspace ID was SHA-256 of the
	// canonical path (internal/resolve/resolve.go:107 before Task 1), which is
	// byte-identical to the new repository ID, so a migrated repository row
	// already carries the right ID and this delete matches nothing. It fires
	// when a repository's identity genuinely moved — a path re-cased or
	// re-canonicalized under an ID derived from the old spelling.
	if _, err := tx.Exec(`
		DELETE FROM last_operations WHERE workspace_id IN (
			SELECT w.id FROM workspaces w
			JOIN repositories r ON r.id = w.repository_id
			WHERE r.repo_root = ? AND r.id <> ?)`,
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("clearing operations for the stale repository at %s: %w", ws.RepoRoot, err)
	}
	if _, err := tx.Exec(`
		DELETE FROM workspaces WHERE repository_id IN (
			SELECT id FROM repositories WHERE repo_root = ? AND id <> ?)`,
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("clearing sessions of the stale repository at %s: %w", ws.RepoRoot, err)
	}
	if _, err := tx.Exec(
		"DELETE FROM container_bindings WHERE repository_id IN ("+
			"SELECT id FROM repositories WHERE repo_root = ? AND id <> ?)",
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("clearing the binding of the stale repository at %s: %w", ws.RepoRoot, err)
	}
	if _, err := tx.Exec(
		"DELETE FROM repositories WHERE repo_root = ? AND id <> ?",
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("replacing the stale repository for %s: %w", ws.RepoRoot, err)
	}

	// The *workspace* ID is where migration 0002 does leave a stale value. It
	// carries the old ID into workspaces.id because SQLite cannot compute
	// SHA-256, but the new derivation hashes the session alongside the path,
	// so the default session's ID changes even though its repository's does
	// not. Inserting the new ID beside the carried-over row would violate
	// UNIQUE (repository_id, session) — that row is already the default
	// session of this repository — and the ON CONFLICT(id) clause below cannot
	// absorb a conflict on a different constraint, so the insert would fail
	// outright and every migrated repository would be unopenable.
	//
	// Re-key the row instead of deleting it. actual_session and applied_digest
	// are what let the next reconciliation adopt the tmux session that is
	// already running rather than treat it as a foreign occupant; dropping
	// them would turn every first post-migration open into a name conflict.
	var stale string
	err = tx.QueryRow(
		"SELECT id FROM workspaces WHERE repository_id = ? AND session = ? AND id <> ?",
		ws.RepositoryID, ws.Session, ws.ID).Scan(&stale)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing carried over for this session: the common steady-state path.
	case err != nil:
		return fmt.Errorf("looking for a stale ID for %s: %w", ws.SessionName, err)
	default:
		// last_operations follows on its own: its workspace_id is declared
		// ON UPDATE CASCADE and foreign keys are enforced on this connection
		// (internal/state/state.go:65). An explicit UPDATE here would match
		// nothing, because the cascade has already moved the row by the time
		// it ran. The regression test asserts the operation survives under
		// the new ID, so a schema change that dropped the cascade would fail
		// there rather than silently orphan the row.
		if _, err := tx.Exec(
			"UPDATE workspaces SET id = ?, updated_at = ? WHERE id = ?",
			ws.ID, encodeTime(now), stale); err != nil {
			return fmt.Errorf("re-keying the migrated session %s: %w", ws.SessionName, err)
		}
	}

	_, err = tx.Exec(`
		INSERT INTO repositories (id, slug, repo_root, registered_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			slug       = excluded.slug,
			repo_root  = excluded.repo_root,
			updated_at = excluded.updated_at`,
		ws.RepositoryID, ws.Slug, ws.RepoRoot, encodeTime(now), encodeTime(now))
	if err != nil {
		return fmt.Errorf("registering repository %s: %w", ws.RepositoryID, err)
	}

	_, err = tx.Exec(`
		INSERT INTO workspaces
			(id, repository_id, session, proposed_session,
			 desired_digest, registered_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			repository_id    = excluded.repository_id,
			session          = excluded.session,
			proposed_session = excluded.proposed_session,
			desired_digest   = excluded.desired_digest,
			updated_at       = excluded.updated_at`,
		ws.ID, ws.RepositoryID, ws.Session, ws.SessionName,
		desiredDigest, encodeTime(now), encodeTime(now))
	if err != nil {
		return fmt.Errorf("registering workspace %s: %w", ws.ID, err)
	}
	return tx.Commit()
}
```

The unused `boolInt` helper (`store.go:19-24`) goes with `is_primary`; delete it.

- [ ] **Step 10: Read the record through the repository join**

Replace `store.go:58-67`, the doc and ORDER BY at `:73-74` / `:92`, and `scanRecord` (`:116-183`):

```go
// selectRecord joins the repository for the identity columns and the
// repository's container binding for the projection Record.Container
// exposes. The binding join is on repository_id, not workspace_id: that is
// the whole point — every session on a repository reads the same binding,
// including one a sibling session wrote.
const selectRecord = `
SELECT
	w.id, w.repository_id, r.slug, r.repo_root, w.session, w.proposed_session,
	w.actual_session, w.desired_digest, w.applied_digest,
	w.registered_at, w.updated_at,
	cb.kind, cb.container_id, cb.container_user, cb.workdir,
	cb.health, cb.observed_at,
	o.operation, o.outcome, o.exit_status, o.error_summary, o.finished_at
FROM workspaces w
JOIN repositories r ON r.id = w.repository_id
LEFT JOIN container_bindings cb ON cb.repository_id = w.repository_id
LEFT JOIN last_operations o ON o.workspace_id = w.id`
```

```go
// Workspaces returns every registered session ordered by slug, repository
// root, then session.
func (s *Store) Workspaces() ([]Record, error) { return queryWorkspaces(s.db) }
```

```go
	rows, err := db.Query(selectRecord + " ORDER BY r.slug, r.repo_root, w.session")
```

```go
func scanRecord(r rowScanner) (Record, error) {
	var (
		rec                 Record
		actual, desired     sql.NullString
		applied             sql.NullString
		registered, updated string
		cKind, cID          sql.NullString
		cUser, cWorkdir     sql.NullString
		cHealth, cObserved  sql.NullString
		oName, oOutcome     sql.NullString
		oSummary, oFinished sql.NullString
		oExit               sql.NullInt64
	)
	err := r.Scan(
		&rec.ID, &rec.RepositoryID, &rec.Slug, &rec.RepoRoot, &rec.Session,
		&rec.ProposedSession, &actual, &desired, &applied, &registered, &updated,
		&cKind, &cID, &cUser, &cWorkdir, &cHealth, &cObserved,
		&oName, &oOutcome, &oExit, &oSummary, &oFinished)
	if err != nil {
		return Record{}, err
	}

	rec.ActualSession = nullable(actual)
	rec.DesiredDigest = nullable(desired)
	rec.AppliedDigest = nullable(applied)
	if rec.RegisteredAt, err = decodeTime(registered); err != nil {
		return Record{}, fmt.Errorf("registered_at: %w", err)
	}
	if rec.UpdatedAt, err = decodeTime(updated); err != nil {
		return Record{}, fmt.Errorf("updated_at: %w", err)
	}

	// container_id is NOT NULL in the table, so its validity is a faithful
	// test for "the LEFT JOIN matched a binding" rather than for "the column
	// happened to be set".
	if cID.Valid {
		observedAt, err := decodeTime(cObserved.String)
		if err != nil {
			return Record{}, fmt.Errorf("observed_at: %w", err)
		}
		rec.Container = &ContainerBinding{
			Kind:          cKind.String,
			ContainerID:   cID.String,
			ContainerUser: cUser.String,
			Workdir:       cWorkdir.String,
			Health:        Health(cHealth.String),
			ObservedAt:    observedAt,
		}
	}

	if oName.Valid {
		finishedAt, err := decodeTime(oFinished.String)
		if err != nil {
			return Record{}, fmt.Errorf("finished_at: %w", err)
		}
		op := &Operation{
			Name:         oName.String,
			Outcome:      Outcome(oOutcome.String),
			ErrorSummary: oSummary.String,
			FinishedAt:   finishedAt,
		}
		if oExit.Valid {
			exit := int(oExit.Int64)
			op.ExitStatus = &exit
		}
		rec.LastOperation = op
	}
	return rec, nil
}
```

- [ ] **Step 11: Add `Repositories`**

Insert after `queryWorkspaces` (`store.go:110`):

```go
const selectRepository = `
SELECT
	r.id, r.slug, r.repo_root, r.registered_at, r.updated_at,
	b.kind, b.container_id, b.container_user, b.workdir, b.health, b.observed_at
FROM repositories r
LEFT JOIN container_bindings b ON b.repository_id = r.id`

// Repositories returns every registered repository ordered by slug, then
// repository root. Autostart iterates this rather than filtering sessions, so
// a shared container is started once per repository by construction.
func (s *Store) Repositories() ([]Repository, error) { return queryRepositories(s.db) }

func queryRepositories(db *sql.DB) ([]Repository, error) {
	rows, err := db.Query(selectRepository + " ORDER BY r.slug, r.repo_root")
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Repository
	for rows.Next() {
		repo, err := scanRepository(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a repository row: %w", err)
		}
		out = append(out, repo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	return out, nil
}

func scanRepository(r rowScanner) (Repository, error) {
	var (
		repo                Repository
		registered, updated string
		cKind, cID          sql.NullString
		cUser, cWorkdir     sql.NullString
		cHealth, cObserved  sql.NullString
	)
	err := r.Scan(
		&repo.ID, &repo.Slug, &repo.RepoRoot, &registered, &updated,
		&cKind, &cID, &cUser, &cWorkdir, &cHealth, &cObserved)
	if err != nil {
		return Repository{}, err
	}
	if repo.RegisteredAt, err = decodeTime(registered); err != nil {
		return Repository{}, fmt.Errorf("registered_at: %w", err)
	}
	if repo.UpdatedAt, err = decodeTime(updated); err != nil {
		return Repository{}, fmt.Errorf("updated_at: %w", err)
	}
	if cKind.Valid {
		observedAt, err := decodeTime(cObserved.String)
		if err != nil {
			return Repository{}, fmt.Errorf("observed_at: %w", err)
		}
		repo.Container = &ContainerBinding{
			Kind:          cKind.String,
			ContainerID:   cID.String,
			ContainerUser: cUser.String,
			Workdir:       cWorkdir.String,
			Health:        Health(cHealth.String),
			ObservedAt:    observedAt,
		}
	}
	return repo, nil
}
```

- [ ] **Step 12: Key container observations on the repository**

Replace `store.go:266-327`:

```go
// RecordContainerObservation upserts the repository's container binding. A
// present observation replaces the binding; missing and unknown update health
// and freshness while retaining the stored identity (design §7). With no
// existing binding, missing and unknown record nothing.
func (s *Store) RecordContainerObservation(repositoryID string, obs ContainerObservation, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordContainer(tx, repositoryID, obs, now); err != nil {
		return err
	}
	return tx.Commit()
}

func recordContainer(tx txExecer, repositoryID string, obs ContainerObservation, now time.Time) error {
	if err := requireRepository(tx, repositoryID); err != nil {
		return err
	}
	switch obs.Health {
	case HealthPresent:
		if obs.ContainerID == "" {
			return errors.New("a present container observation must carry a container ID")
		}
		_, err := tx.Exec(`
			INSERT INTO container_bindings
				(repository_id, kind, container_id, container_user, workdir, health, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repository_id) DO UPDATE SET
				kind           = excluded.kind,
				container_id   = excluded.container_id,
				container_user = excluded.container_user,
				workdir        = excluded.workdir,
				health         = excluded.health,
				observed_at    = excluded.observed_at`,
			repositoryID, obs.Kind, obs.ContainerID, obs.ContainerUser,
			obs.Workdir, string(obs.Health), encodeTime(now))
		if err != nil {
			return fmt.Errorf("recording the container binding: %w", err)
		}
		return nil
	case HealthMissing, HealthUnknown:
		_, err := tx.Exec(
			"UPDATE container_bindings SET health = ?, observed_at = ? WHERE repository_id = ?",
			string(obs.Health), encodeTime(now), repositoryID)
		if err != nil {
			return fmt.Errorf("recording container health: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid container health %q", obs.Health)
	}
}

func requireRepository(tx txExecer, repositoryID string) error {
	var n int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM repositories WHERE id = ?", repositoryID).Scan(&n); err != nil {
		return fmt.Errorf("checking repository %s: %w", repositoryID, err)
	}
	if n == 0 {
		return fmt.Errorf("repository %s: %w", repositoryID, ErrNotFound)
	}
	return nil
}

func requireWorkspace(tx txExecer, workspaceID string) error {
	var n int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM workspaces WHERE id = ?", workspaceID).Scan(&n); err != nil {
		return fmt.Errorf("checking workspace %s: %w", workspaceID, err)
	}
	if n == 0 {
		return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
	}
	return nil
}
```

Then replace the container branch of `CommitReconciliation` (`store.go:405-409`):

```go
	if r.Container != nil {
		// A reconciliation is a session's, but the container it observed is
		// the repository's. Looking the owner up here keeps the caller from
		// having to carry both IDs into a result it assembles per session.
		var repositoryID string
		err := tx.QueryRow(
			"SELECT repository_id FROM workspaces WHERE id = ?", workspaceID).Scan(&repositoryID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("reading workspace %s: %w", workspaceID, err)
		}
		if err := recordContainer(tx, repositoryID, *r.Container, now); err != nil {
			return err
		}
	}
```

- [ ] **Step 13: Run the state tests**

Run: `go test ./internal/state/... -v`
Expected: PASS, including `TestMigration0002MovesEveryWorkspaceIntoARepository`, `TestTwoSessionsOnOneRepositoryCoexist`, and `TestRegisteringReplacesAStaleRepositoryForTheSamePath`.

- [ ] **Step 14: Format and re-run both converted packages**

Run: `gofmt -l internal/state internal/resolve && go test ./internal/state/... ./internal/resolve/...`
Expected: no `gofmt` output, both packages `ok`. `go build ./...` still fails in the CLI, controller, container, rebuild, tmux, and doctor packages; those are later tasks.

- [ ] **Step 15: Commit**

Run:
```
git add internal/state && git commit -m "state: split repositories from sessions in schema 0002

workspaces.worktree was NOT NULL UNIQUE and container_bindings was keyed
on workspace_id, so two sessions on one repository could not be stored
and a shared container had no owner. 0002 moves every row verbatim into
repositories + workspaces and re-keys container_bindings, in pure SQL so
the upgrade cannot fail on a path that has since been deleted; rebuild
corrects the classification, which needs git. Adds Store.Repositories,
which autostart iterates in place of the dropped is_primary flag."
```

---

### Task 3: Repository-scoped locking, and the controller's own path readers

**Files:**
- Modify: `internal/lock/lock.go:1-70` (package comment, `ErrLockHeld`, `Acquire`)
- Modify: `internal/lock/lock_test.go:1-46` (imports and the typed-error assertions)
- Create: `internal/controller/locking.go`
- Modify: `internal/controller/ensure.go:79-85` (the `lock.Acquire` call is line 81)
- Modify: `internal/controller/stop.go:28-34` (the `lock.Acquire` call is line 30)
- Modify: `internal/controller/autostart.go:27-33` (the `lock.Acquire` call is line 29)
- Modify: `internal/controller/ensure.go:255-257, 265, 272, 278, 280, 286, 392, 441` (window/pane dirs, the `SessionSpec` value, and the post-create identity confirmation)
- Modify: `internal/controller/plan.go:107-113` (`SessionBelongsTo` comparison and its doc comment)
- Modify: `internal/controller/fake/fake.go:246-262` (the `Workspaces` ordering)
- Modify: `internal/controller/ensure_test.go:107-115` (`ensureWorkspace`), `:114-116`, `:269`
- Test: `internal/controller/lock_ordering_test.go`
- Test: `internal/controller/plan_test.go:32, 313`
- Test: `internal/controller/observe_test.go:23-25, 86`
- Test: `internal/controller/render_test.go:31, 57, 73`
- Test: `internal/controller/fake/fake_test.go:21-23, 80-81`

**Interfaces:**
- Consumes: `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}` (Task 1); `fake.NewStore() *fake.Store`, `fake.ContainerObserver`, `fake.SessionActuator`, `fake.Clock` (`internal/controller/fake/fake.go`); `controller.Controller`, `controller.Desired`, `controller.Ensure`.
- Produces: `func lock.Acquire(ctx context.Context, dir, key string, timeout time.Duration) (*lock.Lock, error)`; `type lock.ErrLockHeld struct{ Key string }`; unexported `controller.lockPhases(ctx context.Context, dir, repositoryID, workspaceID string, timeout time.Duration) (func(), error)`.

Verified before renaming: the only `lock.Acquire` callers are `internal/controller/ensure.go:81`, `internal/controller/stop.go:30`, `internal/controller/autostart.go:29` and `internal/cli/rebuild.go:260`; the only `ErrLockHeld` construction is `internal/lock/lock.go:61`, and the only assertions on its field are `internal/lock/lock_test.go:41-44` and `internal/controller/ensure_test.go:499-501` (which reads the type, not the field). `internal/cli/rebuild.go:259` (`workspaceLocker.Lock`) locks the recovery workflow's per-session registration and keeps the workspace ID; the parameter rename does not touch it.

Steps 2 through 7 are not about locking. They convert `internal/controller` and its in-memory fake
onto the renamed `resolve.Workspace` and `state.Record` fields, and they live here because this task
is the first in the plan to run a controller test: Step 9's red run cannot report a failing
assertion in a package that does not compile, and `go test ./internal/controller/` links
`internal/controller/fake` whether or not the fake's own tests are being run. They are mechanical
and change no behaviour — the value flowing through each site is the same absolute path it always
was, only now guaranteed to be a main worktree.

Telling the two directions of the mirror apart is the one thing that needs care. Where the code
reads a *workspace or record* field the name becomes `RepoRoot`; where it reads a *tmux-derived*
field on `controller.LiveSession` or writes `controller.SessionSpec` the name stays `Worktree` and
only the right-hand side moves. `ensure.go:392` and `ensure.go:441` are exactly that case, and so
are the `LiveSession` literals scattered through the controller tests, which must be left alone.

- [ ] **Step 1: Give the ensure test workspace a repository ID**

`internal/controller/ensure_test.go`, replacing `ensureWorkspace`:

```go
func ensureWorkspace() resolve.Workspace {
	return resolve.Workspace{
		ID:           "w1",
		RepositoryID: "r1",
		Slug:         "slab",
		RepoRoot:     "/w/slab",
		SessionName:  "slab",
	}
}
```

- [ ] **Step 2: Point `ensure.go`'s window and pane directories at the repository root**

`renderWindows` derives every window's and pane's working directory from the workspace path. Five
reads change, plus the comment above the container-pane branch, which explains the host-side `-c`
in terms of "the worktree".

Before (lines 255-257 and 262-267):

```go
				// its own; inside the container that is the exec relDir,
				// while the host-side -c stays the worktree, matching the
				// window itself.
				relDir := in.RelDir
				if p.RelDir != "" {
					relDir = p.RelDir
				}
				panes = append(panes, PaneSpec{
					Name:    p.Name,
					Command: act.ExecCommand(binding, p.Command, relDir, d.Config.Environment),
					Dir:     d.Workspace.Worktree,
					Focus:   p.Focus,
				})
```

After:

```go
				// its own; inside the container that is the exec relDir,
				// while the host-side -c stays the repository root,
				// matching the window itself.
				relDir := in.RelDir
				if p.RelDir != "" {
					relDir = p.RelDir
				}
				panes = append(panes, PaneSpec{
					Name:    p.Name,
					Command: act.ExecCommand(binding, p.Command, relDir, d.Config.Environment),
					Dir:     d.Workspace.RepoRoot,
					Focus:   p.Focus,
				})
```

Before (lines 269-274, the container window itself):

```go
			specs = append(specs, WindowSpec{
				Name:    in.Name,
				Command: act.ExecCommand(binding, in.Command, in.RelDir, d.Config.Environment),
				Dir:     d.Workspace.Worktree,
				Focus:   in.Focus,
				Panes:   panes,
			})
```

After:

```go
			specs = append(specs, WindowSpec{
				Name:    in.Name,
				Command: act.ExecCommand(binding, in.Command, in.RelDir, d.Config.Environment),
				Dir:     d.Workspace.RepoRoot,
				Focus:   in.Focus,
				Panes:   panes,
			})
```

Before (lines 278-288, the host branch):

```go
		dir := d.Workspace.Worktree
		if in.RelDir != "" {
			dir = filepath.Join(d.Workspace.Worktree, in.RelDir)
		}
		panes := make([]PaneSpec, 0, len(in.Panes))
		for _, p := range in.Panes {
			paneDir := dir
			if p.RelDir != "" {
				paneDir = filepath.Join(d.Workspace.Worktree, p.RelDir)
			}
```

After:

```go
		dir := d.Workspace.RepoRoot
		if in.RelDir != "" {
			dir = filepath.Join(d.Workspace.RepoRoot, in.RelDir)
		}
		panes := make([]PaneSpec, 0, len(in.Panes))
		for _, p := range in.Panes {
			paneDir := dir
			if p.RelDir != "" {
				paneDir = filepath.Join(d.Workspace.RepoRoot, p.RelDir)
			}
```

- [ ] **Step 3: Feed the repository root into the `SessionSpec` and the post-create confirmation**

`SessionSpec.Worktree` is written straight onto the tmux `@dev_worktree` user option by
`internal/tmux/actuate.go`, and `LiveSession.Worktree` is what comes back out of it. Both field
names stay; only the workspace-side expression changes.

Before (lines 388-395):

```go
	spec := SessionSpec{
		Name:        name,
		WorkspaceID: id,
		Slug:        d.Workspace.Slug,
		Worktree:    d.Workspace.Worktree,
		Env:         d.Config.Environment,
		Windows:     windows,
	}
```

After:

```go
	spec := SessionSpec{
		Name:        name,
		WorkspaceID: id,
		Slug:        d.Workspace.Slug,
		Worktree:    d.Workspace.RepoRoot,
		Env:         d.Config.Environment,
		Windows:     windows,
	}
```

Before (lines 440-443, inside `confirmCreation`):

```go
	if live.WorkspaceID != d.Workspace.ID || live.Slug != d.Workspace.Slug ||
		live.Worktree != d.Workspace.Worktree {
		return fmt.Sprintf("session %q carries contradictory identity keys after creation", live.Name)
	}
```

After:

```go
	if live.WorkspaceID != d.Workspace.ID || live.Slug != d.Workspace.Slug ||
		live.Worktree != d.Workspace.RepoRoot {
		return fmt.Sprintf("session %q carries contradictory identity keys after creation", live.Name)
	}
```

- [ ] **Step 4: Update `SessionBelongsTo` and say what the third key now means**

The doc comment calls the third identity key a worktree. Post-change the tag still spells
`@dev_worktree`, but the value it carries is the repository root, and a reader comparing this
function against the design needs the comment to say so.

Before (lines 107-114):

```go
// SessionBelongsTo compares all three load-bearing identity keys
// (design §7): a session with the right workspace ID but a contradictory
// slug or worktree is evidence of corruption or collision, not a match.
// The CLI's status and attach verdicts reuse it so the rendered identity
// can never drift from planning's.
func SessionBelongsTo(s LiveSession, ws resolve.Workspace) bool {
	return s.WorkspaceID == ws.ID && s.Slug == ws.Slug && s.Worktree == ws.Worktree
}
```

After:

```go
// SessionBelongsTo compares all three load-bearing identity keys
// (design §7): a session with the right workspace ID but a contradictory
// slug or repository root is evidence of corruption or collision, not a
// match. The CLI's status and attach verdicts reuse it so the rendered
// identity can never drift from planning's. LiveSession.Worktree keeps
// its name because it mirrors the tmux user option @dev_worktree, which
// is unchanged; the value it carries is now the repository root.
func SessionBelongsTo(s LiveSession, ws resolve.Workspace) bool {
	return s.WorkspaceID == ws.ID && s.Slug == ws.Slug && s.Worktree == ws.RepoRoot
}
```

- [ ] **Step 5: Convert the in-memory fake store's field reads and ordering**

`internal/controller/fake` is a non-test file that stands in for the SQLite store across the
controller, doctor, and rebuild tests, and `go test ./internal/controller/` links it, so it has to
compile before any gate in this task can run. Two functions read the dropped fields. Convert both to
the renamed ones here; Task 5b comes back to `RegisterWorkspace` and reshapes it to write a
*repository* row alongside the session row, which is a behaviour change this task neither needs nor
should make.

`RegisterWorkspace` (lines 101-123) has four reads, two on the update path and two on the insert
path. Before:

```go
	if rec, ok := s.records[ws.ID]; ok {
		rec.Slug = ws.Slug
		rec.Worktree = ws.Worktree
		rec.IsPrimary = ws.IsPrimary
		rec.ProposedSession = ws.SessionName
```

After — `RepositoryID` comes across too, because the record now carries it and the sibling lookups
Task 5b adds read it:

```go
	if rec, ok := s.records[ws.ID]; ok {
		rec.Slug = ws.Slug
		rec.RepositoryID = ws.RepositoryID
		rec.RepoRoot = ws.RepoRoot
		rec.ProposedSession = ws.SessionName
```

Before:

```go
	s.records[ws.ID] = &state.Record{
		ID:              ws.ID,
		Slug:            ws.Slug,
		Worktree:        ws.Worktree,
		IsPrimary:       ws.IsPrimary,
		ProposedSession: ws.SessionName,
```

After:

```go
	s.records[ws.ID] = &state.Record{
		ID:              ws.ID,
		RepositoryID:    ws.RepositoryID,
		Slug:            ws.Slug,
		RepoRoot:        ws.RepoRoot,
		ProposedSession: ws.SessionName,
```

The second is `Workspaces`, whose sort key still names the dropped field.

Before (lines 246-262):

```go
// Workspaces returns every registered workspace ordered by slug, then
// worktree, mirroring the real store's ORDER BY (internal/state/store.go).
func (s *Store) Workspaces() ([]state.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, s.copyRecordLocked(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].Worktree < out[j].Worktree
	})
	return out, nil
}
```

After:

```go
// Workspaces returns every registered workspace ordered by slug, then
// repository root, mirroring the real store's ORDER BY
// (internal/state/store.go).
func (s *Store) Workspaces() ([]state.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Record, 0, len(s.records))
	for _, rec := range s.records {
		out = append(out, s.copyRecordLocked(rec))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].RepoRoot < out[j].RepoRoot
	})
	return out, nil
}
```

- [ ] **Step 6: Convert the controller tests that build workspaces and records**

These are `resolve.Workspace` and `state.Record` literals only. Every `controller.LiveSession`
literal in the same files (`ensure_test.go:134, 329, 402`, `stop_test.go:113`, `plan_test.go:44, 170`)
keeps `Worktree` and must not be touched.

`ensureWorkspace` at `internal/controller/ensure_test.go:110-118` was already converted in
Step 1 of this task, which is also where it gained its `RepositoryID`; it needs nothing
further here.

`internal/controller/ensure_test.go:268-270` before:

```go
	other := resolve.Workspace{
		ID: "w2", Slug: "other", Worktree: "/w/other", SessionName: "other",
	}
```

after:

```go
	other := resolve.Workspace{
		ID: "w2", Slug: "other", RepoRoot: "/w/other", SessionName: "other",
	}
```

`internal/controller/plan_test.go:28-36` before:

```go
	return &state.Record{
		ID:              "w1",
		Slug:            "slabledger",
		Worktree:        "/w/slabledger",
		ProposedSession: "slabledger",
		ActualSession:   actual,
		AppliedDigest:   applied,
	}
```

after:

```go
	return &state.Record{
		ID:              "w1",
		Slug:            "slabledger",
		RepoRoot:        "/w/slabledger",
		ProposedSession: "slabledger",
		ActualSession:   actual,
		AppliedDigest:   applied,
	}
```

`internal/controller/plan_test.go:313` before:

```go
			Workspace: resolve.Workspace{ID: "w1", Slug: "s", Worktree: "/w"},
```

after:

```go
			Workspace: resolve.Workspace{ID: "w1", Slug: "s", RepoRoot: "/w"},
```

`internal/controller/observe_test.go:20-27` before:

```go
		Workspace: resolve.Workspace{
			ID:          "w1",
			Slug:        "slabledger",
			Worktree:    "/w/slabledger",
			SessionName: "slabledger",
			IsPrimary:   true,
		},
```

after:

```go
		Workspace: resolve.Workspace{
			ID:          "w1",
			Slug:        "slabledger",
			RepoRoot:    "/w/slabledger",
			SessionName: "slabledger",
		},
```

`internal/controller/observe_test.go:86` before:

```go
	other.Worktree = "/w/other"
```

after:

```go
	other.RepoRoot = "/w/other"
```

`internal/controller/render_test.go:31, 57, 73` before (three occurrences, the third spanning onto
the following line):

```go
	d := Desired{Workspace: resolve.Workspace{Worktree: "/w/slab"}}
```

```go
	d := Desired{Workspace: resolve.Workspace{Worktree: "/w/slab"},
		Config: config.Config{Environment: map[string]string{"K": "v"}}}
```

after:

```go
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"}}
```

```go
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"},
		Config: config.Config{Environment: map[string]string{"K": "v"}}}
```

Give the `observe_test.go` workspace fixture the same `RepositoryID: "r1"` that `ensureWorkspace`
carries. Nothing in this task reads it, but Task 5b re-keys the fake store's container bindings on
the repository ID, and a fixture that leaves the field empty would file every binding under the
empty string.

- [ ] **Step 7: Convert the fake store's own tests**

`internal/controller/fake/fake_test.go:17-25` before:

```go
func testWorkspace(id, session string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        "slabledger",
		Worktree:    "/w/" + id,
		SessionName: session,
		IsPrimary:   true,
	}
}
```

after:

```go
func testWorkspace(id, session string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        "slabledger",
		RepoRoot:    "/w/" + id,
		SessionName: session,
	}
}
```

`internal/controller/fake/fake_test.go:72-83` before:

```go
// TestFakeStoreWorkspacesOrdersBySlugThenWorktree mirrors the real store's
// ORDER BY w.slug, w.worktree (internal/state/store.go): the fake iterates a
// map, so without an explicit sort the order would be nondeterministic.
func TestFakeStoreWorkspacesOrdersBySlugThenWorktree(t *testing.T) {
	s := NewStore()
	register := func(id, slug, worktree string) {
		t.Helper()
		ws := resolve.Workspace{
			ID: id, Slug: slug, Worktree: worktree,
			SessionName: id, IsPrimary: true,
		}
```

after:

```go
// TestFakeStoreWorkspacesOrdersBySlugThenRepoRoot mirrors the real store's
// ORDER BY w.slug, w.repo_root (internal/state/store.go): the fake iterates a
// map, so without an explicit sort the order would be nondeterministic.
func TestFakeStoreWorkspacesOrdersBySlugThenRepoRoot(t *testing.T) {
	s := NewStore()
	register := func(id, slug, repoRoot string) {
		t.Helper()
		ws := resolve.Workspace{
			ID: id, Slug: slug, RepoRoot: repoRoot,
			SessionName: id,
		}
```

The three `register(...)` calls below it pass positional strings and need no change; the assertion
message at line 106 should read `ordered by (slug, repo root)` for consistency.

Add `RepositoryID: "r-" + id` to `testWorkspace` and `RepositoryID: id` to the `register` closure,
for the same reason: Task 5b keys the fake's bindings on the repository, and an empty repository ID
would collapse the three registrations onto one key.

- [ ] **Step 8: Write the failing concurrency test**

Create `internal/controller/lock_ordering_test.go`:

```go
package controller_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// overlapActuator counts how many StartContainer calls are in flight at
// once. A repository has one container, so the repository lock must hold
// that count at one; the sleep widens the window so an unserialized
// second start overlaps observably rather than by scheduling luck.
type overlapActuator struct {
	mu       sync.Mutex
	inFlight int
	peak     int
	starts   int
}

func (a *overlapActuator) StartContainer(context.Context, resolve.Workspace, config.Config) (controller.ContainerObservation, error) {
	a.mu.Lock()
	a.inFlight++
	a.starts++
	if a.inFlight > a.peak {
		a.peak = a.inFlight
	}
	a.mu.Unlock()

	time.Sleep(50 * time.Millisecond)

	a.mu.Lock()
	a.inFlight--
	a.mu.Unlock()
	return controller.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, nil
}

func (a *overlapActuator) ExecCommand(_ state.ContainerBinding, command, _ string, _ map[string]string) string {
	return command
}

func (a *overlapActuator) StopContainer(context.Context, string) error { return nil }

// repoWorkspace builds one session's workspace on the shared repository.
// Resolution produces only the default session in this slice, so the
// second session is constructed directly here: it is exactly the case
// the repository lock exists to serialize.
func repoWorkspace(id, session, sessionName string) resolve.Workspace {
	return resolve.Workspace{
		ID: id, RepositoryID: "r1", Slug: "slab", RepoRoot: "/w/slab",
		Session: session, SessionName: sessionName,
	}
}

func TestConcurrentOpensOnOneRepositorySerializeTheContainerStart(t *testing.T) {
	lockDir := t.TempDir()
	store := fake.NewStore()
	containers := &overlapActuator{}

	workspaces := []resolve.Workspace{
		repoWorkspace("w1", "", "slab"),
		repoWorkspace("w2", "review", "slab--review"),
	}
	for _, ws := range workspaces {
		if err := store.RegisterWorkspace(ws, "sha256:desired", ensureTime); err != nil {
			t.Fatalf("register %s: %v", ws.ID, err)
		}
		if _, err := store.AllocateSessionName(ws.ID, ensureTime); err != nil {
			t.Fatalf("allocate %s: %v", ws.ID, err)
		}
	}

	ready := make(chan struct{})
	errs := make([]error, len(workspaces))
	var wg sync.WaitGroup
	for i, ws := range workspaces {
		// Only the store, the lock directory, and the container actuator
		// are genuinely shared: the observers record their calls without
		// synchronization, so each goroutine gets its own.
		ctrl := &controller.Controller{
			Store: store,
			Sessions: &scriptedSessions{steps: []func(controller.SessionQuery) (controller.SessionObservation, error){
				liveStep(controller.LiveSession{
					Name: ws.SessionName, WorkspaceID: ws.ID,
					Slug: ws.Slug, Worktree: ws.RepoRoot,
				}),
			}},
			Containers: &fake.ContainerObserver{
				AppliesResult: true,
				DiscoverResult: &controller.ContainerObservation{
					Kind: "devcontainer", Health: state.HealthMissing,
				},
			},
			Clock:        &fake.Clock{Time: ensureTime},
			Actuator:     &fake.SessionActuator{},
			ContainerAct: containers,
		}
		d := controller.Desired{
			Workspace: ws,
			Config: config.Config{
				Version:      1,
				DevContainer: config.DevContainer{Enabled: "true"},
			},
			Digest: "sha256:desired",
		}
		wg.Add(1)
		go func(i int, ctrl *controller.Controller, d controller.Desired) {
			defer wg.Done()
			<-ready
			_, errs[i] = ctrl.Ensure(context.Background(), d,
				[]controller.WindowIntent{{Name: "shell"}}, lockDir, 10*time.Second)
		}(i, ctrl, d)
	}
	close(ready)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure(%s): %v", workspaces[i].ID, err)
		}
	}
	if containers.peak > 1 {
		t.Errorf("%d concurrent devcontainer ups; the repository lock must serialize them",
			containers.peak)
	}
	// Both sessions still start: the lock serializes the container phase,
	// it does not deduplicate it. One up per repository is the shared
	// binding's job, not the lock's.
	if containers.starts != 2 {
		t.Errorf("starts = %d, want 2", containers.starts)
	}
}
```

- [ ] **Step 9: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestConcurrentOpensOnOneRepositorySerializeTheContainerStart -v`
Expected: FAIL with `2 concurrent devcontainer ups; the repository lock must serialize them` — both Ensures lock on their per-session workspace IDs and overlap.

- [ ] **Step 10: Move the lock test onto the renamed field**

`internal/lock/lock_test.go`, in `TestAcquireTimesOutWithTypedError` (currently lines 41-44), and add `"strings"` to the import block:

```go
	var heldErr *ErrLockHeld
	if !errors.As(err, &heldErr) {
		t.Fatalf("err = %v, want *ErrLockHeld", err)
	}
	if heldErr.Key != "w1" {
		t.Errorf("Key = %q, want w1", heldErr.Key)
	}
	if !strings.Contains(heldErr.Error(), "repository or workspace w1") {
		t.Errorf("Error() = %q; it must name what kind of key is locked", heldErr.Error())
	}
```

- [ ] **Step 11: Run the lock test to verify it fails**

Run: `go test ./internal/lock/ -run TestAcquireTimesOutWithTypedError -v`
Expected: FAIL to build with `heldErr.Key undefined (type *ErrLockHeld has no field or method Key)`.

- [ ] **Step 12: Rename the lock key and document the ordering**

`internal/lock/lock.go` lines 1-5 and 17-70:

```go
// Package lock provides the filesystem locks that serialize projectmux's
// mutating commands (design §9). A command takes them before its final
// observation and holds them through external mutations and the
// resulting state commit. They are for local filesystems (the state
// directory), like SQLite itself.
//
// Two kinds of key are locked. Container work locks the repository ID,
// because every session on a repository shares one container; session
// work and the state commit lock the workspace ID (design §6.1). A
// command needing both — open, and stop --container — acquires the
// repository lock first and releases it last. The ordering is global and
// otherwise arbitrary; what it buys is that two commands on one
// repository can never each hold the lock the other is waiting for. It
// is written down here because the single-lock code this replaced had no
// ordering to inherit.
package lock
```

```go
// pollInterval is how often a blocked Acquire retries.
const pollInterval = 100 * time.Millisecond

// ErrLockHeld reports that another operation holds a lock past the
// acquisition timeout. Key is the repository or workspace ID that was
// locked, and the message says which kinds those are: the two are
// indistinguishable hex digests at a glance.
type ErrLockHeld struct{ Key string }

func (e *ErrLockHeld) Error() string {
	return fmt.Sprintf(
		"another projectmux operation holds the lock for repository or workspace %s", e.Key)
}

// Lock is a held lock. The lock file is never deleted: unlinking races a
// concurrent flock on the same path.
type Lock struct{ f *os.File }

// Acquire takes an exclusive lock for the key, polling non-blocking
// flock until the timeout. The fd is close-on-exec (os.OpenFile's
// default on Linux): children spawned while the lock is held must never
// inherit it — a detached tmux server holding a leaked lock fd forever
// is the failure class this package exists to prevent (design §2).
// TestChildDoesNotInheritTheLock pins that property.
func Acquire(ctx context.Context, dir, key string, timeout time.Duration) (*Lock, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the lock directory: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, key+".lock"),
		os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("opening the lock file: %w", err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &Lock{f: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("locking %s: %w", key, err)
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, &ErrLockHeld{Key: key}
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, fmt.Errorf("waiting for the lock on %s: %w", key, ctx.Err())
		case <-time.After(pollInterval):
		}
	}
}
```

- [ ] **Step 13: Run the lock package tests**

Run: `go test ./internal/lock/ -v`
Expected: PASS, including `TestAcquireTimesOutWithTypedError` and `TestChildDoesNotInheritTheLock`.

- [ ] **Step 14: Add the controller's lock helper**

Create `internal/controller/locking.go`:

```go
package controller

import (
	"context"
	"time"

	"github.com/gambtho/projectmux/internal/lock"
)

// lockPhases takes the workspace lock, and the repository lock ahead of
// it when the command has a container phase, in the order the lock
// package documents; the returned release unwinds them in reverse. An
// empty repositoryID means no container phase: a stop that leaves the
// container alone must not queue behind a sibling's devcontainer up.
func lockPhases(ctx context.Context, dir, repositoryID, workspaceID string, timeout time.Duration) (func(), error) {
	releaseRepository := func() {}
	if repositoryID != "" {
		l, err := lock.Acquire(ctx, dir, repositoryID, timeout)
		if err != nil {
			return nil, err
		}
		releaseRepository = func() { _ = l.Release() }
	}
	workspace, err := lock.Acquire(ctx, dir, workspaceID, timeout)
	if err != nil {
		releaseRepository()
		return nil, err
	}
	return func() {
		_ = workspace.Release()
		releaseRepository()
	}, nil
}
```

- [ ] **Step 15: Lock the repository and the workspace in Ensure**

`internal/controller/ensure.go`, replacing lines 81-85:

```go
	release, err := lockPhases(ctx, lockDir,
		d.Workspace.RepositoryID, d.Workspace.ID, lockTimeout)
	if err != nil {
		return EnsureResult{}, err
	}
	defer release()
```

Then drop the now-unused `"github.com/gambtho/projectmux/internal/lock"` import (line 10).

- [ ] **Step 16: Lock the repository in Stop only when the container is in play**

`internal/controller/stop.go`, replacing lines 30-34:

```go
	// Only a stop that touches the shared container needs the repository
	// lock, and it must hold it across the sibling check and the stop
	// that follows (design §6.1). A session-only stop takes the
	// workspace lock alone rather than waiting on a sibling's container
	// work.
	repositoryID := ""
	if stopContainer {
		repositoryID = d.Workspace.RepositoryID
	}
	release, err := lockPhases(ctx, lockDir, repositoryID, d.Workspace.ID, lockTimeout)
	if err != nil {
		return StopResult{}, err
	}
	defer release()
```

Then drop the `"github.com/gambtho/projectmux/internal/lock"` import (line 9).

- [ ] **Step 17: Lock the repository in StartWorkspaceContainer**

`internal/controller/autostart.go`, replacing lines 29-33:

```go
	// Autostart is the container phase, so the repository lock is the one
	// that matters; it still takes the workspace lock because the commit
	// below writes this session's record.
	release, err := lockPhases(ctx, lockDir,
		d.Workspace.RepositoryID, d.Workspace.ID, lockTimeout)
	if err != nil {
		return "", nil, err
	}
	defer release()
```

Then drop the `"github.com/gambtho/projectmux/internal/lock"` import (line 8).

- [ ] **Step 18: Run the controller tests**

Run: `go test ./internal/controller/... -v`
Expected: PASS, including `TestConcurrentOpensOnOneRepositorySerializeTheContainerStart` (`peak` stays 1, `starts` is 2) and the existing `TestEnsureRespectsTheWorkspaceLock`.

- [ ] **Step 19: Run the owned packages and the formatting gate**

Run: `go test ./internal/lock/... ./internal/controller/... && gofmt -l internal/lock internal/controller`
Expected: both packages PASS and `gofmt -l` prints nothing.

The gate is scoped to the packages this task owns rather than to `go test ./...`, because
the tree does not build between Task 1 and Task 5a — see "Task Ordering and Interactions"
above. Every package named on this line has been converted by this task or an earlier one,
so the gate is achievable as written; a full-suite run would fail on readers a later task
owns and say nothing about this one.

- [ ] **Step 20: Commit**

Run:
```
git commit -am "fix(lock): scope container locking to the repository

Container work locks the repository ID and session work the workspace ID,
with repository-before-workspace ordering documented in the lock package.
ErrLockHeld.WorkspaceID becomes Key, since the key is no longer always a
workspace.

The controller's own readers of the renamed workspace and record fields
move in the same commit: session rendering, the post-create identity
confirmation, SessionBelongsTo, the in-memory fake's ordering, and the
tests around them. They are mechanical, but the package does not compile
without them, and this is the first task that runs a controller test."
```

---

### Task 4: Key containers on the repository root

**Files:**
- Modify: `internal/container/adapter.go:48-77` (`Applies` line 56, `configPaths` lines 69-77)
- Modify: `internal/container/adapter.go:93-98` (the `devcontainer.local_folder` filter, line 95)
- Modify: `internal/container/adapter.go:145-153` (`devcontainer up --workspace-folder`, line 150, and the `--config` join, line 152)
- Modify: `internal/container/adapter_test.go:30-32,67,114` (the `adapterWorkspace` fixture and its two `ws.Worktree` readers)
- Modify: `internal/container/integration_test.go:114` (the docker-gated fixture; converted here only because the package's test binary will not compile without it)
- Test: `internal/container/adapter_test.go`

**Interfaces:**
- Consumes: `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}` (Task 1). Task 1 removes the `Worktree` field, and these four reads in `internal/container` are its last references, so the package does not build until Step 5 below.
- Produces: no new exported names. `Adapter.Applies`, `Adapter.DiscoverContainer` and `Adapter.StartContainer` keep their signatures and change which field they read — this is the change that turns N worktrees of one repository into one container.

- [ ] **Step 1: Convert the docker-gated integration fixture**

`internal/container/integration_test.go:114` builds a `resolve.Workspace` in the dropped shape. It
is guarded by a docker build tag and never runs in CI, but the file is still compiled by every
`go test ./internal/container/...` invocation, which makes it a prerequisite of this task's own
gate rather than of a later task:

```go
	ws := resolve.Workspace{ID: "it", RepositoryID: "it-repo", Slug: "it", RepoRoot: worktree}
```

The local variable keeps the name `worktree`: it is the path the fixture created with `git init`,
which is a main worktree and therefore a repository root. Renaming it is churn no gate can see.

- [ ] **Step 2: Point the existing adapter test fixture at the repository root**

`internal/container/adapter_test.go`, replacing `adapterWorkspace` (lines 30-32) and renaming the `writeDevcontainerJSON` parameter so it reads honestly:

```go
func adapterWorkspace(t *testing.T) resolve.Workspace {
	return resolve.Workspace{ID: "w1", RepositoryID: "r1", Slug: "proj", RepoRoot: t.TempDir()}
}
```

```go
func writeDevcontainerJSON(t *testing.T, repoRoot string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".devcontainer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devcontainer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}
```

Then change the two callers `writeDevcontainerJSON(t, ws.Worktree)` (lines 67 and 114) to `writeDevcontainerJSON(t, ws.RepoRoot)`.

- [ ] **Step 3: Write the failing test**

Append to `internal/container/adapter_test.go`:

```go
// recordArgv installs a script that appends its argv to log and writes
// stdout, so a test can compare the argv two calls produced.
func recordArgv(t *testing.T, which *string, log, stdout string) {
	t.Helper()
	fakeBinary(t, which, "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >>'"+log+"'\n"+
		"printf '%s' '"+stdout+"'\n")
}

func argvLines(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("reading the argv log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// TestContainersAreKeyedOnTheRepositoryRoot states the reported defect as
// a test: two sessions on one repository address one container, so every
// docker and devcontainer invocation they produce must be byte-identical
// and must name the repository root rather than any per-session path.
func TestContainersAreKeyedOnTheRepositoryRoot(t *testing.T) {
	a := &Adapter{}
	root := t.TempDir()
	writeDevcontainerJSON(t, root)
	sessions := []resolve.Workspace{
		{ID: "w1", RepositoryID: "r1", Slug: "proj", RepoRoot: root, SessionName: "proj"},
		{ID: "w2", RepositoryID: "r1", Slug: "proj", RepoRoot: root,
			Session: "review", SessionName: "proj--review"},
	}

	upLog := filepath.Join(t.TempDir(), "up.argv")
	recordArgv(t, &devcontainerBinary, upLog,
		`{"outcome":"success","containerId":"c9","remoteUser":"vscode","remoteWorkspaceFolder":"/workspaces/proj"}`)
	for _, ws := range sessions {
		if _, err := a.StartContainer(context.Background(), ws, enabledConfig()); err != nil {
			t.Fatalf("StartContainer(%s): %v", ws.ID, err)
		}
	}
	up := argvLines(t, upLog)
	if len(up) != 2 || up[0] != up[1] {
		t.Fatalf("devcontainer up argv = %q; both sessions must start one container", up)
	}
	if !strings.Contains(up[0], "--workspace-folder "+root) {
		t.Errorf("argv %q does not target the repository root %s", up[0], root)
	}

	psLog := filepath.Join(t.TempDir(), "ps.argv")
	recordArgv(t, &dockerBinary, psLog, "")
	for _, ws := range sessions {
		if _, err := a.DiscoverContainer(context.Background(), ws, autoConfig()); err != nil {
			t.Fatalf("DiscoverContainer(%s): %v", ws.ID, err)
		}
	}
	ps := argvLines(t, psLog)
	if len(ps) != 2 || ps[0] != ps[1] {
		t.Fatalf("docker ps argv = %q; both sessions must discover by one filter", ps)
	}
	if !strings.Contains(ps[0], "label=devcontainer.local_folder="+root) {
		t.Errorf("filter %q does not key on the repository root %s", ps[0], root)
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `go test ./internal/container/ -run TestContainersAreKeyedOnTheRepositoryRoot -v`
Expected: FAIL to build with `ws.Worktree undefined (type resolve.Workspace has no field or method Worktree)` at `adapter.go:56`, `adapter.go:95`, `adapter.go:150` and `adapter.go:152`.

- [ ] **Step 5: Key applicability on the repository root**

`internal/container/adapter.go`, line 56 and the `configPaths` helper (lines 69-77):

```go
	for _, p := range configPaths(ws.RepoRoot, cfg) {
```

```go
// configPaths lists the devcontainer configurations that make a
// repository containerized. They are resolved against the repository
// root, never a linked worktree: a worktree's untracked compose overrides
// are exactly what the old worktree-keyed lookup went missing.
func configPaths(repoRoot string, cfg config.Config) []string {
	if cfg.DevContainer.Config != nil && *cfg.DevContainer.Config != "" {
		return []string{filepath.Join(repoRoot, *cfg.DevContainer.Config)}
	}
	return []string{
		filepath.Join(repoRoot, ".devcontainer", "devcontainer.json"),
		filepath.Join(repoRoot, ".devcontainer.json"),
	}
}
```

- [ ] **Step 6: Key discovery on the repository root**

`internal/container/adapter.go`, replacing lines 93-98:

```go
	res, err := run.Run(ctx, run.Command{
		Argv: []string{dockerBinary, "ps", "-a",
			// The devcontainer CLI labels containers with the folder it
			// was given, which is now the repository root — so every
			// session on the repository discovers the same container.
			"--filter", "label=devcontainer.local_folder=" + ws.RepoRoot,
			"--format", "{{.ID}}\t{{.State}}"},
		Timeout: a.timeout(),
	})
```

- [ ] **Step 7: Start the container at the repository root**

`internal/container/adapter.go`, replacing lines 150-153:

```go
	argv := []string{devcontainerBinary, "up", "--workspace-folder", ws.RepoRoot}
	if cfg.DevContainer.Config != nil && *cfg.DevContainer.Config != "" {
		argv = append(argv, "--config", filepath.Join(ws.RepoRoot, *cfg.DevContainer.Config))
	}
```

- [ ] **Step 8: Run the container package tests**

Run: `go test ./internal/container/ -v`
Expected: PASS, including `TestContainersAreKeyedOnTheRepositoryRoot`, `TestAppliesMatrix`, `TestDiscoverShapes` and `TestStartContainerSuccessAndFailures`.

- [ ] **Step 9: Run the owned package and the formatting gate**

Run: `go test ./internal/container/... && gofmt -l internal/container`
Expected: the container package PASSes and `gofmt -l` prints nothing.

The gate is scoped to the packages this task owns rather than to `go test ./...`, because
the tree does not build between Task 1 and Task 5a — see "Task Ordering and Interactions"
above. Every package named on this line has been converted by this task or an earlier one,
so the gate is achievable as written; a full-suite run would fail on readers a later task
owns and say nothing about this one.

- [ ] **Step 10: Commit**

Run:
```
git commit -am "fix(container): key containers on the repository root

devcontainer up, the local_folder discovery filter, and devcontainer
configuration lookup all read RepoRoot, so N worktrees of one repository
share one container instead of demanding N."
```

---

### Task 5a: The tree builds again — convert the last three packages' source readers

This task and Task 5b were one task until the plan was reviewed for worker-sized units. They
are split at the `go build ./...` gate below, which is the only point between here and Task 6
where the tree is consistent: Task 5b's first step deletes `internal/controller/autostart_test.go`
and its implementation does not arrive until eleven steps later, so no cut inside 5b leaves a
committable tree.

`internal/cli` is the first package whose *test binary* links `internal/container`,
`internal/controller`, `internal/controller/fake`, `internal/doctor`, `internal/rebuild` and
`internal/tmux` together (`go list -deps -test ./internal/cli/...`). That is why the last readers
in `internal/doctor` and `internal/rebuild` convert here rather than in a task of their own: a
Go test binary only links when every package it reaches compiles. This task converts source
files only. No test file changes, and no behaviour changes except the one called out in Step 3,
where `internal/cli/autostart.go`'s primacy filter has no field left to read.

**Files:**
- Modify: `internal/doctor/sessions.go:31` (registered-path map value)
- Modify: `internal/rebuild/classify.go:151, 205` (identity-mismatch gate and its reason string)
- Modify: `internal/rebuild/apply.go:326-334` (`registeredFor` and its doc comment), `:349` (the conflict message's workspace-side argument)
- Modify: `internal/cli/config.go:136,138,163,166`, `internal/cli/open.go:86,88`, `internal/cli/attach.go:139,141`, `internal/cli/stop.go:121,123`, `internal/cli/status.go:178,180,257,259`, `internal/cli/list.go:117-118,134` (envelope construction; the JSON field names stay for Task 7)
- Modify: `internal/cli/autostart.go:11-190` (the primacy filter at `:97` and the record readers at `:107, 112, 135-143`; the function is rewritten wholesale in Task 5b, but it has to compile first)

**Interfaces:**
- Consumes (Task 1): `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}`.
- Consumes (Task 2): `state.Record` without `Worktree` or `IsPrimary`, carrying `RepositoryID` and `RepoRoot`, with `Container` a read-only projection of the repository's binding.
- Produces: no new exported names. The deliverable is `go build ./...` exiting 0 for the first time since Task 1.

- [ ] **Step 1: Convert the doctor orphan check**

`orphanedSessions` builds a workspace-ID-to-path map so `orphanItem` can stat the path and warn
when it has disappeared. Only the field read changes; the local variable and messages downstream
still speak of a worktree, which stays accurate — the repository root is one.

Before (line 29-32):

```go
	registered := make(map[string]string, len(records))
	for _, rec := range records {
		registered[rec.ID] = rec.Worktree
	}
```

After:

```go
	registered := make(map[string]string, len(records))
	for _, rec := range records {
		registered[rec.ID] = rec.RepoRoot
	}
```

`internal/doctor` has no other read of `Worktree` or `IsPrimary`: `internal/doctor/integration_test.go:44`
passes `controller.KeyWorktree`, which is the tmux option name `@dev_worktree` and does not change.

- [ ] **Step 2: Convert the rebuild classifier's identity gate**

`classify.go` compares a live session's tmux keys against the stored row. The `s.*` reads are
`controller.LiveSession` and keep their names; the `row.*` reads are `state.Record` and become
`RepoRoot`.

Before (line 151):

```go
		case row != nil && (row.Slug != s.Slug || row.Worktree != s.Worktree):
```

After:

```go
		case row != nil && (row.Slug != s.Slug || row.RepoRoot != s.Worktree):
```

Before (lines 198-206):

```go
// identityMismatchReason prints both identities side by side, because the
// disagreement is the whole finding.
func identityMismatchReason(s controller.LiveSession, row *state.Record) string {
	return fmt.Sprintf(
		"session %q carries slug %q and worktree %q, but workspace %s is recorded "+
			"as slug %q and worktree %q; that contradiction is evidence of "+
			"corruption or collision rather than a match, so nothing is written.",
		s.Name, s.Slug, s.Worktree, row.ID, row.Slug, row.Worktree)
}
```

After — the operator-facing wording is left byte-for-byte alone so the existing message assertions
keep passing and operators' saved notes keep matching; only the row-side argument moves:

```go
// identityMismatchReason prints both identities side by side, because the
// disagreement is the whole finding.
func identityMismatchReason(s controller.LiveSession, row *state.Record) string {
	return fmt.Sprintf(
		"session %q carries slug %q and worktree %q, but workspace %s is recorded "+
			"as slug %q and worktree %q; that contradiction is evidence of "+
			"corruption or collision rather than a match, so nothing is written.",
		s.Name, s.Slug, s.Worktree, row.ID, row.Slug, row.RepoRoot)
}
```

- [ ] **Step 3: Point the CLI's and rebuild's readers at the repository root**

`internal/cli` is the widest consumer of both renamed types, and its test binary links
`internal/container`, `internal/controller`, `internal/controller/fake`, `internal/doctor`,
`internal/rebuild` and `internal/tmux` as well (`go list -deps -test ./internal/cli/...`). Task 5b
is the first to gate `./internal/cli/...`, so every one of those readers has to compile before it
can run a single test. The conversions are mechanical and each moves only a right-hand side.

Two of the envelope fields have no source left. `worktree` keeps its name and now carries the
repository root, which is what the tmux option `@dev_worktree` has carried since Task 1, so the
value is still true to the key. `is_primary` becomes the constant `true`, which is also true: after
Task 1 every registered workspace *is* a repository. Both fields are deleted, and
`schema_version` bumped, by Task 7 — that is a user-visible envelope decision and it belongs in the
task that owns the envelope, not in a compile-driven conversion step. Until then the v1 envelope
keeps its documented shape and its documented meaning.

The same two-line pair appears at `internal/cli/config.go:136,138`, `internal/cli/open.go:86,88`,
`internal/cli/attach.go:139,141`, `internal/cli/stop.go:121,123` and
`internal/cli/status.go:178,180`. Before:

```go
			Worktree:    ws.Worktree,
			IsPrimary:   ws.IsPrimary,
```

After:

```go
			Worktree:    ws.RepoRoot,
			IsPrimary:   true,
```

`internal/cli/config.go:163,166` and `internal/cli/status.go:257,259` read the same two fields off
`env.Workspace`, which is the `workspaceInfo` DTO rather than a `resolve.Workspace`; they compile
unchanged and must be left for Task 7.

`internal/cli/list.go:117-118` reads a record instead of a workspace. Before:

```go
			Worktree:        rec.Worktree,
			IsPrimary:       rec.IsPrimary,
```

After:

```go
			Worktree:        rec.RepoRoot,
			IsPrimary:       true,
```

`internal/cli/list.go:134` compares a live session against that record. The `s.*` side is a
`controller.LiveSession` and keeps its name; only the record side moves. Before:

```go
			row.IdentityConflict = s.Slug != rec.Slug || s.Worktree != rec.Worktree
```

After:

```go
			row.IdentityConflict = s.Slug != rec.Slug || s.RepoRoot != rec.RepoRoot
```

`internal/cli/autostart.go` needs three changes to compile, and the first is a real behaviour
change rather than a rename: the primacy filter at line 97 has nothing left to test, because the
records the store now returns are one per repository. Delete it. Task 5b's Steps 12 and 13 replace this
whole loop with an iteration over `Repositories()`, so this edit is deliberately the smallest one
that compiles and keeps the current behaviour honest in the meantime. Before (lines 96-99):

```go
	for _, rec := range records {
		if !rec.IsPrimary {
			continue
		}
```

After:

```go
	for _, rec := range records {
```

Lines 107 and 112 stat the stored path and quote it in the failure reason. The operator-facing
wording stays: "worktree no longer exists" is still true of a repository root, and rewording it
would churn saved runbooks for no gain. Before:

```go
		if _, statErr := os.Stat(rec.Worktree); statErr != nil {
```

After:

```go
		if _, statErr := os.Stat(rec.RepoRoot); statErr != nil {
```

Before:

```go
				entry.Reason = "worktree no longer exists: " + rec.Worktree
```

After:

```go
				entry.Reason = "worktree no longer exists: " + rec.RepoRoot
```

Lines 135-143 rebuild a `resolve.Workspace` out of the record to hand to `controller.Desired`. The
repository ID has to be carried across, or the repository lock Task 3 added would key on the empty
string for every autostarted workspace. Before:

```go
		d := controller.Desired{
			Workspace: resolve.Workspace{
				ID:          rec.ID,
				Slug:        rec.Slug,
				Worktree:    rec.Worktree,
				SessionName: rec.ProposedSession,
				IsPrimary:   rec.IsPrimary,
			},
```

After:

```go
		d := controller.Desired{
			Workspace: resolve.Workspace{
				ID:           rec.ID,
				RepositoryID: rec.RepositoryID,
				Slug:         rec.Slug,
				RepoRoot:     rec.RepoRoot,
				SessionName:  rec.ProposedSession,
			},
```

Finally `internal/rebuild/apply.go:326-334`. `registeredFor` reports the resolver's identity for a
workspace, and its doc comment explains a divergence that can no longer happen. Before:

```go
// registeredFor reports the resolver's identity for ws, not the stored
// row's, by design. Slug and Worktree are provably equal to the row's —
// the identity gate above requires it — so only IsPrimary can diverge,
// and only if the worktree's primary-ness changed since registration. In
// that case this reports the resolver's current view, not the row's.
func registeredFor(ws resolve.Workspace, session string) *Registered {
	return &Registered{
		ID:        ws.ID,
		Slug:      ws.Slug,
		Worktree:  ws.Worktree,
		IsPrimary: ws.IsPrimary,
		Session:   session,
	}
}
```

After — the comment's premise is gone with the field, so it says what is now true:

```go
// registeredFor reports the resolver's identity for ws, not the stored
// row's, by design. Slug and the repository root are provably equal to
// the row's — the identity gate above requires it — so the two views can
// no longer diverge at all; the function stays as the single place the
// report is built, rather than being inlined at both call sites.
func registeredFor(ws resolve.Workspace, session string) *Registered {
	return &Registered{
		ID:        ws.ID,
		Slug:      ws.Slug,
		Worktree:  ws.RepoRoot,
		IsPrimary: true,
		Session:   session,
	}
}
```

`internal/rebuild/apply.go:349` closes the same conflict message. The three `s.*` reads on that line
and the one above it are `controller.LiveSession` and stay; only the last argument moves. Before:

```go
		s.WorkspaceID, s.Slug, s.Worktree,
		s.Worktree, ws.ID, ws.Slug, ws.Worktree)
```

After:

```go
		s.WorkspaceID, s.Slug, s.Worktree,
		s.Worktree, ws.ID, ws.Slug, ws.RepoRoot)
```

Run: `go build ./...`

Expected: exit 0 with no output. This is the point in the plan at which the whole tree compiles
again; it has not since Task 1 removed the fields. `go test ./...` still fails, because
`internal/doctor`'s own tests are not converted until Task 9.

- [ ] **Step 4: Confirm this task changed no behaviour the existing tests can see**

Run: `gofmt -l internal/cli internal/doctor internal/rebuild && go test ./internal/state/... ./internal/controller/... ./internal/container/...`

Expected: no files listed by `gofmt`, all three packages PASS.

These are the packages that were green at the end of Task 4 and must stay green: this task edits
none of their files, so a failure here is a genuine regression rather than an expected red.

`./internal/cli/...`, `./internal/doctor/...` and `./internal/rebuild/...` are deliberately *not*
run. Their test files still read the dropped fields, or still assert the pre-split autostart
behaviour, and they are converted in Task 5b and Task 9. `go build ./...` — which excludes
`_test.go` files — is this task's real gate, and it is the claim the rest of the plan depends on.

- [ ] **Step 5: Commit**

```
git commit -am "refactor: point the last field readers at the repository root

internal/cli is the first package whose test binary links container,
controller, doctor, rebuild and tmux together, so the last readers of
the fields Task 1 and Task 2 removed convert here rather than in tasks
of their own: the doctor orphan check, the rebuild classifier and
registeredFor, and the CLI envelope construction.

The one behaviour change is in internal/cli/autostart.go, whose primacy
filter has no field left to read. Iterating repositories properly is
Task 5b; this commit only gets the package compiling.

The envelopes keep their worktree and is_primary field names and their
schema_version 1 shape. worktree now carries the repository root, which
is what @dev_worktree has carried since Task 1, and is_primary is the
constant true, which is also true after the split. Task 7 owns removing
both.

go build ./... is green from here, for the first time since Task 1."
```

---

### Task 5b: Autostart iterates repositories

**Files:**
- Modify: `internal/controller/autostart.go:13-66` (replace `StartWorkspaceContainer` with the repository-scoped form)
- Modify: `internal/controller/observe.go:100,104-146` (`observeContainer` takes a binding, not a record)
- Modify: `internal/controller/interfaces.go:23-33` (`Store` gains `Repositories`; `RecordContainerObservation` takes a repository ID)
- Modify: `internal/controller/fake/fake.go:83-89,358-364` (record the repository ID, not the workspace ID)
- Modify: `internal/controller/fake/fake.go:91-99` (the `Store` struct and `NewStore`), `:101-123` (`RegisterWorkspace` writes both rows), `:155-189` (`RecordContainerObservation` and `recordContainerLocked` key on the repository), `:209-234` (`CommitReconciliation`'s container branch), `:243,253,264-291` (`copyRecord` becomes `copyRecordLocked` and projects the binding), plus the new `Repositories` reader
- Modify: `internal/cli/autostart.go:11-190` (rewritten over repositories; Task 5a converted its readers so the package compiles)
- Modify: `internal/controller/ensure_test.go:624, 692`, `internal/controller/stop_test.go:145, 188`, `internal/controller/observe_test.go:130, 226, 251`, `internal/controller/fake/fake_test.go:51, 54` (container observations re-keyed on the repository)
- Modify: `internal/cli/cli.go:50-51` (usage line)
- Modify: `docs/commands.md:344-363`
- Test: `internal/controller/autostart_test.go` (rewritten in full)
- Test: `internal/cli/autostart_test.go:16-161` (fixture and matrix rewritten)
- Test: `internal/cli/lifecycle_test.go:439-523` (the `is_primary` regression test, rewritten), `:90, 123, 126` (converted in Step 1)
- Test: `internal/cli/attach_test.go:27, 54, 76, 144`
- Test: `internal/cli/open_test.go:110-114`
- Test: `internal/cli/rebuild_test.go:100-105`
- Test: `internal/cli/rebuild_check_test.go:28-30, 150-152`
- Test: `internal/cli/doctor_test.go:285-287`
- Test: `internal/cli/list_test.go:15-22, 34, 39`
- Test: `internal/cli/status_test.go:70-72, 152, 157, 243`
- Test: `internal/cli/stop_test.go:108, 136`

**Interfaces:**
- Consumes (Task 1): `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}`.
- Consumes (Task 2): `state.Repository{ID, Slug, RepoRoot, RegisteredAt, UpdatedAt, Container *ContainerBinding}`; `Repositories() ([]state.Repository, error)` added to `controller.Store` (and so to `cli.stateStore`, which embeds it) and implemented by `*state.Store` and `*fake.Store`; `RecordContainerObservation(repositoryID string, obs state.ContainerObservation, now time.Time) error` — the first argument is now the repository ID, because `container_bindings` is keyed on `repository_id`; `fake.Store.RegisterWorkspace` upserts the repository row from `ws.RepositoryID/RepoRoot/Slug` alongside the workspace row.
- Consumes (Task 3): `lock.Acquire(ctx, dir, key string, timeout time.Duration)`; container phases lock on the repository ID.
- Consumes (Task 5a): every non-test file in the tree compiles; `go build ./...` exits 0.
- Produces: `controller.RepoDesired{Repository state.Repository; Config config.Config; Digest string}`; `func (c *Controller) StartRepositoryContainer(ctx context.Context, d RepoDesired, lockDir string, lockTimeout time.Duration) (ContainerStartOutcome, *ContainerObservation, error)` — no session and no `workspaces` row take part; `func (c *Controller) observeContainer(ctx context.Context, d Desired, binding *state.ContainerBinding) ContainerSnapshot`; the autostart JSON envelope's array is `repositories`, each entry keyed by repository ID and slug.

Autostart's operation record is deliberately dropped: `last_operations` stays keyed on `workspace_id` (spec §5.2 — "an operation is performed by a session, not by a repository"), and autostart is performed by no session. It records the container binding on the repository and nothing else. The tests below assert the binding in place of the old `autostart` operation.

**This task's tree is red from Step 2 to Step 13.** Step 2 rewrites `internal/controller/autostart_test.go` in full against a `StartRepositoryContainer` that does not exist yet, and Step 8 removes the `StartWorkspaceContainer` that `internal/cli/autostart.go` still calls, which takes `go build ./...` back down until Step 13 rewrites the loop body. That window is why this task is not split further. Do not commit inside it.

- [ ] **Step 1: Convert the CLI test readers**

A sweep of the tree found eight CLI test files that read `Workspace.Worktree`, `Record.IsPrimary`,
or build a `resolve.Workspace` literal in the dropped shape, and no earlier task's Files block
names any of them. They land here because this task is the first to gate `./internal/cli/...`, and
a Go test binary only runs when *every* test file in the package compiles — one unconverted
literal in `attach_test.go` would take out this task's own autostart tests along with the rest of
the package.

Most of them are `controller.LiveSession` literals, where the *field* stays `Worktree` — it mirrors
the `@dev_worktree` user option — and only the right-hand side moves to `RepoRoot`. Make these four
edits in `internal/cli/attach_test.go` (lines 27, 54, 76 and 144; line 127's literal
`Worktree: "/somewhere/else"` reads nothing and stays):

```go
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.RepoRoot,
	}
```

`internal/cli/open_test.go:110-114` is the same change inside a helper:

```go
func ownLive(ws resolve.Workspace, name string) controller.LiveSession {
	return controller.LiveSession{
		Name: name, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.RepoRoot,
	}
}
```

`internal/cli/rebuild_test.go:100-105`:

```go
	installLiveSessions(t, []controller.LiveSession{{
		Name:        "new-name",
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.RepoRoot,
	}}, nil)
```

`internal/cli/status_test.go:70-72`:

```go
	live := controller.LiveSession{
		Name: actual, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.RepoRoot,
	}
```

The remaining sites are `resolve.Workspace` literals, which do change field names and must now
carry a repository ID — registration writes a repository row keyed on it.
`internal/cli/rebuild_check_test.go:28-30` before:

```go
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-1", Slug: "slab", Worktree: "/w/slab", SessionName: "slab", IsPrimary: true,
	}, "sha256:abc", time.Now()); err != nil {
```

after:

```go
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-1", RepositoryID: "repo-1", Slug: "slab", RepoRoot: "/w/slab", SessionName: "slab",
	}, "sha256:abc", time.Now()); err != nil {
```

`internal/cli/rebuild_check_test.go:150-152` is the same shape for the other workspace:

```go
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-2", RepositoryID: "repo-2", Slug: "other", RepoRoot: "/w/other", SessionName: "other",
	}, "sha256:def", time.Now()); err != nil {
```

`internal/cli/doctor_test.go:285-287` repeats the `id-1` literal and takes the same replacement:

```go
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-1", RepositoryID: "repo-1", Slug: "slab", RepoRoot: "/w/slab", SessionName: "slab",
	}, "sha256:abc", time.Now()); err != nil {
```

`internal/cli/list_test.go:15-22` is the fixture the whole file registers through, and because
`RecordContainerObservation` now takes a repository ID the two calls that bind `"w1"` have to name
the repository instead. Give the helper a derived repository ID so the two stay in step:

```go
func listWorkspace(id, slug string) resolve.Workspace {
	return resolve.Workspace{
		ID:           id,
		RepositoryID: "r-" + id,
		Slug:         slug,
		RepoRoot:     "/w/" + slug,
		SessionName:  slug,
	}
}
```

Then in `seededListStore`, change both `s.RecordContainerObservation("w1", ...)` calls
(`list_test.go:34` and `:39`) to `s.RecordContainerObservation("r-w1", ...)`. The binding belongs to
the repository, and the list rows still read it through the record's projection, which is exactly
the sharing this plan introduces.

`internal/cli/status_test.go` binds through a resolved workspace rather than a literal, so its three
`RecordContainerObservation(ws.ID, ...)` calls (`:152`, `:157`, `:243`) become
`RecordContainerObservation(ws.RepositoryID, ...)`.

`internal/cli/lifecycle_test.go` has three reads outside the block Step 15 rewrites. Line 90 asserts
the tmux-side value, so the comparison keeps `live[0].Worktree` on the left and moves only the right:

```go
	if len(live) != 1 || live[0].WorkspaceID != ws.ID || live[0].Worktree != ws.RepoRoot {
		t.Fatalf("live = %+v", live)
	}
```

Lines 123 and 126 build a pre-projectmux session by hand; both feed the repository root in, the
second through the unchanged `@dev_worktree` option:

```go
		{"new-session", "-d", "-s", "bash-era", "-c", ws.RepoRoot},
		{"set-option", "-t", "bash-era", controller.KeyWorkspaceID, ws.ID},
		{"set-option", "-t", "bash-era", controller.KeySlug, ws.Slug},
		{"set-option", "-t", "bash-era", controller.KeyWorktree, ws.RepoRoot},
```

- [ ] **Step 2: Write the failing test — rewrite `internal/controller/autostart_test.go` in full**

```go
package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/state"
)

// repoRig wires the container phase alone. It deliberately does not use
// ensureRig: autostart no longer reads a workspace row, and a rig that
// registered one would hide a regression into reading it again.
type repoRig struct {
	store     *fake.Store
	sessions  *scriptedSessions
	actuatorC *fake.ContainerActuator
	ctrl      *controller.Controller
	lockDir   string
}

func newRepoRig(t *testing.T) *repoRig {
	t.Helper()
	r := &repoRig{
		store:    fake.NewStore(),
		sessions: &scriptedSessions{},
		actuatorC: &fake.ContainerActuator{
			StartResult: controller.ContainerObservation{
				Kind: "devcontainer", ContainerID: "cid-1",
				ContainerUser: "vscode", Workdir: "/workspaces/slab",
				Health: state.HealthPresent,
			},
		},
		lockDir: t.TempDir(),
	}
	r.ctrl = &controller.Controller{
		Store:        r.store,
		Sessions:     r.sessions,
		Containers:   &fake.ContainerObserver{AppliesResult: true},
		Clock:        &fake.Clock{Time: ensureTime},
		ContainerAct: r.actuatorC,
	}
	if err := r.store.RegisterWorkspace(ensureWorkspace(), "sha256:x", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	return r
}

func (r *repoRig) start(t *testing.T, d controller.RepoDesired) (controller.ContainerStartOutcome, *controller.ContainerObservation, error) {
	t.Helper()
	return r.ctrl.StartRepositoryContainer(context.Background(), d, r.lockDir, time.Second)
}

func repoDesired() controller.RepoDesired {
	return controller.RepoDesired{
		Repository: state.Repository{ID: "r1", Slug: "slab", RepoRoot: "/w/slab"},
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: "true"},
			Environment:  map[string]string{"FOO": "bar"},
		},
		Digest: "sha256:desired",
	}
}

func repoBinding(t *testing.T, s *fake.Store, id string) *state.ContainerBinding {
	t.Helper()
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, repo := range repos {
		if repo.ID == id {
			return repo.Container
		}
	}
	t.Fatalf("no repository %s in %+v", id, repos)
	return nil
}

func TestStartRepositoryContainerStarts(t *testing.T) {
	r := newRepoRig(t)
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}

	outcome, obs, err := r.start(t, repoDesired())
	if err != nil {
		t.Fatalf("StartRepositoryContainer: %v", err)
	}
	if outcome != controller.ContainerStarted || obs == nil || obs.ContainerID != "cid-1" {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 1 || r.actuatorC.Started[0] != "r1" {
		t.Errorf("Started = %v, want one start keyed on the repository", r.actuatorC.Started)
	}
	if b := repoBinding(t, r.store, "r1"); b == nil || b.ContainerID != "cid-1" {
		t.Errorf("binding = %+v, want cid-1 recorded on the repository", b)
	}
	// The whole pass is container-only: tmux must never be consulted.
	if len(r.sessions.queries) != 0 {
		t.Errorf("session observer was consulted: %v", r.sessions.queries)
	}
}

func TestStartRepositoryContainerAlreadyRunning(t *testing.T) {
	r := newRepoRig(t)
	if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		ProbeResult: controller.ContainerObservation{
			Health: state.HealthPresent, Kind: "devcontainer", ContainerID: "cid-1",
			ContainerUser: "vscode", Workdir: "/workspaces/slab",
		},
	}

	outcome, obs, err := r.start(t, repoDesired())
	if err != nil {
		t.Fatalf("StartRepositoryContainer: %v", err)
	}
	if outcome != controller.ContainerAlreadyRunning || obs == nil {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 0 {
		t.Errorf("an already-running container was started again: %v", r.actuatorC.Started)
	}
}

func TestStartRepositoryContainerNoneApplies(t *testing.T) {
	r := newRepoRig(t)
	r.ctrl.Containers = &fake.ContainerObserver{AppliesResult: false}
	d := repoDesired()
	d.Config.DevContainer.Enabled = "auto"

	outcome, obs, err := r.start(t, d)
	if err != nil {
		t.Fatalf("StartRepositoryContainer: %v", err)
	}
	if outcome != controller.ContainerNoneApplies || obs != nil {
		t.Errorf("outcome = %v, obs = %+v", outcome, obs)
	}
	if len(r.actuatorC.Started) != 0 {
		t.Errorf("Started = %v, want none", r.actuatorC.Started)
	}
}

func TestStartRepositoryContainerUnobservableFails(t *testing.T) {
	r := newRepoRig(t)
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		DiscoverErr:   errors.New("docker down"),
	}

	if _, _, err := r.start(t, repoDesired()); err == nil {
		t.Fatal("an unobservable container start was swallowed")
	}
	if len(r.actuatorC.Started) != 0 {
		t.Error("uncertainty reached the container actuator")
	}
	if b := repoBinding(t, r.store, "r1"); b != nil {
		t.Errorf("binding = %+v, want none recorded on a failed observation", b)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestStartRepositoryContainer -v`
Expected: FAIL to build with `undefined: controller.RepoDesired` and `r.ctrl.StartRepositoryContainer undefined (type *controller.Controller has no field or method StartRepositoryContainer)`.

- [ ] **Step 4: Record the repository ID in the container fakes**

Replace the two `ws.ID` appends in `internal/controller/fake/fake.go` (lines 84 and 359).

```go
func (o *ContainerObserver) DiscoverContainer(_ context.Context, ws resolve.Workspace, _ config.Config) (*controller.ContainerObservation, error) {
	// Containers are per repository, so the repository ID is the honest
	// key to record here: the container phase never carries a session.
	o.Discovered = append(o.Discovered, ws.RepositoryID)
	if o.DiscoverErr != nil {
		return nil, o.DiscoverErr
	}
	return o.DiscoverResult, nil
}
```

```go
func (a *ContainerActuator) StartContainer(_ context.Context, ws resolve.Workspace, _ config.Config) (controller.ContainerObservation, error) {
	a.Started = append(a.Started, ws.RepositoryID)
	if a.StartErr != nil {
		return controller.ContainerObservation{}, a.StartErr
	}
	return a.StartResult, nil
}
```

- [ ] **Step 5: Give the fake store repositories and move its binding onto them**

Steps 2 and 13 call `s.Repositories()` on `*fake.Store` and read `repo.Container`, and `fake` today
exposes only `Workspace` and `Workspaces` (`internal/controller/fake/fake.go:236-262`) with the
binding living on the record. The fake is a non-test file standing in for the SQLite store across
the controller, CLI, doctor, and rebuild tests, so it has to mirror the split the real store just
grew: a repository row per project, the binding keyed on the repository, and the record's
`Container` a read-only projection of it.

First widen the interface the fake satisfies. In `internal/controller/interfaces.go:25-33`, add
`Repositories` to `Store` and rename `RecordContainerObservation`'s first parameter, which is now a
repository ID rather than a workspace ID:

```go
// Store is the slice of the state store the controller uses. *state.Store
// satisfies it; fakes mirror its semantics for tests.
//
// RecordContainerObservation takes a repository ID because a container
// belongs to a repository and is shared by every session on it, while the
// operation and reconciliation calls stay keyed on the workspace: an
// operation is performed by a session (spec §5.2).
type Store interface {
	RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error
	AllocateSessionName(workspaceID string, now time.Time) (string, error)
	AdoptSessionName(workspaceID, name string, now time.Time) error
	RecordContainerObservation(repositoryID string, obs state.ContainerObservation, now time.Time) error
	RecordOperation(workspaceID string, op state.Operation, now time.Time) error
	CommitReconciliation(workspaceID string, r state.ReconciliationResult, now time.Time) error
	Workspace(id string) (state.Record, error)
	Workspaces() ([]state.Record, error)
	Repositories() ([]state.Repository, error)
}
```

`cli.stateStore` embeds `controller.Store`, and `guardedStore` in `internal/cli/wiring_test.go:24-31`
embeds `*fake.Store`, so both pick the new method up without an edit of their own.

Now the fake. Replace the struct and constructor at `internal/controller/fake/fake.go:91-99`:

```go
// Store is an in-memory controller.Store. Repositories and container
// bindings live in maps of their own rather than on the record, mirroring
// the repositories and repository-keyed container_bindings tables: every
// session on a repository must read back the one binding its siblings
// wrote, which is what lets a shared container be started once.
type Store struct {
	mu           sync.Mutex
	records      map[string]*state.Record
	repositories map[string]*state.Repository
	containers   map[string]*state.ContainerBinding
}

func NewStore() *Store {
	return &Store{
		records:      map[string]*state.Record{},
		repositories: map[string]*state.Repository{},
		containers:   map[string]*state.ContainerBinding{},
	}
}
```

Replace `RegisterWorkspace` (`:101-123`) so it writes both rows. Task 3 already moved its field
reads onto `RepositoryID` and `RepoRoot` — that was the minimum that compiled. What changes here is
what it *writes*: a repository row alongside the session row, which is what makes `Repositories()`
below return anything:

```go
func (s *Store) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upsertRepositoryLocked(ws, now)
	digest := desiredDigest
	if rec, ok := s.records[ws.ID]; ok {
		rec.RepositoryID = ws.RepositoryID
		rec.Slug = ws.Slug
		rec.RepoRoot = ws.RepoRoot
		rec.Session = ws.Session
		rec.ProposedSession = ws.SessionName
		rec.DesiredDigest = &digest
		rec.UpdatedAt = now
		return nil
	}
	s.records[ws.ID] = &state.Record{
		ID:              ws.ID,
		RepositoryID:    ws.RepositoryID,
		Slug:            ws.Slug,
		RepoRoot:        ws.RepoRoot,
		Session:         ws.Session,
		ProposedSession: ws.SessionName,
		DesiredDigest:   &digest,
		RegisteredAt:    now,
		UpdatedAt:       now,
	}
	return nil
}

// upsertRepositoryLocked mirrors the real store's two-statement
// registration: the repository row is written first and the session row
// references it. Registering a second session on a repository refreshes
// the repository's mutable columns and deliberately leaves its binding
// alone — a sibling opening a session must not disturb a running
// container.
func (s *Store) upsertRepositoryLocked(ws resolve.Workspace, now time.Time) {
	if repo, ok := s.repositories[ws.RepositoryID]; ok {
		repo.Slug = ws.Slug
		repo.RepoRoot = ws.RepoRoot
		repo.UpdatedAt = now
		return
	}
	s.repositories[ws.RepositoryID] = &state.Repository{
		ID:           ws.RepositoryID,
		Slug:         ws.Slug,
		RepoRoot:     ws.RepoRoot,
		RegisteredAt: now,
		UpdatedAt:    now,
	}
}
```

Replace `RecordContainerObservation` and `recordContainerLocked` (`:155-189`) so both key on the
repository. Tri-state retention is unchanged, only relocated: a degraded observation updates the
repository's binding rather than dropping it:

```go
func (s *Store) RecordContainerObservation(repositoryID string, obs state.ContainerObservation, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recordContainerLocked(repositoryID, obs, now)
}

func (s *Store) recordContainerLocked(repositoryID string, obs state.ContainerObservation, now time.Time) error {
	if _, ok := s.repositories[repositoryID]; !ok {
		return fmt.Errorf("repository %s: %w", repositoryID, state.ErrNotFound)
	}
	switch obs.Health {
	case state.HealthPresent:
		if obs.ContainerID == "" {
			return fmt.Errorf("a present container observation must carry a container ID")
		}
		s.containers[repositoryID] = &state.ContainerBinding{
			Kind:          obs.Kind,
			ContainerID:   obs.ContainerID,
			ContainerUser: obs.ContainerUser,
			Workdir:       obs.Workdir,
			Health:        obs.Health,
			ObservedAt:    now,
		}
	case state.HealthMissing, state.HealthUnknown:
		if b, ok := s.containers[repositoryID]; ok {
			b.Health = obs.Health
			b.ObservedAt = now
		}
	default:
		return fmt.Errorf("invalid container health %q", obs.Health)
	}
	return nil
}
```

`CommitReconciliation` (`:209-234`) still takes a workspace ID, so its container branch has to look
the repository up on the record it already fetched. Replace the branch that reads
`if r.Container != nil {`:

```go
	if r.Container != nil {
		// The observation is recorded against the repository the session
		// belongs to, not the session, so a sibling reads the same binding.
		if err := s.recordContainerLocked(rec.RepositoryID, *r.Container, now); err != nil {
			return err
		}
	}
```

Turn `copyRecord` (`:264-291`) into a method so it can attach the shared binding, which is what the
real store's `LEFT JOIN` produces, and update its two call sites at `:243` and `:253` to
`s.copyRecordLocked(rec)`:

```go
// copyRecordLocked deep-copies a record and attaches the repository's
// shared container binding. A session therefore sees whatever container
// its repository is bound to, including one a sibling session started.
func (s *Store) copyRecordLocked(rec *state.Record) state.Record {
	out := *rec
	if rec.ActualSession != nil {
		v := *rec.ActualSession
		out.ActualSession = &v
	}
	if rec.DesiredDigest != nil {
		v := *rec.DesiredDigest
		out.DesiredDigest = &v
	}
	if rec.AppliedDigest != nil {
		v := *rec.AppliedDigest
		out.AppliedDigest = &v
	}
	out.Container = nil
	if b, ok := s.containers[rec.RepositoryID]; ok {
		c := *b
		out.Container = &c
	}
	if rec.LastOperation != nil {
		o := *rec.LastOperation
		out.LastOperation = &o
		if rec.LastOperation.ExitStatus != nil {
			e := *rec.LastOperation.ExitStatus
			out.LastOperation.ExitStatus = &e
		}
	}
	return out
}
```

Finally add `Repositories` next to `Workspaces`, matching the real store's ordering
(`ORDER BY r.slug, r.repo_root`, Task 2 Step 11) and the fake's existing copy-on-read discipline —
callers get values, never the pointers the map holds:

```go
// Repositories returns every registered repository ordered by slug, then
// repository root, mirroring the real store's ORDER BY
// (internal/state/store.go). Container is the repository's binding, copied
// out so a caller cannot mutate stored state through the result.
func (s *Store) Repositories() ([]state.Repository, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]state.Repository, 0, len(s.repositories))
	for _, repo := range s.repositories {
		copied := *repo
		copied.Container = nil
		if b, ok := s.containers[repo.ID]; ok {
			c := *b
			copied.Container = &c
		}
		out = append(out, copied)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		return out[i].RepoRoot < out[j].RepoRoot
	})
	return out, nil
}
```

- [ ] **Step 6: Re-key every container-observation call site on the repository**

Step 5 changed what the fake's first argument *means* without changing its type, so nothing the
compiler can see moves. Every existing caller still passes a workspace ID, which now writes a
binding under a key no repository will ever read back, and the symptom is a passing compile with
tests that see an empty `Container` projection. Each site below takes the repository ID of the
workspace it already has in hand.

`internal/controller/ensure_test.go:624` and `:692` register through `ensureWorkspace()`, whose
`RepositoryID` is `"r1"` (Task 3, Step 1). Before:

```go
		if err := r.store.RecordContainerObservation("w1", state.ContainerObservation{
```

After:

```go
		if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
```

`internal/controller/stop_test.go:145` and `:188` register through the same fixture and take the
same edit, at one less level of indentation:

```go
	if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
```

`internal/controller/observe_test.go:130`, `:226` and `:251` use the package's own workspace
fixture, which Task 3's Step 6 gave a `RepositoryID`. Pass that same value rather than `"w1"`:

```go
	if err := d.store.RecordContainerObservation("r1", state.ContainerObservation{
```

`internal/controller/fake/fake_test.go:51` and `:54` build their workspace with
`testWorkspace(id, session)`, whose `RepositoryID` is `"r-" + id` (Task 3, Step 7). The two calls
register `"w1"`, so both become:

```go
	if err := s.RecordContainerObservation("r-w1", obs, testTime); err != nil {
```

`internal/controller/autostart_test.go:53` needs no edit: Step 2 of this task rewrote that file in
full, and the rewritten fixtures already key on the repository.

`internal/cli/wiring_test.go:44` declares the method on `guardedStore` with unnamed parameters, so
the widened interface does not change it. Its message string `"RecordContainerObservation"` is the
method name and stays.

`internal/cli/stop_test.go:108` and `:136` hold a resolved workspace. Before:

```go
	if err := s.RecordContainerObservation(ws.ID, state.ContainerObservation{
```

After:

```go
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
```

`internal/cli/list_test.go:34,39` and `internal/cli/status_test.go:152,157,243` are converted by
Step 1 above, which owns those two files.

`internal/doctor/doctor_test.go:505` calls the *real* store, whose key changed in Task 2. It is
converted in Task 9, which is the first task to gate `./internal/doctor/...`.
`internal/state/store_test.go` calls the real store too, and Task 2 owns it: its
`RecordContainerObservation("w1", ...)` calls (`:215`, `:238`, `:243`, `:265`, `:268`, `:272`,
`:291`, `:302`, `:307`) are part of the Step 2 rewrite there, where `"w1"` becomes the registered
repository ID and the `"absent"` case at `:302` becomes an unregistered *repository*.
- [ ] **Step 7: Narrow `observeContainer` to the binding it actually reads**

In `internal/controller/observe.go`, change the parameter and the one stored-binding branch, then the single call site at line 100.

```go
func (c *Controller) observeContainer(ctx context.Context, d Desired, binding *state.ContainerBinding) ContainerSnapshot {
```

```go
	if binding != nil {
		obs, err := c.Containers.ProbeContainer(ctx, *binding)
		if err != nil {
			// Design §9: a failed probe yields unknown, never loss.
			return ContainerSnapshot{
				Observed: &ContainerObservation{Health: state.HealthUnknown},
				Err:      err,
			}
		}
		return ContainerSnapshot{Observed: &obs}
	}
```

```go
	// A container belongs to a repository, so the observation needs the
	// binding and nothing else about the session that asked for it.
	var binding *state.ContainerBinding
	if snap.Stored != nil {
		binding = snap.Stored.Container
	}
	snap.Container = c.observeContainer(ctx, d, binding)
	return snap, nil
```

- [ ] **Step 8: Replace `StartWorkspaceContainer` with `StartRepositoryContainer`**

Replace `internal/controller/autostart.go:22-66` (the doc comment and the function) with the following; the `errors` import is no longer used and comes out.

```go
// RepoDesired is the container phase's input. A container belongs to a
// repository, so starting one needs no session and reads no workspace
// row (spec §6.3).
type RepoDesired struct {
	Repository state.Repository
	Config     config.Config
	Digest     string
}

// containerIdentity renders the repository in the form the container
// adapters accept. Only the repository fields are populated: the adapters
// key on RepoRoot, and inventing a workspace ID here would invent the
// session autostart deliberately does not have.
func (d RepoDesired) containerIdentity() resolve.Workspace {
	return resolve.Workspace{
		RepositoryID: d.Repository.ID,
		Slug:         d.Repository.Slug,
		RepoRoot:     d.Repository.RepoRoot,
	}
}

// StartRepositoryContainer is autostart's engine: the container phase
// alone, under the repository lock. One row per repository is what keeps
// a shared container from being started once per session — the guarantee
// the dropped is_primary flag used to carry by filtering (spec §6.3).
// tmux is deliberately never consulted: at boot there is no tmux server,
// and going through Observe/BuildPlan would let the global
// session-unknown refusal block every container start (spec §3).
func (c *Controller) StartRepositoryContainer(ctx context.Context, d RepoDesired, lockDir string, lockTimeout time.Duration) (ContainerStartOutcome, *ContainerObservation, error) {
	lk, err := lock.Acquire(ctx, lockDir, d.Repository.ID, lockTimeout)
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = lk.Release() }()

	desired := Desired{Workspace: d.containerIdentity(), Config: d.Config, Digest: d.Digest}
	snap := Snapshot{Desired: desired}
	snap.Container = c.observeContainer(ctx, desired, d.Repository.Container)
	action := containerAction(snap)
	if action == ContainerActionProbeFirst {
		// One retry of the observation that came back unknown: a
		// container is never started on uncertainty, and a docker socket
		// that is still coming up at boot is worth a second look.
		snap.Container = c.observeContainer(ctx, desired, d.Repository.Container)
		if snap.Container.Err != nil {
			return "", nil, fmt.Errorf("re-observing the container: %w", snap.Container.Err)
		}
		if action = containerAction(snap); action == ContainerActionProbeFirst {
			return "", nil, fmt.Errorf(
				"the container for %s could not be observed", d.Repository.Slug)
		}
	}

	switch action {
	case ContainerActionNone:
		obs := snap.Container.Observed
		if obs == nil || obs.Health != state.HealthPresent {
			return ContainerNoneApplies, nil, nil
		}
		if err := c.recordBinding(d.Repository.ID, obs); err != nil {
			return "", nil, err
		}
		return ContainerAlreadyRunning, obs, nil
	case ContainerActionStart, ContainerActionAcquire:
		if c.ContainerAct == nil {
			return "", nil, ErrContainerActionUnsupported
		}
		obs, err := c.ContainerAct.StartContainer(ctx, desired.Workspace, d.Config)
		if err != nil {
			return "", nil, fmt.Errorf("starting the container: %w", err)
		}
		if err := c.recordBinding(d.Repository.ID, &obs); err != nil {
			return "", nil, err
		}
		// Acquire is an idempotent up onto a container that was already
		// running, so only a real start may be reported as one.
		if action == ContainerActionStart {
			return ContainerStarted, &obs, nil
		}
		return ContainerAlreadyRunning, &obs, nil
	}
	return "", nil, fmt.Errorf("unexpected container action %q", action)
}

// recordBinding persists the observation against the repository, which is
// what container_bindings is keyed on (spec §5.2). No operation is
// recorded: last_operations belongs to a session, and autostart is
// performed by none.
func (c *Controller) recordBinding(repositoryID string, obs *ContainerObservation) error {
	if err := c.Store.RecordContainerObservation(
		repositoryID, *toStateObservation(obs), c.Clock.Now()); err != nil {
		return fmt.Errorf("recording the container binding: %w", err)
	}
	return nil
}
```

The import block becomes:

```go
import (
	"context"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)
```

- [ ] **Step 9: Run the controller tests to verify they pass**

Run: `go test ./internal/controller/... -v`
Expected: PASS, including the four `TestStartRepositoryContainer*` cases.

- [ ] **Step 10: Write the failing CLI test — rewrite the autostart fixture and matrix**

Replace `internal/cli/autostart_test.go:16-161`. The old "secondary" row (a non-primary worktree) becomes a second *session* on the eligible repository: the successor to the case `is_primary` used to filter.

```go
// autostartFixture builds a config root, a fake store with a mix of
// repositories, and container seams. Repositories: "eligible" (autostart
// on, container applies, and carrying two sessions), "disabled"
// (autostart off), "gone" (autostart on, repository root deleted).
func autostartFixture(t *testing.T) (*fake.Store, *fake.ContainerActuator) {
	t.Helper()
	base := t.TempDir()
	configRoot := filepath.Join(base, "config")
	if err := os.MkdirAll(filepath.Join(configRoot, "workspaces"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(configRoot, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("defaults.yaml", "version: 1\n")
	write("workspaces/eligible.yaml",
		"version: 1\nautostart: true\ndevcontainer:\n  enabled: true\n")
	write("workspaces/disabled.yaml", "version: 1\n")
	write("workspaces/gone.yaml",
		"version: 1\nautostart: true\ndevcontainer:\n  enabled: true\n")
	t.Setenv("PROJECTMUX_CONFIG_ROOT", configRoot)
	t.Setenv("PROJECTMUX_STATE_ROOT", filepath.Join(base, "state"))

	mkRepo := func(slug string) string {
		t.Helper()
		dir := filepath.Join(base, "repos", slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		return dir
	}

	s := fake.NewStore()
	register := func(id, repoID, slug, repoRoot, session, sessionName string) {
		t.Helper()
		ws := resolve.Workspace{
			ID: id, RepositoryID: repoID, Slug: slug, RepoRoot: repoRoot,
			Session: session, SessionName: sessionName,
		}
		if err := s.RegisterWorkspace(ws, "sha256:"+id, cliTestTime); err != nil {
			t.Fatalf("register %s: %v", sessionName, err)
		}
	}
	eligible := mkRepo("eligible")
	register("w-eligible", "r-eligible", "eligible", eligible, "", "eligible")
	// A second session on the same repository. Autostart must still start
	// that repository's container exactly once: the guarantee is_primary
	// used to provide by filtering, now provided by the row count.
	register("w-eligible-2", "r-eligible", "eligible", eligible, "feature-a", "eligible--feature-a")
	register("w-disabled", "r-disabled", "disabled", mkRepo("disabled"), "", "disabled")
	register("w-gone", "r-gone", "gone", filepath.Join(base, "repos", "vanished"), "", "gone")

	installOpenStore(t, s)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})
	installScriptedSessions(t) // exhausts on any call: tmux must never be consulted
	return s, installContainerActuator(t)
}

func decodeAutostart(t *testing.T, stdout string) autostartEnvelope {
	t.Helper()
	var env autostartEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding autostart JSON: %v\n%s", err, stdout)
	}
	return env
}

func entryFor(t *testing.T, env autostartEnvelope, slug string) autostartEntry {
	t.Helper()
	for _, e := range env.Repositories {
		if e.Slug == slug {
			return e
		}
	}
	t.Fatalf("no entry for %s in %+v", slug, env.Repositories)
	return autostartEntry{}
}

func bindingFor(t *testing.T, s *fake.Store, repositoryID string) *state.ContainerBinding {
	t.Helper()
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, repo := range repos {
		if repo.ID == repositoryID {
			return repo.Container
		}
	}
	t.Fatalf("no repository %s in %+v", repositoryID, repos)
	return nil
}

func TestAutostartMatrix(t *testing.T) {
	s, actC := autostartFixture(t)

	code, stdout, stderr := run(t, "autostart", "--json")
	// The vanished repository root makes this a partial failure: exit 1
	// with the full report on stdout (the spec §5 contract amendment).
	if code != ExitError {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitError, stderr)
	}
	env := decodeAutostart(t, stdout)

	eligible := entryFor(t, env, "eligible")
	if eligible.ID != "r-eligible" || eligible.Outcome != "started" || eligible.ContainerID != "cid-1" {
		t.Errorf("eligible = %+v", eligible)
	}
	// One start for the repository, not one per session on it.
	if len(actC.Started) != 1 || actC.Started[0] != "r-eligible" {
		t.Errorf("Started = %v, want exactly one start for r-eligible", actC.Started)
	}
	seen := 0
	for _, e := range env.Repositories {
		if e.ID == "r-eligible" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("r-eligible appears %d times in the report, want once", seen)
	}

	if disabled := entryFor(t, env, "disabled"); disabled.Outcome != "skipped" {
		t.Errorf("disabled = %+v", disabled)
	}
	if gone := entryFor(t, env, "gone"); gone.Outcome != "failed" || gone.Reason == "" {
		t.Errorf("gone = %+v, want failed with a reason", gone)
	}

	// The binding lands on the repository, which is what owns it.
	if b := bindingFor(t, s, "r-eligible"); b == nil || b.ContainerID != "cid-1" {
		t.Errorf("binding = %+v, want cid-1 on the repository", b)
	}
	if !strings.Contains(stderr, "1 repository(ies)") {
		t.Errorf("stderr = %q, want the one-line summary", stderr)
	}
}

func TestAutostartAllHealthyExitsZero(t *testing.T) {
	s, _ := autostartFixture(t)
	// Recreate the vanished repository root so every repository succeeds.
	repos, err := s.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	for _, repo := range repos {
		if repo.ID != "r-gone" {
			continue
		}
		if err := os.MkdirAll(repo.RepoRoot, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	code, stdout, _ := run(t, "autostart", "--json")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, stdout)
	}
}
```

- [ ] **Step 11: Run the CLI test to verify it fails**

Run: `go test ./internal/cli/ -run TestAutostart -v`
Expected: FAIL to build with `env.Repositories undefined (type autostartEnvelope has no field or method Repositories)`. `fake.Store.Repositories` is *not* part of this failure — Step 5 already defined it.

- [ ] **Step 12: Rewrite `runAutostart` over repositories**

Replace `internal/cli/autostart.go:18-42` (help text and envelope types):

```go
const autostartHelp = `usage: projectmux autostart [--json] [--compact]

Start containers for registered repositories: those whose configuration
sets autostart: true and to which a container applies. Every session on a
repository shares one container, so this starts one container per
repository. No tmux sessions are created. Intended for the systemd user
unit.

  --json     emit the versioned JSON envelope instead of human text
  --compact  emit the JSON on a single line (implies --json)
`

// autostartEnvelope is the versioned JSON structure for autostart. It is
// written to stdout even when the command exits 1 (some repositories
// failed) — the report is the output (spec §5). It is keyed by repository
// because that is what owns a container (spec §6.3).
type autostartEnvelope struct {
	SchemaVersion int              `json:"schema_version"`
	Repositories  []autostartEntry `json:"repositories"`
}

type autostartEntry struct {
	ID          string `json:"id"` // repository ID
	Slug        string `json:"slug"`
	Outcome     string `json:"outcome"` // started | already-running | skipped | failed
	Reason      string `json:"reason,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
}
```

- [ ] **Step 13: Rewrite the loop body**

Replace `internal/cli/autostart.go:76-189` (the store read through the tail) with:

```go
	repos, err := st.Repositories()
	if err != nil {
		return fmt.Errorf("reading stored repositories: %w", err)
	}
	stateRoot, err := state.Root()
	if err != nil {
		return err
	}
	lockDir := filepath.Join(stateRoot, "locks")

	ctrl := controller.Controller{
		Store:        st,
		Sessions:     newSessionObserver(), // never consulted; wired for completeness
		Containers:   newContainerObserver(),
		Clock:        systemClock{},
		ContainerAct: newContainerActuator(),
	}

	env := autostartEnvelope{SchemaVersion: OutputSchemaVersion, Repositories: []autostartEntry{}}
	failed := 0
	for _, repo := range repos {
		entry := autostartEntry{ID: repo.ID, Slug: repo.Slug}

		// The repository root must exist before anything else: config
		// loads never touch it, and auto's applicability check would
		// misread absence as "does not apply" — a vanished repository
		// must be a visible boot-log failure, not a silent skip.
		if _, statErr := os.Stat(repo.RepoRoot); statErr != nil {
			entry.Outcome = "failed"
			// Only confirmed absence may claim the repository is gone;
			// permission or I/O failures keep their own story.
			if errors.Is(statErr, os.ErrNotExist) {
				entry.Reason = "repository root no longer exists: " + repo.RepoRoot
			} else {
				entry.Reason = "statting the repository root: " + statErr.Error()
			}
			failed++
			env.Repositories = append(env.Repositories, entry)
			continue
		}

		effective, loadErr := config.Load(root, defaults, repo.Slug)
		if loadErr != nil {
			entry.Outcome = "failed"
			entry.Reason = loadErr.Error()
			failed++
			env.Repositories = append(env.Repositories, entry)
			continue
		}
		if !effective.Config.Autostart {
			entry.Outcome = "skipped"
			entry.Reason = "autostart is not enabled"
			env.Repositories = append(env.Repositories, entry)
			continue
		}

		outcome, obs, startErr := ctrl.StartRepositoryContainer(ctx, controller.RepoDesired{
			Repository: repo,
			Config:     effective.Config,
			Digest:     effective.Digest,
		}, lockDir, lockTimeout)
		switch {
		case startErr != nil:
			entry.Outcome = "failed"
			entry.Reason = startErr.Error()
			failed++
		case outcome == controller.ContainerNoneApplies:
			entry.Outcome = "skipped"
			entry.Reason = "no container applies"
		default:
			entry.Outcome = string(outcome)
			if obs != nil {
				entry.ContainerID = obs.ContainerID
			}
		}
		env.Repositories = append(env.Repositories, entry)
	}

	if *asJSON {
		if err := writeJSON(stdout, env, *compact); err != nil {
			return err
		}
	} else {
		if len(env.Repositories) == 0 {
			fmt.Fprintln(stdout, "no registered repositories")
		}
		for _, e := range env.Repositories {
			line := fmt.Sprintf("%s\t%s", e.Slug, e.Outcome)
			if e.ContainerID != "" {
				line += "\t" + e.ContainerID
			}
			if e.Reason != "" {
				line += "\t(" + e.Reason + ")"
			}
			fmt.Fprintln(stdout, line)
		}
	}

	if failed > 0 {
		return &reportedError{msg: fmt.Sprintf(
			"autostart failed for %d repository(ies); details are in the report above", failed)}
	}
	return nil
}
```

Drop `"github.com/gambtho/projectmux/internal/resolve"` from the import block: nothing in the file builds a workspace any more.

- [ ] **Step 14: Run the CLI autostart tests**

Run: `go test ./internal/cli/ -run TestAutostart -v`
Expected: PASS.

- [ ] **Step 15: Rewrite the `is_primary` regression test**

Replace `internal/cli/lifecycle_test.go:439-523` in full. The behavior it protects — that a recovered installation still starts this container, exactly once — is asserted against the repository row and a real autostart run.

```go
// TestLifecycleRebuildThenAutostart performs the disaster this slice
// exists for, against a real tmux server and the real store: the state
// database is destroyed while the session it described is still running.
// Rebuild re-registers the repository and its session from the live
// session's identity keys, and autostart then starts that repository's
// container. This is the successor to the is_primary regression: the flag
// used to keep autostart from starting a shared container once per
// worktree, and one row per repository now does it structurally (spec
// §6.3). If the repositories table came back wrong, autostart would stop
// starting this container — or start it twice.
func TestLifecycleRebuildThenAutostart(t *testing.T) {
	ws, socket := lifecycleRig(t, "rebuild")
	actC := installContainerActuator(t)
	actC.ExecResult = "sleep 300"
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})

	if env := openJSON(t); env.Action != "created" {
		t.Fatalf("open = %+v", env)
	}

	// The disaster: the database and its sidecars are gone; the tmux
	// session is not.
	stateRoot := os.Getenv("PROJECTMUX_STATE_ROOT")
	path := state.DBPath(stateRoot)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing %s: %v", p, err)
		}
	}

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("rebuild exit %d, stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	env := decodeRebuild(t, stdout)
	if len(env.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", env.Conflicts)
	}
	if len(env.Registered) != 1 {
		t.Fatalf("registered = %+v, want exactly one workspace", env.Registered)
	}
	if got := env.Registered[0]; got.ID != ws.ID || got.Slug != ws.Slug ||
		got.RepoRoot != ws.RepoRoot {
		t.Fatalf("registered = %+v, want %s at %s", got, ws.Slug, ws.RepoRoot)
	}

	// Exactly one repository row, carrying the root autostart will use.
	st, err := state.Open(stateRoot)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	repos, err := st.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 || repos[0].ID != ws.RepositoryID || repos[0].RepoRoot != ws.RepoRoot {
		t.Fatalf("repositories = %+v, want exactly one row for %s", repos, ws.RepoRoot)
	}
	rec, err := st.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != ws.SessionName {
		t.Errorf("actual_session = %v, want %q adopted", rec.ActualSession, ws.SessionName)
	}
	if rec.ProposedSession != ws.SessionName {
		t.Errorf("proposed_session = %q, want %q", rec.ProposedSession, ws.SessionName)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Idempotence: a fully recovered installation has nothing to do.
	code, stdout, stderr = run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("second rebuild exit %d, stderr: %s", code, stderr)
	}
	second := decodeRebuild(t, stdout)
	if len(second.Registered) != 0 || len(second.Conflicts) != 0 {
		t.Errorf("second rebuild = %+v, want an empty report", second)
	}

	// The recovered installation still autostarts, once.
	configDir := os.Getenv("PROJECTMUX_CONFIG_ROOT")
	cfgPath := configDir + "/workspaces/" + ws.Slug + ".yaml"
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if err := os.WriteFile(cfgPath, append(raw, []byte("autostart: true\n")...), 0o644); err != nil {
		t.Fatalf("enabling autostart: %v", err)
	}
	before := len(actC.Started)
	code, stdout, stderr = run(t, "autostart", "--json")
	if code != ExitOK {
		t.Fatalf("autostart exit %d, stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	auto := decodeAutostart(t, stdout)
	if len(auto.Repositories) != 1 {
		t.Fatalf("autostart report = %+v, want one repository", auto.Repositories)
	}
	if e := auto.Repositories[0]; e.ID != ws.RepositoryID || e.Outcome != "started" {
		t.Fatalf("autostart entry = %+v, want %s started", e, ws.RepositoryID)
	}
	if len(actC.Started)-before != 1 {
		t.Errorf("autostart made %d starts, want exactly 1", len(actC.Started)-before)
	}

	// The session was adopted, never recreated or renamed, and autostart
	// never touched tmux.
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != ws.SessionName || live[0].WorkspaceID != ws.ID {
		t.Errorf("live = %+v; rebuild or autostart created or renamed sessions", live)
	}
}
```

- [ ] **Step 16: Run the lifecycle test**

Run: `go test ./internal/cli/ -run TestLifecycleRebuildThenAutostart -v`
Expected: PASS (skips if tmux is not installed).

- [ ] **Step 17: Update the usage line and the command documentation**

`internal/cli/cli.go:50-51`:

```
  autostart [--json] [--compact]
        start containers for registered repositories with autostart: true
```

`docs/commands.md:350-360`, replacing both paragraphs that explain the primary-worktree filter:

```markdown
Starts containers for registered repositories with `autostart: true`. It is a
batch command intended for boot, and it reports one line per repository:

```text
$ projectmux autostart
slabledger	skipped	(autostart is not enabled)
```

The unit of the report is the *repository*, not the session: every session on
a repository shares one container, so autostart starts one container per
repository however many sessions it has. Autostart starts containers; it does
not create tmux sessions.
```

- [ ] **Step 18: Format, run the owned packages, and commit**

Run: `gofmt -l internal/controller internal/cli && go test ./internal/controller/... ./internal/cli/...`
Expected: no files listed by `gofmt`, both packages PASS.

The gate is scoped rather than run as `go test ./...` because `internal/doctor`'s own tests
still read the dropped fields until Task 9 — see "Task Ordering and Interactions" above.
Everything else in the tree builds from the end of Task 5a and passes from the end of this
task, so this line covers every package whose behaviour this task can change.

Confirm the build came back up as well, since Step 8 took it down:

Run: `go build ./...`
Expected: exit 0 with no output.

```
git commit -am "feat(autostart): start one container per repository

Autostart iterated workspaces and skipped non-primary rows so a shared
parent container was not started once per worktree. is_primary is gone
with the schema split, so autostart now iterates repositories: one row
per repository is the same guarantee, enforced by repo_root UNIQUE
rather than by a filter.

StartWorkspaceContainer becomes StartRepositoryContainer and takes a
state.Repository, so starting a container needs no session and reads no
workspace row. Its report is keyed by repository ID and slug. The
autostart operation record is dropped: last_operations belongs to a
session, and autostart is performed by none.

controller.Store gains Repositories and its RecordContainerObservation
takes a repository ID. The in-memory fake follows the same split: a
repositories map, a repository-keyed containers map, and a record whose
Container is a copied-out projection of its repository's binding, which
is the join the real store performs.

The CLI test files that build workspaces by hand convert here too: this
is the first task to gate ./internal/cli/..., and a Go test binary only
links when every file in the package compiles. The envelopes keep their
worktree and is_primary field names and their schema_version 1 shape;
Task 7 owns removing them."
```

### Task 6: `stop --container` refuses while a sibling session is live

**Files:**
- Modify: `internal/controller/stop.go:13-118`
- Modify: `internal/cli/stop.go:18-27,50-114`
- Modify: `internal/cli/cli.go:48-49` (usage line)
- Modify: `docs/commands.md:323-342`
- Test: `internal/controller/stop_test.go:13-16` (the rig call) plus three new tests
- Test: `internal/cli/stop_test.go` (two new tests)

**Interfaces:**
- Consumes (Task 1): `resolve.Workspace{ID, RepositoryID, ...}`.
- Consumes (Task 2): `state.Record` carries `RepositoryID`; `Store.Workspaces()` returns every session row.
- Consumes (Task 3): `lock.Acquire(ctx, dir, key string, timeout time.Duration) (*Lock, error)`, `lock.ErrLockHeld{Key string}`, and the global ordering — repository lock first, then workspace, released in reverse.
- Consumes (Task 5b): `RecordContainerObservation` keyed on the repository ID.
- Consumes (existing): `cli.exitCode` (`internal/cli/cli.go:167-189`) maps `*controller.RefusalError` to `ExitRefused` (= 6). That is the mechanism — the refusal is a typed error returned up, never a hand-written exit code.
- Produces: `controller.StopOptions{Container, Force bool}`; `func (c *Controller) Stop(ctx context.Context, d Desired, opts StopOptions, lockDir string, lockTimeout time.Duration) (StopResult, error)`; `stop --force`.

Sibling rows are not reachable from the resolver in this plan — `Session` is always `""` — but they are reachable from the store: `UNIQUE (repository_id, session)` permits them, and `rebuild` (spec §9) creates them by proposing a linked worktree's basename as a session name for a pre-existing live session. The tests construct them through the store for that reason.

- [ ] **Step 1: Write the failing test — sibling refusal, in `internal/controller/stop_test.go`**

Change the rig helper at lines 13-16 and add the test. Existing callers `r.stop(t, d, false)` become `r.stop(t, d, controller.StopOptions{})`, and `r.stop(t, d, true)` becomes `r.stop(t, d, controller.StopOptions{Container: true})`.

```go
func (r *ensureRig) stop(t *testing.T, d controller.Desired, opts controller.StopOptions) (controller.StopResult, error) {
	t.Helper()
	return r.ctrl.Stop(context.Background(), d, opts, r.lockDir, time.Second)
}

// registerSibling adds a second session on the same repository. The
// resolver cannot produce one yet, but the schema permits it and rebuild
// creates them, so the store is where a sibling comes from.
func registerSibling(t *testing.T, r *ensureRig) {
	t.Helper()
	sib := resolve.Workspace{
		ID: "w2", RepositoryID: "r1", Slug: "slab", RepoRoot: "/w/slab",
		Session: "feature-a", SessionName: "slab--feature-a",
	}
	if err := r.store.RegisterWorkspace(sib, "sha256:x", ensureTime); err != nil {
		t.Fatalf("register sibling: %v", err)
	}
	if _, err := r.store.AllocateSessionName("w2", ensureTime); err != nil {
		t.Fatalf("allocate sibling: %v", err)
	}
}

func siblingSession() controller.LiveSession {
	return controller.LiveSession{
		ID: "$9", Name: "slab--feature-a", WorkspaceID: "w2",
		Slug: "slab", Worktree: "/w/slab",
	}
}

func bindStopContainer(t *testing.T, r *ensureRig) {
	t.Helper()
	if err := r.store.RecordContainerObservation("r1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
		Workdir: "/workspaces/slab", Health: state.HealthPresent,
	}, ensureTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
}

func TestStopContainerRefusesWhileSiblingIsLive(t *testing.T) {
	// One step only: the refusal must land before the workspace's own
	// session is ever observed, so nothing is destroyed.
	r := newEnsureRig(t, liveStep(siblingSession())).withContainerActuator()
	registerStopFixture(t, r)
	registerSibling(t, r)
	bindStopContainer(t, r)

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if !strings.Contains(refusal.Reason, "slab--feature-a") {
		t.Errorf("reason = %q, want the live sibling named", refusal.Reason)
	}
	if len(r.actuator.Killed) != 0 {
		t.Errorf("Killed = %v; a refusal must destroy nothing", r.actuator.Killed)
	}
	if len(r.actuatorC.Stopped) != 0 {
		t.Errorf("Stopped = %v; the shared container was killed anyway", r.actuatorC.Stopped)
	}
}

func TestStopContainerForceOverridesLiveSibling(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)
	registerSibling(t, r)
	bindStopContainer(t, r)

	// Force skips the sibling observation entirely: the user has already
	// answered the question it asks.
	res, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true, Force: true})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.ContainerStopped || res.ContainerID != "cid-1" {
		t.Errorf("result = %+v, want the container stopped", res)
	}
	if len(r.actuatorC.Stopped) != 1 || r.actuatorC.Stopped[0] != "cid-1" {
		t.Errorf("Stopped = %v", r.actuatorC.Stopped)
	}
}

func TestStopContainerRefusesWhenSiblingsCannotBeObserved(t *testing.T) {
	r := newEnsureRig(t, errorStep(errors.New("tmux exploded"))).withContainerActuator()
	registerStopFixture(t, r)
	registerSibling(t, r)
	bindStopContainer(t, r)

	_, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if len(r.actuatorC.Stopped) != 0 {
		t.Error("uncertainty reached the container actuator")
	}
}
```

Add `"strings"` and `"github.com/gambtho/projectmux/internal/resolve"` to the file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestStopContainer -v`
Expected: FAIL to build with `undefined: controller.StopOptions` and `too many arguments in call to r.ctrl.Stop`.

- [ ] **Step 3: Add `StopOptions` and the two-lock preamble**

Replace `internal/controller/stop.go:23-34` (the doc comment, signature, and lock acquisition):

```go
// StopOptions selects a stop's destructive scope. Force overrides the
// shared-container refusal below and nothing else.
type StopOptions struct {
	Container bool
	Force     bool
}

// Stop is the destructive counterpart of Ensure and deliberately
// idempotent: absent sessions and unbound containers are success, and
// nothing is ever destroyed on uncertainty. It targets only the
// identity-matched session, performs no registration, and records
// operations only for workspaces that already have a record.
func (c *Controller) Stop(ctx context.Context, d Desired, opts StopOptions, lockDir string, lockTimeout time.Duration) (StopResult, error) {
	const opName = "stop"
	// Lock ordering is global: repository first, then workspace, released
	// in reverse (spec §6.1). Only a container stop touches the shared
	// repository, so only it takes the outer lock.
	if opts.Container {
		repoLock, err := lock.Acquire(ctx, lockDir, d.Workspace.RepositoryID, lockTimeout)
		if err != nil {
			return StopResult{}, err
		}
		// Held to the end of Stop deliberately: the sibling check below
		// and the container stop that follows it must happen under one
		// continuous hold. Releasing between them is exactly the race the
		// check exists to prevent — a sibling opening in the gap passes
		// the check that already ran and then loses its container.
		defer func() { _ = repoLock.Release() }()
	}
	lk, err := lock.Acquire(ctx, lockDir, d.Workspace.ID, lockTimeout)
	if err != nil {
		return StopResult{}, err
	}
	defer func() { _ = lk.Release() }()
```

- [ ] **Step 4: Add the sibling check and the sibling query**

Insert immediately after `registered := stored != nil` (`internal/controller/stop.go:47`):

```go
	// A container is shared by every session on its repository, so
	// stopping one can kill a container a sibling is working in. Refuse
	// and name them; --force is how the user says they meant it.
	if opts.Container && !opts.Force && stored != nil && stored.Container != nil {
		siblings, sibErr := c.liveSiblings(ctx, d.Workspace)
		if sibErr != nil {
			reason := "tmux could not be observed; refusing to stop a shared container on an unknown session state"
			if registered {
				c.recordFailure(d.Workspace.ID, opName, reason)
			}
			return StopResult{}, &RefusalError{Reason: reason}
		}
		if len(siblings) > 0 {
			reason := fmt.Sprintf(
				"the container is shared with live session(s) %s; refusing to stop it (use --force)",
				strings.Join(siblings, ", "))
			if registered {
				c.recordFailure(d.Workspace.ID, opName, reason)
			}
			return StopResult{}, &RefusalError{Reason: reason}
		}
	}
```

Append at the end of the file:

```go
// liveSiblings names the live sessions of other workspaces on the same
// repository. It starts from the store rather than from tmux alone
// because a session's repository is recorded state: the tmux identity
// keys carry a workspace ID and a root, not the sibling relation.
func (c *Controller) liveSiblings(ctx context.Context, ws resolve.Workspace) ([]string, error) {
	records, err := c.Store.Workspaces()
	if err != nil {
		return nil, fmt.Errorf("reading stored workspaces: %w", err)
	}
	var names []string
	for _, rec := range records {
		if rec.ID == ws.ID || rec.RepositoryID != ws.RepositoryID {
			continue
		}
		q := SessionQuery{
			WorkspaceID:    rec.ID,
			CandidateNames: []string{rec.ProposedSession},
		}
		if rec.ActualSession != nil && *rec.ActualSession != rec.ProposedSession {
			q.CandidateNames = append(q.CandidateNames, *rec.ActualSession)
		}
		obs, err := c.Sessions.ObserveSession(ctx, q)
		if err != nil {
			return nil, err
		}
		if obs.ByIdentity != nil {
			names = append(names, obs.ByIdentity.Name)
		}
	}
	return names, nil
}
```

Add `"strings"` and `"github.com/gambtho/projectmux/internal/resolve"` to the imports, and change the container branch's condition at line 94 from `if stopContainer && ...` to `if opts.Container && stored != nil && stored.Container != nil {`.

- [ ] **Step 5: Run the controller stop tests**

Run: `go test ./internal/controller/ -run TestStop -v`
Expected: PASS for all three new tests and the existing ones.

- [ ] **Step 6: Write the failing test — the lock is held across the stop**

Append to `internal/controller/stop_test.go`:

```go
// lockProbingStopper tries to take the repository lock from inside
// StopContainer. Stop must still hold it there: if the sibling check
// released the lock before the stop, this acquire would succeed, and a
// sibling opening in that window would land in a container about to die.
type lockProbingStopper struct {
	*fake.ContainerActuator
	lockDir  string
	repoID   string
	probeErr error
}

func (a *lockProbingStopper) StopContainer(ctx context.Context, containerID string) error {
	l, err := lock.Acquire(ctx, a.lockDir, a.repoID, 10*time.Millisecond)
	if l != nil {
		_ = l.Release()
	}
	a.probeErr = err
	return a.ContainerActuator.StopContainer(ctx, containerID)
}

func TestStopHoldsTheRepositoryLockThroughTheContainerStop(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)
	bindStopContainer(t, r)
	probe := &lockProbingStopper{
		ContainerActuator: r.actuatorC, lockDir: r.lockDir, repoID: "r1",
	}
	r.ctrl.ContainerAct = probe

	res, err := r.stop(t, ensureDesired(), controller.StopOptions{Container: true})
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.ContainerStopped {
		t.Fatalf("result = %+v, want the container stopped", res)
	}
	var held *lock.ErrLockHeld
	if !errors.As(probe.probeErr, &held) {
		t.Fatalf("acquiring the repository lock during the stop = %v, want *lock.ErrLockHeld; "+
			"the check and the stop must run under one continuous hold", probe.probeErr)
	}
	if held.Key != "r1" {
		t.Errorf("held key = %q, want the repository ID", held.Key)
	}
}
```

Add `"github.com/gambtho/projectmux/internal/lock"` and `"github.com/gambtho/projectmux/internal/controller/fake"` to the imports.

- [ ] **Step 7: Run the lock test**

Run: `go test ./internal/controller/ -run TestStopHoldsTheRepositoryLock -v`
Expected: PASS — the implementation from Step 3 already holds the lock; this step pins it. If it fails with `probeErr = <nil>`, the outer `defer` was replaced by an early release.

- [ ] **Step 8: Write the failing CLI test — exit 6 and the named sibling**

Append to `internal/cli/stop_test.go`:

```go
// registerCLISibling adds a second session on the workspace's repository
// directly through the store: the resolver has no argument form for one
// yet, but rebuild produces them from pre-existing worktree sessions.
func registerCLISibling(t *testing.T, s *fake.Store, ws resolve.Workspace) controller.LiveSession {
	t.Helper()
	sib := resolve.Workspace{
		ID:           ws.ID + "-2",
		RepositoryID: ws.RepositoryID,
		Slug:         ws.Slug,
		RepoRoot:     ws.RepoRoot,
		Session:      "feature-a",
		SessionName:  ws.SessionName + "--feature-a",
	}
	if err := s.RegisterWorkspace(sib, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register sibling: %v", err)
	}
	if _, err := s.AllocateSessionName(sib.ID, cliTestTime); err != nil {
		t.Fatalf("allocate sibling: %v", err)
	}
	return controller.LiveSession{
		ID: "$9", Name: sib.SessionName, WorkspaceID: sib.ID,
		Slug: sib.Slug, Worktree: sib.RepoRoot,
	}
}

func TestStopContainerRefusesWithLiveSiblingExitsSix(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	s, _ := stopFixtureFor(t, ws)
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	sib := registerCLISibling(t, s, ws)
	actC := installContainerActuator(t)
	installScriptedSessions(t, cliLive(sib))

	code, stdout, stderr := run(t, "stop", "--container", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitRefused, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on refusal (nothing was done)", stdout)
	}
	if !strings.Contains(stderr, sib.Name) {
		t.Errorf("stderr = %q, want the live sibling named", stderr)
	}
	if len(actC.Stopped) != 0 {
		t.Errorf("Stopped = %v; the shared container was killed anyway", actC.Stopped)
	}
}

func TestStopContainerForceStopsSharedContainer(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	s, actuator := stopFixtureFor(t, ws)
	if err := s.RecordContainerObservation(ws.RepositoryID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "cid-1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	registerCLISibling(t, s, ws)
	actC := installContainerActuator(t)
	// Force never observes the siblings, so the only scripted step is the
	// workspace's own session.
	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "stop", "--container", "--force", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStop(t, stdout)
	if env.Container == nil || !env.Container.Stopped || env.Container.ContainerID != "cid-1" {
		t.Errorf("container = %+v", env.Container)
	}
	if len(actC.Stopped) != 1 || len(actuator.Killed) != 1 {
		t.Errorf("Stopped = %v, Killed = %v", actC.Stopped, actuator.Killed)
	}
}
```

- [ ] **Step 9: Run the CLI test to verify it fails**

Run: `go test ./internal/cli/ -run TestStopContainer -v`
Expected: FAIL — `TestStopContainerRefusesWithLiveSiblingExitsSix` gets `exit 0, want 6`, and `TestStopContainerForceStopsSharedContainer` gets `flag provided but not defined: -force` (exit 2).

- [ ] **Step 10: Add `--force` to the stop command**

`internal/cli/stop.go:18-27`:

```go
const stopHelp = `usage: projectmux stop [--container] [--force] [--json] [--compact] [<workspace>]

End the workspace's tmux session, and with --container also stop its
bound container. The only destructive command; idempotent — stopping an
already-stopped workspace succeeds.

  --container  also stop the container the workspace's repository shares
  --force      stop that container even while sibling sessions are live
  --json       emit the versioned JSON envelope instead of human text
  --compact    emit the JSON on a single line (implies --json)
`
```

`internal/cli/stop.go:52` and `:106-107`:

```go
	stopContainer := fs.Bool("container", false, "also stop the bound container")
	force := fs.Bool("force", false, "stop a shared container even while sibling sessions are live")
```

```go
	res, stopErr := ctrl.Stop(ctx, controller.Desired{Workspace: ws},
		controller.StopOptions{Container: *stopContainer, Force: *force},
		filepath.Join(stateRoot, "locks"), lockTimeout)
```

- [ ] **Step 11: Run the CLI stop tests**

Run: `go test ./internal/cli/ -run TestStop -v`
Expected: PASS. The refusal exits 6 with no code change to `exitCode`: `Stop` returns `*controller.RefusalError`, `runStop` returns it unchanged because nothing was destroyed (`internal/cli/stop.go:112-114`), and `exitCode` maps it to `ExitRefused`.

- [ ] **Step 12: Update the usage line and the command documentation**

`internal/cli/cli.go:48-49`:

```
  stop [--container] [--force] [--json] [--compact] [<workspace>]
        end the workspace session, and with --container its container
```

`docs/commands.md:325-327` and after line 338:

````markdown
```text
projectmux stop [--container] [--force] [--json] [--compact] [<workspace>]
```
````

```markdown
A container belongs to a repository and is shared by every session on it, so
`stop --container` refuses with exit 6 when another session on the same
repository is live, and names them:

```text
$ projectmux stop --container
projectmux: the container is shared with live session(s) slabledger--feature-a; refusing to stop it (use --force)
```

`--force` stops it anyway. The sibling check and the container stop happen
under one continuous hold of the repository lock, so a sibling cannot open
into the gap between them.
```

- [ ] **Step 13: Format, run the owned packages, and commit**

Run: `gofmt -l internal/controller internal/cli && go test ./internal/controller/... ./internal/cli/...`
Expected: no files listed by `gofmt`, both packages PASS.

The gate is scoped rather than run as `go test ./...` because `internal/doctor`'s own tests
still read the dropped fields until Task 9 — see "Task Ordering and Interactions" above.
Everything else in the tree builds from the end of Task 5a and passes from the end of Task 5b, so this line covers
every package whose behaviour this task can change.

```
git commit -am "feat(stop): refuse to stop a shared container under live siblings

A container is now shared by every session on its repository, so
stop --container could kill a container a sibling is working in. It now
refuses with exit 6, naming the live siblings, and --force overrides.

The sibling check and the container stop run under one continuous hold of
the repository lock, acquired before the workspace lock per the global
ordering. A check released before the stop is the race it exists to
prevent, and TestStopHoldsTheRepositoryLockThroughTheContainerStop pins
the hold by probing the lock from inside StopContainer."

---

### Task 7: JSON envelopes to `schema_version` 2

**Files:**
- Modify: `internal/cli/config.go:19-22` (the `OutputSchemaVersion` constant), `internal/cli/config.go:51-57` (`workspaceInfo`), `internal/cli/config.go:131-143`, `internal/cli/config.go:162-167`
- Modify: `internal/cli/open.go:81-90`
- Modify: `internal/cli/attach.go:134-142`
- Modify: `internal/cli/stop.go:117-125`
- Modify: `internal/cli/status.go:172-181`, `internal/cli/status.go:255-259`
- Modify: `internal/cli/list.go:33-46` (`listRow`), `internal/cli/list.go:113-121`, `internal/cli/list.go:155-163`
- Modify: `internal/cli/rebuild.go:50-56` (`rebuildRegistered`), `internal/cli/rebuild.go:124-131`, `internal/cli/rebuild.go:224-229` (the `worktreeResolver` comment)
- Modify: `internal/rebuild/apply.go:45-52` (`Registered`), `internal/rebuild/apply.go:321-334` (`registeredFor`)
- Modify: `internal/cli/cli_test.go:185-189`, `internal/cli/cli_test.go:226-228`
- Test: `internal/rebuild/classify_test.go:26-33`
- Test: `internal/rebuild/apply_test.go:152-155, 169-183, 193, 216-222, 233-235, 496, 504-505, 516-518`
- Modify: `docs/commands.md:29-32`, `docs/commands.md:85-89`, `docs/commands.md:100-104`, `docs/commands.md:193-197`
- Test: `internal/cli/schema_version_test.go`

**Interfaces:**
- Consumes: `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}` (Task 1); `state.Record` with `RepositoryID`, `RepoRoot`, `Session` and no `Worktree`/`IsPrimary` (Task 2).
- Produces: `cli.OutputSchemaVersion == 2`; `cli.workspaceInfo{ID, Slug, RepoRoot, Session, SessionName}` with JSON `id`, `slug`, `repo_root`, `session`, `session_name`; `cli.listRow.RepoRoot`/`.Session`; `cli.rebuildRegistered.RepoRoot`; `rebuild.Registered{ID, Slug, RepoRoot, Session}`.

Two things about this task's starting point. Its Step 1 converts the `internal/rebuild` tests, which
belong to no earlier task's Files block and which this task is the first to gate — `go test
./internal/rebuild/...` at Step 14 cannot run while `apply_test.go` builds workspaces in the dropped
shape. And the carriers this task reshapes were already made to *compile* in Task 5a: every
`Worktree:` line below now reads `ws.RepoRoot` or `rec.RepoRoot` on the right and every `IsPrimary:`
line reads the constant `true`. The field names, the JSON tags and the line structure are untouched,
which is what this task removes; where a "before" fence below still shows `ws.Worktree`, read it as
the same line with Task 5a's right-hand side.

Nine envelopes read `OutputSchemaVersion` (`doctor.go:113`, `validate.go:75`, `autostart.go:94` besides the seven here), so the single bump moves all of them. Only the seven in the table carry `worktree`/`is_primary`; `autostart`'s own per-repository fields are the autostart task's, and inherit this constant rather than a second one.

- [ ] **Step 1: Convert the rebuild tests**

`internal/rebuild`'s test files build `state.Record` and `resolve.Workspace` literals in the dropped
shape. Task 5a converted the package's non-test files so that `internal/cli` could link it, but a
test file only has to compile when its own package is tested, and this task is the first to do that.

`internal/rebuild/classify_test.go:26-33` before:

```go
func stored(id, slug, worktree, actual string) state.Record {
	rec := state.Record{
		ID:              id,
		Slug:            slug,
		Worktree:        worktree,
		IsPrimary:       true,
		ProposedSession: slug,
	}
```

after:

```go
func stored(id, slug, repoRoot, actual string) state.Record {
	rec := state.Record{
		ID:              id,
		Slug:            slug,
		RepoRoot:        repoRoot,
		ProposedSession: slug,
	}
```

The `live()` helper above it builds a `controller.LiveSession` and keeps `Worktree`, as do the
message assertions quoting `worktree "/w/slab"` — Task 5a's classifier conversion left the reason string's wording intact
precisely so these keep passing.

`internal/rebuild/apply_test.go:169-177` before:

```go
func workspace(id, slug, worktree, sessionName string, primary bool) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        slug,
		Worktree:    worktree,
		SessionName: sessionName,
		IsPrimary:   primary,
	}
}
```

after — the `primary` parameter has nothing left to set, so it goes, and the two call sites follow:

```go
func workspace(id, slug, repoRoot, sessionName string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        slug,
		RepoRoot:    repoRoot,
		SessionName: sessionName,
	}
}
```

`internal/rebuild/apply_test.go:179-183` before:

```go
func projectmux() resolve.Workspace {
	return workspace(
		"1111111111111111111111111111111111111111111111111111111111111111",
		"projectmux", "/src/projectmux", "projectmux", true)
}
```

after:

```go
func projectmux() resolve.Workspace {
	return workspace(
		"1111111111111111111111111111111111111111111111111111111111111111",
		"projectmux", "/src/projectmux", "projectmux")
}
```

`internal/rebuild/apply_test.go:152-155` before:

```go
func (h *harness) know(ws resolve.Workspace, digest string) {
	h.resolver.byWorktree[ws.Worktree] = ws
	h.config.digests[ws.Slug] = digest
}
```

after:

```go
func (h *harness) know(ws resolve.Workspace, digest string) {
	h.resolver.byWorktree[ws.RepoRoot] = ws
	h.config.digests[ws.Slug] = digest
}
```

`internal/rebuild/apply_test.go:187-195` before:

```go
func liveSession(ws resolve.Workspace, name string) controller.LiveSession {
	return controller.LiveSession{
		ID:          "$1",
		Name:        name,
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.Worktree,
	}
}
```

after — the `LiveSession` field name stays, only its source moves:

```go
func liveSession(ws resolve.Workspace, name string) controller.LiveSession {
	return controller.LiveSession{
		ID:          "$1",
		Name:        name,
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.RepoRoot,
	}
}
```

`internal/rebuild/apply_test.go:216-235` before:

```go
	want := []Registered{{
		ID:        ws.ID,
		Slug:      "projectmux",
		Worktree:  "/src/projectmux",
		IsPrimary: true,
		Session:   "projectmux",
	}}
	if !reflect.DeepEqual(report.Registered, want) {
		t.Fatalf("Registered = %+v, want %+v", report.Registered, want)
	}

	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	if !rec.IsPrimary {
		t.Errorf("IsPrimary = false, want true — it comes from the resolver, not the session keys")
	}
```

after — the `IsPrimary` assertion made the point that registration reads the resolver rather than
the tmux keys, and `RepoRoot` is now the field that carries that point, so it takes over the check
rather than being deleted outright:

```go
	want := []Registered{{
		ID:       ws.ID,
		Slug:     "projectmux",
		RepoRoot: "/src/projectmux",
		Session:  "projectmux",
	}}
	if !reflect.DeepEqual(report.Registered, want) {
		t.Fatalf("Registered = %+v, want %+v", report.Registered, want)
	}

	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	if rec.RepoRoot != "/src/projectmux" {
		t.Errorf("RepoRoot = %q, want %q — it comes from the resolver, not the session keys",
			rec.RepoRoot, "/src/projectmux")
	}
```

`internal/rebuild/apply_test.go:492-518` before:

```go
// Slug and worktree deliberately match: a row disagreeing on those is an
// identity mismatch, which classification refuses before it ever reaches
// application.
func seedRecorded(t *testing.T, store *fake.Store, ws resolve.Workspace) {
	t.Helper()
	recorded := workspace(ws.ID, ws.Slug, ws.Worktree, "recorded-proposed", false)
	if err := store.RegisterWorkspace(recorded, "sha256:recorded", testTime); err != nil {
		t.Fatalf("seeding the recorded row: %v", err)
	}
}

func assertRecordedFieldsUntouched(t *testing.T, rec state.Record, ws resolve.Workspace) {
	t.Helper()
	if rec.IsPrimary {
		t.Errorf("IsPrimary = true, want the recorded false: adoption must not re-register")
	}
	if rec.ProposedSession != "recorded-proposed" {
		t.Errorf("ProposedSession = %q, want %q", rec.ProposedSession, "recorded-proposed")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:recorded" {
		t.Errorf("DesiredDigest = %v, want %q", rec.DesiredDigest, "sha256:recorded")
	}
	if rec.Slug != ws.Slug {
		t.Errorf("Slug = %q, want %q", rec.Slug, ws.Slug)
	}
	if rec.Worktree != ws.Worktree {
		t.Errorf("Worktree = %q, want %q", rec.Worktree, ws.Worktree)
	}
}
```

after — `ProposedSession` and `DesiredDigest` still differ between the seeded row and what a
re-registration would write, so the test keeps its teeth without `IsPrimary`:

```go
// Slug and repository root deliberately match: a row disagreeing on those
// is an identity mismatch, which classification refuses before it ever
// reaches application.
func seedRecorded(t *testing.T, store *fake.Store, ws resolve.Workspace) {
	t.Helper()
	recorded := workspace(ws.ID, ws.Slug, ws.RepoRoot, "recorded-proposed")
	if err := store.RegisterWorkspace(recorded, "sha256:recorded", testTime); err != nil {
		t.Fatalf("seeding the recorded row: %v", err)
	}
}

func assertRecordedFieldsUntouched(t *testing.T, rec state.Record, ws resolve.Workspace) {
	t.Helper()
	if rec.ProposedSession != "recorded-proposed" {
		t.Errorf("ProposedSession = %q, want %q: adoption must not re-register",
			rec.ProposedSession, "recorded-proposed")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:recorded" {
		t.Errorf("DesiredDigest = %v, want %q", rec.DesiredDigest, "sha256:recorded")
	}
	if rec.Slug != ws.Slug {
		t.Errorf("Slug = %q, want %q", rec.Slug, ws.Slug)
	}
	if rec.RepoRoot != ws.RepoRoot {
		t.Errorf("RepoRoot = %q, want %q", rec.RepoRoot, ws.RepoRoot)
	}
}
```

The two `controller.LiveSession` literals at lines 254-260 and 410-416 name a vanished path in
`Worktree`; that field is unchanged and they stay exactly as they are.

- [ ] **Step 2: Write the failing test**

Create `internal/cli/schema_version_test.go`:

```go
package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
)

// assertSchemaV2 checks the two properties every migrated envelope shares.
//
// The version is compared against the literal 2, not against
// OutputSchemaVersion: every existing envelope test asserts equality with
// the constant, which holds whatever the constant says and therefore
// cannot catch a bump that never happened.
func assertSchemaV2(t *testing.T, stdout string) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &top); err != nil {
		t.Fatalf("decoding the envelope: %v\n%s", err, stdout)
	}
	raw, ok := top["schema_version"]
	if !ok {
		t.Fatalf("envelope carries no schema_version:\n%s", stdout)
	}
	var version int
	if err := json.Unmarshal(raw, &version); err != nil {
		t.Fatalf("decoding schema_version: %v", err)
	}
	if version != 2 {
		t.Errorf("schema_version = %d, want 2", version)
	}
	if strings.Contains(stdout, "is_primary") {
		t.Errorf("envelope still carries is_primary:\n%s", stdout)
	}
}

// Every carrier is asserted separately rather than through one shared
// fixture: workspaceInfo covers five of the seven commands, so a single
// assertion would pass for list and rebuild by inheritance and a missed
// carrier would ship silently.
func TestConfigEnvelopeIsSchemaV2(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})

	code, stdout, stderr := run(t, "config", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("config envelope has no repo_root:\n%s", stdout)
	}
}

func TestOpenEnvelopeIsSchemaV2(t *testing.T) {
	ws := openWorkspace(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("open envelope has no repo_root:\n%s", stdout)
	}
}

func TestAttachEnvelopeIsSchemaV2(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := ownLive(ws, ws.SessionName)
	installSessionObserver(t, controller.SessionObservation{
		ByIdentity: &live, ByName: []controller.LiveSession{live},
	}, nil)

	code, stdout, stderr := run(t, "attach", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("attach envelope has no repo_root:\n%s", stdout)
	}
}

func TestStopEnvelopeIsSchemaV2(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	stopFixtureFor(t, ws)
	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("stop envelope has no repo_root:\n%s", stdout)
	}
}

func TestStatusEnvelopeIsSchemaV2(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("status envelope has no repo_root:\n%s", stdout)
	}
}

func TestListEnvelopeIsSchemaV2(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, nil, nil)

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("list rows have no repo_root:\n%s", stdout)
	}
}

func TestRebuildEnvelopeIsSchemaV2(t *testing.T) {
	ws := rebuildEnv(t, fake.NewStore(), nil)
	live := ownLive(ws, ws.SessionName)
	installLiveSessions(t, []controller.LiveSession{live}, nil)
	installScriptedSessions(t, cliLive(live))

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	// The registered array must be non-empty, or the is_primary check
	// above would pass over an envelope that renders no registration at
	// all and proves nothing about rebuildRegistered's fields.
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("rebuild registration has no repo_root:\n%s", stdout)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run 'EnvelopeIsSchemaV2' -v`

Expected: all seven FAIL with `schema_version = 1, want 2`, plus `envelope still carries is_primary` and `has no repo_root` on each.

- [ ] **Step 4: Bump the constant**

In `internal/cli/config.go:19-22`:

```go
// OutputSchemaVersion versions the JSON envelope. Human output is not a
// compatibility contract; this is. Bump it only for a breaking change to the
// structure below.
//
// Version 2 renamed the workspace's path field from "worktree" to
// "repo_root", dropped "is_primary", and added "session". The rename is
// deliberate: a consumer that breaks loudly on a missing field is better
// than one that silently reads a repository root as a worktree path.
const OutputSchemaVersion = 2
```

- [ ] **Step 5: Reshape `workspaceInfo`**

In `internal/cli/config.go:51-57`:

```go
// workspaceInfo is the resolved identity shared by config, open, attach,
// stop, and status. Session is the empty string for a repository's default
// session, and is always emitted: a consumer cannot tell an absent field
// from a default one.
type workspaceInfo struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	RepoRoot    string `json:"repo_root"`
	Session     string `json:"session"`
	SessionName string `json:"session_name"`
}
```

- [ ] **Step 6: Update `config`'s construction and human rendering**

In `internal/cli/config.go:133-139`:

```go
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			RepoRoot:    ws.RepoRoot,
			Session:     ws.Session,
			SessionName: ws.SessionName,
		},
```

and in `writeHuman` (`internal/cli/config.go:162-167`), replacing the `worktree` and `primary` lines:

```go
	fmt.Fprintf(tw, "workspace\t%s\n", env.Workspace.Slug)
	fmt.Fprintf(tw, "repository\t%s\n", env.Workspace.RepoRoot)
	fmt.Fprintf(tw, "id\t%s\n", env.Workspace.ID)
	fmt.Fprintf(tw, "session\t%s\n", env.Workspace.SessionName)
	fmt.Fprintf(tw, "digest\t%s\n", env.Digest)
```

- [ ] **Step 7: Update `open`, `attach`, and `stop`**

The same five-field literal replaces the block at `open.go:83-89`, `attach.go:136-142`, and `stop.go:118-124`:

```go
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			RepoRoot:    ws.RepoRoot,
			Session:     ws.Session,
			SessionName: ws.SessionName,
		},
```

- [ ] **Step 8: Update `status`**

In `internal/cli/status.go:175-181` use the same literal, and in `writeStatusHuman` (`status.go:256-259`) replace the `worktree` and `primary` lines:

```go
	fmt.Fprintf(tw, "workspace\t%s\n", env.Workspace.Slug)
	fmt.Fprintf(tw, "repository\t%s\n", env.Workspace.RepoRoot)
	fmt.Fprintf(tw, "id\t%s\n", env.Workspace.ID)
```

- [ ] **Step 9: Update `list`'s own fields**

In `internal/cli/list.go:36-39`:

```go
type listRow struct {
	ID               string               `json:"id"`
	Slug             string               `json:"slug"`
	RepoRoot         string               `json:"repo_root"`
	Session          string               `json:"session"`
```

at `list.go:114-118`:

```go
		row := listRow{
			ID:              rec.ID,
			Slug:            rec.Slug,
			RepoRoot:        rec.RepoRoot,
			Session:         rec.Session,
			ProposedSession: rec.ProposedSession,
```

and at `list.go:132-134` and `list.go:155-160`, where a live session's recorded path is compared and rendered. `controller.LiveSession.Worktree` keeps its field name — it mirrors the `@dev_worktree` tmux key, which cannot be renamed without stranding running sessions — but its value is now the repository root:

```go
			row.IdentityConflict = s.Slug != rec.Slug || s.Worktree != rec.RepoRoot
```

```go
		env.Workspaces = append(env.Workspaces, listRow{
			ID:               s.WorkspaceID,
			Slug:             s.Slug,
			RepoRoot:         s.Worktree,
			SessionState:     "live",
```

- [ ] **Step 10: Update `rebuild`'s own fields and the `Registered` type behind them**

In `internal/rebuild/apply.go:45-52`:

```go
// Registered is one workspace rebuild recovered.
type Registered struct {
	ID       string
	Slug     string
	RepoRoot string
	Session  string
}
```

and `registeredFor` at `apply.go:321-334`, whose comment existed only to explain `IsPrimary`:

```go
// registeredFor reports the resolver's identity for ws rather than the
// stored row's. The identity gate above requires the two to agree on slug
// and repository root, so the only field that can differ is the session
// name, which is what this run just recorded.
func registeredFor(ws resolve.Workspace, session string) *Registered {
	return &Registered{
		ID:       ws.ID,
		Slug:     ws.Slug,
		RepoRoot: ws.RepoRoot,
		Session:  session,
	}
}
```

Then `internal/cli/rebuild.go:50-56`:

```go
type rebuildRegistered struct {
	ID       string `json:"id"`
	Slug     string `json:"slug"`
	RepoRoot string `json:"repo_root"`
	Session  string `json:"session"`
}
```

and `rebuild.go:124-131`:

```go
	for _, r := range report.Registered {
		env.Registered = append(env.Registered, rebuildRegistered{
			ID:       r.ID,
			Slug:     r.Slug,
			RepoRoot: r.RepoRoot,
			Session:  r.Session,
		})
	}
```

- [ ] **Step 11: Fix the `worktreeResolver` comment that names the dropped field**

`internal/cli/rebuild.go:224-229` claims resolution is "what recovers IsPrimary and the proposed session name". Only half of that still exists:

```go
// worktreeResolver re-derives a workspace's identity the way every other
// command does: from the directory, never from the tmux keys. That is
// what recovers the proposed session name, which tmux does not carry
// (spec §3), and it is what lets rebuild verify the keys it was handed.
```

- [ ] **Step 12: Fix the two stale assertions in `cli_test.go`**

At `cli_test.go:187-189`, `Worktree` no longer exists:

```go
	if env.Workspace.RepoRoot != repo {
		t.Errorf("repo_root = %q, want %q", env.Workspace.RepoRoot, repo)
	}
```

and delete the `IsPrimary` assertion at `cli_test.go:226-228` outright — every resolved workspace is a main worktree by construction, so there is nothing left to assert.

- [ ] **Step 13: Run the new tests**

Run: `go test ./internal/cli/ -run 'EnvelopeIsSchemaV2' -v`

Expected: all seven PASS.

- [ ] **Step 14: Run the packages this task owns**

Run: `go test ./internal/cli/... ./internal/rebuild/...`

Expected: PASS. The `rebuild` and `list` package tests that construct `Registered`/`listRow` literals are updated in place if the compiler reports them.

The gate is scoped rather than run as `go test ./...` because `internal/doctor`'s own tests
still read the dropped fields until Task 9 — see "Task Ordering and Interactions" above.
Everything else in the tree builds from the end of Task 5a and passes from the end of Task 5b, so this line covers
every package whose behaviour this task can change.

Those two packages are exactly this task's Files block: every carrier it edits lives in
`internal/cli`, and `internal/rebuild/apply.go`'s `Registered` is the one type it reshapes outside
it. `internal/rebuild`'s own tests are converted by Step 1 of this task, which is why the gate can
include them; `internal/doctor`'s are not converted until Task 9, which is the only thing keeping
`go test ./...` off this line.

- [ ] **Step 15: Update the envelope documentation**

In `docs/commands.md`, the `--json` paragraph at 29-32 gains the version and its meaning:

```markdown
**`--json` and `--compact`.** Every command that produces a report accepts
`--json`, which emits a versioned envelope carrying `schema_version`, and
`--compact`, which puts that envelope on one line and implies `--json`. The
current version is **2**: it renamed the workspace's path field from
`worktree` to `repo_root`, dropped `is_primary`, and added `session`. A
consumer of version 1 breaks loudly on the missing field, which is the point
— a repository root read as a worktree path would be silently wrong.
```

Replace `worktree`/`primary` with `repository` in the `config` sample (85-89) and the `status` sample (193-197), and reword the identity paragraph at 100-104 so it describes a repository root and a session rather than a worktree and a primary flag.

- [ ] **Step 16: Format and commit**

Run: `gofmt -l internal docs 2>/dev/null; gofmt -w internal`

Then:

```
git commit -m "feat(cli): move the JSON envelopes to schema_version 2

workspaceInfo is shared by config, open, attach, stop, and status, and
list and rebuild declare the same two fields independently, so one bump
of OutputSchemaVersion covers all seven carriers. worktree becomes
repo_root, is_primary is gone, and session is new.

The rename is deliberate rather than a reuse of worktree with a new
meaning: a consumer that breaks loudly beats one that silently reads a
repository root as a worktree path. Each carrier gets its own test, so a
missed one fails rather than passing by inheritance from workspaceInfo."
```

---

### Task 8: `rebuild` collapses linked-worktree rows

**Files:**
- Create: `internal/rebuild/migrate.go`
- Create: `internal/rebuild/migrate_test.go`
- Create: `internal/cli/rebuild_migration_test.go`
- Modify: `internal/rebuild/apply.go:16-21` (`Resolver`), `internal/rebuild/apply.go:38-43` (`Locker`), `internal/rebuild/apply.go:54-60` (`Report`), `internal/rebuild/apply.go:62-73` (`Applier`)
- Modify: `internal/state/store.go` (add `DropRepository` after `RegisterWorkspace`, `:29-56`)
- Modify: `internal/tmux/actuate.go` (add `RetagSession` after `KillSession`, `:161-177`)
- Modify: `internal/tmux/actuate_test.go` (add `TestRetagSessionRewritesTheIdentityKeys`, and `"path/filepath"` to the imports)
- Modify: `internal/controller/fake/fake.go:92-99` (add `DropRepository`)
- Modify: `internal/cli/wiring.go:24-46` (`stateStore`, session seams)
- Modify: `internal/cli/wiring_test.go:24-58` (`guardedStore`)
- Modify: `internal/cli/rebuild.go:41-48`, `:116-139`, `:143-163`, `:166-220`, `:221-232`, `:245-253`
- Modify: `internal/cli/status.go:26-37`, `:148-169`, `:253-259`
- Modify: `docs/commands.md:384-412` (the rebuild section)
- Test: `internal/rebuild/migrate_test.go`, `internal/cli/rebuild_migration_test.go`, `internal/cli/status_test.go`

**Interfaces:**
- Consumes: `resolve.Resolve(name string, roots []string, cwd string) (resolve.Workspace, error)` returning a `RepoRoot` that is always a main worktree (Task 1); `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}`; `func (s *state.Store) Repositories() ([]state.Repository, error)` and `state.Repository{ID, Slug, RepoRoot, RegisteredAt, UpdatedAt, Container}` (Task 2), mirrored on `fake.Store`; `lock.Acquire(ctx, dir, key string, timeout time.Duration) (*lock.Lock, error)` (Task 3); `rebuild.Registered{ID, Slug, RepoRoot, Session}` and `OutputSchemaVersion == 2` (Task 7).
- Produces: `rebuild.MigrationStore`, `rebuild.Retagger`, `rebuild.Migrated{Subject, Action, Into}`, `rebuild.MigrationResult{Live, Migrated, Conflicts}`, `func (a *Applier) Migrate(ctx context.Context, live []controller.LiveSession) MigrationResult`; `func (s *state.Store) DropRepository(id string) error`; `func (c *tmux.Client) RetagSession(ctx context.Context, target, workspaceID, repoRoot string) error`; `cli.statusEnvelope.NeedsRebuild`.

Migration 0002 moved rows verbatim, treating every stored path as a repository root — deliberately over-counted but never wrong, because classifying a main worktree needs git and a schema migration must never fail on a filesystem that changed (design §9). This task is the correction pass.

- [ ] **Step 1: Write the failing test for collapsing a stored row**

Create `internal/rebuild/migrate_test.go`:

```go
package rebuild

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// migrateStore is a MigrationStore over literals. The package's other
// tests use fakes for the same reason: the case analysis is the part most
// likely to be wrong, and it is exhaustively testable without SQLite.
type migrateStore struct {
	repos   []state.Repository
	dropped []string
	err     error
}

func (m *migrateStore) Repositories() ([]state.Repository, error) {
	return m.repos, m.err
}

func (m *migrateStore) DropRepository(id string) error {
	m.dropped = append(m.dropped, id)
	return nil
}

// mapResolver resolves recorded paths from a table. A path absent from
// both maps is one the filesystem no longer has.
type mapResolver struct {
	roots  map[string]resolve.Workspace
	exists map[string]bool
}

func (r mapResolver) Resolve(repoRoot string) (resolve.Workspace, error) {
	ws, ok := r.roots[repoRoot]
	if !ok {
		return resolve.Workspace{}, errors.New("no such directory: " + repoRoot)
	}
	return ws, nil
}

func (r mapResolver) Exists(path string) bool { return r.exists[path] }

type recordingRetagger struct {
	calls [][3]string
	err   error
}

func (r *recordingRetagger) RetagSession(_ context.Context, target, workspaceID, repoRoot string) error {
	r.calls = append(r.calls, [3]string{target, workspaceID, repoRoot})
	return r.err
}

type nopLocker struct{}

func (nopLocker) Lock(context.Context, string) (func(), error) { return func() {}, nil }

type fixedDigest struct{}

func (fixedDigest) Digest(string) (string, error) { return "sha256:d", nil }

type registerRecorder struct {
	Store
	registered []resolve.Workspace
}

func (r *registerRecorder) RegisterWorkspace(ws resolve.Workspace, _ string, _ time.Time) error {
	r.registered = append(r.registered, ws)
	return nil
}

func repoWorkspace(root string) resolve.Workspace {
	return resolve.Workspace{
		ID:           "ws-" + root,
		RepositoryID: "repo-" + root,
		Slug:         "slabledger",
		RepoRoot:     root,
		SessionName:  "slabledger",
	}
}

// A row recorded at a linked worktree resolves to its parent repository
// and collapses into it. This is the state migration 0002 leaves behind
// for every worktree that was its own workspace before the change.
func TestMigrateCollapsesALinkedWorktreeRow(t *testing.T) {
	parent := repoWorkspace("/repo")
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/repo/.worktrees/1529", Slug: "slabledger", RepoRoot: "/repo/.worktrees/1529"},
		{ID: "repo-/repo", Slug: "slabledger", RepoRoot: "/repo"},
	}}
	registrar := &registerRecorder{}
	a := &Applier{
		Store:  registrar,
		Repos:  store,
		Config: fixedDigest{},
		Locker: nopLocker{},
		Clock:  &fixedClock{},
		Resolver: mapResolver{
			roots: map[string]resolve.Workspace{
				"/repo":                  parent,
				"/repo/.worktrees/1529":  parent,
			},
			exists: map[string]bool{"/repo": true, "/repo/.worktrees/1529": true},
		},
	}

	res := a.Migrate(context.Background(), nil)

	if len(res.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", res.Conflicts)
	}
	if len(registrar.registered) != 1 || registrar.registered[0].RepoRoot != "/repo" {
		t.Fatalf("registered = %+v, want one registration at /repo", registrar.registered)
	}
	// Registration first, drop second: a crash between them leaves an
	// extra row that resolves to the same repository, which the next run
	// merges. The other order loses the row outright.
	if len(store.dropped) != 1 || store.dropped[0] != "repo-/repo/.worktrees/1529" {
		t.Fatalf("dropped = %v, want the linked-worktree row", store.dropped)
	}
	if len(res.Migrated) != 1 || res.Migrated[0].Action != "collapsed" ||
		res.Migrated[0].Into != "/repo" {
		t.Fatalf("migrated = %+v", res.Migrated)
	}
}

// A row whose recorded path is gone is dropped rather than carried
// forward. Nothing resolves it, so nothing can correct it.
func TestMigrateDropsAVanishedRow(t *testing.T) {
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/gone", Slug: "gone", RepoRoot: "/gone"},
	}}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    store,
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    &fixedClock{},
		Resolver: mapResolver{roots: map[string]resolve.Workspace{}, exists: map[string]bool{}},
	}

	res := a.Migrate(context.Background(), nil)

	if len(store.dropped) != 1 || store.dropped[0] != "repo-/gone" {
		t.Fatalf("dropped = %v, want the vanished row", store.dropped)
	}
	if len(res.Migrated) != 1 || res.Migrated[0].Action != "dropped" {
		t.Fatalf("migrated = %+v", res.Migrated)
	}
}

// A path that still exists but will not resolve is a conflict, not a
// drop. Deleting a row because git happened to fail would discard state
// the operator can still recover.
func TestMigrateRefusesAnUnresolvableExistingPath(t *testing.T) {
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/broken", Slug: "broken", RepoRoot: "/broken"},
	}}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    store,
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    &fixedClock{},
		Resolver: mapResolver{roots: map[string]resolve.Workspace{}, exists: map[string]bool{"/broken": true}},
	}

	res := a.Migrate(context.Background(), nil)

	if len(store.dropped) != 0 {
		t.Errorf("dropped = %v; an existing path must not be discarded", store.dropped)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", res.Conflicts)
	}
}

// A live session created before the change carries @dev_worktree pointing
// at a linked worktree and a workspace ID derived from it. Both are
// rewritten in place, because the alternative — treating the ID mismatch
// as a different workspace — registers a second row for a session that is
// already running.
func TestMigrateRetagsALiveSessionOntoItsRepository(t *testing.T) {
	parent := repoWorkspace("/repo")
	retagger := &recordingRetagger{}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    &migrateStore{},
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    &fixedClock{},
		Retagger: retagger,
		Resolver: mapResolver{
			roots:  map[string]resolve.Workspace{"/repo/.worktrees/1529": parent},
			exists: map[string]bool{"/repo/.worktrees/1529": true},
		},
	}

	res := a.Migrate(context.Background(), []controller.LiveSession{{
		Name: "slabledger--1529", WorkspaceID: "old-id",
		Slug: "slabledger", Worktree: "/repo/.worktrees/1529",
	}})

	if len(retagger.calls) != 1 {
		t.Fatalf("retag calls = %v, want one", retagger.calls)
	}
	if got := retagger.calls[0]; got[0] != "slabledger--1529" ||
		got[1] != parent.ID || got[2] != "/repo" {
		t.Errorf("retag = %v, want the session retagged onto %s", got, parent.ID)
	}
	// The corrected session is what classification must see; the stale
	// one would classify as an unrelated workspace and register a second
	// row for a session already running.
	if len(res.Live) != 1 || res.Live[0].WorkspaceID != parent.ID ||
		res.Live[0].Worktree != "/repo" {
		t.Fatalf("live = %+v", res.Live)
	}
	if len(res.Migrated) != 1 || res.Migrated[0].Action != "retagged" {
		t.Fatalf("migrated = %+v", res.Migrated)
	}
}

// A session whose keys already agree is left alone: a second run must be
// a silent no-op, the same property Classify's settled case protects.
func TestMigrateLeavesACorrectSessionUntouched(t *testing.T) {
	parent := repoWorkspace("/repo")
	retagger := &recordingRetagger{}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    &migrateStore{},
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    &fixedClock{},
		Retagger: retagger,
		Resolver: mapResolver{
			roots:  map[string]resolve.Workspace{"/repo": parent},
			exists: map[string]bool{"/repo": true},
		},
	}

	res := a.Migrate(context.Background(), []controller.LiveSession{{
		Name: "slabledger", WorkspaceID: parent.ID,
		Slug: parent.Slug, Worktree: "/repo",
	}})

	if len(retagger.calls) != 0 {
		t.Errorf("retag calls = %v, want none", retagger.calls)
	}
	if len(res.Migrated) != 0 {
		t.Errorf("migrated = %+v, want nothing reported", res.Migrated)
	}
}

// --dry-run predicts the pass without performing it, matching Apply's
// contract that a preview reports exactly what the real run would do.
func TestMigrateDryRunWritesNothing(t *testing.T) {
	parent := repoWorkspace("/repo")
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/repo/.worktrees/1529", Slug: "slabledger", RepoRoot: "/repo/.worktrees/1529"},
	}}
	registrar := &registerRecorder{}
	retagger := &recordingRetagger{}
	a := &Applier{
		Store:    registrar,
		Repos:    store,
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    &fixedClock{},
		Retagger: retagger,
		DryRun:   true,
		Resolver: mapResolver{
			roots:  map[string]resolve.Workspace{"/repo/.worktrees/1529": parent},
			exists: map[string]bool{"/repo/.worktrees/1529": true},
		},
	}

	res := a.Migrate(context.Background(), []controller.LiveSession{{
		Name: "slabledger--1529", WorkspaceID: "old-id",
		Slug: "slabledger", Worktree: "/repo/.worktrees/1529",
	}})

	if len(store.dropped) != 0 || len(registrar.registered) != 0 || len(retagger.calls) != 0 {
		t.Errorf("dry run wrote: dropped %v, registered %+v, retagged %v",
			store.dropped, registrar.registered, retagger.calls)
	}
	if len(res.Migrated) != 2 {
		t.Errorf("migrated = %+v, want the collapse and the retag predicted", res.Migrated)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/rebuild/ -run TestMigrate -v`

Expected: build FAILS with `a.Migrate undefined`, `unknown field Repos in struct literal of type Applier`, `unknown field Retagger`, and `mapResolver does not implement Resolver (missing method Exists)`.

- [ ] **Step 3: Widen `Resolver` and `Locker`, and add the two new dependencies to `Applier`**

In `internal/rebuild/apply.go:16-21`:

```go
// Resolver derives a workspace from a repository root. It is an interface
// rather than a direct call to resolve.Resolve so application is testable
// without a real git repository.
//
// Exists is separate from Resolve's error because the two failures call
// for opposite actions: a path the filesystem no longer has is a row to
// drop, while a path that is present but will not resolve is a refusal.
// Resolve reports both as a plain error, so the distinction has to be
// asked for.
type Resolver interface {
	Resolve(repoRoot string) (resolve.Workspace, error)
	Exists(path string) bool
}
```

In `apply.go:38-43`, the key is no longer always a workspace ID:

```go
// Locker takes a filesystem lock and returns its release. The release is a
// plain func so the caller cannot forget which lock it belongs to. The key
// is a workspace ID for session work and a repository ID for work that is
// shared by every session on a repository.
type Locker interface {
	Lock(ctx context.Context, key string) (func(), error)
}
```

and in `apply.go:65-73`:

```go
type Applier struct {
	Store    Store
	Repos    MigrationStore
	Sessions controller.SessionObserver
	Retagger Retagger
	Resolver Resolver
	Config   ConfigLoader
	Locker   Locker
	Clock    controller.Clock
	DryRun   bool
}
```

- [ ] **Step 4: Write `internal/rebuild/migrate.go`**

```go
package rebuild

import (
	"context"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// MigrationStore is the slice of the state store the upgrade pass writes
// through. It is separate from Store because the two answer to different
// invariants: Store is fill-only, and this one deletes.
type MigrationStore interface {
	Repositories() ([]state.Repository, error)
	DropRepository(id string) error
}

// Retagger rewrites the identity keys of a live tmux session.
type Retagger interface {
	RetagSession(ctx context.Context, target, workspaceID, repoRoot string) error
}

// Migrated is one correction the upgrade pass made. Action is
// "collapsed", "dropped", or "retagged"; Into is the repository root the
// subject now belongs to, and is empty for a drop.
type Migrated struct {
	Subject string
	Action  string
	Into    string
}

// MigrationResult is the pass's output. Live carries the sessions
// classification should see — corrected where they were stale — rather
// than the ones tmux reported.
type MigrationResult struct {
	Live      []controller.LiveSession
	Migrated  []Migrated
	Conflicts []Conflict
}

// Migrate completes what migration 0002 could not. 0002 is pure SQL and
// moved every stored row verbatim, treating each recorded path as a
// repository root, because telling a main worktree from a linked one
// requires git and a schema migration must never fail on a filesystem
// that changed since the last run (design §9). The result is
// over-counted, never wrong: extra repository rows that resolve to the
// same real repository. This is the pass that merges them.
//
// It runs before classification, not as part of it, because both halves
// change the inputs Classify reads: rows move, and live sessions change
// the workspace ID they claim.
func (a *Applier) Migrate(ctx context.Context, live []controller.LiveSession) MigrationResult {
	res := MigrationResult{Live: live}
	a.collapseRows(ctx, &res)
	a.retagSessions(ctx, &res)
	return res
}

// collapseRows folds every repository row that is really a linked
// worktree into its parent, and drops the rows whose path is gone.
func (a *Applier) collapseRows(ctx context.Context, res *MigrationResult) {
	repos, err := a.Repos.Repositories()
	if err != nil {
		res.Conflicts = append(res.Conflicts, Conflict{
			Subject: "repositories",
			Reason: "reading the stored repositories failed: " + err.Error() +
				"; nothing was migrated",
		})
		return
	}

	for _, repo := range repos {
		ws, resolveErr := a.Resolver.Resolve(repo.RepoRoot)
		if resolveErr != nil {
			if a.Resolver.Exists(repo.RepoRoot) {
				// The directory is there and git would not answer for it.
				// Dropping the row here would discard state over a
				// transient failure, so this refuses instead.
				res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
					"resolving the repository failed: %v; the directory still exists, "+
						"so the row is kept and nothing was written", resolveErr))
				continue
			}
			res.Migrated = append(res.Migrated, Migrated{
				Subject: repo.RepoRoot, Action: "dropped",
			})
			if a.DryRun {
				continue
			}
			if err := a.Repos.DropRepository(repo.ID); err != nil {
				res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
					"dropping the repository whose path is gone failed: %v", err))
			}
			continue
		}
		if ws.RepoRoot == repo.RepoRoot {
			// Already a main worktree. Deliberately silent, so a second
			// run over a migrated installation reports nothing.
			continue
		}
		a.collapseInto(ctx, repo, ws, res)
	}
}

// collapseInto registers the parent repository and then drops the stale
// row. The order is load-bearing: a crash between the two leaves an extra
// row that resolves to the same repository — the over-counted state this
// pass already knows how to merge — where the other order would lose the
// registration outright.
func (a *Applier) collapseInto(ctx context.Context, repo state.Repository, ws resolve.Workspace, res *MigrationResult) {
	res.Migrated = append(res.Migrated, Migrated{
		Subject: repo.RepoRoot, Action: "collapsed", Into: ws.RepoRoot,
	})
	if a.DryRun {
		return
	}

	digest, err := a.Config.Digest(ws.Slug)
	if err != nil {
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"loading the configuration for %q failed: %v; nothing was written",
			ws.Slug, err))
		return
	}

	// The repository lock, not the workspace lock: this rewrites rows
	// every session on the repository shares. It is taken and released
	// here, before Apply takes any workspace lock, so the plan's global
	// repository-then-workspace ordering holds without nesting.
	release, err := a.Locker.Lock(ctx, ws.RepositoryID)
	if err != nil {
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"taking the repository lock: %v", err))
		return
	}
	defer release()

	if err := a.Store.RegisterWorkspace(ws, digest, a.Clock.Now()); err != nil {
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"registering the parent repository %s: %v", ws.RepoRoot, err))
		return
	}
	if err := a.Repos.DropRepository(repo.ID); err != nil {
		res.Conflicts = append(res.Conflicts, conflictAt(repo.RepoRoot,
			"the parent repository %s was registered, but dropping the "+
				"linked-worktree row failed: %v; a later rebuild completes it",
			ws.RepoRoot, err))
	}
}

// retagSessions points sessions created before the change at their
// repository.
//
// Existing workspace IDs change even for repositories that were already
// main worktrees, because the session name is now an input to the hash.
// The ID lives on the session as @dev_workspace_id, so a mismatch alone
// says nothing about whether this is the same workspace. The session is
// therefore matched by @dev_worktree — the one key whose value identifies
// a tree rather than a derivation — and the ID is rewritten rather than
// read as evidence of a different workspace.
//
// @dev_worktree keeps its name. Renaming it would strand every running
// session from the rebuild that is supposed to recover it (design §5.1);
// only its value changes, to the repository root.
func (a *Applier) retagSessions(ctx context.Context, res *MigrationResult) {
	for i, sess := range res.Live {
		if sess.WorkspaceID == "" {
			continue
		}
		ws, err := a.Resolver.Resolve(sess.Worktree)
		if err != nil {
			// A session whose tree is gone is not this pass's problem:
			// applyCandidate already reports it, with the reason an
			// operator needs.
			continue
		}
		if sess.WorkspaceID == ws.ID && sess.Worktree == ws.RepoRoot {
			continue
		}

		res.Migrated = append(res.Migrated, Migrated{
			Subject: sess.Name, Action: "retagged", Into: ws.RepoRoot,
		})
		if a.DryRun {
			continue
		}

		release, err := a.Locker.Lock(ctx, ws.ID)
		if err != nil {
			res.Conflicts = append(res.Conflicts, conflictAt(sess.Name,
				"taking the workspace lock: %v", err))
			continue
		}
		err = a.Retagger.RetagSession(ctx, sess.Name, ws.ID, ws.RepoRoot)
		release()
		if err != nil {
			res.Conflicts = append(res.Conflicts, conflictAt(sess.Name,
				"rewriting the identity keys of session %q failed: %v; the session "+
					"keeps its old keys and a later rebuild retries", sess.Name, err))
			continue
		}
		res.Live[i].WorkspaceID = ws.ID
		res.Live[i].Worktree = ws.RepoRoot
	}
}

func conflictAt(subject, format string, args ...any) Conflict {
	return *conflictf(subject, format, args...)
}
```

Add the `resolve` import to the file's import block:

```go
import (
	"context"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)
```

- [ ] **Step 5: Run the migration tests**

Run: `go test ./internal/rebuild/ -run TestMigrate -v`

Expected: PASS. If `fixedClock` is not already defined in the package's test files, add it to `migrate_test.go`:

```go
type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) }
```

- [ ] **Step 6: Run the rest of the rebuild package**

Run: `go test ./internal/rebuild/`

Expected: build FAILS in `apply_test.go` — its resolver fakes do not implement `Exists`. Add the method to each, returning `true`, since those tests are about resolution rather than about a vanished path.

- [ ] **Step 7: Add `DropRepository` to the real store**

In `internal/state/store.go`, after `RegisterWorkspace` (`:29-56`):

```go
// DropRepository removes a repository and everything keyed to it. The
// workspaces, container binding, and last operations go with it: every
// foreign key into repositories cascades (migration 0002), and the
// connection string enables foreign_keys (state.go:65).
//
// Dropping an id that is not there succeeds. Rebuild is re-runnable, and
// a second pass over an already-migrated installation must be a no-op
// rather than an error.
func (s *Store) DropRepository(id string) error {
	if _, err := s.db.Exec(`DELETE FROM repositories WHERE id = ?`, id); err != nil {
		return fmt.Errorf("dropping repository %s: %w", id, err)
	}
	return nil
}
```

- [ ] **Step 8: Write the failing test for `RetagSession`**

In `internal/tmux/actuate_test.go`, add. Note the seam: this package has no injectable runner — `Client`'s only fields are `Socket` and `Timeout` (`internal/tmux/tmux.go:62-65`) — so every test here swaps the package-global `tmuxBinary` for a shell script through `fakeTmux` (`internal/tmux/client_test.go:14-23`), the way `TestKillSessionSuccessAndIdempotency` does (`actuate_test.go:375-391`). Recording argv therefore means having the script append it to a file. Add `"path/filepath"` to the import block; `context`, `fmt`, `os`, `strings`, `testing` and the `controller` import are already there.

```go
// TestRetagSessionRewritesTheIdentityKeys asserts both calls and, more
// importantly, their order. The script logs one line per invocation, so
// the log doubles as a call count: a chained single command or a reversed
// pair both fail here.
func TestRetagSessionRewritesTheIdentityKeys(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "argv.log")
	fakeTmux(t, fmt.Sprintf("#!/bin/sh\necho \"$@\" >> %s\nexit 0\n", logPath))

	if err := (&Client{}).RetagSession(context.Background(), "slabledger--1529",
		"new-id", "/repo"); err != nil {
		t.Fatalf("RetagSession: %v", err)
	}

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading the argv log: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(body)), "\n")
	if len(calls) != 2 {
		t.Fatalf("calls = %q, want two set-option invocations", calls)
	}
	// The worktree is written first. If the second call fails the session
	// keeps its old workspace ID, so the next rebuild still matches it by
	// @dev_worktree and retries; the other order leaves a session no
	// lookup can find.
	//
	// Socket is empty here, so no -L argument precedes the subcommand.
	// Every argument is space-free, so the script's `echo "$@"` is a
	// faithful rendering of argv.
	want := []string{
		"set-option -t slabledger--1529 " + controller.KeyWorktree + " /repo",
		"set-option -t slabledger--1529 " + controller.KeyWorkspaceID + " new-id",
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("call %d = %q, want %q", i, calls[i], w)
		}
	}
}
```

- [ ] **Step 9: Run it to verify it fails**

Run: `go test ./internal/tmux/ -run TestRetagSession -v`

Expected: build FAILS with `(&Client{}).RetagSession undefined (type *Client has no field or method RetagSession)`.

- [ ] **Step 10: Implement `RetagSession`**

In `internal/tmux/actuate.go`, after `KillSession` (`:161-177`):

```go
// RetagSession rewrites a live session's identity keys so that a session
// created before repositories became the unit of a workspace is adopted
// rather than duplicated (design §9).
//
// Two invocations rather than one chained command, and in this order:
// @dev_worktree is what rebuild matches a stale session by, so it is the
// key that must never be left pointing somewhere the ID no longer agrees
// with. A failure after the first call leaves a session whose worktree is
// correct and whose ID is stale — exactly the state this function already
// knows how to fix on the next run.
//
// target is the plain session name: set-option's -t is not a
// target-session, so it rejects the "=" exact-match prefix (verified on
// tmux 3.4, see createArgv).
func (c *Client) RetagSession(ctx context.Context, target, workspaceID, repoRoot string) error {
	for _, kv := range [][2]string{
		{controller.KeyWorktree, repoRoot},
		{controller.KeyWorkspaceID, workspaceID},
	} {
		res, err := c.exec(ctx, "set-option", "-t", target, kv[0], kv[1])
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("tmux set-option %s exited %d: %s",
				kv[0], res.ExitCode, bytes.TrimSpace(res.Stderr))
		}
	}
	return nil
}
```

- [ ] **Step 11: Run the tmux tests**

Run: `go test ./internal/tmux/`

Expected: PASS.

- [ ] **Step 12: Add `DropRepository` to the fake store and forbid it on the guarded one**

In `internal/controller/fake/fake.go`, after `Workspaces` (`:248-262`):

```go
// DropRepository removes every workspace belonging to a repository,
// mirroring the cascade the real schema performs (migration 0002).
func (s *Store) DropRepository(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for wsID, rec := range s.records {
		if rec.RepositoryID == id {
			delete(s.records, wsID)
		}
	}
	return nil
}
```

and in `internal/cli/wiring_test.go`, beside the other refusals (`:36-58`):

```go
func (g guardedStore) DropRepository(string) error {
	return g.forbidden("DropRepository")
}
```

- [ ] **Step 13: Widen the CLI's store interface and add the retag seam**

In `internal/cli/wiring.go:24-46`:

```go
// stateStore is what the commands need from the state store. Rebuild's
// migration pass deletes, which nothing else does, so its slice is named
// separately rather than folded into controller.Store.
type stateStore interface {
	controller.Store
	rebuild.MigrationStore
	Close() error
}
```

and beside `newSessionActuator` (`:83-86`):

```go
// newSessionRetagger is the mutation seam for rewriting a live session's
// identity keys, used only by rebuild's upgrade pass.
var newSessionRetagger = func() rebuild.Retagger {
	return &tmux.Client{Socket: tmux.EnvSocket()}
}
```

Add `"github.com/gambtho/projectmux/internal/rebuild"` to the file's imports.

- [ ] **Step 14: Report migrations in the rebuild envelope**

In `internal/cli/rebuild.go:41-48`:

```go
type rebuildEnvelope struct {
	SchemaVersion int                 `json:"schema_version"`
	DryRun        bool                `json:"dry_run"`
	Migrated      []rebuildMigrated   `json:"migrated"`
	Registered    []rebuildRegistered `json:"registered"`
	Conflicts     []rebuildConflict   `json:"conflicts"`
}

// rebuildMigrated is one correction the upgrade pass made to state
// migration 0002 left structurally valid but semantically stale.
type rebuildMigrated struct {
	Subject string `json:"subject"`
	Action  string `json:"action"`
	Into    string `json:"into,omitempty"`
}
```

In `rebuildEnvelopeFrom` (`:116-139`), initialize `Migrated: []rebuildMigrated{}` beside the other two always-arrays and copy the entries:

```go
	for _, m := range report.Migrated {
		env.Migrated = append(env.Migrated, rebuildMigrated{
			Subject: m.Subject, Action: m.Action, Into: m.Into,
		})
	}
```

In `writeRebuildHuman` (`:143-163`), migrations print before registrations, since a collapse explains the registration that follows it:

```go
	if len(env.Registered) == 0 && len(env.Conflicts) == 0 && len(env.Migrated) == 0 {
		fmt.Fprintln(w, "nothing to rebuild: every live session is already recorded")
		return nil
	}
```

```go
	for _, m := range env.Migrated {
		fmt.Fprintln(tw, cells(m.Action, m.Subject, m.Into))
	}
```

And add `Migrated []Migrated` to `rebuild.Report` (`apply.go:54-60`), non-nil like the other two.

- [ ] **Step 15: Run the migration pass from `buildRebuild`**

In `internal/cli/rebuild.go:166-220`, the applier is built before the records are read, and the records are read after the pass rewrites them:

```go
	applier := &rebuild.Applier{
		Store:    st,
		Repos:    st,
		Sessions: newSessionObserver(),
		Retagger: newSessionRetagger(),
		Resolver: worktreeResolver{},
		Config:   configDigests{root: configRoot, defaults: defaults},
		Locker:   stateLocker{dir: filepath.Join(stateRoot, "locks")},
		Clock:    systemClock{},
		DryRun:   dryRun,
	}

	// The upgrade pass runs before the records are read: it moves rows and
	// rewrites the workspace IDs live sessions claim, so a classification
	// over the pre-pass state would register duplicates for sessions that
	// are already running (design §9).
	migration := applier.Migrate(ctx, live)

	records, err := st.Workspaces()
	if err != nil {
		return rebuild.Report{}, fmt.Errorf("reading stored workspaces: %w", err)
	}

	report := applier.Apply(ctx, rebuild.Classify(migration.Live, records))
	report.Migrated = migration.Migrated
	report.Conflicts = append(report.Conflicts, migration.Conflicts...)
	return report, nil
```

Rename `workspaceLocker` to `stateLocker` at `:245-253` and update its comment, since it now takes repository locks too:

```go
// stateLocker is the filesystem lock every mutating command takes before
// its final observation and holds through the resulting state commit. The
// key is a workspace ID or a repository ID depending on what is being
// changed; the lock directory is the same either way.
type stateLocker struct{ dir string }

func (w stateLocker) Lock(ctx context.Context, key string) (func(), error) {
	l, err := lock.Acquire(ctx, w.dir, key, lockTimeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}
```

- [ ] **Step 16: Give `worktreeResolver` its `Exists` method**

In `internal/cli/rebuild.go:221-232`:

```go
func (worktreeResolver) Resolve(repoRoot string) (resolve.Workspace, error) {
	// No name and no roots: roots feed only lookup by name, and rebuild
	// resolves from a directory.
	return resolve.Resolve("", nil, repoRoot)
}

// Exists separates "the directory is gone" from "git would not answer",
// which Resolve reports identically. A row whose path is gone is dropped;
// a row whose path is present is never discarded on a resolution failure.
func (worktreeResolver) Exists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
```

Add `"os"` to the file's imports.

- [ ] **Step 17: Write the failing end-to-end migration test**

Create `internal/cli/rebuild_migration_test.go`:

```go
package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
)

// linkedWorktree adds a linked worktree to the repository the test is
// running in and returns its canonical path.
func linkedWorktree(t *testing.T, name string) string {
	t.Helper()
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	path := filepath.Join(repo, ".worktrees", name)
	cmd := exec.Command("git", "worktree", "add", "-b", name, path)
	cmd.Dir = repo
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v\n%s", err, out)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	return canonical
}

// TestRebuildCollapsesAMigratedLinkedWorktreeRow drives the second half
// of the documented upgrade against a real repository, a real linked
// worktree, and the real SQLite store. Migration 0002 moves rows verbatim
// and cannot tell the two apart; this is the run that corrects it.
func TestRebuildCollapsesAMigratedLinkedWorktreeRow(t *testing.T) {
	ws := openWorkspace(t)
	worktree := linkedWorktree(t, "1529")

	root, err := state.Root()
	if err != nil {
		t.Fatalf("state.Root: %v", err)
	}
	st, err := state.Open(root)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	// Exactly what 0002 leaves behind: a repository row whose recorded
	// root is a linked worktree. It is registered through the ordinary
	// primitive with a hand-built identity, because no resolver will
	// produce one after Task 1.
	stale := resolve.Workspace{
		ID:           "stale-workspace-id",
		RepositoryID: "stale-repository-id",
		Slug:         ws.Slug,
		RepoRoot:     worktree,
		SessionName:  ws.Slug + "--1529",
	}
	if err := st.RegisterWorkspace(stale, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	installLiveSessions(t, nil, nil)
	installScriptedSessions(t)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}
	if !strings.Contains(stdout, `"collapsed"`) {
		t.Errorf("report does not record the collapse:\n%s", stdout)
	}

	st, err = state.Open(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = st.Close() }()
	repos, err := st.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("repositories = %+v, want one", repos)
	}
	if repos[0].RepoRoot != ws.RepoRoot {
		t.Errorf("repo_root = %q, want the main worktree %q", repos[0].RepoRoot, ws.RepoRoot)
	}
}

// TestRebuildRetagsALiveSessionOntoItsRepository drives the retag against
// a real tmux server on its own socket, the isolation the override exists
// for (socket_integration_test.go). The session carries the keys a
// pre-change projectmux wrote: an ID derived from the linked worktree,
// and @dev_worktree pointing at it.
func TestRebuildRetagsALiveSessionOntoItsRepository(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	ws := openWorkspace(t)
	worktree := linkedWorktree(t, "1529")

	socket := "projectmux-migrate-" + t.Name()
	t.Cleanup(func() { _ = exec.Command("tmux", "-L", socket, "kill-server").Run() })
	tmuxRun := func(args ...string) {
		t.Helper()
		full := append([]string{"-L", socket}, args...)
		if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}
	name := ws.Slug + "--1529"
	tmuxRun("new-session", "-d", "-s", name)
	tmuxRun("set-option", "-t", name, controller.KeyWorkspaceID, "stale-workspace-id")
	tmuxRun("set-option", "-t", name, controller.KeySlug, ws.Slug)
	tmuxRun("set-option", "-t", name, controller.KeyWorktree, worktree)
	t.Setenv(tmux.SocketEnv, socket)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit %d\nstdout: %s\nstderr: %s", code, stdout, stderr)
	}

	got := func(key string) string {
		t.Helper()
		out, err := exec.Command("tmux", "-L", socket,
			"show-options", "-v", "-t", name, key).Output()
		if err != nil {
			t.Fatalf("show-options %s: %v", key, err)
		}
		return strings.TrimSpace(string(out))
	}
	if got(controller.KeyWorkspaceID) != ws.ID {
		t.Errorf("@dev_workspace_id = %q, want the repository's ID %q",
			got(controller.KeyWorkspaceID), ws.ID)
	}
	if got(controller.KeyWorktree) != ws.RepoRoot {
		t.Errorf("@dev_worktree = %q, want the repository root %q",
			got(controller.KeyWorktree), ws.RepoRoot)
	}
	if !strings.Contains(stdout, `"retagged"`) {
		t.Errorf("report does not record the retag:\n%s", stdout)
	}
}
```

- [ ] **Step 18: Run the end-to-end tests**

Run: `go test ./internal/cli/ -run 'TestRebuild(Collapses|Retags)' -v`

Expected: PASS. `openWorkspace` already points `PROJECTMUX_STATE_ROOT` at a temp directory, so both tests use a real database and a real lock directory without touching the developer's installation.

- [ ] **Step 19: Write the failing status test**

In `internal/cli/status_test.go`:

```go
// A repository still recorded at a linked worktree is what migration 0002
// leaves behind, and only rebuild can correct it. Status is where an
// operator looks first, so status is where it has to say so.
func TestStatusReportsAStaleRepositoryRootAsNeedingRebuild(t *testing.T) {
	ws := statusWorkspace(t)
	worktree := linkedWorktree(t, "1529")
	s := fake.NewStore()
	if err := s.RegisterWorkspace(resolve.Workspace{
		ID:           "stale-workspace-id",
		RepositoryID: "stale-repository-id",
		Slug:         ws.Slug,
		RepoRoot:     worktree,
		SessionName:  ws.Slug + "--1529",
	}, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if !env.NeedsRebuild {
		t.Fatalf("needs_rebuild = false; %s is recorded as a repository root", worktree)
	}
	if !strings.Contains(env.NeedsRebuildReason, worktree) {
		t.Errorf("reason %q does not name the stale root", env.NeedsRebuildReason)
	}
}

func TestStatusIsQuietWhenNoRepositoryRootIsStale(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if env := decodeStatus(t, stdout); env.NeedsRebuild {
		t.Errorf("needs_rebuild = true on a migrated installation: %s", env.NeedsRebuildReason)
	}
}
```

- [ ] **Step 20: Run it to verify it fails**

Run: `go test ./internal/cli/ -run TestStatus.*Rebuild -v`

Expected: build FAILS with `env.NeedsRebuild undefined (type statusEnvelope has no field or method NeedsRebuild)`.

- [ ] **Step 21: Report the stale root from `status`**

In `internal/cli/status.go:26-37`:

```go
type statusEnvelope struct {
	SchemaVersion      int            `json:"schema_version"`
	Workspace          workspaceInfo  `json:"workspace"`
	NeedsRebuild       bool           `json:"needs_rebuild"`
	NeedsRebuildReason string         `json:"needs_rebuild_reason,omitempty"`
	Registered         bool           `json:"registered"`
	Stored             *storedInfo    `json:"stored,omitempty"`
	Session            sessionInfo    `json:"session"`
	Container          *containerInfo `json:"container,omitempty"`
	Config             configInfo     `json:"config"`
	LastOperation      *operationInfo `json:"last_operation,omitempty"`
	Plan               planInfo       `json:"plan"`
}
```

Add the helper beside `statusEnvelopeFrom`:

```go
// staleRepositoryRoots names repository rows recorded at a path that is
// not this repository's root but resolves to it — what migration 0002
// leaves behind for every linked worktree that used to be its own
// workspace. Only `rebuild` can correct it, because the classification
// needs git (design §9).
//
// Rows are filtered to this repository's slug first, so status makes at
// most a couple of git calls: a linked worktree inherits its parent's
// slug (resolve.slugFor), and 0002 moved the stored slug verbatim, so a
// stale row for this repository always carries this repository's slug.
//
// A row whose path no longer resolves at all is skipped rather than
// reported. It is rebuild's business — it will be dropped — but it cannot
// be attributed to this repository, and two repositories of the same
// directory name in two roots share a slug.
func staleRepositoryRoots(st stateStore, ws resolve.Workspace) ([]string, error) {
	repos, err := st.Repositories()
	if err != nil {
		return nil, fmt.Errorf("reading stored repositories: %w", err)
	}
	var stale []string
	for _, repo := range repos {
		if repo.Slug != ws.Slug || repo.RepoRoot == ws.RepoRoot {
			continue
		}
		resolved, err := resolve.Resolve("", nil, repo.RepoRoot)
		if err != nil || resolved.RepoRoot != ws.RepoRoot {
			continue
		}
		stale = append(stale, repo.RepoRoot)
	}
	return stale, nil
}
```

In `buildStatus`, after the store is opened (`status.go:150-153`) and before `Observe`:

```go
	stale, err := staleRepositoryRoots(st, ws)
	if err != nil {
		return statusEnvelope{}, err
	}
```

and pass it into the envelope, setting the two fields after `statusEnvelopeFrom` returns:

```go
	env := statusEnvelopeFrom(ws, effective, snap, controller.BuildPlan(snap))
	if len(stale) > 0 {
		env.NeedsRebuild = true
		env.NeedsRebuildReason = fmt.Sprintf(
			"%s is still recorded as a repository root but is a linked worktree of %s; "+
				"run `projectmux rebuild` to collapse it",
			strings.Join(stale, ", "), ws.RepoRoot)
	}
	return env, nil
```

Add `"strings"` to the imports, and render it in `writeStatusHuman` after the identity block (`status.go:256-259`):

```go
	if env.NeedsRebuild {
		fmt.Fprintf(tw, "needs rebuild\t%s\n", env.NeedsRebuildReason)
	}
```

- [ ] **Step 22: Run the status tests**

Run: `go test ./internal/cli/ -run TestStatus -v`

Expected: PASS.

- [ ] **Step 23: Run the packages this task owns**

Run: `go test ./internal/rebuild/... ./internal/state/... ./internal/tmux/... ./internal/cli/...`

Expected: PASS.

The gate is scoped rather than run as `go test ./...` because `internal/doctor`'s own tests
still read the dropped fields until Task 9 — see "Task Ordering and Interactions" above.
Everything else in the tree builds from the end of Task 5a and passes from the end of Task 5b, so this line covers
every package whose behaviour this task can change.


- [ ] **Step 24: Document the upgrade step**

In `docs/commands.md`, extend the rebuild section (384-412) — the identity-keys paragraph at 388-391 still describes a worktree, and the "what it does not do" list needs the one thing it now does:

```markdown
**It completes the repository-scoped upgrade.** The schema migration that
introduced repositories moves every stored row verbatim, treating each
recorded path as a repository root, because telling a main worktree from a
linked one needs git and a migration must never fail because a directory
moved. Rebuild is what corrects that: rows recorded at a linked worktree
collapse into their parent repository, and rows whose path is gone are
dropped. Live sessions from before the upgrade are matched by the tree they
record and retagged onto their repository, so a running session is adopted
rather than duplicated. `status` reports a repository whose recorded root is
not a main worktree as needing this run.
```

- [ ] **Step 25: Format and commit**

Run: `gofmt -w internal && go test ./internal/rebuild/... ./internal/state/... ./internal/tmux/... ./internal/cli/...`

The test selection matches Step 23 for the same reason: `internal/doctor` still holds unconverted
test readers until Task 9, which is where the full suite becomes a meaningful gate again.

Then:

```
git commit -m "feat(rebuild): collapse linked-worktree rows into their repository

Migration 0002 is pure SQL and moves rows verbatim, so every stored path
becomes a repository root: over-counted, never wrong. Rebuild is the pass
that corrects it. Rows recorded at a linked worktree register their
parent and then drop the stale row — that order, so a crash leaves an
extra mergeable row rather than losing the registration. Rows whose path
is gone are dropped; a path that still exists but will not resolve is a
conflict, not a deletion.

Workspace IDs change for every repository, main worktree or not, because
the session name is now a hash input. Rebuild therefore matches a live
session by @dev_worktree and rewrites @dev_workspace_id, instead of
reading the mismatch as a different workspace. @dev_worktree keeps its
name — renaming it would strand every running session — and only its
value moves to the repository root.

status reports a repository still recorded at a linked worktree as
needing this run, since nothing else can tell the operator."

---

### Task 9: Convert the doctor tests and close the conversion out

**Files:**
- Test: `internal/doctor/doctor_test.go:381-383` (the registered-path assertions), `:505` (the container observation's key)

**Interfaces:**
- Consumes: `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName string}` and
  `state.Record{ID, RepositoryID, Slug, RepoRoot, Session, ProposedSession, ActualSession,
  DesiredDigest, AppliedDigest, RegisteredAt, UpdatedAt, Container, LastOperation}` — both from the
  earlier `internal/resolve` and `internal/state` tasks, with `Worktree` and `IsPrimary` removed.
  Also consumes `state.Store.RecordContainerObservation(repositoryID string, obs
  state.ContainerObservation, now time.Time) error` as Task 2 leaves it.
  `controller.LiveSession.Worktree` and `controller.SessionSpec.Worktree` keep their names: both are
  the Go-side mirror of the tmux user option `@dev_worktree`, whose name is unchanged.
- Produces: no new exported names. This task only rewires existing readers.

`internal/doctor` is the last package holding readers of the dropped fields, and it holds them only
in its tests: Task 5a converted `sessions.go` because `internal/cli`'s test binary links the package,
but a test file has to compile only when its own package is tested, and no gate before this one
does that. So this task is small, and it is the point at which `go test ./...` becomes a meaningful
gate again rather than the point at which the tree starts building — that happened at the end of
Task 5a.

The rule for these edits is the same as everywhere else in the plan. Where the code reads a
*workspace or record* field the name changes to `RepoRoot`; where it reads a *tmux-derived* field on
`controller.LiveSession` or writes `controller.SessionSpec` the name stays `Worktree` and only the
right-hand side moves. `internal/doctor/integration_test.go:44` passes `controller.KeyWorktree`,
which is the tmux option name `@dev_worktree` and does not change.

- [ ] **Step 1: Build the whole tree**

Run: `go build ./...`

Expected: exit 0 with no output. The build first went green at the end of Task 5a; this re-checks it
before the full test gate, because Tasks 6 through 8 all changed non-test code under scoped gates.
If it fails, the failure belongs to whichever task last touched the package — fix it there rather
than absorbing it here.

- [ ] **Step 2: Convert the doctor tests**

`internal/doctor/doctor_test.go:376-385` before:

```go
func register(t *testing.T, store *fake.Store, slug, worktree string) {
	t.Helper()
	ws := resolve.Workspace{
		ID:          "id-" + slug,
		Slug:        slug,
		Worktree:    worktree,
		SessionName: slug,
		IsPrimary:   true,
	}
```

after:

```go
func register(t *testing.T, store *fake.Store, slug, repoRoot string) {
	t.Helper()
	ws := resolve.Workspace{
		ID:          "id-" + slug,
		Slug:        slug,
		RepoRoot:    repoRoot,
		SessionName: slug,
	}
```

`seedStore` at line 369 forwards its second parameter to `register`, so renaming its own parameter
from `worktree` to `repoRoot` keeps the two helpers reading alike. The test name
`TestOrphanedSessionsReportsUnregisteredAndMissingWorktree` and the `"worktree no longer exists"`
assertion at line 413 stay as they are: that string is `orphanItem`'s operator-facing detail, which
Task 5a's doctor conversion deliberately left unchanged.

`register` also needs a repository ID, because Task 5b re-keyed the fake store's container bindings
on the repository and a fixture that leaves the field empty files every binding under the empty
string. Add it to the literal above:

```go
	ws := resolve.Workspace{
		ID:           "id-" + slug,
		RepositoryID: "r-" + slug,
		Slug:         slug,
		RepoRoot:     repoRoot,
		SessionName:  slug,
	}
```

Then `internal/doctor/doctor_test.go:505`, the one call that writes a binding, takes the same key.
Before:

```go
	if err := store.RecordContainerObservation("id-"+slug, state.ContainerObservation{
```

After:

```go
	if err := store.RecordContainerObservation("r-"+slug, state.ContainerObservation{
```

- [ ] **Step 3: Confirm nothing is left**

Run: `grep -rn 'IsPrimary' --include='*.go' . ; grep -rn '\.Worktree' --include='*.go' internal/`

Expected: the only `IsPrimary` hits are the two envelope carriers Task 7 rewrote — grep after that
task and they are gone too, so at this point the first command should print nothing at all. Every
remaining `.Worktree` hit reads a `controller.LiveSession` or writes a `controller.SessionSpec`,
both of which keep the name: `internal/tmux/actuate.go:60,149`, `internal/tmux/tmux.go:118`,
`internal/tmux/actuate_test.go`, `internal/tmux/integration_test.go`,
`internal/controller/ensure.go`, `internal/controller/plan.go`, `internal/controller/plan_test.go`,
`internal/rebuild/classify.go`, `internal/rebuild/apply.go`, `internal/cli/list.go:158`, and
`internal/cli/lifecycle_test.go:90`. Any hit whose receiver is a `resolve.Workspace` or a
`state.Record` — that is, any surviving `ws.Worktree`, `rec.Worktree`, or `row.Worktree` — is a miss
belonging to whichever task owns that package, and must be fixed before continuing.

- [ ] **Step 4: Test and format**

Run: `go test ./... && gofmt -l .`

Expected: all packages report `ok` or `no test files`, and `gofmt -l .` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add internal/doctor/doctor_test.go
git commit -m "test(doctor): move the doctor fixtures onto RepoRoot

The doctor package's non-test files converted with the CLI, because the
CLI test binary links them; its own tests could wait for the first gate
that compiles them, which is this one. The fixture gains a repository ID
so its container binding lands under the key the store now reads, and
the container observation follows it.

The operator-facing wording is left alone: 'worktree no longer exists'
is still true of a repository root, and rewording it would churn saved
runbooks for no gain."
```

---

### Task 10: dotfiles companion changes — rename `bin/dev` to `bin/pm` and make worktrees link relatively

**Files:**
- Rename: `bin/dev` → `bin/pm` (via `git mv`)
- Modify: `tools/projectmux/README.md:11-12`
- Modify: `core/git/gitconfig.symlink:37-38` (append a `[worktree]` block after `[init]`)
- Modify: `bin/relink:110-112` (insert a git feature-gate guard before the closing `log_success`)
- Test: `tests/repository_hygiene.bats:129` (append one test)
- Test: `tests/git_identity.bats:1087` (append two tests)

**Interfaces:**
- Consumes: nothing. This task is independent of every ProjectMux task in this plan and can land before or after them.
- Produces: (1) the `pm` command on `PATH` (`~/.dotfiles/bin` is prepended by `core/shell/zprofile.symlink:16` and `core/shell/bash_profile.symlink:17-18`, so no symlink or installer step is involved); (2) `worktree.useRelativePaths = true` in the tracked global git config, so every future linked worktree carries a **relative** `gitdir:` and resolves through the shared container mount; (3) a relink-time warning on git older than 2.48.

**Pre-verified findings — read before starting.** These were checked against the checkout at `275edbc`, and two of them correct the spec:

- **§8's rename claim holds.** The only occurrence of the string `bin/dev` anywhere outside `.git/` is prose at `tools/projectmux/README.md:12`. No Bats test, no `Makefile` target (`Makefile` invokes `bin/install`, `bin/bootstrap`, `bin/dot-update`, `bin/relink`, `bin/versions`, `bin/validate-ai`, `bin/list-check-files` — never `bin/dev`), no systemd unit, and no shell alias or function named `dev` (`grep -rn "alias .*\bdev\b|^dev()|function dev" core/ ai/ tools/ config/` returns nothing). `bin/list-check-files` discovers `bin/` scripts by pattern (`is_direct_bin_shell`, line 25-31), not by name, so the shellcheck/shfmt gates follow the rename automatically. `bin/pm` is not gitignored (`git check-ignore -v bin/pm` exits 1).
- **CORRECTION 1 to the task brief: `tools/dev/` no longer exists.** It was deleted in design §13 step 8. What remains is `tools/projectmux/migrate-from-dev.sh`, which matches the retired platform's *installed artifacts* by the literal strings `dev-event` (`HOOK_MARKER`, line 42) and `dev-autostart.service` (`DEV_UNIT_NAME`, line 31), plus the tests at `tests/projectmux_migrate.bats:35,146` that assert against those same strings. Those strings are a **different** `dev` and must not be touched. The rename is a single `git mv` plus one prose line; no `sed` sweep is warranted, and none is used below.
- **CORRECTION 2 to the spec: §8 omits that `worktree.useRelativePaths` is not a free-floating preference.** Verified empirically on git 2.54.0: setting it and creating a worktree writes `extensions.relativeWorktrees = true` into the **repository's** config and raises `core.repositoryformatversion` from `0` to `1`. `git help config` confirms: "setting worktree.useRelativePaths to 'true' implies enabling the extensions.relativeWorktrees config, thus making it incompatible with older versions of Git." So a repository that has created one relative worktree becomes unreadable by git < 2.48 *anywhere it is opened*, including inside a container image with an older git. That is what makes the version guard in Step 5 load-bearing rather than cosmetic, and it is the fact §8 should have carried.
- **Git version verified on this machine: 2.54.0**, which supports the key (`git help config` documents `worktree.useRelativePaths` at line 7742). It is currently **unset** (`git config --global --get worktree.useRelativePaths` exits 1). `~/.gitconfig` is a symlink to `/home/tng/.dotfiles/core/git/gitconfig.symlink`, so the tracked template **is** the global config — that is the mechanism this task uses, not a loose `git config --global` command.
- **Existing worktrees needing repair, counted on this machine:** 17 repositories under `~/workspace` have linked worktrees (largest: `slabledger` 35, `authGD` 29, `projectmux` 11, `abyssalwatch` 11), plus 4 for `.dotfiles` itself under `/tmp/dotfiles-worktrees`. Step 8 repairs them.

---

- [ ] **Step 1: Confirm the reference list before renaming anything**

```bash
cd /home/tng/.dotfiles
grep -rIn 'bin/dev' --include='*' . 2>/dev/null | grep -v '^\./\.git/'
```

Expected: exactly one line, and nothing else:

```
tools/projectmux/README.md:12:(design §13). `bin/dev` is now a thin `exec projectmux "$@"` wrapper.
```

If any other path appears, stop and add it to this task before continuing — the spec's "verified safe" claim would be wrong.

- [ ] **Step 2: Rename the wrapper**

```bash
cd /home/tng/.dotfiles
git mv bin/dev bin/pm
git status --short
```

Expected:

```
R  bin/dev -> bin/pm
```

- [ ] **Step 3: Update the wrapper's own comment and the README prose**

Edit `bin/pm` lines 2-3 to read:

```bash
# pm - thin wrapper over ProjectMux.
# Named pm, not dev: `dev` reads as "run the dev server" in every repo it is
# typed in. The Bash implementation under tools/dev/ was retired in design §13
# step 8; tools/projectmux/migrate-from-dev.sh still cleans up what it left.
```

Edit `tools/projectmux/README.md:12` to read:

```
(design §13). `bin/pm` is now a thin `exec projectmux "$@"` wrapper.
```

Then confirm the file is unchanged in behavior and the prose is consistent:

```bash
cd /home/tng/.dotfiles
tail -1 bin/pm
grep -rIn 'bin/dev' --include='*' . 2>/dev/null | grep -v '^\./\.git/'; echo "grep_exit=$?"
```

Expected:

```
exec projectmux "$@"
grep_exit=1
```

- [ ] **Step 4: Add `worktree.useRelativePaths` to the tracked global git config**

Append to `core/git/gitconfig.symlink` immediately after the `[init]` block (line 38), before the identity-routing comment at line 48:

```
[worktree]
	# Linked worktrees link with a relative `gitdir:` instead of an absolute
	# host path. Under repo-scoped sessions a worktree is reached *through* the
	# repository's container, where the host's absolute path does not exist, so
	# an absolute link makes git fail inside the worktree. Requires git >= 2.48
	# and implies extensions.relativeWorktrees on any repository that creates
	# one -- bin/relink warns when the local git is too old to honor this.
	useRelativePaths = true
```

Use a literal tab for indentation, matching the surrounding `[core]`/`[push]` blocks. Verify git parses it:

```bash
cd /home/tng/.dotfiles
git config --file core/git/gitconfig.symlink --type bool --get worktree.useRelativePaths
git config --global --type bool --get worktree.useRelativePaths
```

Expected — both commands print, on their own line:

```
true
```

(The second reads through the `~/.gitconfig` symlink, proving the setting is live for this user with no separate `git config --global` step.)

- [ ] **Step 5: Add the older-git guard to `bin/relink`**

Insert into `bin/relink` between line 110 (`fi`, closing the `SKIPPED_DESTINATIONS` block) and line 112 (`log_success "Relink complete."`):

```bash
# ── Step 3: git feature gates ─────────────────────────────────────────────────

# gitconfig.symlink sets worktree.useRelativePaths. Git older than 2.48 does
# not know the key and silently ignores it, which is indistinguishable from it
# working until a worktree fails inside a container. Warn rather than fail:
# relink must still finish on an old machine, and the setting is inert there.
# The key also implies extensions.relativeWorktrees, which bumps a repository
# to core.repositoryformatversion=1 -- so a repository that has created one
# relative worktree is unreadable by a git older than 2.48 anywhere, including
# inside a container image. That is why the floor is reported, not assumed.
check_relative_worktree_support() {
  local want=2.48.0 have
  have=$(git --version | awk '{print $3}')
  if [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -1)" != "$want" ]; then
    log_warning "git $have is older than $want; worktree.useRelativePaths is ignored."
    log_warning "New worktrees keep absolute gitdir paths and will fail inside containers."
    return 0
  fi
  log_info "git $have honors worktree.useRelativePaths."
}

check_relative_worktree_support
```

Verify the comparison logic on the four cases that matter:

```bash
check() { want=2.48.0; have=$1; if [ "$(printf '%s\n%s\n' "$want" "$have" | sort -V | head -1)" != "$want" ]; then echo "$have -> WARN"; else echo "$have -> OK"; fi; }
check 2.54.0; check 2.48.0; check 2.47.1; check 2.39.3
```

Expected:

```
2.54.0 -> OK
2.48.0 -> OK
2.47.1 -> WARN
2.39.3 -> WARN
```

- [ ] **Step 6: Add the hygiene test pinning the rename**

Append to `tests/repository_hygiene.bats`:

```bash
@test "the ProjectMux wrapper is bin/pm, not bin/dev" {
  # `dev` collides with the "run the dev server" meaning in every repo it is
  # typed in. The retired Bash platform's own artifacts -- dev-event hooks and
  # dev-autostart.service, matched by tools/projectmux/migrate-from-dev.sh --
  # are a different `dev` and are deliberately not covered by this assertion.
  [ ! -e "$REPO_ROOT/bin/dev" ]
  [ -x "$REPO_ROOT/bin/pm" ]
  run rg -n 'exec projectmux "\$@"' "$REPO_ROOT/bin/pm"
  [ "$status" -eq 0 ]
  run rg -n 'bin/dev' "$REPO_ROOT/tools" "$REPO_ROOT/bin" "$REPO_ROOT/Makefile"
  [ "$status" -eq 1 ]
}
```

Run it:

```bash
cd /home/tng/.dotfiles && bats tests/repository_hygiene.bats
```

Expected: 16 tests, all `ok`, final line `ok 16 the ProjectMux wrapper is bin/pm, not bin/dev` (the suite had 15 before this task).

- [ ] **Step 7: Add the git-config tests**

Append to `tests/git_identity.bats`:

```bash
@test "the tracked gitconfig links worktrees relatively" {
  # An absolute `gitdir:` names a host path that does not exist inside the
  # repository's container, where linked worktrees are now reached from.
  run git config --file "$REPO_ROOT/core/git/gitconfig.symlink" \
    --type bool --get worktree.useRelativePaths
  [ "$status" -eq 0 ]
  [ "$output" = "true" ]
}

@test "worktree.useRelativePaths actually produces a relative gitdir" {
  # Asserts the behavior, not just the key: a git that does not support the
  # option parses the config without complaint and writes an absolute link.
  local repo="$TEST_ROOT/relwt"
  git init -q -b main "$repo"
  git -C "$repo" config user.email test@example.com
  git -C "$repo" config user.name Test
  echo one >"$repo/file"
  git -C "$repo" add file
  git -C "$repo" -c commit.gpgsign=false commit -q -m one
  git -C "$repo" config worktree.useRelativePaths true
  git -C "$repo" worktree add -q "$TEST_ROOT/relwt-a" -b feat-a

  run cat "$TEST_ROOT/relwt-a/.git"
  [ "$status" -eq 0 ]
  [ "$output" = "gitdir: ../relwt/.git/worktrees/relwt-a" ]

  # The relative link must still be a working repository.
  run git -C "$TEST_ROOT/relwt-a" rev-parse --abbrev-ref HEAD
  [ "$status" -eq 0 ]
  [ "$output" = "feat-a" ]
}
```

Run it:

```bash
cd /home/tng/.dotfiles && bats tests/git_identity.bats
```

Expected: 93 tests, all `ok`, ending with `ok 93 worktree.useRelativePaths actually produces a relative gitdir` (the suite had 91 before this task). If the second test fails with an absolute `gitdir:`, the local git predates 2.48 and Step 5's guard is the correct response — do not weaken the assertion.

- [ ] **Step 8: Repair every existing worktree on this machine**

Existing worktrees keep their absolute `gitdir:` until repaired; only new ones pick up the config. Run from the main worktree of each repository:

```bash
cd /home/tng/.dotfiles
for r in /home/tng/workspace/*/ /home/tng/.dotfiles; do
  [ -d "$r/.git" ] || continue
  [ "$(git -C "$r" worktree list | wc -l)" -gt 1 ] || continue
  echo "== $r"
  git -C "$r" worktree repair
done
```

Expected: for each repository with linked worktrees, a header line followed by one or more lines of the form

```
== /home/tng/workspace/projectmux/
repair: .git file absolute/relative path mismatch: /home/tng/workspace/projectmux/.worktrees/1529
```

`git worktree repair` prints that `mismatch` line on **every** run, including after the link is already relative — the message is not idempotent even though the result is, so a repeated run printing the same lines is correct, not a failure. Exit status is 0 throughout.

- [ ] **Step 9: Confirm the repair converted a real worktree**

```bash
cat /home/tng/.dotfiles/../workspace/projectmux/.claude/worktrees/repo-scoped-sessions/.git 2>/dev/null \
  || cat "$(git -C /home/tng/workspace/projectmux worktree list --porcelain | awk '/^worktree /{print $2}' | sed -n 2p)/.git"
git -C "$(git -C /home/tng/workspace/projectmux worktree list --porcelain | awk '/^worktree /{print $2}' | sed -n 2p)" status --short --branch | head -1
```

Expected: a `gitdir:` line beginning with `../` or `.claude/`-relative segments and containing **no** leading `/`, for example

```
gitdir: ../../../.git/worktrees/repo-scoped-sessions
```

followed by a branch line such as `## repo-scoped-sessions`. A `gitdir:` starting with `/home/tng/` means Step 8 did not reach that repository.

- [ ] **Step 10: End-to-end manual verification of a fresh worktree**

```bash
cd /home/tng/.dotfiles
git worktree add /tmp/dotfiles-worktrees/relpath-check -b tmp/relpath-check
cat /tmp/dotfiles-worktrees/relpath-check/.git
git -C /tmp/dotfiles-worktrees/relpath-check status --short --branch
git -C /tmp/dotfiles-worktrees/relpath-check rev-parse --show-toplevel
```

Expected:

```
gitdir: ../../home/tng/.dotfiles/.git/worktrees/relpath-check
## tmp/relpath-check
/tmp/dotfiles-worktrees/relpath-check
```

The `gitdir:` line must contain no absolute path. `status` printing only the branch line (no `??`/` M` entries) confirms git resolves the repository through the relative link.

- [ ] **Step 11: Tear down the probe worktree**

```bash
cd /home/tng/.dotfiles
git worktree remove /tmp/dotfiles-worktrees/relpath-check
git branch -D tmp/relpath-check
git worktree list | grep -c relpath-check; echo "grep_exit=$?"
```

Expected:

```
Deleted branch tmp/relpath-check (was 275edbc).
0
grep_exit=1
```

- [ ] **Step 12: Run the repository's full verification gate**

`make` is broken under this machine's zsh ("function definition file not found"), so invoke the real binary by absolute path:

```bash
cd /home/tng/.dotfiles && /usr/bin/make check
```

Expected: `syntax`, `lint` (shellcheck 0.9.0 and `shfmt -d -i 2 -ci` — the latter prints nothing when formatting is correct; `bin/relink` and `bin/pm` are both in the `shfmt`/`shellcheck` sets via `is_direct_bin_shell`), `test` (the full `bats tests` run, all `ok`), `python-test` (`OK`), and `validate` all completing with exit status 0 and no `FAIL`/`not ok` lines.

If `shfmt` reports a diff against `bin/relink`, apply it with `shfmt -w -i 2 -ci bin/relink` and re-run this step.

- [ ] **Step 13: Inspect the final diff**

```bash
cd /home/tng/.dotfiles
git status --short
/usr/bin/diff <(git show HEAD:bin/dev) bin/pm
git diff -- core/git/gitconfig.symlink bin/relink tools/projectmux/README.md
```

Expected from `git status --short`: exactly six paths, no more. Five modifications and the rename:

```
bin/relink
core/git/gitconfig.symlink
tests/git_identity.bats
tests/repository_hygiene.bats
tools/projectmux/README.md
bin/dev -> bin/pm
```

Do not pin the two-letter status codes. The rename's codes depend on whether Step 3's comment-block edit landed before or after `git add`: staged-then-edited shows `RM bin/dev -> bin/pm` as a *single* entry, staged-after-editing shows `R  bin/dev -> bin/pm`. Both are correct here. What matters is that the rename appears as one entry rather than a delete plus an add — if `git status` shows `D bin/dev` and `?? bin/pm` instead, the rename was not staged and git did not detect it, so `git add -A bin/pm` in Step 14 would commit the new file while leaving `bin/dev` in the tree.

The `/usr/bin/diff` output must show only the comment-block change from Step 3 (lines 2-3 replaced), with `exec projectmux "$@"` identical on both sides. Nothing under `tools/projectmux/migrate-from-dev.sh` or `tests/projectmux_migrate.bats` may appear — those carry the unrelated `dev-event` / `dev-autostart.service` strings.

If any step needs to be reverted, restore tracked files with `git checkout -- <path>` (`cp` is interactive on this machine and can silently decline), and remove untracked files with `rm -f` (bare `rm` silently no-ops here).

- [ ] **Step 14: Commit**

```bash
cd /home/tng/.dotfiles
git add -A bin/pm bin/relink core/git/gitconfig.symlink \
  tests/git_identity.bats tests/repository_hygiene.bats tools/projectmux/README.md
git commit -m "feat(git): rename bin/dev to bin/pm and link worktrees relatively

Repo-scoped ProjectMux sessions reach a linked worktree *through* the
repository's container rather than giving it a container of its own, so a
worktree's absolute \`gitdir:\` now names a host path that does not exist on
the inside and git fails there. Set worktree.useRelativePaths in the tracked
global config so every future worktree links relatively and resolves from
both sides; existing worktrees convert with \`git worktree repair\`.

The option implies extensions.relativeWorktrees, which raises an affected
repository to core.repositoryformatversion=1 and makes it unreadable by git
older than 2.48. bin/relink therefore reports the local git version instead
of assuming support: on an older git the key is silently ignored, which is
otherwise indistinguishable from it working.

Rename bin/dev to bin/pm in the same change: \`dev\` reads as \"run the dev
server\" in every repository it is typed in. Only prose referenced the old
path. The retired Bash platform's dev-event hooks and dev-autostart.service,
which migrate-from-dev.sh still cleans up, are a different \`dev\` and are
untouched."
git log --oneline -1
```

Expected: `git commit` reports 6 files changed with a rename detected, and `git log --oneline -1` prints the new SHA followed by `feat(git): rename bin/dev to bin/pm and link worktrees relatively`.

---


---

### Task 11: Concurrent opens issue exactly one `devcontainer up`

**Files:**
- Create: `internal/controller/concurrent_container_test.go`
- Inspect (only if Step 3 fails — see the failure signature there): `internal/controller/fake/fake.go`, whose repository-keyed binding map and `copyRecordLocked` projection Task 5b already landed
- Test: `internal/controller/concurrent_container_test.go`

**Interfaces:**
- Consumes: `resolve.Workspace{ID, RepositoryID, Slug, RepoRoot, Session, SessionName}` (Task 1); `fake.NewStore() *fake.Store` and `(*Store).Workspace(id string) (state.Record, error)` (Task 2); `lockPhases(ctx, dir, repositoryID, workspaceID string, timeout time.Duration) (func(), error)` indirectly, through `Ensure` (Task 3); `controller.ContainerObserver` / `controller.ContainerActuator` (`internal/controller/interfaces.go:52-73`); the existing `scriptedSessions`, `absentStep`, `liveStep`, `ensureTime` helpers in `internal/controller/ensure_test.go:26-58,21`.
- Produces: no new exported names. One regression test. The repository-keyed binding map inside the unexported `fake.Store` is Task 5b's, not this task's; Steps 4 and 5 only verify it survived.

**What the code already says, before writing anything**

`Ensure` acquires the lock as its very first statement (`internal/controller/ensure.go:81`) and releases it in a `defer` (`:85`), so registration (`:87`), the observation pass (`:91`), the container phase (`:106`), and the terminal `CommitReconciliation` (`:415`) all run *inside* one continuous hold. Task 3 replaces line 81 with `lockPhases(...)` at the same position, returning a single release closure, so the repository lock likewise spans the whole pass. **The observe phase therefore runs inside the repository lock, and no lock-ordering fix is needed.** The second goroutine genuinely waits until the first has committed its binding.

What is *not* guaranteed by that alone is the second half of the composition. Real discovery matches on the `devcontainer.local_folder` label and cannot recover the remote user or workdir (`internal/container/adapter.go:94-96,126-129`), so a discovered-but-unbound container plans as `ContainerActionAcquire` (`internal/controller/plan.go:145-150`), and acquire runs `StartContainer` again (`internal/controller/ensure.go:197-210`). The second goroutine skips `up` only if `Observe` takes the *probe* branch (`internal/controller/observe.go:127-136`), which requires `snap.Stored.Container` to be non-nil — that is, the repository-keyed `container_bindings` row written by the first session must be readable through the *second* session's record. That is the exact composition under test, and it is the thing most likely to be missing.

- [ ] **Step 1: Create the test file with a stateful shared-container fake**

`fake.ContainerObserver` returns a fixed `*DiscoverResult` on every call (`internal/controller/fake/fake.go:83-89`) and cannot express "missing until started, then present". It is also unsynchronized — `Discovered`, `Probed`, and `ContainerActuator.Started` are plain slice appends (`fake.go:76,84,359`) — and an flock gives the Go race detector no happens-before edge, so sharing those fakes across two goroutines would be reported as a data race even when the code under test is correct. Both reasons point the same way: define a local, mutex-guarded fake here rather than extend the shared one.

```go
// The concurrency test lives in the external test package for the same
// reason the ensure tests do: it needs the exported fakes, and
// controller/fake imports controller.
package controller_test

import (
	"context"
	"path"
	"sync"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

const (
	sharedRepoID        = "repo-1"
	sharedRepoRoot      = "/w/slab"
	sharedContainerID   = "cid-shared"
	sharedContainerUser = "vscode"
	sharedContainerDir  = "/workspaces/slab"
)

// sharedContainer is an observer and an actuator in one, standing in for
// the single real container every session on a repository shares. It is
// stateful where fake.ContainerObserver is canned: discovery reports
// missing until a start has succeeded and reports the started container
// afterwards, which is what the repository-keyed local_folder filter gives
// the real adapter (internal/container/adapter.go:94-137) and is the whole
// subject of this test.
//
// It is local rather than an extension of fake.ContainerObserver for a
// second reason: the shared fakes record their calls with unsynchronized
// slice appends (internal/controller/fake/fake.go:76,84,359). Two Ensure
// passes are serialized by an flock, which the race detector cannot see as
// a happens-before edge, so sharing those fakes across goroutines would be
// reported as a data race even though the serialization is real.
type sharedContainer struct {
	mu      sync.Mutex
	started bool
	starts  int
}

var (
	_ controller.ContainerObserver = (*sharedContainer)(nil)
	_ controller.ContainerActuator = (*sharedContainer)(nil)
)

func (c *sharedContainer) Applies(context.Context, resolve.Workspace, config.Config) (bool, error) {
	return true, nil
}

// DiscoverContainer mirrors the adapter's label query: before any start
// there is nothing to find, and afterwards the match is present but
// unbound, because a label cannot supply the remote user or workdir.
func (c *sharedContainer) DiscoverContainer(context.Context, resolve.Workspace, config.Config) (*controller.ContainerObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started {
		return &controller.ContainerObservation{
			Kind: "devcontainer", Health: state.HealthMissing,
		}, nil
	}
	return &controller.ContainerObservation{
		Kind: "devcontainer", ContainerID: sharedContainerID, Health: state.HealthPresent,
	}, nil
}

// ProbeContainer mirrors the adapter's inspect: a live container carries
// the stored binding's identity back out, so a session that already knows
// the binding needs no acquiring start.
func (c *sharedContainer) ProbeContainer(_ context.Context, b state.ContainerBinding) (controller.ContainerObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || b.ContainerID != sharedContainerID {
		return controller.ContainerObservation{Health: state.HealthMissing}, nil
	}
	return controller.ContainerObservation{
		Kind:          b.Kind,
		ContainerID:   b.ContainerID,
		ContainerUser: b.ContainerUser,
		Workdir:       b.Workdir,
		Health:        state.HealthPresent,
	}, nil
}

// StartContainer counts the devcontainer up invocations this test exists
// to bound, and makes the container observable from here on.
func (c *sharedContainer) StartContainer(context.Context, resolve.Workspace, config.Config) (controller.ContainerObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.starts++
	c.started = true
	return controller.ContainerObservation{
		Kind:          "devcontainer",
		ContainerID:   sharedContainerID,
		ContainerUser: sharedContainerUser,
		Workdir:       sharedContainerDir,
		Health:        state.HealthPresent,
	}, nil
}

func (c *sharedContainer) ExecCommand(b state.ContainerBinding, command, relDir string, _ map[string]string) string {
	return "fake-exec " + b.ContainerID + " " + path.Join(b.Workdir, relDir) + " " + command
}

func (c *sharedContainer) StopContainer(context.Context, string) error { return nil }

func (c *sharedContainer) startCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.starts
}
```

- [ ] **Step 2: Append the two-session fixtures and the concurrent test**

The two workspaces share `RepositoryID` and `RepoRoot` and differ only in `ID` and `Session`. Named sessions are a later plan, so the second workspace is constructed directly rather than resolved. The IDs are literals rather than real SHA-256 hex because `Ensure` treats them as opaque map and lock-file keys, and readable ones make a failure legible.

```go
// repoWorkspace builds one session on the shared repository. The default
// session carries the bare slug as its session name; a named one gets the
// slug--session form the resolver produces.
func repoWorkspace(session string) resolve.Workspace {
	ws := resolve.Workspace{
		ID:           "ws-default",
		RepositoryID: sharedRepoID,
		Slug:         "slab",
		RepoRoot:     sharedRepoRoot,
		SessionName:  "slab",
	}
	if session != "" {
		ws.ID = "ws-" + session
		ws.Session = session
		ws.SessionName = "slab--" + session
	}
	return ws
}

func repoDesired(ws resolve.Workspace) controller.Desired {
	return controller.Desired{
		Workspace: ws,
		Config: config.Config{
			Version:      1,
			DevContainer: config.DevContainer{Enabled: "true"},
			Environment:  map[string]string{"FOO": "bar"},
		},
		Digest: "sha256:desired",
	}
}

// repoSession is the live tmux session the post-create confirmation must
// see. @dev_worktree keeps its name but now carries the repository root
// (design §5.1), so both sessions report the same value there.
func repoSession(ws resolve.Workspace) controller.LiveSession {
	return controller.LiveSession{
		Name:        ws.SessionName,
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.RepoRoot,
	}
}

// TestConcurrentOpensIssueOneContainerStart is the §6.1 race stated as a
// test: two sessions on one repository opened at the same instant must
// produce one devcontainer up between them. Deduplication is a composition
// of two mechanisms, and neither the locking test nor the adapter test
// covers it — the repository lock makes the second pass wait, and the
// repository-keyed binding then lets it observe the container the first
// pass started instead of starting a second one.
//
// Each goroutine gets its own Controller because scriptedSessions and the
// session actuator are per-pass scripts; the store, the container, and the
// lock directory are what they share, exactly as two processes would.
func TestConcurrentOpensIssueOneContainerStart(t *testing.T) {
	store := fake.NewStore()
	containers := &sharedContainer{}
	lockDir := t.TempDir()

	workspaces := []resolve.Workspace{repoWorkspace(""), repoWorkspace("review")}
	results := make([]controller.EnsureResult, len(workspaces))
	errs := make([]error, len(workspaces))

	release := make(chan struct{})
	var wg sync.WaitGroup
	for i, ws := range workspaces {
		wg.Add(1)
		go func(i int, ws resolve.Workspace) {
			defer wg.Done()
			ctrl := &controller.Controller{
				Store: store,
				Sessions: &scriptedSessions{steps: []func(controller.SessionQuery) (controller.SessionObservation, error){
					absentStep(),              // initial observation
					absentStep(),              // allocated-name squat check
					liveStep(repoSession(ws)), // post-create confirmation
				}},
				Containers:   containers,
				Clock:        &fake.Clock{Time: ensureTime},
				Actuator:     &fake.SessionActuator{},
				ContainerAct: containers,
			}
			// Both goroutines block here and are released together, so the
			// lock is contended rather than merely exercised in sequence.
			<-release
			results[i], errs[i] = ctrl.Ensure(context.Background(), repoDesired(ws),
				[]controller.WindowIntent{{Name: "shell"}}, lockDir, 5*time.Second)
		}(i, ws)
	}
	close(release)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure for session %q: %v", workspaces[i].SessionName, err)
		}
	}
	if got := containers.startCount(); got != 1 {
		t.Fatalf("devcontainer up invocations = %d, want exactly 1", got)
	}
	if results[0].Session == results[1].Session {
		t.Errorf("both opens landed on session %q; the two sessions must stay distinct",
			results[0].Session)
	}
	for i, res := range results {
		if res.Container == nil || res.Container.ContainerID != sharedContainerID {
			t.Errorf("session %q reported container %+v, want %q",
				workspaces[i].SessionName, res.Container, sharedContainerID)
		}
	}
	for _, ws := range workspaces {
		rec, err := store.Workspace(ws.ID)
		if err != nil {
			t.Fatalf("Workspace(%s): %v", ws.ID, err)
		}
		if rec.Container == nil || rec.Container.ContainerID != sharedContainerID {
			t.Errorf("stored binding for session %q = %+v, want %q",
				ws.SessionName, rec.Container, sharedContainerID)
		}
	}
}
```

- [ ] **Step 3: Run the test under the race detector and record which way it goes**

Run: `go test ./internal/controller/ -run TestConcurrentOpensIssueOneContainerStart -race -v`

This step decides whether Task 11 is test-only. Two outcomes are possible, and only the run distinguishes them:

- **PASS** — the repository lock plus the repository-keyed binding Task 5b landed already compose. This is the expected outcome; skip Steps 4 and 5 and go straight to Step 6.
- **FAIL** with `devcontainer up invocations = 2, want exactly 1` — the second goroutine's `Observe` found no stored binding on its own record, fell to `DiscoverContainer`, saw the label shape (present, no workdir), and planned `acquire`, which runs a second `up` (`internal/controller/ensure.go:197-210`). The binding is written under the repository ID but is not readable through a sibling session's record; Steps 4 and 5 locate which half of Task 5b's fake-store change went missing.

- [ ] **Step 4 (only on the Step 3 failure): Confirm the fake keys its bindings on the repository**

Task 5b already moved the fake's container bindings off the record and into a repository-keyed map,
because autostart records a binding for a repository that has no session at all. If Step 3 failed,
the first thing to rule out is that this change is missing or was written back.

Run: `grep -n 'containers\[' internal/controller/fake/fake.go`

Expected: `recordContainerLocked` writes `s.containers[repositoryID]` and reads it back for the
degraded-health branch, and nothing assigns to `rec.Container`. If instead the binding is stored on
the record, restore the Task 5b form — a fake that keeps the binding on the workspace row contradicts
the repository-keyed `container_bindings` table (design §5.2) and would let this test pass or fail
for reasons the production store does not share.

- [ ] **Step 5 (only on the Step 3 failure): Confirm every record read projects the repository binding**

The second half of the composition is `copyRecordLocked`, which Task 5b introduced so that a record
carries whatever container its repository is bound to — including one a sibling session started.
That projection is exactly what `Observe` needs to take the probe branch instead of rediscovering.

Run: `grep -n 'copyRecord' internal/controller/fake/fake.go`

Expected: `copyRecordLocked` is a method on `*Store`, it sets `out.Container` from
`s.containers[rec.RepositoryID]`, and both `Workspace` and `Workspaces` call it. If the free
function `copyRecord` is still there, the sibling never sees the binding and Step 3's failure is
explained; restore the Task 5b form.

- [ ] **Step 6 (only on the Step 3 failure): Re-run the target test**

Run: `go test ./internal/controller/ -run TestConcurrentOpensIssueOneContainerStart -race -v`
Expected: PASS, with one `devcontainer up` and both sessions bound to `cid-shared`.

- [ ] **Step 7: Run the controller and state packages under the race detector**

Run: `go test ./internal/controller/... ./internal/state/... -race`
Expected: PASS. This is the targeted regression check for Steps 4-5 — the fake store's tri-state retention and binding semantics are asserted by the existing ensure and lifecycle tests, and relocating the binding must not move any of them.

- [ ] **Step 8: Run the full suite and the formatter**

Run: `go test ./... && gofmt -l .`
Expected: PASS with no output from `gofmt`.

- [ ] **Step 9: Commit**

Run:
```bash
git add internal/controller/concurrent_container_test.go internal/controller/fake/fake.go
git commit -m "test(controller): one devcontainer up for concurrent opens on a repository

Two sessions on one repository opened at the same instant must issue a
single devcontainer up. Locking alone does not prove that: it only makes
the second pass wait. Deduplication is the composition of the repository
lock with repository-keyed discovery, which lets the waiting pass observe
the container the first one started instead of acquiring it again.

The test drives both goroutines from one release channel through a
stateful container fake that reports missing until a start succeeds and
the label shape afterwards, matching the adapter. The shared fakes record
calls with unsynchronized appends and flock gives the race detector no
happens-before edge, so this one is mutex-guarded and local.

The fake store's container binding moves to a repository-keyed map, which
is where the 0002 schema puts it; a binding hanging off the workspace row
kept siblings from ever seeing each other's container."
```

(Drop `internal/controller/fake/fake.go` from the `git add` and the final commit-message paragraph if Step 3 passed and Steps 4-5 were skipped.)

## Acceptance: spec §12 verification cases mapped to tasks

Every verification case the spec names, and the task that carries it. A case with no task is a plan defect; the two marked deferred are the ones this plan's Scope section excludes.

| Spec §12 case | Task |
|---|---|
| `internal/resolve` tests invert — a linked worktree resolves to its parent, nested-directory lookup finds nothing | Task 1 |
| Two targets on one repository produce one container identity and two session identities | Task 4 (identical argv and discovery filter) with Task 2 (two session rows) |
| Two sessions on one repository register successfully — unrepresentable before `0002`, and fails against a column rename | Task 2 |
| Concurrent `open` of two sessions issues exactly one `devcontainer up` | Task 11, over Task 3's serialization |
| `stop --container` refuses while a sibling is live with exit 6, and proceeds under `--force` | Task 6 |
| Autostart over a repository with several sessions starts its container once, replacing `lifecycle_test.go:495` | Task 5b |
| Every `--json` envelope reports `schema_version: 2` and omits `is_primary`, asserted per command | Task 7 |
| Socket-isolated integration: `0002` succeeds with the filesystem absent, and a following `rebuild` collapses a linked-worktree row | Task 2 (migration) and Task 8 (collapse) |
| Every remaining `Worktree`/`IsPrimary` reader converts and the tree builds again | Tasks 3-5 (build), Task 9 (full suite) |
| Bind rejection for a path outside the repository root, including via symlink | **Deferred** — `bind` is the second plan |
| Bind fallback when the bound path is deleted | **Deferred** — `bind` is the second plan |

## Final gate

After Task 11, before calling the plan done:

- [ ] `go build ./...` — still green. It first goes green at the end of Task 5a; Tasks 6 through 11 must not take it back down.
- [ ] `go test ./...` — every package passes.
- [ ] `gofmt -l .` — no output.
- [ ] `git log --oneline` shows one commit per task, in order.
- [ ] In the dotfiles repo: `/usr/bin/make check` passes (Task 10's own gate, run again in case Task 10 landed early).
