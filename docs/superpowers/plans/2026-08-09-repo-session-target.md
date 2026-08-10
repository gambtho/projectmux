# `<repo>/<session>` Targets and Per-Session Working Directories — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let one repository hold several named tmux sessions, each addressable as `<repo>/<session>` and each able to open in a directory below the repository root.

**Architecture:** A new `internal/target` package sits between the CLI and `internal/resolve`, parsing the target argument and choosing a session; `resolve` gains a session parameter and stays pure. A fourth tmux identity key, `@dev_session`, makes a named session recoverable rather than a false identity conflict. A nullable `bind` column on the workspaces table records a session's base directory, which `controller.Ensure` composes under every window's and pane's `dir:` before either actuator sees a path.

**Tech Stack:** Go 1.25, modernc.org/sqlite (embedded numbered SQL migrations), tmux 3.4 as the session actuator, `gopkg.in/yaml.v3` for configuration.

## Global Constraints

- The session component of a target must match `^[A-Za-z0-9][A-Za-z0-9_-]*$` and be at most 64 characters — deliberately stricter than tmux's own rules, so a mistyped path fails as a malformed target rather than as an unknown workspace.
- `<target>` is `<repo>` or `<repo>/<session>`. `/` is the separator because it cannot appear in a git repository directory name.
- The tmux session name stays `<slug>--<session>` for named sessions and `<slug>` for the default one.
- Identity derivation is frozen: `RepositoryID = sha256(repo_root)` and `Workspace.ID = sha256(repo_root + "\x00" + session)`. No existing stored ID may change.
- An absent `@dev_session` reads as `""`, which is exactly what a v0.5.0 default session is. No existing session is invalidated and no user is forced to rebuild.
- `OutputSchemaVersion` stays **2**. Every reporting change is an added field or an added column; nothing is renamed, retyped, or removed.
- `state.SchemaVersion` goes 2 → 3. Migration 0003 is additive (a nullable column) and requires no rebuild.
- No new exit codes: 2 for a malformed target or an invalid bind path, 3 for an ambiguous bind, 4 for an unknown repository, 1 for a propagated state-store failure.
- A bind is stored relative to the repository root, and its containment is re-checked at **every use**, not only at bind time.
- `resolve` neither reads configuration files nor mutates any resource, and owns every git invocation. Session selection must not break that property.
- Configuration stays keyed on slug: `config.Load(root, defaults, ws.Slug)` is unchanged, so both sessions on a repository share `workspaces/<slug>.yaml`.
- Out of scope: a `doctor` check for dangling binds, and per-session configuration.
- The full check is what CI runs (`.github/workflows/ci.yml`): `gofmt -l .` (must be empty), `go mod tidy` followed by `git diff --exit-code -- go.mod go.sum`, `go vet ./...`, `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...`, `go test -race ./...`, and `CGO_ENABLED=0 go build ./cmd/projectmux`.

---

## File Structure

**New packages**

- `internal/target/target.go` — parses a target argument into a `Ref` and selects the session to act on. The only place that knows the target grammar. It is separate from `resolve` because selection reads the state store, which `resolve` must not do.
- `internal/bindpath/bindpath.go` — the path rules a bind obeys: canonicalization, containment re-verification, component-wise comparison, and conversion to the stored relative form. Its own package because both `internal/target` (lookup) and `internal/controller` (window rendering) need identical rules, and `controller` must not import `target`.

**New data**

- `internal/state/migrations/0003_bind.sql` — adds the nullable `bind TEXT` column.

**Modified, by responsibility**

- `internal/resolve/resolve.go` — takes the session as a parameter; adds `WithSession` for pure re-derivation.
- `internal/controller/types.go`, `interfaces.go`, `plan.go`, `ensure.go`, `observe.go` — the fourth identity key, the bind on `Desired`, the base-directory composition, and the `SetBind` store method.
- `internal/tmux/decode.go`, `tmux.go`, `actuate.go` — read and write `@dev_session`. Two fixed-size arrays (`fieldFormats` and the decode loop's `values`) grow from 4 to 5 together.
- `internal/state/types.go`, `store.go`, `migrate.go` — the `bind` column end to end.
- `internal/rebuild/apply.go` — resolves a live session against its own session component instead of always the default.
- `internal/cli/*.go` — every command taking a workspace argument routes through `target`; `open` gains `--cwd`; `bind` is a new command; `list` and `status` report the bind.
- `docs/commands.md`, `docs/worktrees.md`, and the decision records — the user-facing surface and the two design departures.

---

## Tasks

### Task 1: `resolve` takes the session as a parameter

`internal/resolve` owns every git invocation and derives workspace identity from
a repository root plus a session component. Today `Resolve` hardcodes
`session := ""` with a comment saying the `<repo>/<session>` argument form
arrives later. This task deletes that hardcoding, takes the session as a
parameter, and adds `WithSession`, the pure re-derivation `rebuild` and `status`
will use in Part B to reconstruct a live session's identity from its
`@dev_session` key without a second git call.

Identity derivation is the one thing in this repository that must never change
silently: `Workspace.ID` is the primary key of every row in the user's state
store. So the derivation lives in exactly one function (`WithSession`), `Resolve`
calls it rather than repeating the arithmetic, and a golden test pins the
digests to literal hex.

**Files:**
- Modify: `internal/resolve/resolve.go:79-117` (the `Resolve` signature, the
  hardcoded `session := ""`, and the derivation block)
- Test: `internal/resolve/resolve_test.go` (helper `mustResolve` at 60-67, and
  every `Resolve(`/`mustResolve(` call site in the file: lines 62, 77, 78, 79,
  92, 107, 127, 150, 152, 176, 192, 215, 223, 250, 263, 281, 292)
- Modify (call sites, each gains a `""` session argument):
  - `internal/cli/rebuild.go:280`
  - `internal/cli/open.go:137`
  - `internal/cli/config.go:128`
  - `internal/cli/autostart.go:142`
  - `internal/cli/attach.go:87`
  - `internal/cli/status.go:143`
  - `internal/cli/status.go:225`
  - `internal/cli/status.go:259`
  - `internal/cli/stop.go:86`
  - `internal/cli/lifecycle_test.go:58`
  - `internal/cli/open_test.go:103`
  - `internal/cli/open_test.go:271`
  - `internal/cli/status_test.go:32`
  - `internal/cli/autostart_test.go:203`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  - `func resolve.Resolve(name, session string, roots []string, cwd string) (resolve.Workspace, error)`
  - `func resolve.WithSession(ws resolve.Workspace, session string) resolve.Workspace`

---

- [ ] **Step 1: Write the failing golden test for the derivation**

`WithSession` is a pure function of `RepoRoot`, `Slug` and the session, so it can
be pinned to literal digests with no filesystem at all. That is what makes this a
real golden test rather than a restatement of the implementation.

Append to `internal/resolve/resolve_test.go`:

```go
// The workspace ID is the primary key of every row in a user's state store.
// These digests are hex(sha256(repo_root + "\x00" + session)) for a fixed root,
// computed independently of this package. If a refactor changes them, every
// existing installation silently loses its recorded sessions — so they are
// pinned as literals rather than recomputed from the same expression the
// implementation uses.
func TestWorkspaceIDDerivationIsPinned(t *testing.T) {
	const repoRoot = "/home/u/workspace/euro_trip"
	base := Workspace{
		RepositoryID: "28ac435953b10fee07569890551989d4707354301f7c4d467cbf2967b7da2907",
		Slug:         "euro_trip",
		RepoRoot:     repoRoot,
	}

	cases := []struct {
		session  string
		wantID   string
		wantName string
	}{
		{
			session:  "",
			wantID:   "bb04096e4b690f60b0cbfbe2954f9901ce30f9c67f7e378f4189a2e9ca3c6223",
			wantName: "euro_trip",
		},
		{
			session:  "feature-a",
			wantID:   "8c178a36549771dd4a145551d4c0a23298d2f186b8bb5deb2306c448568ba826",
			wantName: "euro_trip--feature-a",
		},
	}

	for _, tc := range cases {
		got := WithSession(base, tc.session)
		if got.ID != tc.wantID {
			t.Errorf("WithSession(%q).ID = %q, want %q", tc.session, got.ID, tc.wantID)
		}
		if got.SessionName != tc.wantName {
			t.Errorf("WithSession(%q).SessionName = %q, want %q",
				tc.session, got.SessionName, tc.wantName)
		}
		if got.Session != tc.session {
			t.Errorf("WithSession(%q).Session = %q", tc.session, got.Session)
		}
		// The repository is not a function of the session.
		if got.RepositoryID != base.RepositoryID || got.Slug != base.Slug ||
			got.RepoRoot != base.RepoRoot {
			t.Errorf("WithSession(%q) disturbed the repository fields: %+v", tc.session, got)
		}
	}
}

// Resolve and WithSession must agree, or a workspace reached through the CLI
// and the same workspace reconstructed from a live session's @dev_session key
// would carry different IDs.
func TestResolveAgreesWithWithSession(t *testing.T) {
	base := root(t)
	makeRepo(t, filepath.Join(base, "euro_trip"))

	def := mustResolve(t, "euro_trip", "", []string{base}, base)
	named := mustResolve(t, "euro_trip", "feature-a", []string{base}, base)

	if got := WithSession(def, "feature-a"); got != named {
		t.Errorf("WithSession(default, %q) = %+v, want %+v", "feature-a", got, named)
	}
	if got := WithSession(named, ""); got != def {
		t.Errorf("WithSession(named, \"\") = %+v, want the default workspace %+v", got, def)
	}
}

func TestResolveDerivesTheNamedSession(t *testing.T) {
	base := root(t)
	repo := makeRepo(t, filepath.Join(base, "euro_trip"))
	ws := mustResolve(t, "euro_trip", "feature-a", []string{base}, base)

	if ws.Session != "feature-a" {
		t.Errorf("session = %q", ws.Session)
	}
	if ws.SessionName != "euro_trip--feature-a" {
		t.Errorf("session name = %q, want %q", ws.SessionName, "euro_trip--feature-a")
	}
	if ws.Slug != "euro_trip" || ws.RepoRoot != repo {
		t.Errorf("slug/root = %q/%q, want %q/%q", ws.Slug, ws.RepoRoot, "euro_trip", repo)
	}

	// Two sessions on one repository share the repository, not the workspace.
	def := mustResolve(t, "euro_trip", "", []string{base}, base)
	if ws.RepositoryID != def.RepositoryID {
		t.Errorf("RepositoryID differs between sessions on one repository")
	}
	if ws.ID == def.ID {
		t.Errorf("the named session reused the default session's ID %q", ws.ID)
	}
}
```

- [ ] **Step 2: Update the test helper and the existing call sites in `resolve_test.go`**

`mustResolve` gains the session parameter. Replace lines 60-67 of
`internal/resolve/resolve_test.go`:

```go
func mustResolve(t *testing.T, name, session string, roots []string, cwd string) Workspace {
	t.Helper()
	ws, err := Resolve(name, session, roots, cwd)
	if err != nil {
		t.Fatalf("Resolve(%q, %q): %v", name, session, err)
	}
	return ws
}
```

Then insert `""` as the second argument at each existing call site in the file.
All of these keep today's behavior — the default session:

| Line | Before | After |
| --- | --- | --- |
| 77 | `mustResolve(t, "", nil, repo).ID` | `mustResolve(t, "", "", nil, repo).ID` |
| 78 | `mustResolve(t, "", nil, repo+string(filepath.Separator)).ID` | `mustResolve(t, "", "", nil, repo+string(filepath.Separator)).ID` |
| 79 | `mustResolve(t, "", nil, link).ID` | `mustResolve(t, "", "", nil, link).ID` |
| 92 | `mustResolve(t, "euro_trip", []string{base}, base)` | `mustResolve(t, "euro_trip", "", []string{base}, base)` |
| 107 | `mustResolve(t, "euro_trip", []string{base}, base)` | `mustResolve(t, "euro_trip", "", []string{base}, base)` |
| 127 | `mustResolve(t, "euro_trip-pr5", []string{base}, base)` | `mustResolve(t, "euro_trip-pr5", "", []string{base}, base)` |
| 150 | `mustResolve(t, "", nil, repo)` | `mustResolve(t, "", "", nil, repo)` |
| 152 | `mustResolve(t, "", nil, cwd)` | `mustResolve(t, "", "", nil, cwd)` |
| 176 | `Resolve("review", []string{base}, base)` | `Resolve("review", "", []string{base}, base)` |
| 192 | `Resolve("slabledger", []string{a, b}, a)` | `Resolve("slabledger", "", []string{a, b}, a)` |
| 215 | `mustResolve(t, "euro_trip", roots, base)` | `mustResolve(t, "euro_trip", "", roots, base)` |
| 223 | `Resolve("nosuchproject", []string{base}, base)` | `Resolve("nosuchproject", "", []string{base}, base)` |
| 250 | `Resolve(name, []string{base}, base)` | `Resolve(name, "", []string{base}, base)` |
| 263 | `Resolve("anything", nil, base)` | `Resolve("anything", "", nil, base)` |
| 281 | `mustResolve(t, "", nil, dir)` | `mustResolve(t, "", "", nil, dir)` |
| 292 | `Resolve("", nil, filepath.Join(base, "gone"))` | `Resolve("", "", nil, filepath.Join(base, "gone"))` |

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/resolve/ -run 'TestWorkspaceIDDerivationIsPinned|TestResolveAgreesWithWithSession|TestResolveDerivesTheNamedSession' -v`

Expected: FAIL to build, with `undefined: WithSession` and
`too many arguments in call to Resolve`.

- [ ] **Step 4: Write the implementation**

Replace `internal/resolve/resolve.go:79-117` with:

```go
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
```

- [ ] **Step 5: Update the 14 call sites outside the resolve package**

Each gains `""` as the new second argument, preserving today's behavior. None of
these commands can address a named session yet; Part B rewires the ones that
should.

| File:line | Before | After |
| --- | --- | --- |
| `internal/cli/rebuild.go:280` | `resolve.Resolve("", nil, repoRoot)` | `resolve.Resolve("", "", nil, repoRoot)` |
| `internal/cli/open.go:137` | `resolve.Resolve(name, defaults.Layer.RepositoryRoots, cwd)` | `resolve.Resolve(name, "", defaults.Layer.RepositoryRoots, cwd)` |
| `internal/cli/config.go:128` | `resolve.Resolve(name, defaults.Layer.RepositoryRoots, cwd)` | `resolve.Resolve(name, "", defaults.Layer.RepositoryRoots, cwd)` |
| `internal/cli/autostart.go:142` | `resolve.Resolve("", nil, repo.RepoRoot)` | `resolve.Resolve("", "", nil, repo.RepoRoot)` |
| `internal/cli/attach.go:87` | `resolve.Resolve(name, defaults.Layer.RepositoryRoots, cwd)` | `resolve.Resolve(name, "", defaults.Layer.RepositoryRoots, cwd)` |
| `internal/cli/status.go:143` | `resolve.Resolve(name, defaults.Layer.RepositoryRoots, cwd)` | `resolve.Resolve(name, "", defaults.Layer.RepositoryRoots, cwd)` |
| `internal/cli/status.go:225` | `resolve.Resolve("", nil, s.Worktree)` | `resolve.Resolve("", "", nil, s.Worktree)` |
| `internal/cli/status.go:259` | `resolve.Resolve("", nil, repo.RepoRoot)` | `resolve.Resolve("", "", nil, repo.RepoRoot)` |
| `internal/cli/stop.go:86` | `resolve.Resolve(fs.Arg(0), defaults.Layer.RepositoryRoots, cwd)` | `resolve.Resolve(fs.Arg(0), "", defaults.Layer.RepositoryRoots, cwd)` |
| `internal/cli/lifecycle_test.go:58` | `resolve.Resolve("", nil, cwd)` | `resolve.Resolve("", "", nil, cwd)` |
| `internal/cli/open_test.go:103` | `resolve.Resolve("", nil, cwd)` | `resolve.Resolve("", "", nil, cwd)` |
| `internal/cli/open_test.go:271` | `resolve.Resolve("", nil, cwd)` | `resolve.Resolve("", "", nil, cwd)` |
| `internal/cli/status_test.go:32` | `resolve.Resolve("", nil, cwd)` | `resolve.Resolve("", "", nil, cwd)` |
| `internal/cli/autostart_test.go:203` | `resolve.Resolve("", nil, cwd)` | `resolve.Resolve("", "", nil, cwd)` |

- [ ] **Step 6: Run the tests to verify they pass**

Run:

```bash
go build ./...
go test ./internal/resolve/ -v
go test ./...
gofmt -l internal
```

Expected: `go build` silent, all packages pass, `gofmt -l` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/resolve internal/cli
git commit -m "refactor(resolve): take the session as a parameter and add WithSession

Resolve hardcoded session := \"\" with a note that the <repo>/<session>
argument form would arrive later. It arrives now: Resolve takes the session
and WithSession re-derives ID, Session and SessionName for a different
session on the same repository without a git call, which is how rebuild and
status will reconstruct a live session's identity from its recorded session
component.

Resolve delegates to WithSession so the derivation exists in one place and
the two can never disagree. A golden test pins the hex digests for the
default and a named session: the workspace ID is the primary key of every
row in a user's state store, so no refactor may change it silently.

Every existing call site passes \"\" and keeps today's behavior."
```

---

### Task 2: the `internal/target` package — the target grammar

`<target>` is `<repo>` or `<repo>/<session>`. `/` is the separator because it
cannot appear in a git repository directory name, so no existing bare workspace
name becomes ambiguous.

The session component must match `^[A-Za-z0-9][A-Za-z0-9_-]*$` and be at most 64
characters. That is deliberately stricter than tmux's own rules, and the reason
is the bare-workspace shorthand in `cli.go`: an unrecognized first argument is
treated as a workspace name and passed to `open`. So `projectmux
docs/commands.md` — a mistyped path, a tab-completed filename — must fail as a
malformed target (exit 2, naming the grammar) rather than be resolved as a
workspace name and reported as an unknown workspace (exit 4). A restrictive
grammar converts a confusing error into an accurate one.

This task is **parsing only**. Choosing which session a bare target refers to
requires reading the state store for a bind; that is Part B's Task 5 and must not
appear here.

**Files:**
- Create: `internal/target/target.go`
- Create: `internal/target/target_test.go`
- Modify: `internal/cli/cli.go:168-190` (the `exitCode` classifier) and its import
  block
- Test: `internal/cli/exit_test.go`

**Interfaces:**
- Consumes: nothing from Task 1.
- Produces:
  - `type target.Ref struct { Present bool; Name string; Session string; HasSession bool }`
  - `type target.MalformedError struct { Arg string; Reason string }`
  - `func target.Parse(arg string) (target.Ref, error)`
  - `const target.MaxSessionLength = 64`

---

- [ ] **Step 1: Write the failing test**

Create `internal/target/target_test.go`:

```go
package target

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsValidTargets(t *testing.T) {
	long := strings.Repeat("s", MaxSessionLength)

	cases := []struct {
		arg  string
		want Ref
	}{
		{"", Ref{}},
		{"myrepo", Ref{Present: true, Name: "myrepo"}},
		{"myrepo/feature-a", Ref{Present: true, Name: "myrepo", Session: "feature-a", HasSession: true}},
		{"myrepo/A1", Ref{Present: true, Name: "myrepo", Session: "A1", HasSession: true}},
		{"myrepo/1", Ref{Present: true, Name: "myrepo", Session: "1", HasSession: true}},
		{"myrepo/a_b-c", Ref{Present: true, Name: "myrepo", Session: "a_b-c", HasSession: true}},
		{"myrepo/" + long, Ref{Present: true, Name: "myrepo", Session: long, HasSession: true}},
		// The repository component is not validated here; resolve.byName owns
		// that rule and reports its own error.
		{"euro_trip.old", Ref{Present: true, Name: "euro_trip.old"}},
	}

	for _, tc := range cases {
		got, err := Parse(tc.arg)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", tc.arg, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.arg, got, tc.want)
		}
	}
}

func TestParseRejectsMalformedTargets(t *testing.T) {
	cases := []struct {
		name string
		arg  string
	}{
		{"empty session", "repo/"},
		{"empty repository", "/session"},
		{"more than one separator", "a/b/c"},
		{"leading dash", "repo/-feature"},
		{"leading underscore", "repo/_feature"},
		{"session over the length limit", "repo/" + strings.Repeat("s", MaxSessionLength+1)},
		{"session with a dot", "repo/feature.a"},
		{"session with a space", "repo/feature a"},
		// The case the restrictive grammar exists for: a mistyped path must
		// report the grammar, not "unknown workspace".
		{"a path mistaken for a target", "docs/commands.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.arg)
			var malformed *MalformedError
			if !errors.As(err, &malformed) {
				t.Fatalf("Parse(%q) = %+v, %v; want *MalformedError", tc.arg, got, err)
			}
			if got != (Ref{}) {
				t.Errorf("Parse(%q) returned %+v alongside its error; want the zero Ref", tc.arg, got)
			}
			if malformed.Arg != tc.arg {
				t.Errorf("MalformedError.Arg = %q, want %q", malformed.Arg, tc.arg)
			}
			// Every message names the grammar, because the whole point of the
			// restrictive grammar is that the user is told what a target is.
			msg := err.Error()
			for _, want := range []string{tc.arg, "<repo>/<session>"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			if malformed.Reason == "" {
				t.Error("MalformedError carries no reason")
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/target/ -v`

Expected: FAIL — the package does not exist
(`no required module provides package .../internal/target`).

- [ ] **Step 3: Write the implementation**

Create `internal/target/target.go`:

```go
// Package target parses the CLI's <target> argument, which is <repo> or
// <repo>/<session>. It is the grammar layer only: it neither resolves a
// repository nor chooses a session, so it makes no git call and reads no
// state.
//
// "/" is the separator because it cannot appear in a git repository directory
// name, so no bare workspace name that worked before becomes ambiguous.
package target

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxSessionLength bounds the session component. Together with
// sessionPattern this is deliberately stricter than tmux's own session-name
// rules. The reason is the bare-workspace shorthand in the CLI: an
// unrecognized first argument is treated as a workspace name and opened. A
// mistyped path such as "docs/commands.md" must therefore fail as a malformed
// target naming the grammar (exit 2), not as an unknown workspace (exit 4).
const MaxSessionLength = 64

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Ref is a parsed target argument.
type Ref struct {
	Present    bool   // an argument was given at all
	Name       string // repository component; "" when !Present
	Session    string // session component; "" for the default session
	HasSession bool   // the argument carried a "/<session>"
}

// MalformedError reports an argument that is not a valid target. The CLI maps
// it to exit 2.
type MalformedError struct {
	Arg    string
	Reason string
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf(
		"malformed target %q: %s; a target is <repo> or <repo>/<session>, where "+
			"<session> begins with a letter or a digit, continues with letters, "+
			"digits, \"-\" or \"_\", and is at most %d characters",
		e.Arg, e.Reason, MaxSessionLength)
}

// Parse splits a target argument into its repository and session components.
// An empty argument is the absent target and is not an error: Ref.Present
// distinguishes "no target" from "a target naming the default session", and
// the two select the session differently.
//
// The repository component is checked only for emptiness. resolve.byName
// already rejects path separators, "." and "..", and glob metacharacters, and
// reports an UnknownWorkspaceError naming the searched roots; duplicating that
// rule here would create two sources of truth that could disagree.
func Parse(arg string) (Ref, error) {
	if arg == "" {
		return Ref{}, nil
	}

	name, session, hasSeparator := strings.Cut(arg, "/")
	if !hasSeparator {
		return Ref{Present: true, Name: arg}, nil
	}

	switch {
	case strings.Contains(session, "/"):
		return Ref{}, &MalformedError{Arg: arg, Reason: `it carries more than one "/" separator`}
	case name == "":
		return Ref{}, &MalformedError{Arg: arg, Reason: "the repository component is empty"}
	case session == "":
		return Ref{}, &MalformedError{Arg: arg, Reason: "the session component is empty"}
	case len(session) > MaxSessionLength:
		return Ref{}, &MalformedError{
			Arg:    arg,
			Reason: fmt.Sprintf("the session component is %d characters long", len(session)),
		}
	case !sessionPattern.MatchString(session):
		return Ref{}, &MalformedError{
			Arg:    arg,
			Reason: fmt.Sprintf("the session component %q is not a valid session name", session),
		}
	}

	return Ref{Present: true, Name: name, Session: session, HasSession: true}, nil
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/target/ -v`

Expected: PASS for `TestParseAcceptsValidTargets` and every subtest of
`TestParseRejectsMalformedTargets`.

- [ ] **Step 5: Write the failing exit-code test**

Append to `internal/cli/exit_test.go`:

```go
// A malformed target is a usage failure. It must not be allowed to fall
// through to the generic exit 1: the grammar exists precisely so a mistyped
// path is reported as bad usage rather than as an unknown workspace.
func TestMalformedTargetExitsOnUsage(t *testing.T) {
	err := &target.MalformedError{Arg: "docs/commands.md", Reason: "the session component is invalid"}

	if got := exitCode(err); got != ExitUsage {
		t.Errorf("exitCode(MalformedError) = %d, want %d", got, ExitUsage)
	}
}
```

Add the import to that file's import block:

```go
import (
	"errors"
	"testing"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/target"
)
```

- [ ] **Step 6: Run the exit-code test to verify it fails**

Run: `go test ./internal/cli/ -run TestMalformedTargetExitsOnUsage -v`

Expected: FAIL with `exitCode(MalformedError) = 1, want 2`.

- [ ] **Step 7: Register the error in the CLI's exit-code mapping**

In `internal/cli/cli.go`, add the import alongside the other internal packages:

```go
	"github.com/gambtho/projectmux/internal/target"
```

Then replace `exitCode` at `internal/cli/cli.go:168-190`:

```go
func exitCode(err error) int {
	var (
		usageErr   *usageError
		malformed  *target.MalformedError
		ambiguous  *resolve.AmbiguousError
		unknown    *resolve.UnknownWorkspaceError
		invalidCfg *config.InvalidConfigError
		refusal    *controller.RefusalError
	)
	switch {
	case errors.As(err, &usageErr):
		return ExitUsage
	// A malformed target is bad usage, not an unknown workspace: the
	// restrictive grammar exists so a mistyped path reports what a target is
	// rather than which roots were searched for it.
	case errors.As(err, &malformed):
		return ExitUsage
	case errors.As(err, &ambiguous):
		return ExitAmbiguous
	case errors.As(err, &unknown):
		return ExitUnknownWorkspace
	case errors.As(err, &invalidCfg):
		return ExitInvalidConfig
	case errors.As(err, &refusal):
		return ExitRefused
	default:
		return ExitError
	}
}
```

- [ ] **Step 8: Run the tests to verify they pass**

Run:

```bash
go build ./...
go test ./internal/target/ ./internal/cli/
gofmt -l internal
```

Expected: both packages pass, `gofmt -l` prints nothing.

- [ ] **Step 9: Commit**

```bash
git add internal/target internal/cli/cli.go internal/cli/exit_test.go
git commit -m "feat(target): parse the <repo>/<session> target grammar

A target is <repo> or <repo>/<session>. \"/\" is the separator because it
cannot appear in a git repository directory name, so no bare workspace name
that worked before becomes ambiguous.

The session component is restricted to ^[A-Za-z0-9][A-Za-z0-9_-]*\$ and 64
characters, which is stricter than tmux's own rules. The CLI treats an
unrecognized first argument as a workspace name, so without a restrictive
grammar a mistyped path such as 'projectmux docs/commands.md' resolves as a
workspace name and reports an unknown workspace (exit 4). It now reports a
malformed target naming the grammar (exit 2).

The repository component is checked only for emptiness: resolve.byName
already rejects separators, \".\", \"..\" and glob metacharacters, and two
copies of that rule could disagree.

Session selection for a bare target needs the state store and is not part of
this package."
```

---

### Task 3: `@dev_session`, the fourth identity key

Sessions carry three tmux user options today — `@dev_workspace_id`, `@dev_slug`,
`@dev_worktree` — and none of them records the session component. That is a
correctness bug the moment named sessions exist: `rebuild`'s `worktreeResolver`
re-derives a live session's identity by resolving its worktree unconditionally,
so a live `myrepo--feature-a` re-resolves to the *default* workspace ID and
`rebuild` reports a false identity conflict.

This task adds `@dev_session` as a fourth key: written at creation, carried on
`LiveSession`, and compared in `SessionBelongsTo` and `confirmCreation`. An
absent key reads as `""`, which is exactly what a v0.5.0 default session is — so
no existing ID changes, no existing session is invalidated, and no user is forced
to rebuild. That backward-compatibility guarantee is the most important test in
this task.

**DECISION: `RetagSession` (`internal/tmux/actuate.go:193-208`) is deliberately
left writing only two keys.** It exists solely to retag sessions created before
repositories became the unit of a workspace. Those sessions are all default
sessions, whose `@dev_session` is `""`, and an absent key already reads as `""`.
Writing the key would be a no-op that costs a third subprocess and disturbs the
deliberate crash-safe ordering documented at `actuate.go:183-188` — `@dev_worktree`
is written first because it is what `rebuild` matches a stale session by, so a
failure between calls must leave the worktree correct and the ID stale, a state
the next run already knows how to repair.

**Files:**
- Modify: `internal/controller/types.go:11-15` (add `KeySession`) and `33-39`
  (add `LiveSession.Session`)
- Modify: `internal/controller/interfaces.go:143-150` (add `SessionSpec.Session`)
- Modify: `internal/tmux/decode.go:20-30` (`fieldFormats` `[4]string` → `[5]string`)
- Modify: `internal/tmux/tmux.go:70-76` (doc comment: "four per-field" → "five")
  and `97-120` (`var values [4]string` → `[5]string`, `LiveSession` literal)
- Modify: `internal/tmux/actuate.go:16-20` (doc comment: "three identity keys")
  and `57-61` (`createArgv`'s `set-option` chain)
- Modify: `internal/controller/plan.go:107-116` (`SessionBelongsTo` and its doc
  comment)
- Modify: `internal/controller/ensure.go:388-394` (the `SessionSpec` literal) and
  `427-443` (`confirmCreation` and its doc comment)
- Modify: `internal/rebuild/classify.go:151` and `200-206`
  (`identityMismatchReason`) — the identity comparison that does not call
  `SessionBelongsTo`
- Modify: `internal/cli/list.go:135` — the other one
- Test: `internal/tmux/client_test.go:26-51` (`oneSessionScript`) and `53-64`
  (`TestSessionsObservesRawValues`)
- Test: `internal/tmux/decode_test.go:67-77` (`TestFieldFormatsOrderAndKeys`
  hardcodes `[4]string`; it must grow with the array or the package will not
  compile)
- Test: `internal/tmux/actuate_test.go:29-47` (`TestCreateArgvShape`) and
  `115-138` (`TestCreateArgvEscapesSessionNameInTargets`)
- Test: `internal/controller/plan_test.go` (new `SessionBelongsTo` tests)
- Test: `internal/rebuild/classify_test.go` and `internal/cli/list_test.go` (the
  two identity comparisons that do not route through `SessionBelongsTo`)

No existing test constructs a `controller.LiveSession` or a
`controller.SessionSpec` positionally — every literal in the repository uses
keyed fields, and the two helpers that look positional
(`internal/rebuild/classify_test.go:12` `live(name, id, slug, worktree)` and
`internal/tmux/actuate_test.go:16` `actuateSpec()`) build keyed literals inside.
So adding a field compiles everywhere and no unrelated test needs touching.

**Interfaces:**
- Consumes: `resolve.Workspace.Session` (unchanged field, now actually populated
  by Task 1's `Resolve`).
- Produces:
  - `const controller.KeySession = "@dev_session"`
  - `controller.LiveSession.Session string`
  - `controller.SessionSpec.Session string`
  - `func controller.SessionBelongsTo(s LiveSession, ws resolve.Workspace) bool`
    (unchanged signature, now comparing four keys)

---

- [ ] **Step 1: Write the failing `SessionBelongsTo` tests**

Append to `internal/controller/plan_test.go`:

```go
// The backward-compatibility guarantee. A session created by v0.5.0 carries
// no @dev_session at all, and tmux reports an absent user option as the empty
// string — which is exactly what a default session's session component is. So
// every session a user has running right now keeps matching, and nobody is
// forced to rebuild.
func TestSessionBelongsToMatchesAPreV060DefaultSession(t *testing.T) {
	ws := resolve.Workspace{
		ID:       "w1",
		Slug:     "slabledger",
		RepoRoot: "/w/slabledger",
		Session:  "",
	}
	legacy := controller.LiveSession{
		Name:        "slabledger",
		WorkspaceID: "w1",
		Slug:        "slabledger",
		Worktree:    "/w/slabledger",
		// Session is deliberately absent, as tmux reports it for a session
		// created before the key existed.
	}

	if !controller.SessionBelongsTo(legacy, ws) {
		t.Error("a v0.5.0 default session no longer belongs to its workspace")
	}
}

func TestSessionBelongsToMatchesANamedSession(t *testing.T) {
	ws := resolve.Workspace{
		ID:       "w2",
		Slug:     "slabledger",
		RepoRoot: "/w/slabledger",
		Session:  "feature-a",
	}
	live := controller.LiveSession{
		Name:        "slabledger--feature-a",
		WorkspaceID: "w2",
		Slug:        "slabledger",
		Worktree:    "/w/slabledger",
		Session:     "feature-a",
	}

	if !controller.SessionBelongsTo(live, ws) {
		t.Error("a named session does not belong to its own workspace")
	}
}

// A disagreeing session component is evidence of corruption or collision, not
// a match — the same rule the other three keys already enforce.
func TestSessionBelongsToRejectsADisagreeingSessionComponent(t *testing.T) {
	base := resolve.Workspace{
		ID:       "w1",
		Slug:     "slabledger",
		RepoRoot: "/w/slabledger",
	}
	live := controller.LiveSession{
		Name:        "slabledger",
		WorkspaceID: "w1",
		Slug:        "slabledger",
		Worktree:    "/w/slabledger",
	}

	cases := []struct {
		name       string
		wsSession  string
		livSession string
	}{
		{"live is named, workspace is default", "", "feature-a"},
		{"workspace is named, live is default", "feature-a", ""},
		{"both named, differently", "feature-a", "feature-b"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ws := base
			ws.Session = tc.wsSession
			s := live
			s.Session = tc.livSession
			if controller.SessionBelongsTo(s, ws) {
				t.Errorf("session %q was accepted for workspace session %q",
					tc.livSession, tc.wsSession)
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run 'TestSessionBelongsTo' -v`

Expected: FAIL to build with `unknown field Session in struct literal of type
controller.LiveSession`.

- [ ] **Step 3: Add the key constant and the `LiveSession` field**

Replace `internal/controller/types.go:8-15`:

```go
// The tmux session-scoped identity keys. The first three are reused verbatim
// from the Phase 1 Bash implementation (design §7); adoption of live
// Bash-created sessions depends on those exact spellings. KeySession was
// added when a repository gained the ability to hold more than one session:
// without it a live "<slug>--<session>" re-resolves to the default workspace
// ID and rebuild reports a false identity conflict. An absent key reads as
// "", which is exactly a default session, so no session created before it
// existed is invalidated.
const (
	KeyWorkspaceID = "@dev_workspace_id"
	KeySlug        = "@dev_slug"
	KeyWorktree    = "@dev_worktree"
	KeySession     = "@dev_session"
)
```

Replace `internal/controller/types.go:33-39`:

```go
type LiveSession struct {
	ID          string
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string
	// Session is the session component, empty for the repository's default
	// session — which is also what an absent @dev_session decodes to.
	Session string
}
```

- [ ] **Step 4: Compare the fourth key in `SessionBelongsTo`**

Replace `internal/controller/plan.go:107-116`:

```go
// SessionBelongsTo compares all four load-bearing identity keys (design §7): a
// session with the right workspace ID but a contradictory slug, repository
// root or session component is evidence of corruption or collision, not a
// match. The CLI's status and attach verdicts reuse it so the rendered
// identity can never drift from planning's. LiveSession.Worktree keeps its
// name because it mirrors the tmux user option @dev_worktree, which is
// unchanged; the value it carries is now the repository root. A session
// created before @dev_session existed reports "" for it, which is exactly a
// default session, so such a session still matches.
func SessionBelongsTo(s LiveSession, ws resolve.Workspace) bool {
	return s.WorkspaceID == ws.ID && s.Slug == ws.Slug &&
		s.Worktree == ws.RepoRoot && s.Session == ws.Session
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run 'TestSessionBelongsTo' -v`

Expected: PASS, all three tests and all three subtests.

- [ ] **Step 6: Write the failing decode test**

The observation path reads one field per subprocess. Update the fake tmux script
and its assertion in `internal/tmux/client_test.go`, replacing lines 26-64:

```go
// oneSessionScript answers the two observation phases: enumeration
// returns one session id, and each per-field display-message call
// returns a canned value — the worktree deliberately spans lines to
// prove raw values round-trip through the client untouched.
const oneSessionScript = `#!/bin/sh
while [ "$1" = "-L" ]; do shift 2; done
cmd="$1"
shift
case "$cmd" in
list-sessions)
	printf '$0\n'
	;;
display-message)
	# args: -p -t <target> <format>
	case "$4" in
	'#{session_name}') printf 'alpha\n' ;;
	'#{@dev_workspace_id}') printf 'w1\n' ;;
	'#{@dev_slug}') printf 'proj\n' ;;
	'#{@dev_worktree}') printf '/w/evil\npath\n' ;;
	'#{@dev_session}') printf 'feature-a\n' ;;
	*) exit 2 ;;
	esac
	;;
*)
	exit 2
	;;
esac
`

// legacySessionScript is a session created before @dev_session existed: tmux
// reports an unset user option as an empty value, which must decode to the
// default session rather than to anything that would invalidate the session.
const legacySessionScript = `#!/bin/sh
while [ "$1" = "-L" ]; do shift 2; done
cmd="$1"
shift
case "$cmd" in
list-sessions)
	printf '$0\n'
	;;
display-message)
	case "$4" in
	'#{session_name}') printf 'alpha\n' ;;
	'#{@dev_workspace_id}') printf 'w1\n' ;;
	'#{@dev_slug}') printf 'proj\n' ;;
	'#{@dev_worktree}') printf '/w/alpha\n' ;;
	'#{@dev_session}') printf '\n' ;;
	*) exit 2 ;;
	esac
	;;
*)
	exit 2
	;;
esac
`

func TestSessionsObservesRawValues(t *testing.T) {
	fakeTmux(t, oneSessionScript)
	live, err := (&Client{}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := controller.LiveSession{
		ID: "$0", Name: "alpha", WorkspaceID: "w1", Slug: "proj",
		Worktree: "/w/evil\npath", Session: "feature-a",
	}
	if len(live) != 1 || live[0] != want {
		t.Errorf("Sessions = %+v, want [%+v]", live, want)
	}
}

func TestSessionsDecodesAnAbsentSessionKeyAsTheDefaultSession(t *testing.T) {
	fakeTmux(t, legacySessionScript)
	live, err := (&Client{}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("Sessions = %+v, want one session", live)
	}
	if live[0].Session != "" {
		t.Errorf("Session = %q, want the default session", live[0].Session)
	}
}
```

- [ ] **Step 7: Run the decode test to verify it fails**

Run: `go test ./internal/tmux/ -run 'TestSessionsObservesRawValues|TestSessionsDecodesAnAbsentSessionKey' -v`

Expected: FAIL. `TestSessionsObservesRawValues` reports a `Session:""` field
where `"feature-a"` was wanted — the client never queries the key, so the fake
script's new `@dev_session` branch is never reached.
`TestSessionsDecodesAnAbsentSessionKeyAsTheDefaultSession` passes vacuously at
this point and only becomes meaningful after Step 8; it is written now so the
absent-key guarantee is never added as an afterthought.

- [ ] **Step 8: Query and decode the fourth field**

Replace `internal/tmux/decode.go:20-30`:

```go
// fieldFormats queries one field per subprocess, in this fixed order:
// name, workspace ID, slug, worktree, session. The whole output of one
// query is one raw value, so no in-band framing exists for a value to
// forge — tmux emits option values verbatim in formats, and identity
// values are not newline-free (spec §5). A session created before
// @dev_session existed answers its query with an empty value, which is
// exactly the default session.
var fieldFormats = [5]string{
	"#{session_name}",
	"#{" + controller.KeyWorkspaceID + "}",
	"#{" + controller.KeySlug + "}",
	"#{" + controller.KeyWorktree + "}",
	"#{" + controller.KeySession + "}",
}
```

Replace `internal/tmux/tmux.go:99` and the literal at `113-119`:

```go
		var values [5]string
```

```go
		live = append(live, controller.LiveSession{
			ID:          id,
			Name:        values[0],
			WorkspaceID: values[1],
			Slug:        values[2],
			Worktree:    values[3],
			Session:     values[4],
		})
```

And update the count in the `Sessions` doc comment at `internal/tmux/tmux.go:70-76`:

```go
// Sessions lists every live session with whatever identity keys it
// carries, in two phases: a strictly validated session-id enumeration,
// then five per-field display-message calls per session whose entire
// output is one raw value (spec §5) — no in-band framing exists for a
// value to forge. No server is absence: an empty list and nil error.
// Any other failure is an error, which callers must render as
// uncertainty, never as absence (design §9).
```

Finally, `internal/tmux/decode_test.go:67-77` asserts the array's exact
contents against a hardcoded `[4]string`, so the package will not compile
until it grows too. This is not optional cleanup — it is part of the same
edit. Replace it:

```go
func TestFieldFormatsOrderAndKeys(t *testing.T) {
	want := [5]string{
		"#{session_name}",
		"#{" + controller.KeyWorkspaceID + "}",
		"#{" + controller.KeySlug + "}",
		"#{" + controller.KeyWorktree + "}",
		"#{" + controller.KeySession + "}",
	}
	if fieldFormats != want {
		t.Errorf("fieldFormats = %v, want %v", fieldFormats, want)
	}
}
```

- [ ] **Step 9: Run the decode tests to verify they pass**

Run: `go test ./internal/tmux/ -run 'TestSessions|TestFieldFormats' -v`

Expected: PASS. `TestFieldFormatsOrderAndKeys` is included deliberately: it
is the test that proves the array and its assertion moved together, and
running only `TestSessions` would let a stale `[4]string` fail the build
one step later instead.

- [ ] **Step 10: Write the failing `createArgv` test**

In `internal/tmux/actuate_test.go`, add `Session: "feature-a"` to the shared spec
helper at lines 16-27:

```go
func actuateSpec() controller.SessionSpec {
	return controller.SessionSpec{
		Name:        "slab",
		WorkspaceID: "w1",
		Slug:        "proj",
		Worktree:    "/w/slab",
		Session:     "feature-a",
		Env:         map[string]string{"B_KEY": "2", "A_KEY": "1"},
		Windows: []controller.WindowSpec{
			{Name: "agent-1", Command: "claude", Dir: "/w/slab", Focus: true},
			{Name: "shell", Dir: "/w/slab/sub"},
		},
	}
}
```

Add the fourth `set-option` to the expectation list in `TestCreateArgvShape`
(line 38, after the `@dev_worktree` entry):

```go
		"; set-option -t slab @dev_worktree /w/slab",
		"; set-option -t slab @dev_session feature-a",
```

Add the same to `TestCreateArgvEscapesSessionNameInTargets` (line 125, after the
`@dev_worktree` entry) — every new argv element must go through
`escapeChainArg`, and this is the test that proves it for the mid-chain `-t`:

```go
		`set-option -t slab\; @dev_worktree`,
		`set-option -t slab\; @dev_session`,
```

Append a test that the value itself is escaped, since a trailing `;` in a session
value would otherwise be silently truncated by tmux's chain parser:

```go
// A session component cannot contain ";" under the target grammar, but
// createArgv must not depend on a validation that lives in another package:
// every argv element it emits goes through escapeChainArg, or the chain parser
// strips the trailing ";" and truncates the value.
func TestCreateArgvEscapesTheSessionValue(t *testing.T) {
	spec := actuateSpec()
	spec.Session = "feature;"
	joined := strings.Join(createArgv(spec), " ")

	if !strings.Contains(joined, `@dev_session feature\;`) {
		t.Errorf("argv %q does not carry an escaped @dev_session value", joined)
	}
}

// A default session writes the key with an empty value rather than omitting
// it, so the four identity keys are always set together and a mid-chain
// failure cannot leave a session tagged with three of them. The assertion
// walks argv rather than a joined string: the empty value is invisible once
// joined with spaces.
func TestCreateArgvWritesTheSessionKeyForTheDefaultSession(t *testing.T) {
	spec := actuateSpec()
	spec.Session = ""
	argv := createArgv(spec)

	i := slices.Index(argv, controller.KeySession)
	if i < 0 {
		t.Fatalf("argv %v does not set @dev_session for the default session", argv)
	}
	if i+1 >= len(argv) || argv[i+1] != "" {
		t.Errorf("argv %v: @dev_session is not followed by an empty value", argv)
	}
	if i < 4 || argv[i-1] != "slab" || argv[i-2] != "-t" || argv[i-3] != "set-option" {
		t.Errorf("argv %v: @dev_session is not a set-option on the session target", argv)
	}
}
```

- [ ] **Step 11: Run the argv test to verify it fails**

Run: `go test ./internal/tmux/ -run TestCreateArgv -v`

Expected: FAIL — `TestCreateArgvShape` reports `missing "; set-option -t slab
@dev_session feature-a"`, and both new tests report a missing `@dev_session`.

- [ ] **Step 12: Emit the fourth `set-option`**

Replace `internal/tmux/actuate.go:57-61`:

```go
	argv = append(argv,
		";", "set-option", "-t", target, controller.KeyWorkspaceID, escapeChainArg(spec.WorkspaceID),
		";", "set-option", "-t", target, controller.KeySlug, escapeChainArg(spec.Slug),
		";", "set-option", "-t", target, controller.KeyWorktree, escapeChainArg(spec.Worktree),
		";", "set-option", "-t", target, controller.KeySession, escapeChainArg(spec.Session),
	)
```

Update the `CreateSession` doc comment at `internal/tmux/actuate.go:16-20`:

```go
// CreateSession creates the workspace session in one chained tmux
// invocation (verified on tmux 3.4): new-session with the first window,
// the four identity keys via set-option, remaining windows detached,
// and an explicit focus selection when configured. One subprocess makes
// creation-with-identity near-atomic (open/attach spec §4).
```

Add the `Session` field to `SessionSpec` in
`internal/controller/interfaces.go:143-150`:

```go
type SessionSpec struct {
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string
	// Session is the session component the created session is tagged with,
	// empty for the repository's default session.
	Session string
	Env     map[string]string
	Windows []WindowSpec
}
```

- [ ] **Step 13: Run the argv tests to verify they pass**

Run: `go test ./internal/tmux/ -run TestCreateArgv -v`

Expected: PASS.

- [ ] **Step 14: Populate and confirm the key in `Ensure`**

`createSession` builds the spec and `confirmCreation` independently re-checks the
identity keys after creation; both must know about the fourth.

Replace `internal/controller/ensure.go:388-394`:

```go
	spec := SessionSpec{
		Name:        name,
		WorkspaceID: id,
		Slug:        d.Workspace.Slug,
		Worktree:    d.Workspace.RepoRoot,
		Session:     d.Workspace.Session,
		Env:         d.Config.Environment,
		Windows:     windows,
	}
```

Replace `internal/controller/ensure.go:427-443` (the doc comment and the identity
comparison inside `confirmCreation`):

```go
// confirmCreation reports why the post-create observation does not
// confirm the created session, or "" when it does: live, agreeing on
// all four identity keys, under the allocated name.
func confirmCreation(snap Snapshot, d Desired, allocated string) string {
	switch snap.Session.State {
	case SessionUnknown:
		return "tmux became unobservable after creation"
	case SessionAbsent:
		return "the session is absent after creation"
	}
	live := snap.Session.ByIdentity
	if live == nil {
		return "no identity-matched session was observed after creation"
	}
	if live.WorkspaceID != d.Workspace.ID || live.Slug != d.Workspace.Slug ||
		live.Worktree != d.Workspace.RepoRoot || live.Session != d.Workspace.Session {
		return fmt.Sprintf("session %q carries contradictory identity keys after creation", live.Name)
	}
```

- [ ] **Step 15: Write the failing tests for the two comparisons that bypass `SessionBelongsTo`**

`SessionBelongsTo` is not the only place identity is compared. Two callers
derive it themselves from the same fields, and both were written when three
keys were the whole of identity:

- `internal/rebuild/classify.go:151` — `row.Slug != s.Slug || row.RepoRoot != s.Worktree`
- `internal/cli/list.go:135` — `s.Slug != rec.Slug || s.Worktree != rec.RepoRoot`

Left alone, a live session whose `@dev_session` contradicts its record slips
past both: `Classify` falls through to the settled or adopt case and writes the
adoption, and `list` reports `identity_conflict: false` for the same corruption.
Adding a fourth key to `SessionBelongsTo` and not to these two is worse than not
adding it at all — the checks would then disagree about what identity means.

Add to `internal/rebuild/classify_test.go`:

```go
// TestClassifyRejectsASessionWhoseSessionKeyContradictsTheRecord covers the
// fourth identity key. Slug and worktree agree here; only @dev_session
// disagrees, which is exactly the case a three-key comparison cannot see.
func TestClassifyRejectsASessionWhoseSessionKeyContradictsTheRecord(t *testing.T) {
	s := live("proj--feature-a", "w1", "proj", "/w/proj")
	s.Session = "feature-b"
	rows := []state.Record{{
		ID: "w1", Slug: "proj", RepoRoot: "/w/proj", Session: "feature-a",
	}}

	plan := Classify([]controller.LiveSession{s}, rows)

	if len(plan.Candidates) != 0 {
		t.Errorf("Candidates = %+v, want none: a contradiction is not an adoption",
			plan.Candidates)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", plan.Conflicts)
	}
	if !strings.Contains(plan.Conflicts[0].Reason, "feature-b") {
		t.Errorf("Reason = %q, want it to name the live session component",
			plan.Conflicts[0].Reason)
	}
}
```

Add to `internal/cli/list_test.go`:

```go
// TestListReportsAnIdentityConflictOnTheSessionKey is the list-side half of
// the same rule: what rebuild refuses to adopt, list must not report as a
// clean live session.
func TestListReportsAnIdentityConflictOnTheSessionKey(t *testing.T) {
	h := newListHarness()
	h.record(state.Record{
		ID: "w1", Slug: "proj", RepoRoot: "/w/proj", Session: "feature-a",
	})
	h.observe(controller.LiveSession{
		Name: "proj--feature-a", WorkspaceID: "w1",
		Slug: "proj", Worktree: "/w/proj", Session: "feature-b",
	})

	env := h.run(t)

	if len(env.Workspaces) != 1 {
		t.Fatalf("Workspaces = %+v, want one", env.Workspaces)
	}
	if !env.Workspaces[0].IdentityConflict {
		t.Error("IdentityConflict = false, want true: @dev_session contradicts the record")
	}
}
```

Both test files gain whatever imports these need (`strings` in
`classify_test.go`). If `newListHarness`, `record`, or `observe` are spelled
differently in `internal/cli/list_test.go`, follow that file's existing helpers
rather than introducing these — the assertion is what matters, not the harness.

- [ ] **Step 16: Run the two tests to verify they fail**

Run: `go test ./internal/rebuild/ ./internal/cli/ -run 'SessionKey' -v`

Expected: FAIL. `Classify` reports one adopt candidate and no conflict; `list`
reports `IdentityConflict = false`. Both because the comparison stops at three
keys.

- [ ] **Step 17: Compare the fourth key in both callers**

In `internal/rebuild/classify.go:151`:

```go
		case row != nil && (row.Slug != s.Slug || row.RepoRoot != s.Worktree ||
			row.Session != s.Session):
			conflict(s, identityMismatchReason(s, row))
```

And widen the reason at `internal/rebuild/classify.go:200-206` so it names the
key that actually disagrees:

```go
func identityMismatchReason(s controller.LiveSession, row *state.Record) string {
	return fmt.Sprintf(
		"session %q carries slug %q, worktree %q, and session component %q, but "+
			"workspace %s is recorded as slug %q, worktree %q, and session "+
			"component %q; that contradiction is evidence of corruption or "+
			"collision rather than a match, so nothing is written.",
		s.Name, s.Slug, s.Worktree, s.Session,
		row.ID, row.Slug, row.RepoRoot, row.Session)
}
```

In `internal/cli/list.go:135`:

```go
			row.IdentityConflict = s.Slug != rec.Slug || s.Worktree != rec.RepoRoot ||
				s.Session != rec.Session
```

- [ ] **Step 18: Run the two tests to verify they pass**

Run: `go test ./internal/rebuild/ ./internal/cli/ -v`

Expected: PASS, the whole of both packages. Watch for existing
`identityMismatchReason` assertions in `classify_test.go` that match on the old
sentence — reword those to the new one rather than reverting the message.

- [ ] **Step 19: Run the full suite**

Run:

```bash
go build ./...
go test ./...
gofmt -l internal
```

Expected: everything passes and `gofmt -l` prints nothing. `RetagSession` and its
tests are untouched by design — see the decision recorded at the head of this
task.

- [ ] **Step 20: Commit**

```bash
git add internal/controller internal/tmux internal/rebuild internal/cli
git commit -m "feat(identity): record the session component as @dev_session

Sessions carried three tmux user options and none of them recorded which
session on the repository they were. That is a correctness bug the moment a
repository can hold more than one: rebuild's worktreeResolver re-derives a
live session's identity by resolving its worktree unconditionally, so a live
<slug>--<session> re-resolved to the default workspace ID and rebuild
reported a false identity conflict.

@dev_session is written with the other three in the same chained
new-session, carried on LiveSession, and compared by SessionBelongsTo and
confirmCreation. tmux reports an absent user option as \"\", which is
exactly what a default session's component is, so every session created by
v0.5.0 keeps matching: no stored ID changes and nobody is forced to rebuild.

rebuild.Classify and list derive identity themselves rather than calling
SessionBelongsTo, so both compare the fourth key too. Widening one and not
the others would leave three checks disagreeing about what identity is,
which is worse than the three-key comparison they replace.

RetagSession deliberately still writes two keys. It exists only to retag
sessions created before repositories became the unit of a workspace, all of
which are default sessions whose @dev_session is \"\" — writing it would be
a no-op costing a third subprocess and disturbing the crash-safe ordering
that keeps @dev_worktree ahead of the ID."
```

---
### Task 4: `internal/bindpath` — the path rules a bind obeys

A bind is *stored* relative to the repository root and must lie inside the
repository after `EvalSymlinks`. Containment is re-checked at **every use**, not
only at bind time (spec §4): a stored in-repository path can later be replaced
by a symlink pointing outside, after which host window creation would follow the
escaped path through `filepath.Join`. Two packages need these rules —
`internal/target` for the bind lookup and `internal/controller` for window
rendering — so they live in one package rather than being duplicated.

The stored form and the *argument* form are deliberately different, and the
difference is the one thing to get right in this task:

- `Resolve(repoRoot, rel)` reads the **stored** form, which is always
  repository-relative. That is what makes a bind survive the repository moving.
- `Rel(repoRoot, path)` converts a **user-typed** argument to the stored form,
  and takes a relative argument against the process's working directory — the
  ordinary CLI convention, and the only rule under which `--cwd .` and shell
  tab-completion mean what the user typed.

Those two rules disagree for the same string, so `Rel` carries a hint: when an
argument escapes the repository but `repoRoot/<argument>` *would* have resolved
inside it, the error names that directory. This is the copy-from-`list` case —
`list`'s `BIND` column shows the stored, repository-relative form, and pasting it
back into `bind` from anywhere but the repository root would otherwise fail with
a bare containment error that explains nothing. Spec §4 records the rule; the
hint is what keeps it from being a footgun.

**Files:**
- Create: `internal/bindpath/bindpath.go`
- Test: `internal/bindpath/bindpath_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks; standard library only.
- Produces:
  ```go
  func Resolve(repoRoot, rel string) (string, error)
  func Rel(repoRoot, path string) (string, error)
  func Contains(dir, path string) bool
  type EscapedError struct{ Rel, Resolved, RepoRoot, Suggestion string }
  func (e *EscapedError) Error() string
  ```

- [ ] **Step 1: Write the failing test**

Create `internal/bindpath/bindpath_test.go`:

```go
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
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/bindpath/ -v`

Expected: FAIL — the package does not compile, with `undefined: Contains`,
`undefined: Resolve`, `undefined: Rel`, `undefined: EscapedError`.

- [ ] **Step 3: Write the implementation**

Create `internal/bindpath/bindpath.go`:

```go
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
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/bindpath/ -v`

Expected: PASS, every subtest of `TestContainsComparesPathComponents` included,
and both working-directory tests
(`TestRelTakesARelativeArgumentAgainstTheWorkingDirectory`,
`TestRelSuggestsTheRepositoryRelativeReadingWhenAnArgumentEscapes`) among them.

Then `go build ./...` — expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/bindpath/bindpath.go internal/bindpath/bindpath_test.go
git commit -m "feat(bindpath): the containment rules a session bind obeys

A bind is stored relative to the repository root and must resolve inside
it after EvalSymlinks. Containment is component-wise, not a string
prefix, so a bind of services/api does not claim services/apixyz, and it
is re-checked on every read rather than only at bind time.

A typed argument is read against the working directory, so --cwd . means
the directory the user is standing in. Because that disagrees with the
stored form, an argument that escapes carries a did-you-mean naming the
directory the repository-relative reading would have found."
```

---

### Task 5: migration 0003 and the stored bind

Spec §4: migration 0003 adds a nullable `bind TEXT` to the workspaces table. It
is additive and does not require a rebuild. `RegisterWorkspace` stays fill-only
with respect to the bind — it never mentions the column — which is what lets
`rebuild` re-register a workspace without dropping its bind.

**Files:**
- Create: `internal/state/migrations/0003_bind.sql`
- Modify: `internal/state/migrate.go:23` (`SchemaVersion`)
- Modify: `internal/state/types.go:19-23` is untouched; `internal/state/types.go:46-60` (`Record`)
- Modify: `internal/state/store.go:19-23` (the `RegisterWorkspace` doc), `store.go:162-173` (`selectRecord`), `store.go:308-376` (`scanRecord`), and a new `SetBind` appended after `AdoptSessionName` (`store.go:675`)
- Test: `internal/state/migrate_test.go`, `internal/state/store_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks.
- Produces:
  ```go
  // state.Record gains:
  Bind *string
  // and:
  func (s *Store) SetBind(workspaceID string, bind *string, now time.Time) error
  const SchemaVersion = 3
  ```

- [ ] **Step 1: Write the failing migration test**

Append to `internal/state/migrate_test.go`:

```go
// columnsOf reads a table's column names through PRAGMA table_info, which is
// how the schema is inspected here rather than by parsing sqlite_master.
func columnsOf(t *testing.T, s *Store, table string) []string {
	t.Helper()
	// PRAGMA does not accept bound parameters; table is a literal in tests.
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer func() { _ = rows.Close() }()

	var names []string
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			t.Fatalf("scanning table_info(%s): %v", table, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	return names
}

func TestMigration0003AddsTheBindColumn(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if version != 3 {
		t.Errorf("user_version = %d, want 3", version)
	}

	cols := columnsOf(t, s, "workspaces")
	if !slices.Contains(cols, "bind") {
		t.Errorf("workspaces columns = %v, want a bind column", cols)
	}
}

// TestMigration0003UpgradesWithoutDataLoss runs the whole ladder — 1 to 3 — on
// a seeded pre-0002 database, because seedSchema1 is the only upgrade fixture
// this file has and a bind column added by an ALTER must not disturb the rows
// 0002 moved.
func TestMigration0003UpgradesWithoutDataLoss(t *testing.T) {
	root := seedSchema1(t)
	s, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = s.Close() }()

	cols := columnsOf(t, s, "workspaces")
	if !slices.Contains(cols, "bind") {
		t.Errorf("workspaces columns = %v, want a bind column", cols)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("bind = %v, want nil: an upgraded session has never been bound", rec.Bind)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "slabledger" ||
		rec.AppliedDigest == nil || *rec.AppliedDigest != "sha256:bbbb" {
		t.Errorf("record = %+v, want the assignment and digests preserved across 0003", rec)
	}
	if rec.LastOperation == nil || rec.LastOperation.Name != "open" {
		t.Errorf("last operation = %+v, want the row preserved across 0003", rec.LastOperation)
	}
}
```

Add `"slices"` to that file's import block.

- [ ] **Step 2: Run the migration test to verify it fails**

Run: `go test ./internal/state/ -run 'TestMigration0003' -v`

Expected: FAIL to compile with `rec.Bind undefined (type Record has no field or
method Bind)`. Comment out the two `rec.Bind` lines to see the schema half
report `user_version = 2, want 3` and `want a bind column`, then restore them.

- [ ] **Step 3: Add the migration and bump the schema version**

Create `internal/state/migrations/0003_bind.sql`:

```sql
-- Schema version 3: a session may open somewhere other than the repository
-- root (design §4). The path is stored relative to the repository root so it
-- survives the repository moving, and NULL means the session is unbound.
--
-- This migration is pure SQL by design (design §9) and runs inside the
-- transaction migrate.go opens, so PRAGMA foreign_keys would be a no-op here
-- and no statement may depend on one. Nothing here needs either: ADD COLUMN
-- rewrites no rows, touches no foreign key, and needs no table rebuild, which
-- is why upgrading to this version does not require `projectmux rebuild`.
--
-- Containment is not expressible as a CHECK constraint: whether a stored path
-- still resolves inside the repository depends on symlinks on the filesystem,
-- and it is re-verified on every read instead (internal/bindpath).
ALTER TABLE workspaces ADD COLUMN bind TEXT;
```

Modify `internal/state/migrate.go:23`:

```go
// SchemaVersion is the newest schema this build understands.
const SchemaVersion = 3
```

- [ ] **Step 4: Add `Record.Bind` and read it back**

Modify `internal/state/types.go`, inside `Record` (after `AppliedDigest`):

```go
	AppliedDigest   *string
	// Bind is the session's base directory, stored relative to the
	// repository root and slash-separated. It is nil when the session is
	// unbound, in which case the repository root is the base. It is
	// re-verified against the filesystem on every use rather than trusted
	// (design §4); see internal/bindpath.
	Bind            *string
	RegisteredAt    time.Time
```

Modify `internal/state/store.go:162-173` — add `w.bind` to the positional
column list:

```go
const selectRecord = `
SELECT
	w.id, w.repository_id, r.slug, r.repo_root, w.session, w.proposed_session,
	w.actual_session, w.desired_digest, w.applied_digest, w.bind,
	w.registered_at, w.updated_at,
	cb.kind, cb.container_id, cb.container_user, cb.workdir,
	cb.health, cb.observed_at,
	o.operation, o.outcome, o.exit_status, o.error_summary, o.finished_at
FROM workspaces w
JOIN repositories r ON r.id = w.repository_id
LEFT JOIN container_bindings cb ON cb.repository_id = w.repository_id
LEFT JOIN last_operations o ON o.workspace_id = w.id`
```

Modify `scanRecord` (`store.go:308-376`) in three places. The declaration:

```go
	var (
		rec                 Record
		actual, desired     sql.NullString
		applied, bind       sql.NullString
		registered, updated string
```

the scan, which is positional and must match the column list exactly:

```go
	err := r.Scan(
		&rec.ID, &rec.RepositoryID, &rec.Slug, &rec.RepoRoot, &rec.Session,
		&rec.ProposedSession, &actual, &desired, &applied, &bind,
		&registered, &updated,
		&cKind, &cID, &cUser, &cWorkdir, &cHealth, &cObserved,
		&oName, &oOutcome, &oExit, &oSummary, &oFinished)
```

and the projection:

```go
	rec.ActualSession = nullable(actual)
	rec.DesiredDigest = nullable(desired)
	rec.AppliedDigest = nullable(applied)
	rec.Bind = nullable(bind)
```

- [ ] **Step 5: Run the migration test to verify it passes**

Run: `go test ./internal/state/ -run 'TestMigration0003' -v`

Expected: PASS.

Then `go test ./internal/state/ -run 'TestOpenCreatesTheLatestSchema|TestMigration0002' -v`
— expected: PASS, confirming the ladder still ends where the other tests expect.

- [ ] **Step 6: Write the failing `SetBind` test**

Append to `internal/state/store_test.go`:

```go
func TestSetBindRoundTripsAndClears(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	bind := "services/api"
	if err := s.SetBind(ws.ID, &bind, testTime); err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != bind {
		t.Fatalf("bind = %v, want %q", rec.Bind, bind)
	}

	if err := s.SetBind(ws.ID, nil, testTime); err != nil {
		t.Fatalf("SetBind(nil): %v", err)
	}
	rec, err = s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("bind = %v, want nil after clearing", rec.Bind)
	}
}

// TestSetBindOnAnUnregisteredWorkspace pins that SetBind records a bind and
// does not create the session: `bind` on a session that does not exist yet is
// a register-then-bind, and the registration is the caller's job.
func TestSetBindOnAnUnregisteredWorkspace(t *testing.T) {
	s := openTestStore(t)
	bind := "services/api"
	err := s.SetBind("absent", &bind, testTime)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("SetBind on an unregistered workspace = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 7: Run the `SetBind` test to verify it fails**

Run: `go test ./internal/state/ -run 'TestSetBind' -v`

Expected: FAIL to compile with `s.SetBind undefined (type *Store has no field
or method SetBind)`.

- [ ] **Step 8: Write `SetBind`**

Append to `internal/state/store.go`, after `AdoptSessionName`:

```go
// SetBind records a session's bind, or clears it when bind is nil. It does
// not create the workspace: the caller registers it first.
//
// The bind is written on its own rather than through RegisterWorkspace,
// because registration is fill-only with respect to it — see the workspaces
// upsert, which does not mention the column — and that is what lets `rebuild`
// re-register a session without dropping where it opens.
func (s *Store) SetBind(workspaceID string, bind *string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := requireWorkspace(tx, workspaceID); err != nil {
		return err
	}
	// A nil *string binds as SQL NULL only through an untyped nil any; a
	// typed (*string)(nil) would be rejected by the driver.
	var value any
	if bind != nil {
		value = *bind
	}
	if _, err := tx.Exec(
		"UPDATE workspaces SET bind = ?, updated_at = ? WHERE id = ?",
		value, encodeTime(now), workspaceID); err != nil {
		return fmt.Errorf("recording the bind for workspace %s: %w", workspaceID, err)
	}
	return tx.Commit()
}
```

- [ ] **Step 9: Run the `SetBind` test to verify it passes**

Run: `go test ./internal/state/ -run 'TestSetBind' -v`

Expected: PASS (both tests).

- [ ] **Step 10: Pin the fill-only guarantee**

This one is a regression guard on behavior the code already has, so it passes
on first run. Falsify it before trusting it: add `bind = excluded.bind` to the
workspaces upsert (`store.go:128-133`), watch this test fail, then remove the
line again.

Append to `internal/state/store_test.go`:

```go
// TestRegisterWorkspacePreservesTheBind is what protects `rebuild`. Rebuild
// re-runs registration over every recovered session, and registration that
// wrote the bind column would clear the bind of every session it touched.
func TestRegisterWorkspacePreservesTheBind(t *testing.T) {
	s := openTestStore(t)
	ws := testWorkspace("w1")
	mustRegister(t, s, ws)

	bind := "services/api"
	if err := s.SetBind(ws.ID, &bind, testTime); err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	if err := s.RegisterWorkspace(ws, "sha256:bbbb", testTime); err != nil {
		t.Fatalf("re-registering: %v", err)
	}

	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != bind {
		t.Fatalf("bind = %v, want %q preserved across re-registration", rec.Bind, bind)
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:bbbb" {
		t.Errorf("desired digest = %v, want the re-registration's value", rec.DesiredDigest)
	}
}
```

Also update the `RegisterWorkspace` doc comment (`store.go:19-23`) so the
guarantee is stated where the upsert is, not only in the test:

```go
// RegisterWorkspace upserts the repository and the session on it.
// Re-registration refreshes everything derivable from resolution and
// configuration while preserving registered_at, the assigned session name,
// the applied digest, the container binding, and the session's bind —
// rebuilding the database is simply re-running registration (design §7).
```

- [ ] **Step 11: Run the whole state package**

Run: `go test ./internal/state/ -v`

Expected: PASS throughout, including the pre-existing read-only and migration
tests, which all key on `SchemaVersion` rather than a literal.

Then `go build ./...` — expected: no output.

- [ ] **Step 12: Commit**

```bash
git add internal/state/migrations/0003_bind.sql internal/state/migrate.go \
        internal/state/types.go internal/state/store.go \
        internal/state/migrate_test.go internal/state/store_test.go
git commit -m "feat(state): store a session's bind

Migration 0003 adds a nullable, repository-relative bind TEXT to
workspaces. It is a plain ADD COLUMN: no table rebuild, so upgrading does
not require rebuild. SetBind writes and clears it; RegisterWorkspace
deliberately never mentions the column, so rebuild's re-registration
cannot drop where a session opens."
```

---

### Task 6: bind-based session selection in `internal/target`

Spec §3. This is the second half of `internal/target`: given a parsed `Ref`,
produce the `resolve.Workspace` to act on.

Selection keys on **target presence**, not on whether the target happened to
carry a session. Collapsing the two would make `open myrepo`, run from inside a
bound directory, silently open a *named* session the user did not ask for — the
mistake this design's first draft made.

**Files:**
- Create: `internal/target/select.go`
- Test: `internal/target/select_test.go`

**Interfaces:**
- Consumes:
  ```go
  func resolve.Resolve(name, session string, roots []string, cwd string) (resolve.Workspace, error)
  func resolve.WithSession(ws resolve.Workspace, session string) resolve.Workspace
  type target.Ref struct{ Present bool; Name string; Session string; HasSession bool }
  func bindpath.Resolve(repoRoot, rel string) (string, error)
  func bindpath.Rel(repoRoot, path string) (string, error)
  func bindpath.Contains(dir, path string) bool
  func state.OpenReadOnly(root string) (*state.ReadOnlyStore, state.Inspection, error)
  func state.IsMissingDatabase(err error) bool
  // state.Record.Bind *string, state.Record.RepositoryID string
  ```
- Produces:
  ```go
  func Select(ref Ref, roots []string, cwd, stateRoot string) (resolve.Workspace, error)
  ```

- [ ] **Step 1: Write the failing test for target presence**

This pair is the most important test in the task. Create
`internal/target/select_test.go`:

```go
package target

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var testTime = time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)

// The test environment has no gitconfig, so identity and the initial branch
// name are supplied explicitly (copied from internal/resolve/resolve_test.go).
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

// base returns a symlink-free temporary directory, so resolved paths compare
// equal to the canonical ones resolve and bindpath return.
func base(t *testing.T) string {
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

// fixture is one repository under a searchable root, plus a state root.
type fixture struct {
	roots     []string
	repo      string
	stateRoot string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := base(t)
	return fixture{
		roots:     []string{dir},
		repo:      makeRepo(t, filepath.Join(dir, "slabledger")),
		stateRoot: t.TempDir(),
	}
}

// bindSession registers session on the fixture's repository and records its
// bind. It goes through the real store so the lookup reads exactly what the
// bind command will write.
func (f fixture) bindSession(t *testing.T, session, bind string) {
	t.Helper()
	ws, err := resolve.Resolve("", session, f.roots, f.repo)
	if err != nil {
		t.Fatalf("resolving %q: %v", session, err)
	}
	st, err := state.Open(f.stateRoot)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = st.Close() }()
	if err := st.RegisterWorkspace(ws, "sha256:aaaa", testTime); err != nil {
		t.Fatalf("RegisterWorkspace(%q): %v", session, err)
	}
	if bind != "" {
		if err := st.SetBind(ws.ID, &bind, testTime); err != nil {
			t.Fatalf("SetBind(%q): %v", session, err)
		}
	}
}

func mustSelect(t *testing.T, ref Ref, f fixture, cwd string) resolve.Workspace {
	t.Helper()
	ws, err := Select(ref, f.roots, cwd, f.stateRoot)
	if err != nil {
		t.Fatalf("Select(%+v) from %s: %v", ref, cwd, err)
	}
	return ws
}

// TestSelectKeysOnTargetPresence is the pair spec §7 calls out. From inside a
// bound directory, a bare `<repo>` target must still address the default
// session; only the absence of a target lets the cwd choose.
func TestSelectKeysOnTargetPresence(t *testing.T) {
	f := newFixture(t)
	bound := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")

	explicit := mustSelect(t, Ref{Present: true, Name: "slabledger"}, f, bound)
	if explicit.Session != "" {
		t.Errorf("Session = %q from an explicit <repo> target, want the default session",
			explicit.Session)
	}

	implicit := mustSelect(t, Ref{}, f, bound)
	if implicit.Session != "api" {
		t.Errorf("Session = %q with no target inside the bound directory, want api",
			implicit.Session)
	}
	if implicit.SessionName != "slabledger--api" {
		t.Errorf("SessionName = %q, want slabledger--api", implicit.SessionName)
	}
	if implicit.RepositoryID != explicit.RepositoryID {
		t.Errorf("the two selections disagree on the repository: %s vs %s",
			implicit.RepositoryID, explicit.RepositoryID)
	}
	if implicit.ID == explicit.ID {
		t.Error("the bound session and the default session share a workspace ID")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/target/ -run TestSelectKeysOnTargetPresence -v`

Expected: FAIL to compile with `undefined: Select`.

- [ ] **Step 3: Write `Select` and the bind lookup**

Create `internal/target/select.go`:

```go
package target

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/gambtho/projectmux/internal/bindpath"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// Select turns a parsed target into the workspace to act on. stateRoot is
// the state directory; the bind lookup opens its database read-only.
//
// Selection keys on target *presence*, not on whether the target carried a
// session (design §3). There are exactly three cases:
//
//  1. <repo>/<session> — that session. Explicit and final.
//  2. <repo>          — the default session. The cwd gets no vote; this is
//     also the only way to address the default session from inside a bound
//     directory.
//  3. no target       — resolve the repository from the cwd, then look up a
//     bind that contains the cwd, falling back to the default session.
//
// Cases 1 and 2 are both "the target is present", which is why the branch
// below tests ref.Present rather than ref.HasSession. Collapsing them would
// make `open myrepo`, run from inside a bound directory, open a named session
// the user did not ask for.
func Select(ref Ref, roots []string, cwd, stateRoot string) (resolve.Workspace, error) {
	ws, err := resolve.Resolve(ref.Name, ref.Session, roots, cwd)
	if err != nil {
		return resolve.Workspace{}, err
	}
	if ref.Present {
		return ws, nil
	}
	session, err := sessionForCwd(ws, cwd, stateRoot)
	if err != nil {
		return resolve.Workspace{}, err
	}
	return resolve.WithSession(ws, session), nil
}

// sessionForCwd finds the session on ws's repository whose bind contains cwd,
// or "" for the default session when none does.
func sessionForCwd(ws resolve.Workspace, cwd, stateRoot string) (string, error) {
	// state.OpenReadOnly's failures are typed and deliberately not
	// interchangeable (readonly.go:14-73). Collapsing them all to "fall back
	// to the default session" would let a corrupt database silently open the
	// wrong workspace, so the rule is stated as two named fallbacks and
	// "propagate everything else":
	//
	//   Fall back, silently:
	//     - state.IsMissingDatabase(err) — a fresh installation, in which
	//       nothing is registered and so nothing can be bound.
	//     - *state.PendingMigrationError from insp.Usable() — a diagnosis,
	//       and resolution is not the command that should act on it.
	//
	//   Propagate:
	//     - any other OpenReadOnly error, and
	//     - any other insp.Usable() error — an integrity failure and
	//       *state.FutureSchemaError land here.
	//
	// A permission failure and *state.IncompleteWALError have no dedicated
	// predicate to test for, which is exactly why the rule is written as
	// "propagate everything that is not the two named fallbacks" rather than
	// as a list of propagating types: a refusal added to readonly.go later
	// propagates by default instead of silently joining the fallbacks.
	ro, insp, err := state.OpenReadOnly(stateRoot)
	if err != nil {
		if state.IsMissingDatabase(err) {
			return "", nil
		}
		return "", err
	}
	defer func() { _ = ro.Close() }()
	if err := insp.Usable(); err != nil {
		var pending *state.PendingMigrationError
		if errors.As(err, &pending) {
			return "", nil
		}
		return "", err
	}

	records, err := ro.Workspaces()
	if err != nil {
		return "", err
	}

	// Canonicalize the cwd the same way a bind is canonicalized, by taking it
	// through the repository-relative form and back. A cwd outside the
	// repository root — a linked worktree, which lives outside the main tree —
	// cannot be contained by any repository-relative bind, so it answers with
	// the default session rather than an error.
	relCwd, err := bindpath.Rel(ws.RepoRoot, cwd)
	if err != nil {
		return "", nil
	}
	canonicalCwd, err := bindpath.Resolve(ws.RepoRoot, relCwd)
	if err != nil {
		return "", nil
	}

	best := -1
	var matched []string
	for _, rec := range records {
		if rec.RepositoryID != ws.RepositoryID || rec.Bind == nil {
			continue
		}
		// A bind that no longer canonicalizes inside the repository is
		// treated as missing rather than followed (design §4). It is not a
		// hard failure here: the session simply does not claim this cwd.
		resolved, err := bindpath.Resolve(ws.RepoRoot, *rec.Bind)
		if err != nil {
			continue
		}
		if !bindpath.Contains(resolved, canonicalCwd) {
			continue
		}
		switch d := depth(resolved); {
		case d > best:
			best, matched = d, []string{rec.Session}
		case d == best:
			matched = append(matched, rec.Session)
		}
	}

	switch len(matched) {
	case 0:
		return "", nil
	case 1:
		return matched[0], nil
	}
	// Equal depth and both containing the cwd means the same directory, so
	// this is two sessions bound to one place. resolve.AmbiguousError is the
	// exit-3 shape the CLI already maps; its message is worded for repository
	// names, so the candidates are given as targets the user can pass, which
	// is the actionable part.
	candidates := make([]string, 0, len(matched))
	for _, session := range matched {
		candidates = append(candidates, targetString(ws.Slug, session))
	}
	slices.Sort(candidates)
	return "", &resolve.AmbiguousError{Name: ws.Slug, Candidates: candidates}
}

// depth counts the path components of an absolute path. Longest match is
// measured in components, not string length: /r/services/apiary is longer as a
// string than /r/services/api/v1 but shallower as a path.
func depth(path string) int {
	n := 0
	for _, part := range strings.Split(filepath.ToSlash(filepath.Clean(path)), "/") {
		if part != "" && part != "." {
			n++
		}
	}
	return n
}

// targetString renders the argument form that addresses a session.
func targetString(slug, session string) string {
	if session == "" {
		return slug
	}
	return fmt.Sprintf("%s/%s", slug, session)
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/target/ -run TestSelectKeysOnTargetPresence -v`

Expected: PASS.

Then `go build ./...` — expected: no output.

- [ ] **Step 5: Write the failing lookup tests**

Append to `internal/target/select_test.go`, adding `"errors"` to its import
block — the earlier tests did not need it:

```go
// TestSelectExplicitSessionIgnoresTheCwd is case 1: an explicit
// <repo>/<session> is final, whatever the cwd is bound to.
func TestSelectExplicitSessionIgnoresTheCwd(t *testing.T) {
	f := newFixture(t)
	bound := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")
	f.bindSession(t, "web", "services/web")

	ws := mustSelect(t, Ref{Present: true, Name: "slabledger", Session: "web", HasSession: true}, f, bound)
	if ws.Session != "web" {
		t.Errorf("Session = %q, want web", ws.Session)
	}
}

// TestSelectTakesTheLongestBind covers nested binds. services/api is deeper
// than services, so a cwd below both belongs to the api session.
func TestSelectTakesTheLongestBind(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api", "cmd"))
	f.bindSession(t, "svc", "services")
	f.bindSession(t, "api", "services/api")

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "api" {
		t.Errorf("Session = %q, want api: the deeper bind wins", ws.Session)
	}
}

// TestSelectAmbiguousBindsAreExit3 covers two sessions bound to one directory.
func TestSelectAmbiguousBindsAreExit3(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")
	f.bindSession(t, "other", "services/api")

	_, err := Select(Ref{}, f.roots, cwd, f.stateRoot)
	var ambiguous *resolve.AmbiguousError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("Select = %v (%T), want *resolve.AmbiguousError", err, err)
	}
	want := []string{"slabledger/api", "slabledger/other"}
	if len(ambiguous.Candidates) != len(want) {
		t.Fatalf("Candidates = %v, want %v", ambiguous.Candidates, want)
	}
	for i, c := range want {
		if ambiguous.Candidates[i] != c {
			t.Errorf("Candidates = %v, want %v", ambiguous.Candidates, want)
			break
		}
	}
}

// TestSelectDoesNotMatchASiblingNamePrefix is the component-wise comparison a
// string prefix would fail: services/apixyz is not inside services/api.
func TestSelectDoesNotMatchASiblingNamePrefix(t *testing.T) {
	f := newFixture(t)
	mkdir(t, filepath.Join(f.repo, "services", "api"))
	cwd := mkdir(t, filepath.Join(f.repo, "services", "apixyz"))
	f.bindSession(t, "api", "services/api")

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session: services/apixyz is "+
			"a sibling of the bind, not inside it", ws.Session)
	}
}

// TestSelectUnbindableBindIsTreatedAsMissing covers design §4's fallback: a
// stored path replaced by a symlink out of the repository does not claim the
// cwd, and does not fail the command either.
func TestSelectUnbindableBindIsTreatedAsMissing(t *testing.T) {
	f := newFixture(t)
	outside := mkdir(t, filepath.Join(base(t), "outside"))
	if err := os.Symlink(outside, filepath.Join(f.repo, "escaped")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	f.bindSession(t, "api", "escaped")

	ws := mustSelect(t, Ref{}, f, f.repo)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session: a bind that escapes "+
			"the repository is treated as missing", ws.Session)
	}
}

func TestSelectNoBindsFallsBackToTheDefaultSession(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "")

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session", ws.Session)
	}
}
```

- [ ] **Step 6: Run the lookup tests**

Run: `go test ./internal/target/ -run 'TestSelect' -v`

Expected: PASS. These exercise branches Step 3 already wrote, so a failure here
is a bug in that code rather than a missing feature — read the failure before
changing anything.

- [ ] **Step 7: Write the failing read-only-store outcome tests**

Append to `internal/target/select_test.go`, adding `"database/sql"`, `"fmt"`,
and the blank driver import `_ "modernc.org/sqlite"` to its import block:

```go
// TestSelectMissingDatabaseFallsBack is the fresh-installation path: nothing
// is registered, so nothing can be bound.
func TestSelectMissingDatabaseFallsBack(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	if _, err := os.Stat(state.DBPath(f.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the fixture already has a database; this test proves nothing")
	}

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session", ws.Session)
	}
	if _, err := os.Stat(state.DBPath(f.stateRoot)); !errors.Is(err, os.ErrNotExist) {
		t.Error("the lookup created the state database")
	}
}

// TestSelectPendingMigrationFallsBack covers the other silent fallback. The
// schema version is rolled back through a raw connection because migrating
// forwards is the only thing state.Open will do.
func TestSelectPendingMigrationFallsBack(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")
	setUserVersion(t, state.DBPath(f.stateRoot), state.SchemaVersion-1)

	ws := mustSelect(t, Ref{}, f, cwd)
	if ws.Session != "" {
		t.Errorf("Session = %q, want the default session: a schema this build "+
			"does not read must not be interpreted", ws.Session)
	}
}

// TestSelectIntegrityFailurePropagates is the case that must not fall back.
// Falling back would silently open the wrong workspace on a corrupt database.
// The corruption is staged the way internal/state/readonly_test.go stages it
// (TestOpenReadOnlyCorruptDatabaseReportsIntegrity): keep the SQLite header so
// the file is still recognizably a database, and scribble over the pages
// behind it.
func TestSelectIntegrityFailurePropagates(t *testing.T) {
	f := newFixture(t)
	cwd := mkdir(t, filepath.Join(f.repo, "services", "api"))
	f.bindSession(t, "api", "services/api")

	path := state.DBPath(f.stateRoot)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the database: %v", err)
	}
	for i := 100; i < len(raw); i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the corrupt database: %v", err)
	}

	ws, err := Select(Ref{}, f.roots, cwd, f.stateRoot)
	if err == nil {
		t.Fatalf("a corrupt state database selected %q instead of failing", ws.Session)
	}
}

// setUserVersion rewrites PRAGMA user_version through a raw connection.
// state.Open would migrate the database forward again, so the version is set
// on a connection that does not go through it.
func setUserVersion(t *testing.T, path string, version int) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("opening the database directly: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		t.Fatalf("setting user_version: %v", err)
	}
}
```


Two coverage limits worth stating rather than papering over:

- The permission-failure and indeterminate-WAL cases are **not** covered here.
  `seedUnrecoveredWAL` and `walStateOf` are unexported helpers in package
  `state`, so staging an unrecovered log from `target` cannot be verified as
  having been staged, and a test that silently degraded to a healthy database
  would prove nothing. Both cases are already pinned inside package `state`
  (`readonly_test.go`: `TestOpenReadOnlyUnrecoveredWALIsTyped`,
  `TestOpenReadOnlyUnreadableDatabaseIsNotCorruption`), and here they reach
  `Select` through the same "propagate everything that is not the two named
  fallbacks" branch the integrity test exercises. What is verified at this
  level is that the branch propagates; what is verified in `state` is that
  those two errors take it.
- `TestSelectPendingMigrationFallsBack` uses `state.SchemaVersion-1`, so it
  keeps testing a pending migration after the next migration lands rather than
  pinning the literal 2.

- [ ] **Step 8: Run the read-only-store outcome tests**

Run: `go test ./internal/target/ -run 'TestSelect(MissingDatabase|PendingMigration|IntegrityFailure)' -v`

Expected: PASS. If `TestSelectIntegrityFailurePropagates` reports a nil error,
the `insp.Usable()` branch is falling back too eagerly — the integrity error is
not a `*state.PendingMigrationError` and must reach the caller.

- [ ] **Step 9: Run the full package and build**

Run: `go test ./internal/target/ -v`

Expected: PASS, including the grammar tests from the earlier `Parse` task.

Run: `go test ./... && go build ./...`

Expected: PASS, no build output.

- [ ] **Step 10: Commit**

```bash
git add internal/target/select.go internal/target/select_test.go
git commit -m "feat(target): choose a session from the target, then the bind

Selection keys on target presence, not on whether the target carried a
session: <repo> addresses the default session even from inside a bound
directory, and only a bare invocation lets the cwd choose. The bind
lookup reads the store read-only, matches path components rather than
string prefixes, takes the longest match, and reports a tie as
resolve.AmbiguousError. A missing database and a pending migration fall
back to the default session; every other read-only failure propagates,
because guessing which session is correct is the one thing resolution
must not do."
```
### Task 7: the bind as the session's base directory

Spec §4 makes the bind the session's **base directory**: every window's and
pane's `dir:` composes on top of it rather than competing with it. This task is
where a stored bind first changes what a session actually opens, so it carries
three pieces that only make sense together — persisting the bind inside
`Ensure`'s one critical section, computing the base once, and threading it
through every site that turns a `RelDir` into a path.

Five sites in `renderWindows` join or pass a `RelDir`, and all five take the
prefix. Verified against the file rather than the spec's citation, which is off
by a line at two of them:

| site | line | today |
| --- | --- | --- |
| container pane command | `ensure.go:264` | `act.ExecCommand(binding, p.Command, relDir, …)` |
| container pane host `-c` | `ensure.go:265` | `Dir: d.Workspace.RepoRoot` |
| container window command | `ensure.go:271` | `act.ExecCommand(binding, in.Command, in.RelDir, …)` |
| container window host `-c` | `ensure.go:272` | `Dir: d.Workspace.RepoRoot` |
| host window dir | `ensure.go:278-281` | `filepath.Join(d.Workspace.RepoRoot, in.RelDir)` |
| host pane dir | `ensure.go:283-287` | `filepath.Join(d.Workspace.RepoRoot, p.RelDir)` |

(Six rows, five `RelDir`-bearing sites: the two host `-c` values are the same
constant and become the same `base.Host`.)

Two facts the code settles, both confirmed by reading it:

- A pane's `RelDir` **replaces** the window's rather than nesting under it —
  `ensure.go:258-261` for the container branch and `ensure.go:283-287` for the
  host branch both join the pane's own `RelDir` onto the *root*, not onto the
  window's directory. So the base must prefix whichever `RelDir` wins, not only
  the window's. (The spec cites 258-260 and 284-286; the real ranges are
  258-261 and 283-287.)
- `container/exec.go:31` does `path.Join(binding.Workdir, relDir)`, so the
  container adapter needs **no change**: the base is folded into `relDir`
  before it reaches the adapter, and `/workspaces/<repo>` + `services/api/cmd`
  comes out right for free.

`internal/tmux` also needs no change. `windowDir` (`actuate.go:145-150`) falls
back to `spec.Worktree` when `w.Dir == ""`, and that host-side default is what
the base must displace — but `renderWindows` already emits a non-empty absolute
`Dir` for every window, and `base.Host` is never empty (it is the repository
root when there is no bind), so the fallback stays unreachable. `SessionSpec.
Worktree` must **not** become the bound directory: `createSession` writes it to
`@dev_worktree` (`ensure.go:392`, `types.go:14`), which is an identity key
compared by `SessionBelongsTo`. A test below pins that separation.

**Files:**
- Modify: `internal/controller/ensure.go:24-30` (`EnsureResult`), `ensure.go:78-89`
  (the critical section), `ensure.go:226-297` (`renderWindows`), and a new
  `bindBase` + `resolveBindBase` appended after `renderWindows`
- Modify: `internal/controller/observe.go:30-34` (`Desired`)
- Modify: `internal/controller/interfaces.go:30-41` (`Store`)
- Modify: `internal/controller/fake/fake.go:308-339` (`copyRecordLocked`) and a
  new `SetBind` appended after `AdoptSessionName` (`fake.go:453`)
- Test: `internal/controller/render_test.go`, `internal/controller/ensure_test.go`,
  `internal/controller/fake/fake_test.go`

**Interfaces:**
- Consumes:
  ```go
  func bindpath.Resolve(repoRoot, rel string) (string, error)   // Task 4
  type bindpath.EscapedError struct{ Rel, Resolved, RepoRoot string } // Task 4
  // state.Record.Bind *string                                   // Task 5
  ```
- Produces:
  ```go
  // controller.Desired gains:
  Bind *string
  // controller.EnsureResult gains:
  BindWarning string
  // controller.Store gains:
  SetBind(workspaceID string, bind *string, now time.Time) error
  // unexported, within package controller:
  type bindBase struct{ Host, Rel string }
  func (b bindBase) hostDir(relDir string) string
  func (b bindBase) containerDir(relDir string) string
  func resolveBindBase(repoRoot string, stored *state.Record) (bindBase, string)
  func renderWindows(intents []WindowIntent, d Desired, base bindBase, container *ContainerObservation, act ContainerActuator) ([]WindowSpec, error)
  // and on the fake:
  func (s *fake.Store) SetBind(workspaceID string, bind *string, now time.Time) error
  ```

---

- [ ] **Step 1: Write the failing test for the fake store's `SetBind`**

`controller.Store` is about to gain `SetBind`, and `fake.go:24` asserts
`var _ controller.Store = (*Store)(nil)` — so the fake must implement it or the
package stops compiling. Test the fake's own semantics first: set, read back
through the copy, clear, and reject an unknown workspace the way
`AdoptSessionName` does (`fake.go:441-444`).

Append to `internal/controller/fake/fake_test.go`:

```go
func TestFakeStoreSetBindRoundTrips(t *testing.T) {
	s := NewStore()
	if err := s.RegisterWorkspace(testWorkspace("w1", "slab"), "sha256:a", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}

	rec, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("a fresh record has Bind = %v, want nil", rec.Bind)
	}

	bind := "services/api"
	if err := s.SetBind("w1", &bind, testTime); err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	rec, err = s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != "services/api" {
		t.Fatalf("Bind = %v, want services/api", rec.Bind)
	}
	// The record read back is a copy: mutating it must not reach the store.
	*rec.Bind = "mutated"
	again, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if *again.Bind != "services/api" {
		t.Errorf("Bind = %q; the returned record aliased the stored one", *again.Bind)
	}

	if err := s.SetBind("w1", nil, testTime); err != nil {
		t.Fatalf("SetBind(nil): %v", err)
	}
	cleared, err := s.Workspace("w1")
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if cleared.Bind != nil {
		t.Errorf("Bind = %v after clearing, want nil", cleared.Bind)
	}
}

func TestFakeStoreSetBindRejectsAnUnknownWorkspace(t *testing.T) {
	s := NewStore()
	bind := "services/api"
	if err := s.SetBind("nope", &bind, testTime); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("error = %v, want state.ErrNotFound", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/controller/fake/ -run SetBind -v`

Expected: FAIL to build —
`s.SetBind undefined (type *Store has no field or method SetBind)`.

- [ ] **Step 3: Add `SetBind` to the interface and the fake**

In `internal/controller/interfaces.go`, add the method to `Store` after
`AdoptSessionName` (line 33):

```go
	AdoptSessionName(workspaceID, name string, now time.Time) error
	// SetBind records the session's bind — a repository-relative
	// directory, or nil to clear it. It is separate from
	// RegisterWorkspace because registration is fill-only with respect
	// to the bind: rebuild re-registers a workspace and must not drop
	// one (spec §4).
	SetBind(workspaceID string, bind *string, now time.Time) error
```

In `internal/controller/fake/fake.go`, append after `AdoptSessionName`:

```go
// SetBind mirrors the real store: an unknown workspace is ErrNotFound, and
// the stored pointer is a copy, so a caller keeping its own pointer cannot
// mutate the record afterwards.
func (s *Store) SetBind(workspaceID string, bind *string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[workspaceID]
	if !ok {
		return fmt.Errorf("workspace %s: %w", workspaceID, state.ErrNotFound)
	}
	rec.Bind = nil
	if bind != nil {
		v := *bind
		rec.Bind = &v
	}
	rec.UpdatedAt = now
	return nil
}
```

and add the bind to `copyRecordLocked` (`fake.go:311`), beside the other
pointer fields:

```go
	if rec.Bind != nil {
		v := *rec.Bind
		out.Bind = &v
	}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/controller/fake/ -run SetBind -v`

Expected: PASS — `TestFakeStoreSetBindRoundTrips` and
`TestFakeStoreSetBindRejectsAnUnknownWorkspace` both ok.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/interfaces.go internal/controller/fake/fake.go internal/controller/fake/fake_test.go
git commit -m "feat(controller): add Store.SetBind and implement it on the fake"
```

- [ ] **Step 6: Write the failing `renderWindows` base-directory tests**

These are the spec §7 line "ensure-level base-directory joining, verified on
both the host and the container path, and including a pane that sets its own
`dir:`". `render_test.go` is in the internal `package controller`, so `bindBase`
can be constructed directly and no store is involved.

Append to `internal/controller/render_test.go`:

```go
// TestRenderWindowsHostTakesTheBase pins the two host sites: the window dir
// and the pane dir. The pane sets its own RelDir, which replaces the
// window's rather than nesting under it — so it must take the base too, or
// a bound session's pane escapes to the repository root.
func TestRenderWindowsHostTakesTheBase(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"}}
	base := bindBase{Host: "/w/slab/services/api", Rel: "services/api"}
	intents := []WindowIntent{{
		Name:   "dev",
		RelDir: "cmd",
		Panes: []PaneIntent{
			{Name: "shell"},
			{Name: "logs", RelDir: "internal"},
		},
	}}
	specs, err := renderWindows(intents, d, base, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Dir != "/w/slab/services/api/cmd" {
		t.Errorf("window dir = %q, want the window cwd under the base", specs[0].Dir)
	}
	if got := specs[0].Panes[0].Dir; got != "/w/slab/services/api/cmd" {
		t.Errorf("inheriting pane dir = %q, want the window's directory", got)
	}
	if got := specs[0].Panes[1].Dir; got != "/w/slab/services/api/internal" {
		t.Errorf("pane dir = %q, want the pane cwd under the base", got)
	}
}

// TestRenderWindowsHostWithoutABindIsUnchanged pins the no-bind case as a
// no-op: base.Host is the repository root and base.Rel is empty.
func TestRenderWindowsHostWithoutABindIsUnchanged(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"}}
	intents := []WindowIntent{{Name: "dev", Panes: []PaneIntent{{Name: "shell"}}}}
	specs, err := renderWindows(intents, d, bindBase{Host: "/w/slab"}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Dir != "/w/slab" || specs[0].Panes[0].Dir != "/w/slab" {
		t.Errorf("unbound rendering changed: %+v", specs[0])
	}
}

// TestRenderWindowsContainerTakesTheBase pins the three container sites: the
// window's exec relDir, the pane's exec relDir, and the host-side -c both
// carry. The relDir is prefixed on this side precisely so
// container/exec.go's path.Join(Workdir, relDir) needs no change.
func TestRenderWindowsContainerTakesTheBase(t *testing.T) {
	d := Desired{Workspace: resolve.Workspace{RepoRoot: "/w/slab"},
		Config: config.Config{Environment: map[string]string{"K": "v"}}}
	base := bindBase{Host: "/w/slab/services/api", Rel: "services/api"}
	obs := &ContainerObservation{Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab"}
	act := renderPaneActuator{}
	binding := state.ContainerBinding{Kind: "devcontainer", ContainerID: "abc",
		ContainerUser: "dev", Workdir: "/workspaces/slab"}
	intents := []WindowIntent{{
		Name:     "dev",
		Command:  "claude",
		RelDir:   "cmd",
		Location: WindowContainer,
		Panes:    []PaneIntent{{Name: "logs", RelDir: "internal"}},
	}}
	specs, err := renderWindows(intents, d, base, obs, act)
	if err != nil {
		t.Fatal(err)
	}
	wantWindow := act.ExecCommand(binding, "claude", "services/api/cmd", d.Config.Environment)
	if specs[0].Command != wantWindow {
		t.Errorf("window command = %q, want %q", specs[0].Command, wantWindow)
	}
	wantPane := act.ExecCommand(binding, "", "services/api/internal", d.Config.Environment)
	if specs[0].Panes[0].Command != wantPane {
		t.Errorf("pane command = %q, want %q", specs[0].Panes[0].Command, wantPane)
	}
	if specs[0].Dir != "/w/slab/services/api" || specs[0].Panes[0].Dir != "/w/slab/services/api" {
		t.Errorf("container host -c = %q/%q, want the base on both",
			specs[0].Dir, specs[0].Panes[0].Dir)
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run RenderWindows -v`

Expected: FAIL to build — `undefined: bindBase` and
`too many arguments in call to renderWindows`.

- [ ] **Step 8: Add `bindBase` and thread it through `renderWindows`**

In `internal/controller/ensure.go`, add `"path"` to the imports, then append
after `renderWindows`:

```go
// bindBase is the session's base directory, computed once per Ensure and
// threaded to every site that turns a RelDir into a path — rather than
// prefixing at five call sites, where a sixth would eventually forget.
// Host is the absolute directory host-side paths join under; Rel is the
// repository-relative form folded into the container adapter's relDir,
// since container/exec.go joins that onto the binding's workdir. With no
// bind, Host is the repository root and Rel is "", so every join is a
// no-op.
type bindBase struct {
	Host string
	Rel  string
}

func (b bindBase) hostDir(relDir string) string {
	if relDir == "" {
		return b.Host
	}
	return filepath.Join(b.Host, relDir)
}

// containerDir composes with POSIX semantics: both halves are
// slash-separated container-side paths, never host paths.
func (b bindBase) containerDir(relDir string) string {
	return path.Join(b.Rel, relDir)
}
```

Change the signature and the five sites in `renderWindows`:

```go
func renderWindows(intents []WindowIntent, d Desired, base bindBase, container *ContainerObservation, act ContainerActuator) ([]WindowSpec, error) {
```

```go
				panes = append(panes, PaneSpec{
					Name:    p.Name,
					Command: act.ExecCommand(binding, p.Command, base.containerDir(relDir), d.Config.Environment),
					Dir:     base.Host,
					Focus:   p.Focus,
				})
			}
			specs = append(specs, WindowSpec{
				Name:    in.Name,
				Command: act.ExecCommand(binding, in.Command, base.containerDir(in.RelDir), d.Config.Environment),
				Dir:     base.Host,
				Focus:   in.Focus,
				Panes:   panes,
			})
			continue
		}
		dir := base.hostDir(in.RelDir)
		panes := make([]PaneSpec, 0, len(in.Panes))
		for _, p := range in.Panes {
			paneDir := dir
			if p.RelDir != "" {
				paneDir = base.hostDir(p.RelDir)
			}
```

Update the comment at `ensure.go:254-257` so it no longer says the host-side
`-c` is the repository root:

```go
				// A pane inherits the window's directory unless it sets
				// its own; inside the container that is the exec relDir,
				// while the host-side -c stays the session's base
				// directory, matching the window itself.
```

Finally update the three existing `renderWindows` calls in `render_test.go`
(`TestRenderWindowsHostPanes`, `TestRenderWindowsPaneInheritsWindowDir`,
`TestRenderWindowsContainerPanes`) to pass `bindBase{Host: "/w/slab"}` as the
third argument, and the one call in `Ensure` (`ensure.go:111`) — for now with
the unbound base, which the next step replaces:

```go
	windows, err := renderWindows(intents, d, bindBase{Host: d.Workspace.RepoRoot}, containerObs, c.ContainerAct)
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run RenderWindows -v`

Expected: PASS — the three new tests plus the three pre-existing
`TestRenderWindows*` tests, all ok.

- [ ] **Step 10: Commit**

```bash
git add internal/controller/ensure.go internal/controller/render_test.go
git commit -m "feat(controller): thread a bind base through window rendering"
```

- [ ] **Step 11: Write the failing test for persisting the bind inside Ensure**

Spec §4: `open --cwd <path>` does not bind and then open — the bind is a field
on `Desired`, persisted in the same critical section that registers the
workspace, before the observation the windows are planned from. This test pins
both halves in one call: the record carries the bind afterwards, **and** the
window the actuator received was already built from it. It also pins that
`SessionSpec.Worktree` stays the repository root, because that value becomes
`@dev_worktree` and is an identity key.

Append to `internal/controller/ensure_test.go` (add `"os"` and `"path/filepath"`
to the imports):

```go
// boundRepo returns a canonical repository root containing services/api.
// EvalSymlinks matches internal/bindpath's canonicalization, so expected
// paths compare equal on macOS, where /var is a symlink to /private/var.
func boundRepo(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "services", "api"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	return root
}

func boundDesired(root string, bind *string) controller.Desired {
	d := ensureDesired()
	ws := ensureWorkspace()
	ws.RepoRoot = root
	d.Workspace = ws
	d.Bind = bind
	return d
}

func TestEnsurePersistsTheBindAndPlansWindowsFromIt(t *testing.T) {
	root := boundRepo(t)
	live := controller.LiveSession{
		Name: "slab", WorkspaceID: "w1", Slug: "slab", Worktree: root,
	}
	r := newEnsureRig(t,
		absentStep(),   // initial observation
		absentStep(),   // allocated-name squat check
		liveStep(live), // post-create confirmation
	)
	bind := "services/api"
	res, err := r.ensure(t, boundDesired(root, &bind))
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.BindWarning != "" {
		t.Errorf("BindWarning = %q, want none for a usable bind", res.BindWarning)
	}

	rec, err := r.store.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != "services/api" {
		t.Fatalf("stored Bind = %v, want services/api", rec.Bind)
	}

	if len(r.actuator.Created) != 1 {
		t.Fatalf("actuator calls = %d, want 1", len(r.actuator.Created))
	}
	spec := r.actuator.Created[0]
	want := filepath.Join(root, "services", "api")
	if spec.Windows[0].Dir != want {
		t.Errorf("window dir = %q, want %q; the bind was not visible to planning",
			spec.Windows[0].Dir, want)
	}
	// Worktree is written to @dev_worktree and compared by
	// SessionBelongsTo: it is identity, not a working directory, and the
	// bind must not move it.
	if spec.Worktree != root {
		t.Errorf("SessionSpec.Worktree = %q, want the repository root %q", spec.Worktree, root)
	}
}
```

- [ ] **Step 12: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestEnsurePersistsTheBind -v`

Expected: FAIL to build —
`unknown field Bind in struct literal of type controller.Desired` (from
`boundDesired`) and `res.BindWarning undefined`.

- [ ] **Step 13: Persist the bind and compute the base from the stored record**

In `internal/controller/observe.go`, add the field to `Desired`:

```go
type Desired struct {
	Workspace resolve.Workspace
	Config    config.Config
	Digest    string
	// Bind is the session's base directory, repository-relative. A nil
	// pointer leaves whatever is stored alone — an open that carries no
	// --cwd must not clear an existing bind. Clearing goes through a
	// nil-valued SetBind from the CLI instead.
	Bind *string
}
```

In `internal/controller/ensure.go`, add the `BindWarning` field to
`EnsureResult`:

```go
type EnsureResult struct {
	Action                EnsureAction
	Session               string
	Drifted               bool
	Container             *ContainerObservation // nil when no container is in play
	ContainerWindowsStale bool
	// BindWarning is set when the stored bind was unusable and the
	// repository root was used instead. An unusable bind is not fatal
	// (spec §4), so this is reported rather than returned as an error.
	BindWarning string
}
```

Add the write between `RegisterWorkspace` and `Observe`, so the bind is
committed before the observation the windows are planned from:

```go
	if err := c.Store.RegisterWorkspace(d.Workspace, d.Digest, c.Clock.Now()); err != nil {
		return EnsureResult{}, fmt.Errorf("registering the workspace: %w", err)
	}

	// The bind is persisted inside the same critical section that
	// registers the workspace and before the observation below, so no
	// window is ever built from a bind that changed underneath it. It
	// persists even when the open fails afterwards: it is a declaration
	// about the session, not a side effect of a successful open (§4).
	if d.Bind != nil {
		if err := c.Store.SetBind(d.Workspace.ID, d.Bind, c.Clock.Now()); err != nil {
			c.recordFailure(d.Workspace.ID, opName, "recording the bind: "+err.Error())
			return EnsureResult{}, fmt.Errorf("recording the bind: %w", err)
		}
	}
```

Add `resolveBindBase` beside `bindBase`, and import
`"github.com/gambtho/projectmux/internal/bindpath"`:

```go
// resolveBindBase turns the stored bind into the session's base
// directory. Containment is re-verified here rather than trusted from
// bind time: a stored in-repository path can later be replaced by a
// symlink pointing outside the repository (spec §4).
func resolveBindBase(repoRoot string, stored *state.Record) bindBase {
	root := bindBase{Host: repoRoot}
	if stored == nil || stored.Bind == nil || *stored.Bind == "" {
		return root
	}
	abs, err := bindpath.Resolve(repoRoot, *stored.Bind)
	if err != nil {
		return root
	}
	return bindBase{Host: abs, Rel: *stored.Bind}
}
```

and use it for the render, replacing the placeholder from Step 8
(`ensure.go:111`):

```go
	base := resolveBindBase(d.Workspace.RepoRoot, snap.Stored)
	windows, err := renderWindows(intents, d, base, containerObs, c.ContainerAct)
```

- [ ] **Step 14: Run the test to verify it passes**

Run: `go test ./internal/controller/ -run TestEnsure -v`

Expected: PASS — `TestEnsurePersistsTheBindAndPlansWindowsFromIt` ok, and every
pre-existing `TestEnsure*` still ok (`Desired.Bind` is nil for all of them, so
the base is the repository root and nothing they assert moves).

- [ ] **Step 15: Commit**

```bash
git add internal/controller/ensure.go internal/controller/observe.go internal/controller/ensure_test.go
git commit -m "feat(controller): persist the bind in Ensure and open under it"
```

- [ ] **Step 16: Write the failing test for an unusable bind**

Spec §4: if a bind is unusable at open time — the directory was deleted, a
linked worktree was pruned, or it no longer canonicalizes inside the
repository — `open` falls back to the repository root **and says so** rather
than failing. Two cases, because they fail in `bindpath.Resolve` for different
reasons and only the second is the symlink re-check.

Append to `internal/controller/ensure_test.go`:

```go
func TestEnsureFallsBackWhenTheBindIsUnusable(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, root string) string
	}{
		{
			name: "the directory is gone",
			setup: func(t *testing.T, root string) string {
				return "services/gone"
			},
		},
		{
			name: "the bind now resolves outside the repository",
			setup: func(t *testing.T, root string) string {
				outside := t.TempDir()
				if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return "escape"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := boundRepo(t)
			bind := tc.setup(t, root)
			live := controller.LiveSession{
				Name: "slab", WorkspaceID: "w1", Slug: "slab", Worktree: root,
			}
			r := newEnsureRig(t, absentStep(), absentStep(), liveStep(live))

			res, err := r.ensure(t, boundDesired(root, &bind))
			if err != nil {
				t.Fatalf("Ensure: %v; an unusable bind must not be fatal", err)
			}
			if res.BindWarning == "" {
				t.Error("BindWarning is empty; the fallback was silent")
			}
			if !strings.Contains(res.BindWarning, bind) {
				t.Errorf("BindWarning = %q, want it to name the bind %q", res.BindWarning, bind)
			}
			spec := r.actuator.Created[0]
			if spec.Windows[0].Dir != root {
				t.Errorf("window dir = %q, want the repository root %q", spec.Windows[0].Dir, root)
			}
			// The bind is not discarded: fixing the directory and
			// reopening must work without re-binding.
			rec, err := r.store.Workspace("w1")
			if err != nil {
				t.Fatalf("Workspace: %v", err)
			}
			if rec.Bind == nil || *rec.Bind != bind {
				t.Errorf("stored Bind = %v, want it kept as %q", rec.Bind, bind)
			}
		})
	}
}
```

- [ ] **Step 17: Run the test to verify it fails**

Run: `go test ./internal/controller/ -run TestEnsureFallsBack -v`

Expected: FAIL — `BindWarning is empty; the fallback was silent`, for both
subtests. The fallback itself already works from Step 13; nothing reports it.

- [ ] **Step 18: Report the fallback**

Change `resolveBindBase` in `internal/controller/ensure.go` to return the
reason alongside the base:

```go
// resolveBindBase turns the stored bind into the session's base
// directory, and returns a non-empty warning when the bind was unusable
// and the repository root was substituted. Containment is re-verified
// here rather than trusted from bind time: a stored in-repository path
// can later be replaced by a symlink pointing outside the repository
// (spec §4). An unusable bind is reported, never followed and never
// fatal.
func resolveBindBase(repoRoot string, stored *state.Record) (bindBase, string) {
	root := bindBase{Host: repoRoot}
	if stored == nil || stored.Bind == nil || *stored.Bind == "" {
		return root, ""
	}
	abs, err := bindpath.Resolve(repoRoot, *stored.Bind)
	if err != nil {
		return root, fmt.Sprintf(
			"the bind %q is unusable, so this session opened at the repository root instead: %v",
			*stored.Bind, err)
	}
	return bindBase{Host: abs, Rel: *stored.Bind}, ""
}
```

Capture it at the call site and carry it out through all three result paths.
Replace the render line:

```go
	base, bindWarning := resolveBindBase(d.Workspace.RepoRoot, snap.Stored)
	windows, err := renderWindows(intents, d, base, containerObs, c.ContainerAct)
```

Then add `BindWarning: bindWarning,` to the `EnsureResult` literals returned by
the `SessionActionNone` and `SessionActionAdopt` cases, and set it on the
create path's result beside the container:

```go
	case SessionActionCreate:
		res, err := c.createSession(ctx, d, windows, containerObs)
		if err != nil {
			return EnsureResult{}, err
		}
		res.Container = containerObs
		res.BindWarning = bindWarning
		return res, nil
```

- [ ] **Step 19: Run the test to verify it passes**

Run: `go test ./internal/controller/ -run TestEnsure -v`

Expected: PASS — both `TestEnsureFallsBackWhenTheBindIsUnusable` subtests ok,
and every pre-existing `TestEnsure*` still ok.

- [ ] **Step 20: Run the full verification**

Run: `gofmt -l . && go test -race ./...`

Expected: `gofmt -l` prints nothing, and every package passes — including
`internal/tmux` and `internal/container`, which this task deliberately does not
touch: the container adapter's `path.Join(binding.Workdir, relDir)` receives
the already-prefixed relDir, and `windowDir`'s `spec.Worktree` fallback stays
unreachable because `renderWindows` always emits an absolute `Dir`.

- [ ] **Step 21: Commit**

```bash
git add internal/controller/ensure.go internal/controller/ensure_test.go
git commit -m "feat(controller): report an unusable bind instead of failing the open"
```
### Task 8: every command routes through the target seam

Five commands take a workspace argument today and each one hand-rolls the same
three lines: `os.Getwd`, `resolve.Resolve(arg, "", roots, cwd)`, done. That
shape cannot express `<repo>/<session>` and has no place to run the bind
lookup, so it is replaced by one helper — `selectWorkspace` — that every
command calls: `target.Parse` for the grammar, `target.Select` for the choice.

The commands that take a workspace argument, confirmed by reading each one:
`open` (`open.go:65-75`), `attach` (`attach.go:45-52`), `stop`
(`stop.go:65-86`), `status` (`status.go:110-117`), and `config`
(`config.go:83-99`). `autostart` takes **no** workspace argument — it rejects
any argument at `autostart.go:59-61` and iterates every registered repository —
so it is out of this task's scope; its `resolve.Resolve` call at
`autostart.go:142` is a repository-root classification, not a target, and
Task 1 already gave it its `""` session argument. `list`, `doctor`, and
`rebuild` take no positional argument either.

One deliberate exclusion: `config --validate <name>`. Its argument names a
*workspace configuration file*, not a target — `config.go:36-40` documents
exactly that, so that a workspace whose worktree has moved can still be
checked — and it never reaches resolution (`config.go:91-97` returns before
`buildEnvelope`). It keeps taking a raw name.

`open --cwd <path>` is the other half. It does **not** bind and then open. The
bind is a field on `Desired`, so `Ensure` persists it in the same critical
section that registers the workspace, before the observation the windows are
planned from — one lock acquisition, no window ever built from a bind that
changed underneath it, and no two-command race. That is a structural property
of passing `Desired.Bind` rather than something a CLI test can observe
directly; what the tests do pin is the consequence spec §7 names, that a bind
persists when the open carrying it fails afterwards.

**Files:**
- Modify: `internal/cli/wiring.go` (new `selectWorkspace`, appended after the
  observation seams at `36-50`)
- Modify: `internal/cli/cli.go:44` (the `open` line in `usage`)
- Modify: `internal/cli/open.go:19-28` (`openHelp`), `52-78` (flags and the
  `ensureWorkspace` call), `121-171` (`ensureWorkspace`), `34-43`
  (`openEnvelope`), `80-103` (the JSON block), `105-114` (the human block)
- Modify: `internal/cli/attach.go:73-90` (`buildAttach`'s resolution) and
  `3-14` (imports: `os` and `resolve` both become unused)
- Modify: `internal/cli/stop.go:72-89` (resolution) and `3-16` (imports: `os`
  and `resolve` become unused)
- Modify: `internal/cli/status.go:130-146` (`buildStatus`'s resolution) and
  `3-16` (imports: `os` becomes unused; `resolve` stays, used by
  `rebuildReasons`, `staleSessions`, `staleRepositoryRoots`, and
  `statusEnvelopeFrom`)
- Modify: `internal/cli/config.go:114-136` (`buildEnvelope`'s resolution) and
  `3-17` (imports: `os` and `resolve` become unused)
- Test: `internal/cli/target_test.go` (new: the grammar through every command)
- Test: `internal/cli/open_test.go` (new `--cwd` tests)

**Interfaces:**
- Consumes:
  ```go
  type target.Ref struct{ Present bool; Name string; Session string; HasSession bool }
  type target.MalformedError struct{ Arg string; Reason string }
  func target.Parse(arg string) (target.Ref, error)
  func target.Select(ref target.Ref, roots []string, cwd, stateRoot string) (resolve.Workspace, error)
  func bindpath.Rel(repoRoot, path string) (string, error)
  func state.Root() (string, error)
  // controller.Desired.Bind *string  (nil leaves the stored bind alone)
  // controller.EnsureResult.BindWarning string
  ```
- Produces:
  ```go
  func selectWorkspace(arg string, roots []string) (resolve.Workspace, error)
  func ensureWorkspace(ctx context.Context, name, cwdFlag string) (controller.EnsureResult, resolve.Workspace, error)
  // openEnvelope gains: BindWarning string `json:"bind_warning,omitempty"`
  ```

---

`target.MalformedError`'s exit-2 mapping is **not** in this task. Task 2 ships
`exitCode`'s new case and `TestMalformedTargetExitsTwo` in the same commit that
defines the error type, because an error type whose exit code lands one task
later is an error type that is briefly wrong. This task consumes that mapping;
it does not repeat it.

- [ ] **Step 1: Write the failing test for the seam across every command**

Create `internal/cli/target_test.go`:

```go
package cli

import (
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller/fake"
)

// Every command that takes a workspace argument parses it with the same
// grammar. The bare form is included because `projectmux <target>` is the
// open shorthand (cli.go:157-163), which is where a tab-completed
// filename actually lands.
func TestMalformedTargetIsAUsageErrorForEveryCommand(t *testing.T) {
	for _, argv := range [][]string{
		{"open", "slabledger/"},
		{"attach", "slabledger/"},
		{"stop", "slabledger/"},
		{"status", "slabledger/"},
		{"config", "slabledger/"},
		{"docs/commands.md"},
	} {
		t.Run(strings.Join(argv, " "), func(t *testing.T) {
			openWorkspace(t)
			installOpenStore(t, fake.NewStore())

			code, stdout, stderr := run(t, argv...)
			if code != ExitUsage {
				t.Fatalf("exit = %d, want %d (stderr: %s)", code, ExitUsage, stderr)
			}
			if stdout != "" {
				t.Errorf("a failing command wrote to stdout: %q", stdout)
			}
		})
	}
}

// A well-formed target naming a session resolves to that session rather
// than to the repository's default one. config is the cheapest command to
// prove it with: it resolves and reports identity without touching tmux.
func TestNamedSessionTargetResolvesToThatSession(t *testing.T) {
	slug := openWorkspace(t).Slug
	installOpenStore(t, fake.NewStore())

	code, stdout, stderr := run(t, "config", "--json", slug+"/feature-a")
	if code != 0 {
		t.Fatalf("exit = %d, stderr: %s", code, stderr)
	}
	var env envelope
	decodeJSON(t, stdout, &env)
	if env.Workspace.Session != "feature-a" {
		t.Errorf("session = %q, want %q", env.Workspace.Session, "feature-a")
	}
	if env.Workspace.SessionName != slug+"--feature-a" {
		t.Errorf("session_name = %q, want %q", env.Workspace.SessionName, slug+"--feature-a")
	}
}
```

Add the shared decoder to `internal/cli/target_test.go` as well — the existing
per-envelope decoders (`decodeOpen`, `decodeStatus`, `decodeStop`) each wrap
one type, and this file needs two:

```go
func decodeJSON(t *testing.T, stdout string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(stdout), into); err != nil {
		t.Fatalf("decoding JSON: %v\n%s", err, stdout)
	}
}
```

with `"encoding/json"` added to the file's imports.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestMalformedTargetIsAUsageErrorForEveryCommand|TestNamedSessionTargetResolvesToThatSession'`

Expected: FAIL. The malformed cases report `exit = 4, want 2` — `slabledger/`
reaches `resolve.Resolve` as a literal name and comes back
`UnknownWorkspaceError` — and the named-session case reports `session = ""`,
because nothing splits the argument on `/`.

- [ ] **Step 3: Add `selectWorkspace` and route all five commands**

Append to `internal/cli/wiring.go`, adding `"os"` (already imported) and
`"github.com/gambtho/projectmux/internal/resolve"` and
`"github.com/gambtho/projectmux/internal/target"` to its imports:

```go
// selectWorkspace turns a command's workspace argument into the workspace
// to act on: internal/target parses the grammar and chooses the session,
// including the bind lookup a bare invocation runs (spec §3).
//
// It is one function rather than a line in each command so the grammar and
// the selection rule cannot drift apart between `open` and `status`.
func selectWorkspace(arg string, roots []string) (resolve.Workspace, error) {
	ref, err := target.Parse(arg)
	if err != nil {
		return resolve.Workspace{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return resolve.Workspace{}, fmt.Errorf("determining the current directory: %w", err)
	}
	stateRoot, err := state.Root()
	if err != nil {
		return resolve.Workspace{}, err
	}
	return target.Select(ref, roots, cwd, stateRoot)
}
```

Then replace the resolution block in each command. In
`internal/cli/config.go`, `buildEnvelope` loses its `os.Getwd` and its
`resolve.Resolve`:

```go
	ws, err := selectWorkspace(name, defaults.Layer.RepositoryRoots)
	if err != nil {
		return envelope{}, err
	}
```

In `internal/cli/attach.go`, `buildAttach`:

```go
	ws, err := selectWorkspace(name, defaults.Layer.RepositoryRoots)
	if err != nil {
		return zero, "", err
	}
```

In `internal/cli/status.go`, `buildStatus`:

```go
	ws, err := selectWorkspace(name, defaults.Layer.RepositoryRoots)
	if err != nil {
		return statusEnvelope{}, err
	}
```

In `internal/cli/stop.go`, `runStop`:

```go
	ws, err := selectWorkspace(fs.Arg(0), defaults.Layer.RepositoryRoots)
	if err != nil {
		return err
	}
```

In `internal/cli/open.go`, `ensureWorkspace`:

```go
	ws, err := selectWorkspace(name, defaults.Layer.RepositoryRoots)
	if err != nil {
		return zero, resolve.Workspace{}, err
	}
```

Drop the now-unused `"os"` import from all five files, and the now-unused
`"github.com/gambtho/projectmux/internal/resolve"` import from `attach.go`,
`stop.go`, and `config.go`. `open.go` keeps `resolve` (its return type) and
`status.go` keeps it (`rebuildReasons`, `staleSessions`,
`staleRepositoryRoots`, `statusEnvelopeFrom`).

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/cli/ -run 'TestMalformedTargetIsAUsageErrorForEveryCommand|TestNamedSessionTargetResolvesToThatSession'`

Expected: PASS, all seven subtests.

Then `go test ./internal/cli/` — expected: PASS. Then `gofmt -l internal/cli`
— expected: no output.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/wiring.go internal/cli/open.go internal/cli/attach.go \
  internal/cli/stop.go internal/cli/status.go internal/cli/config.go \
  internal/cli/target_test.go
git commit -m "feat(cli): route every workspace argument through the target seam

open, attach, stop, status, and config each hand-rolled the same
Getwd-then-Resolve pair, which cannot express <repo>/<session> and has
nowhere to run the bind lookup. One selectWorkspace helper replaces all
five, so the grammar and the selection rule cannot drift between
commands. config --validate keeps its raw name: its argument is a
configuration file, not a target."
```

- [ ] **Step 6: Write the failing `--cwd` tests**

Append to `internal/cli/open_test.go`, adding `"path/filepath"` to its imports:

```go
// mkSubdir creates a directory inside the test repository and returns its
// repository-relative form, which is what a bind is stored as.
func mkSubdir(t *testing.T, rel string) string {
	t.Helper()
	repo, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(repo, filepath.FromSlash(rel)), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	return rel
}

func TestOpenCwdRecordsTheBind(t *testing.T) {
	ws := openWorkspace(t)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, _, stderr := run(t, "open", "--cwd", rel, "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("bind = %v, want %q", rec.Bind, rel)
	}
}

// A bind is a declaration about the session, not a side effect of a
// successful open (spec §4). Ensure registers and persists before it
// plans, so a refusal afterwards leaves the bind in place and retrying
// keeps it.
func TestOpenCwdBindSurvivesAFailedOpen(t *testing.T) {
	ws := openWorkspace(t)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	installScriptedSessions(t,
		func(controller.SessionQuery) (controller.SessionObservation, error) {
			return controller.SessionObservation{}, errors.New("tmux exploded")
		})

	code, _, _ := run(t, "open", "--cwd", rel, "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("bind = %v, want %q kept after the failed open", rec.Bind, rel)
	}
}

// A --cwd outside the repository is a caller mistake (spec §6: exit 2),
// and nothing is registered on the way to reporting it.
func TestOpenCwdOutsideTheRepositoryExitsTwo(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)

	code, stdout, stderr := run(t, "open", "--cwd", t.TempDir(), "--json")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitUsage, stderr)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if _, err := s.Workspace(ws.ID); err == nil {
		t.Error("a rejected --cwd registered the workspace anyway")
	}
}
```

- [ ] **Step 7: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run TestOpenCwd`

Expected: FAIL — `flag provided but not defined: -cwd`, so all three exit 2 and
the first two fail on the exit code (the third passes for the wrong reason,
which the next step corrects).

- [ ] **Step 8: Implement `--cwd`**

In `internal/cli/open.go`, add `"github.com/gambtho/projectmux/internal/bindpath"`
to the imports, extend `openHelp`:

```go
const openHelp = `usage: projectmux open [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]

Observe, ensure, record, and attach the workspace session, resolved
either from <target> or from the current directory. <target> is <repo> or
<repo>/<session>. The bare form "projectmux <target>" is shorthand for
this command (no flags).

  --no-attach  ensure and record without attaching the terminal
  --cwd <path> record <path> as the session's base directory before
               opening, in the same locked operation
  --json       emit the versioned JSON envelope (implies --no-attach)
  --compact    emit the JSON on a single line (implies --json)
`
```

add the flag and thread it through `runOpen`:

```go
	cwdFlag := fs.String("cwd", "", "record the session's base directory")
```

```go
	res, ws, err := ensureWorkspace(ctx, fs.Arg(0), *cwdFlag)
```

extend the envelope and both outputs:

```go
type openEnvelope struct {
	SchemaVersion         int                `json:"schema_version"`
	Workspace             workspaceInfo      `json:"workspace"`
	Action                string             `json:"action"`
	Session               string             `json:"session"`
	Drifted               bool               `json:"drifted"`
	BindWarning           string             `json:"bind_warning,omitempty"`
	Container             *openContainerInfo `json:"container,omitempty"`
	ContainerWindowsStale bool               `json:"container_windows_stale,omitempty"`
}
```

```go
			Drifted:               res.Drifted,
			BindWarning:           res.BindWarning,
			ContainerWindowsStale: res.ContainerWindowsStale,
```

```go
	if res.BindWarning != "" {
		fmt.Fprintln(stdout, res.BindWarning)
	}
```

placed directly after the `session %s (%s)` line, and finally give
`ensureWorkspace` the flag:

```go
// ensureWorkspace runs the read-only pipeline, derives the actuator
// windows, and calls the controller's Ensure under the workspace lock.
//
// cwdFlag carries --cwd into Desired.Bind rather than binding first and
// opening second. Ensure persists it in the same critical section that
// registers the workspace, before the observation the windows are planned
// from: one lock acquisition, and no window built from a bind that changed
// underneath it (spec §4). An empty cwdFlag leaves any stored bind alone.
func ensureWorkspace(ctx context.Context, name, cwdFlag string) (controller.EnsureResult, resolve.Workspace, error) {
```

with the bind computed immediately after resolution and before the config load,
so a rejected path costs nothing:

```go
	var bind *string
	if cwdFlag != "" {
		rel, relErr := bindpath.Rel(ws.RepoRoot, cwdFlag)
		if relErr != nil {
			// A path that does not exist or leaves the repository is a
			// caller mistake, not a failure: spec §6 puts it at exit 2.
			return zero, ws, usagef("open: --cwd: %s", relErr)
		}
		bind = &rel
	}
```

and passed on the `Desired`:

```go
	res, err := ctrl.Ensure(ctx, controller.Desired{
		Workspace: ws,
		Config:    effective.Config,
		Digest:    effective.Digest,
		Bind:      bind,
	}, intents, filepath.Join(stateRoot, "locks"), lockTimeout)
```

Update `usage` in `internal/cli/cli.go` to match:

```go
  open [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]
        observe, ensure, record, and attach the workspace session
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestOpenCwd`

Expected: PASS, all three.

Then `go test -race ./...` — expected: PASS. Then `gofmt -l internal/cli` —
expected: no output.

- [ ] **Step 10: Commit**

```bash
git add internal/cli/open.go internal/cli/cli.go internal/cli/open_test.go
git commit -m "feat(open): --cwd records the bind inside Ensure's critical section

--cwd sets Desired.Bind rather than running a bind and then an open, so
the bind is persisted by the same RegisterWorkspace that Ensure already
performs before it observes and plans. One lock acquisition, no window
built from a bind that changed underneath it, and a bind that survives
the open that carried it failing."
```

---

### Task 9: the `bind` command

`projectmux bind [--clear] [--json] [--compact] <target> [<path>]` declares
where a session opens. It is the only command that writes state without
observing anything, and the only one that creates a workspace record without
starting a session — binding is how a new named session is declared (spec §4).

Two properties carry the design. First, it takes the **workspace lock only**:
`lockPhases` (`internal/controller/locking.go:15`) is
`lockPhases(ctx, dir, repositoryID, workspaceID string, timeout time.Duration)`
and documents an empty `repositoryID` as "no container phase", so passing `""`
is what keeps a bind from queueing behind a sibling's `devcontainer up`.
`lockPhases` is unexported, so the locking stays in the controller and the CLI
calls a new `Controller.SetBind` — the same shape as `Stop`
(`internal/controller/stop.go:36-49`), which is the other command with a
partial lock set.

Second, a record created here carries the bind and **no applied digest**.
`BuildPlan` treats `snap.Stored.AppliedDigest == nil` as reapply
(`internal/controller/plan.go:71-73`, verified: `p.Reapply = snap.Stored == nil
|| snap.Stored.AppliedDigest == nil || *snap.Stored.AppliedDigest !=
snap.Desired.Digest`), so the first `open` on a bound session converges with no
special case. `RegisterWorkspace` sets `desired_digest` and never touches
`applied_digest` (`internal/state/store.go:123-138`), so creating the record
with an empty desired digest is enough; the next `open` writes the real one.
The record is created *only* when absent, because re-registering an existing
one would overwrite its desired digest with the empty string.

Output reuses the existing envelope helpers rather than inventing any:
`writeJSON` (`internal/cli/config.go:157-166`), `workspaceInfo`
(`config.go:62-68`), and `OutputSchemaVersion` (`config.go:29`), which stays 2 —
this is an added command, not a changed structure.

**Files:**
- Create: `internal/controller/bind.go`
- Create: `internal/cli/bind.go`
- Modify: `internal/cli/cli.go:37-71` (`usage`) and `cli.go:132-156` (`dispatch`)
- Modify: `internal/cli/wiring_test.go:31-62` (a `guardedStore` refusal for
  `SetBind`)
- Test: `internal/controller/bind_test.go`
- Test: `internal/cli/bind_test.go`

**Interfaces:**
- Consumes:
  ```go
  func target.Parse(arg string) (target.Ref, error)
  func target.Select(ref target.Ref, roots []string, cwd, stateRoot string) (resolve.Workspace, error)
  func bindpath.Rel(repoRoot, path string) (string, error)
  func selectWorkspace(arg string, roots []string) (resolve.Workspace, error)   // Task 8
  func (s *state.Store) SetBind(workspaceID string, bind *string, now time.Time) error  // Task 5
  // state.Record.Bind *string  (Task 5)
  ```
  Task 7 declares `SetBind` on the `controller.Store` interface
  (`internal/controller/interfaces.go:30-41`) and implements it on
  `fake.Store` (`internal/controller/fake/fake.go:98-140`) so that `Ensure` can
  persist `Desired.Bind`; this task consumes both and adds neither.
- Produces:
  ```go
  func (c *Controller) SetBind(ctx context.Context, ws resolve.Workspace, bind *string, lockDir string, lockTimeout time.Duration) (bool, error)
  func runBind(ctx context.Context, args []string, stdout io.Writer) error
  func bindArgument(repoRoot string, clearBind bool, path string) (*string, error)
  type bindEnvelope struct {
  	SchemaVersion int
  	Workspace     workspaceInfo
  	Bind          *string
  	Created       bool
  }
  ```

---

- [ ] **Step 1: Write the failing `Controller.SetBind` tests**

Create `internal/controller/bind_test.go`. It reuses `ensureWorkspace()` and
`ensureTime` from `ensure_test.go:110-118` and `:20`, which are in the same
external test package:

```go
package controller_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/lock"
)

// bindRig is newEnsureRig without the session machinery: SetBind observes
// nothing and actuates nothing, so a store, a clock, and a lock directory
// are the whole controller it needs.
func bindRig(t *testing.T) (*controller.Controller, *fake.Store, string) {
	t.Helper()
	store := fake.NewStore()
	ctrl := &controller.Controller{Store: store, Clock: &fake.Clock{Time: ensureTime}}
	return ctrl, store, t.TempDir()
}

// Binding a session that has never been opened creates its record, with
// the bind and no applied digest, so BuildPlan's nil-digest reapply rule
// (plan.go:71-73) makes the first open converge with no special case.
func TestSetBindCreatesTheRecord(t *testing.T) {
	ctrl, store, lockDir := bindRig(t)
	rel := "services/api"

	created, err := ctrl.SetBind(context.Background(), ensureWorkspace(), &rel, lockDir, time.Second)
	if err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	if !created {
		t.Error("created = false, want true for an unregistered session")
	}
	rec, err := store.Workspace("w1")
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("bind = %v, want %q", rec.Bind, rel)
	}
	if rec.AppliedDigest != nil {
		t.Errorf("applied digest = %v, want nil so the next open reapplies", rec.AppliedDigest)
	}
}

// Clearing removes the bind and leaves everything else, including the
// assigned session name, in place (spec §4).
func TestSetBindClearKeepsTheRecord(t *testing.T) {
	ctrl, store, lockDir := bindRig(t)
	ws := ensureWorkspace()
	if err := store.RegisterWorkspace(ws, "sha256:seed", ensureTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	name, err := store.AllocateSessionName(ws.ID, ensureTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	rel := "services/api"
	if _, err := ctrl.SetBind(context.Background(), ws, &rel, lockDir, time.Second); err != nil {
		t.Fatalf("SetBind: %v", err)
	}

	created, err := ctrl.SetBind(context.Background(), ws, nil, lockDir, time.Second)
	if err != nil {
		t.Fatalf("SetBind clear: %v", err)
	}
	if created {
		t.Error("created = true, want false for a session that already had a record")
	}
	rec, err := store.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("bind = %v, want nil after --clear", rec.Bind)
	}
	if rec.ActualSession == nil || *rec.ActualSession != name {
		t.Errorf("actual session = %v, want %q kept", rec.ActualSession, name)
	}
}

// Spec §4: bind has no container phase, so it must not queue behind a
// sibling's devcontainer up — but it must still serialize against another
// operation on the same session.
func TestSetBindTakesTheWorkspaceLockOnly(t *testing.T) {
	ctrl, _, lockDir := bindRig(t)
	ws := ensureWorkspace()
	rel := "services/api"

	repo, err := lock.Acquire(context.Background(), lockDir, ws.RepositoryID, time.Second)
	if err != nil {
		t.Fatalf("pre-acquiring the repository lock: %v", err)
	}
	defer func() { _ = repo.Release() }()

	if _, err := ctrl.SetBind(context.Background(), ws, &rel,
		lockDir, 200*time.Millisecond); err != nil {
		t.Fatalf("SetBind queued behind the repository lock: %v", err)
	}

	held, err := lock.Acquire(context.Background(), lockDir, ws.ID, time.Second)
	if err != nil {
		t.Fatalf("pre-acquiring the workspace lock: %v", err)
	}
	defer func() { _ = held.Release() }()

	_, err = ctrl.SetBind(context.Background(), ws, &rel, lockDir, 200*time.Millisecond)
	var lockErr *lock.ErrLockHeld
	if !errors.As(err, &lockErr) {
		t.Fatalf("err = %v, want *lock.ErrLockHeld", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/controller/ -run TestSetBind`

Expected: FAIL — the package does not compile:
`ctrl.SetBind undefined (type *controller.Controller has no field or method SetBind)`.

- [ ] **Step 3: Implement `Controller.SetBind`**

Create `internal/controller/bind.go`:

```go
package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// SetBind records a session's base directory, or clears it when bind is
// nil. It reports whether the session's record was created by this call.
//
// It takes the workspace lock and nothing else. Binding has no container
// phase, and lockPhases documents an empty repositoryID as exactly that,
// so a bind must not queue behind a sibling's devcontainer up (spec §4).
//
// A session that has never been opened is created here. The desired
// digest is left empty deliberately: bind loads no workspace
// configuration, so it has none to claim, and RegisterWorkspace never
// touches applied_digest — which leaves BuildPlan's nil-digest reapply
// rule (plan.go:71-73) to make the first open converge with no special
// case. Registration runs only when the record is absent, because
// re-registering an existing session would overwrite its desired digest
// with the empty string.
func (c *Controller) SetBind(ctx context.Context, ws resolve.Workspace, bind *string, lockDir string, lockTimeout time.Duration) (bool, error) {
	release, err := lockPhases(ctx, lockDir, "", ws.ID, lockTimeout)
	if err != nil {
		return false, err
	}
	defer release()

	now := c.Clock.Now()
	created := false
	if _, err := c.Store.Workspace(ws.ID); err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			return false, fmt.Errorf("reading the workspace record: %w", err)
		}
		if err := c.Store.RegisterWorkspace(ws, "", now); err != nil {
			return false, fmt.Errorf("registering the workspace: %w", err)
		}
		created = true
	}
	if err := c.Store.SetBind(ws.ID, bind, now); err != nil {
		return created, fmt.Errorf("recording the bind: %w", err)
	}
	return created, nil
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/controller/ -run TestSetBind -v`

Expected: PASS, all three.

- [ ] **Step 5: Commit**

```bash
git add internal/controller/bind.go internal/controller/bind_test.go
git commit -m "feat(controller): SetBind under the workspace lock alone

Binding has no container phase, so it passes an empty repositoryID to
lockPhases and never queues behind a sibling's devcontainer up. A
session that has never been opened is created here with the bind and no
applied digest, which BuildPlan already reads as reapply."
```

- [ ] **Step 6: Write the failing CLI tests**

Create `internal/cli/bind_test.go`:

```go
package cli

import (
	"testing"

	"github.com/gambtho/projectmux/internal/controller/fake"
)

func TestBindRecordsThePathRelativeToTheRepository(t *testing.T) {
	ws := openWorkspace(t)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", "--json", ws.Slug, rel)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, OutputSchemaVersion)
	}
	if env.Bind == nil || *env.Bind != rel {
		t.Errorf("bind = %v, want %q", env.Bind, rel)
	}
	if !env.Created {
		t.Error("created = false; binding an unregistered session should create its record")
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != rel {
		t.Errorf("stored bind = %v, want %q", rec.Bind, rel)
	}
}

// Binding a session that does not exist yet is how a named session is
// declared (spec §4), so the record it creates is that session's, not the
// repository's default one.
func TestBindCreatesANamedSession(t *testing.T) {
	ws := openWorkspace(t)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", "--json", ws.Slug+"/feature-a", rel)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.Workspace.Session != "feature-a" {
		t.Fatalf("session = %q, want feature-a", env.Workspace.Session)
	}
	if env.Workspace.ID == ws.ID {
		t.Error("the named session was recorded under the default session's ID")
	}
	if _, err := s.Workspace(ws.ID); err == nil {
		t.Error("binding a named session also created the default session's record")
	}
}

// The path defaults to the current directory, which is the repository
// root in these tests.
func TestBindDefaultsToTheCurrentDirectory(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", "--json", ws.Slug)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.Bind == nil || *env.Bind != "." {
		t.Errorf("bind = %v, want %q", env.Bind, ".")
	}
}

func TestBindClearRemovesTheBindAndKeepsTheRecord(t *testing.T) {
	ws := openWorkspace(t)
	rel := mkSubdir(t, "services/api")
	s := fake.NewStore()
	installOpenStore(t, s)

	if code, _, stderr := run(t, "bind", "--json", ws.Slug, rel); code != 0 {
		t.Fatalf("seeding bind: exit %d, stderr: %s", code, stderr)
	}
	code, stdout, stderr := run(t, "bind", "--clear", "--json", ws.Slug)
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	var env bindEnvelope
	decodeJSON(t, stdout, &env)
	if env.Bind != nil {
		t.Errorf("bind = %v, want null after --clear", env.Bind)
	}
	if env.Created {
		t.Error("created = true, want false: --clear must not re-create the record")
	}
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("--clear removed the record: %v", err)
	}
	if rec.Bind != nil {
		t.Errorf("stored bind = %v, want nil", rec.Bind)
	}
}

// Spec §4: the directory must exist at bind time, and the message names
// the path so the typo is visible without re-running anything.
func TestBindRejectsAMissingPath(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)

	code, stdout, stderr := run(t, "bind", ws.Slug, "services/nope")
	if code != ExitUsage {
		t.Fatalf("exit %d, want %d (stderr: %s)", code, ExitUsage, stderr)
	}
	if !strings.Contains(stderr, "services/nope") {
		t.Errorf("stderr = %q, should name the path", stderr)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if _, err := s.Workspace(ws.ID); err == nil {
		t.Error("a rejected bind created the record anyway")
	}
}

func TestBindRequiresATarget(t *testing.T) {
	openWorkspace(t)
	installOpenStore(t, fake.NewStore())

	code, _, _ := run(t, "bind")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}
```

Add `"strings"` to the file's imports (`TestBindRejectsAMissingPath` uses it).
`mkSubdir` and `decodeJSON` come from Task 8's `open_test.go` and
`target_test.go`, in the same package.

- [ ] **Step 7: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run TestBind`

Expected: FAIL — `undefined: bindEnvelope`, so the package does not compile.

- [ ] **Step 8: Implement the command**

Create `internal/cli/bind.go`:

```go
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/gambtho/projectmux/internal/bindpath"
	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

const bindHelp = `usage: projectmux bind [--clear] [--json] [--compact] <target> [<path>]

Record the directory a session opens in. <path> defaults to the current
directory, is interpreted relative to the repository root, must exist, and
must lie inside the repository; it is stored relative so it survives the
repository moving.

The bind is the session's base directory: every window's cwd composes on
top of it. Binding a session that does not exist yet creates it, so bind
is how a new named session is declared.

  --clear    remove the bind and keep the session
  --json     emit the versioned JSON envelope instead of human text
  --compact  emit the JSON on a single line (implies --json)
`

// bindEnvelope is the versioned JSON structure for projectmux bind. Bind
// is always emitted, and is null after --clear: a consumer cannot tell an
// absent field from a cleared one.
type bindEnvelope struct {
	SchemaVersion int           `json:"schema_version"`
	Workspace     workspaceInfo `json:"workspace"`
	Bind          *string       `json:"bind"`
	Created       bool          `json:"created"`
}

func runBind(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("bind")
	clearBind := fs.Bool("clear", false, "remove the bind and keep the session")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, bindHelp)
			return nil
		}
		return usagef("bind: %s", err)
	}
	if *compact {
		*asJSON = true
	}
	switch {
	case fs.NArg() == 0:
		return usagef("bind: expected a target")
	case *clearBind && fs.NArg() > 1:
		return usagef("bind: --clear takes no path, got %q", fs.Arg(1))
	case fs.NArg() > 2:
		return usagef("bind: expected at most a target and a path, got %d arguments", fs.NArg())
	}

	// Identity only. Like stop, bind loads no workspace configuration, so
	// a broken workspace YAML can never block declaring where a session
	// opens.
	root, err := config.Root()
	if err != nil {
		return err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return err
	}
	ws, err := selectWorkspace(fs.Arg(0), defaults.Layer.RepositoryRoots)
	if err != nil {
		return err
	}
	bind, err := bindArgument(ws.RepoRoot, *clearBind, fs.Arg(1))
	if err != nil {
		return err
	}

	st, err := openStore()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	stateRoot, err := state.Root()
	if err != nil {
		return err
	}

	ctrl := controller.Controller{Store: st, Clock: systemClock{}}
	created, err := ctrl.SetBind(ctx, ws, bind,
		filepath.Join(stateRoot, "locks"), lockTimeout)
	if err != nil {
		return err
	}

	env := bindEnvelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			RepoRoot:    ws.RepoRoot,
			Session:     ws.Session,
			SessionName: ws.SessionName,
		},
		Bind:    bind,
		Created: created,
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	if bind == nil {
		fmt.Fprintf(stdout, "cleared the bind on %s\n", ws.SessionName)
	} else {
		fmt.Fprintf(stdout, "bound %s to %s\n", ws.SessionName, *bind)
	}
	if created {
		fmt.Fprintf(stdout,
			"created session %s; run `projectmux open` on it to start it\n", ws.SessionName)
	}
	return nil
}

// bindArgument turns the command line into the value stored on the
// record: nil for --clear, and otherwise the repository-relative form of
// the path, defaulting to the current directory.
//
// A path that does not exist, or that leaves the repository, is a caller
// mistake rather than a failure, so bindpath's error is re-typed here to
// land on exit 2 (spec §6). It already names the path.
func bindArgument(repoRoot string, clearBind bool, path string) (*string, error) {
	if clearBind {
		return nil, nil
	}
	if path == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("determining the current directory: %w", err)
		}
		path = cwd
	}
	rel, err := bindpath.Rel(repoRoot, path)
	if err != nil {
		return nil, usagef("bind: %s", err)
	}
	return &rel, nil
}
```

Register it in `internal/cli/cli.go`'s `dispatch`, directly after the `stop`
case:

```go
	case "bind":
		return runBind(ctx, rest, stdout)
```

and in `usage`, after the `stop` entry:

```go
  bind [--clear] [--json] [--compact] <target> [<path>]
        record the directory a session opens in, relative to the
        repository root; creates the session if it does not exist
```

Finally, add the observation-command guard to
`internal/cli/wiring_test.go`, beside the other refusals:

```go
func (g guardedStore) SetBind(string, *string, time.Time) error {
	return g.forbidden("SetBind")
}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestBind -v`

Expected: PASS, all six.

Then `go test -race ./...` — expected: PASS. Then `gofmt -l .` — expected: no
output.

- [ ] **Step 10: Commit**

```bash
git add internal/cli/bind.go internal/cli/bind_test.go internal/cli/cli.go \
  internal/cli/wiring_test.go
git commit -m "feat(cli): the bind command

bind declares where a session opens, under the workspace lock alone so it
never queues behind a sibling's devcontainer up. The path is stored
relative to the repository root and must exist; --clear removes it and
keeps the session. Binding a session that does not exist creates its
record with no applied digest, which is how a named session is declared."
```
### Task 10: rebuild resolves a session against its own session component

`rebuild.Applier.applyCandidate` calls `a.Resolver.Resolve(sess.Worktree)`
(`internal/rebuild/apply.go:123`) *before* the identity gate at
`apply.go:135`. The resolver has no session parameter, so a live
`slab--feature-a` re-derives the **default** workspace's ID and
`SessionBelongsTo` fails: `rebuild` reports a false identity conflict and
refuses to recover a perfectly healthy named session. This task threads the
session component into `rebuild.Resolver.Resolve`, taken from
`controller.LiveSession.Session` (the `@dev_session` key added in Task 3).

`internal/rebuild/migrate.go` holds the other two call sites. At
`migrate.go:78` the subject is a legacy pre-session *repository* row, which
has no session component, so it passes `""`. At `migrate.go:221`
(`retagSessions`) the subject is a live session, and it must pass
`sess.Session`: that loop keys `claimants` by `ws.ID`, so resolving two
sibling sessions of one repository with `""` would derive one shared ID and
report a bogus duplicate-claimant collision.

`status` needs no change and must not be given one. `staleSessions`
(`internal/cli/status.go:219-232`) calls `resolve.Resolve` only after
`SessionBelongsTo` has already rejected the session, and `snap.Session.ByName`
holds only occupants of a *candidate name* for the queried workspace, so a
sibling never enters the loop; `staleRepositoryRoots` compares only
`RepoRoot` and never an ID. An earlier draft of the design asserted otherwise
and was wrong — do not "fix" either one.

This task also covers `stop --container`'s sibling check
(`internal/controller/stop.go:75-95`). The refusal branch has been unreachable
in practice because a repository could only ever hold one session, and the
existing tests reach it only by hand-registering a second row. Named sessions
make it genuinely reachable, and in the direction no test covers today:
stopping the *named* session's container while the *default* sibling is live.

**Files:**
- Modify: `internal/rebuild/apply.go:25-28` (the `Resolver` interface),
  `internal/rebuild/apply.go:123` (the call)
- Modify: `internal/rebuild/migrate.go:78`, `internal/rebuild/migrate.go:221`
- Modify: `internal/cli/rebuild.go:277-280` (`worktreeResolver.Resolve`)
- Test: `internal/rebuild/apply_test.go` (`mapResolver`, two new tests)
- Test: `internal/rebuild/migrate_test.go:44` (`migrateResolver.Resolve`)
- Test: `internal/controller/stop_test.go:240-262` (sibling fixtures, one new
  test)

**Interfaces:**
- Consumes:
  - `func resolve.Resolve(name, session string, roots []string, cwd string) (resolve.Workspace, error)` (Task 1)
  - `controller.LiveSession.Session string` (Task 3)
  - `func controller.SessionBelongsTo(s controller.LiveSession, ws resolve.Workspace) bool` (Task 3, now four keys)
  - `state.Record.Bind *string` (Task 7)
  - `func (*fake.Store) SetBind(workspaceID string, bind *string, now time.Time) error` (Task 8)
- Produces:
  - `rebuild.Resolver.Resolve(repoRoot, session string) (resolve.Workspace, error)`

---

- [ ] **Step 1: Write the failing named-session recovery test**

The fixture is a second workspace on the same repository root, differing only
in its session component — which is exactly the pair the one-argument resolver
cannot tell apart.

Add to `internal/rebuild/apply_test.go`, after `liveSession`:

```go
// namedWorkspace is projectmux()'s sibling: the same repository root and
// slug, a different session component, and therefore a different ID.
func namedWorkspace() resolve.Workspace {
	ws := workspace(
		"2222222222222222222222222222222222222222222222222222222222222222",
		"projectmux", "/src/projectmux", "projectmux--feature-a")
	ws.Session = "feature-a"
	return ws
}

// namedLiveSession carries the fourth identity key, the way a session
// opened by this build does.
func namedLiveSession(ws resolve.Workspace, name string) controller.LiveSession {
	s := liveSession(ws, name)
	s.Session = ws.Session
	return s
}

func TestApplyRecoversANamedSession(t *testing.T) {
	ws := namedWorkspace()
	sess := namedLiveSession(ws, "projectmux--feature-a")
	h := newHarness()
	h.know(projectmux(), "sha256:default")
	h.know(ws, "sha256:desired")
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none: the session resolves to its own workspace",
			report.Conflicts)
	}
	want := []Registered{{
		ID:       ws.ID,
		Slug:     "projectmux",
		RepoRoot: "/src/projectmux",
		Session:  "projectmux--feature-a",
	}}
	if !reflect.DeepEqual(report.Registered, want) {
		t.Fatalf("Registered = %+v, want %+v", report.Registered, want)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux--feature-a" {
		t.Errorf("ActualSession = %v, want the named session", rec.ActualSession)
	}
}
```

Teach the harness to hold both, keyed on the pair rather than the root alone.
Replace `know` (`apply_test.go:160-163`) and `mapResolver`
(`apply_test.go:46-61`):

```go
// resolverKey is the pair the resolver is keyed on. Keying on the root
// alone cannot hold a repository's default and named workspaces at the
// same time, which is the case these tests exist to cover.
type resolverKey struct {
	root    string
	session string
}

// mapResolver stands in for resolve.Resolve, which shells out to git. A
// missing entry is the vanished-worktree case; errs models a tree that
// exists but will not resolve.
type mapResolver struct {
	byWorktree map[resolverKey]resolve.Workspace
	errs       map[string]error
}

func (r *mapResolver) Resolve(worktree, session string) (resolve.Workspace, error) {
	if err := r.errs[worktree]; err != nil {
		return resolve.Workspace{}, err
	}
	ws, ok := r.byWorktree[resolverKey{root: worktree, session: session}]
	if !ok {
		return resolve.Workspace{}, fmt.Errorf("no worktree at %s", worktree)
	}
	return ws, nil
}
```

```go
func (h *harness) know(ws resolve.Workspace, digest string) {
	h.resolver.byWorktree[resolverKey{root: ws.RepoRoot, session: ws.Session}] = ws
	h.config.digests[ws.Slug] = digest
}
```

and `newHarness` (`apply_test.go:151`):

```go
		resolver:  &mapResolver{byWorktree: map[resolverKey]resolve.Workspace{}, errs: map[string]error{}},
```

- [ ] **Step 2: Run it and see the compile failure**

```bash
go test ./internal/rebuild/ -run TestApplyRecoversANamedSession
```

Expected: the package does not build —
`internal/rebuild/apply.go:123:36: not enough arguments in call to a.Resolver.Resolve` (and the same at `migrate.go:78` and `migrate.go:221`). The
test doubles now demand the session; production does not supply it.

- [ ] **Step 3: Thread the session through the resolver**

`internal/rebuild/apply.go`, the interface at lines 25-28:

```go
type Resolver interface {
	Resolve(repoRoot, session string) (resolve.Workspace, error)
	Exists(path string) bool
}
```

and the call at `apply.go:123`:

```go
	// The session component is an input to the workspace ID (decision
	// 0001), so re-deriving identity without it resolves every named
	// session to its repository's default workspace and fails the gate
	// below as a false identity conflict.
	ws, err := a.Resolver.Resolve(sess.Worktree, sess.Session)
```

`internal/rebuild/migrate.go:78` — a legacy repository row predates session
components and has none:

```go
		ws, resolveErr := a.Resolver.Resolve(repo.RepoRoot, "")
```

`internal/rebuild/migrate.go:221` — `claimants` is keyed by `ws.ID`, so
resolving siblings without their session would collapse them onto one ID and
report a duplicate that does not exist:

```go
		ws, err := a.Resolver.Resolve(sess.Worktree, sess.Session)
```

`internal/cli/rebuild.go:277-280`:

```go
func (worktreeResolver) Resolve(repoRoot, session string) (resolve.Workspace, error) {
	// No name and no roots: roots feed only lookup by name, and rebuild
	// resolves from a directory. The session comes from @dev_session, so
	// a named session re-derives its own ID rather than the default's.
	return resolve.Resolve("", session, nil, repoRoot)
}
```

`internal/rebuild/migrate_test.go:44` — that pass only ever resolves legacy
rows, so its double keeps its single-key map and ignores the session:

```go
func (r migrateResolver) Resolve(repoRoot, _ string) (resolve.Workspace, error) {
	if err := r.errs[repoRoot]; err != nil {
		return resolve.Workspace{}, err
	}
	ws, ok := r.roots[repoRoot]
	if !ok {
		return resolve.Workspace{}, fmt.Errorf("no repository at %s", repoRoot)
	}
	return ws, nil
}
```

- [ ] **Step 4: Run it and see PASS**

```bash
go test ./internal/rebuild/ ./internal/cli/ -run 'TestApply|TestMigrate|TestRebuild'
```

Expected: PASS, including `TestApplyRecoversANamedSession`.

- [ ] **Step 5: Commit the resolver change**

```bash
git add internal/rebuild/apply.go internal/rebuild/migrate.go internal/cli/rebuild.go internal/rebuild/apply_test.go internal/rebuild/migrate_test.go
git commit -m "fix(rebuild): resolve a session against its own session component

Re-deriving identity without @dev_session mapped every named session
onto its repository's default workspace, so rebuild reported a false
identity conflict instead of recovering it."
```

- [ ] **Step 6: Write the unresolvable-bind pinning test**

A named session's stored row can carry a `bind` that no longer resolves —
deleted, or replaced by a symlink pointing outside the repository. `rebuild`
must still recover the session and must leave the bind alone: dropping a
column the operator set, on a pass whose whole purpose is recovery, destroys
state instead of restoring it. `rebuild` deliberately never writes `bind`
(`RegisterWorkspace` upserts slug, repository root, session, proposed session,
and desired digest only), so this test pins that as a property rather than an
accident.

**This is the one test in the plan with no red phase, and that is deliberate.**
It is a pinning test: the property already holds the moment `state.Record.Bind`
exists, because no rebuild code path names the column. Manufacturing a red
phase here would mean writing a wrong implementation first, which for a
"nothing touches this" property means deliberately breaking rebuild. Step 7
therefore expects PASS on the first run, and its value is entirely in the
future run where someone adds a `RegisterWorkspace` call to the adopt path and
this test stops them.

The bind must be *real stored state*, not an overlay on reads: a wrapper store
that synthesizes `rec.Bind` on every `Workspace` call would return the bind
whatever the write paths did, so it could not fail. `fake.Store.SetBind` (Task
8) is the writer, so the row is seeded exactly the way `bind` seeds it.

The candidate is `CaseAdopt`, not `CaseRegister`: a register candidate has no
row, so it has no bind to preserve and the test would be about nothing.

Add to `internal/rebuild/apply_test.go`:

```go
func TestApplyKeepsANamedSessionsBindWhenItNoLongerResolves(t *testing.T) {
	ws := namedWorkspace()
	sess := namedLiveSession(ws, "projectmux--feature-a")
	h := newHarness()
	h.know(ws, "sha256:desired")

	// seedRecorded's fixture carries no session component, which now reads
	// as an identity mismatch against a named session (Task 3), so the row
	// is seeded here with one.
	recorded := workspace(ws.ID, ws.Slug, ws.RepoRoot, "recorded-proposed")
	recorded.Session = ws.Session
	if err := h.fakeStore.RegisterWorkspace(recorded, "sha256:recorded", testTime); err != nil {
		t.Fatalf("seeding the recorded row: %v", err)
	}
	bind := "services/gone"
	if err := h.fakeStore.SetBind(ws.ID, &bind, testTime); err != nil {
		t.Fatalf("SetBind: %v", err)
	}
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseAdopt, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v; a bind that cannot be resolved is not a reason to refuse recovery",
			report.Conflicts)
	}
	if len(report.Registered) != 1 || report.Registered[0].Session != "projectmux--feature-a" {
		t.Fatalf("Registered = %+v, want the named session reported", report.Registered)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.Bind == nil || *rec.Bind != "services/gone" {
		t.Errorf("Bind = %v, want it preserved: rebuild recovers state, it does not discard it",
			rec.Bind)
	}
}
```

- [ ] **Step 7: Run it and confirm it passes unchanged**

```bash
go test ./internal/rebuild/ -run TestApplyKeepsANamedSessionsBindWhenItNoLongerResolves -v
```

Expected: PASS, with no production edit.

If it fails to compile on `rec.Bind` or `SetBind`, Tasks 7 and 8 have not
landed and this task is being run out of order — stop and check the sequence
rather than adding the column here.

If it fails on the `Bind` assertion, a rebuild write path is clearing the
column, which is the regression this test exists to catch: fix that path, do
not relax the assertion.

- [ ] **Step 8: Run the whole package**

`applyCandidate` never reads or writes `bind`, and the adopt branch calls
`AdoptSessionName` alone — never `RegisterWorkspace`, whose conflict branch
overwrites slug, repository root, session, proposed session, and desired
digest. The column survives untouched.

```bash
go test ./internal/rebuild/
```

Expected: PASS, all tests in the package.

- [ ] **Step 9: Commit the bind regression test**

```bash
git add internal/rebuild/apply_test.go
git commit -m "test(rebuild): pin that an unresolvable bind is kept, not dropped

Adoption calls AdoptSessionName alone, so the bind column survives. This
is a pinning test with no red phase: it guards the property against a
future write path rather than driving one."
```

- [ ] **Step 10: Write the failing sibling-refusal test**

The existing sibling tests stop the *default* session while a named sibling is
live. The newly reachable direction is the reverse — stopping the named
session's container while the default sibling is live — and it is the one that
exercises `liveSiblings` skipping by `rec.ID` rather than by name.

In `internal/controller/stop_test.go`, replace `registerSibling` and
`siblingSession` (lines 240-262) with:

```go
// siblingWorkspace is the second session on repository r1. The resolver
// can produce one as of the target grammar, and the schema has permitted
// it since #31.
func siblingWorkspace() resolve.Workspace {
	return resolve.Workspace{
		ID: "w2", RepositoryID: "r1", Slug: "slab", RepoRoot: "/w/slab",
		Session: "feature-a", SessionName: "slab--feature-a",
	}
}

func siblingDesired() controller.Desired {
	d := ensureDesired()
	d.Workspace = siblingWorkspace()
	return d
}

// registerSibling adds a second session on the same repository.
func registerSibling(t *testing.T, r *ensureRig) {
	t.Helper()
	if err := r.store.RegisterWorkspace(siblingWorkspace(), "sha256:x", ensureTime); err != nil {
		t.Fatalf("register sibling: %v", err)
	}
	if _, err := r.store.AllocateSessionName("w2", ensureTime); err != nil {
		t.Fatalf("allocate sibling: %v", err)
	}
}

func siblingSession() controller.LiveSession {
	return controller.LiveSession{
		ID: "$9", Name: "slab--feature-a", WorkspaceID: "w2",
		Slug: "slab", Worktree: "/w/slab", Session: "feature-a",
	}
}
```

and add:

```go
// TestStopContainerRefusesTheNamedSessionWhileTheDefaultIsLive is the
// direction that only became reachable with named sessions: the target
// is the named workspace and the live sibling is the repository's
// default. liveSiblings skips by record ID, so nothing about the two
// sharing a slug may let the target observe itself.
func TestStopContainerRefusesTheNamedSessionWhileTheDefaultIsLive(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	registerStopFixture(t, r)
	registerSibling(t, r)
	bindStopContainer(t, r)

	_, err := r.stop(t, siblingDesired(), controller.StopOptions{Container: true})
	var refusal *controller.RefusalError
	if !errors.As(err, &refusal) {
		t.Fatalf("err = %v, want *RefusalError", err)
	}
	if !strings.Contains(refusal.Reason, "slab") ||
		!strings.Contains(refusal.Reason, "use --force") {
		t.Errorf("reason = %q, want the live default sibling named and --force offered",
			refusal.Reason)
	}
	if len(r.sessions.queries) != 1 || r.sessions.queries[0].WorkspaceID != "w1" {
		t.Errorf("queries = %+v, want one query naming the default workspace w1",
			r.sessions.queries)
	}
	if len(r.actuator.Killed) != 0 {
		t.Errorf("Killed = %v; a refusal must destroy nothing", r.actuator.Killed)
	}
	if len(r.actuatorC.Stopped) != 0 {
		t.Errorf("Stopped = %v; the shared container was killed anyway", r.actuatorC.Stopped)
	}
}
```

- [ ] **Step 11: Run it and see PASS with no production change**

```bash
go test ./internal/controller/ -run TestStopContainer
```

Expected: PASS. The branch was correct all along; it had no test reaching it
from this direction. If it fails on `queries[0].WorkspaceID`, `liveSiblings`
is matching by something other than the record ID and the refusal names the
wrong session.

- [ ] **Step 12: Commit the sibling test**

```bash
git add internal/controller/stop_test.go
git commit -m "test(stop): cover the sibling refusal from the named session

Stopping a named session's shared container while the repository's
default session is live is the direction named sessions made reachable."
```

- [ ] **Step 13: Run the full verification and commit nothing new**

```bash
gofmt -l ./cmd ./internal
go test -race ./...
```

Expected: `gofmt -l` prints nothing, and every package passes.
### Task 11: reporting the session and the bind

Two facts the earlier tasks made real — a session component on the workspace and
a bind on the record — are still invisible to every reader. This task surfaces
them: `list` gains a `BIND` column and renders each row as the target a user
would actually type (`slug/session` for a named session, bare `slug` for the
repository's default one), and `bind` appears in the JSON for `list`, `status`,
and `open`. `status` also surfaces the unusable-bind condition, which today only
`open` can see because only `Ensure` computes it.

`OutputSchemaVersion` stays **2**. Every change here is an *added* JSON field or
an *added* table column: no key is renamed, no key changes type, no key is
removed, and the human table keeps all five of its existing columns. That is not
a claim to take on trust — Step 1 pins it, by decoding a `list` row and asserting
the complete pre-existing key set is still present, that `session` and the new
`bind` both decode as strings, and that `schema_version` is the literal 2 rather
than whatever the constant happens to say (the trick `schema_version_test.go:20`
already documents).

The new nullable fields follow the local convention for nullable JSON in these
envelopes — `*string` with `omitempty`, as `listRow.ActualSession`
(`list.go:41`), `listRow.LiveSession` (`list.go:43`), and `storedInfo.ActualSession`
(`status.go:45`) all do — so an unbound workspace carries no `bind` key at all
rather than an explicit null.

`status` never calls `Ensure`, so it cannot read `EnsureResult.BindWarning`
directly. Rather than re-derive the condition (and drift from Ensure's wording),
this task exports a one-line wrapper over the check `Ensure` already runs,
`controller.BindWarning`, so both commands report the same condition in the same
words from the same code. `open` reports the effective bind through a new
`EnsureResult.Bind`, because only the controller — inside the workspace lock,
after `SetBind` and after the observation — knows which bind the session was
actually planned from.

**Files:**
- Modify: `internal/cli/list.go:35-47` (`listRow` gains `Bind`), `112-123` (the
  recorded row), `155-163` (the unrecorded row), `170-187` (`writeListHuman`),
  and new `listWorkspaceCell`/`listBindCell` beside `listSessionCell:189-200`
- Modify: `internal/cli/status.go:28-40` (`statusEnvelope` gains `BindWarning`),
  `42-47` (`storedInfo` gains `Bind`), `299-322` (`statusEnvelopeFrom`'s stored
  block), `359-369` (the human recorded block)
- Modify: `internal/cli/open.go:34-43` (`openEnvelope` gains `Bind`), `80-93`
  (the JSON envelope literal), `104-108` (the human block)
- Modify: `internal/controller/ensure.go` (`EnsureResult` gains `Bind`; new
  exported `BindWarning` beside `resolveBindBase`; the three result paths that
  Task 7 already threads `BindWarning` through)
- Test: `internal/cli/schema_version_test.go`, `internal/cli/list_test.go`,
  `internal/cli/status_test.go`, `internal/cli/open_test.go`

**Interfaces:**
- Consumes:
  ```go
  // state.Record.Bind *string                                    // Task 5
  // controller.EnsureResult.BindWarning string                   // Task 7
  // controller.LiveSession.Session string                        // Task 3
  // resolve.Workspace.Session string (populated by Resolve)       // Task 1
  func bindpath.Resolve(repoRoot, rel string) (string, error)     // Task 4
  func (s *fake.Store) SetBind(workspaceID string, bind *string, now time.Time) error // Task 7
  ```
- Produces:
  ```go
  // controller.EnsureResult gains:
  Bind *string
  func controller.BindWarning(repoRoot string, stored *state.Record) string
  // internal/cli, unexported:
  // listRow.Bind *string          `json:"bind,omitempty"`
  // storedInfo.Bind *string       `json:"bind,omitempty"`
  // statusEnvelope.BindWarning string `json:"bind_warning,omitempty"`
  // openEnvelope.Bind *string     `json:"bind,omitempty"`
  func listWorkspaceCell(row listRow) string
  func listBindCell(row listRow) string
  func boundListStore(t *testing.T) *fake.Store
  ```

---

- [ ] **Step 1: Write the failing schema-compatibility test**

This is the test that proves the version is still honestly 2. It needs a store
holding a bound, named-session workspace beside a plain one;
`seededListStore` (`list_test.go:25`) cannot be reused, because three existing
tests index its rows positionally (`list_test.go:81-108`, `143-154`) and would
break the moment its shape changed.

Append to `internal/cli/list_test.go`:

```go
// boundListStore seeds a workspace carrying both a named session and a
// bind beside a plain default-session one, so rendering can be checked
// on each. It is separate from seededListStore because that fixture's
// row count and order are asserted positionally by three tests.
func boundListStore(t *testing.T) *fake.Store {
	t.Helper()
	s := fake.NewStore()
	named := listWorkspace("w1", "alpha")
	named.Session = "feature-a"
	named.SessionName = "alpha--feature-a"
	if err := s.RegisterWorkspace(named, "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	bind := "services/api"
	if err := s.SetBind("w1", &bind, cliTestTime); err != nil {
		t.Fatalf("set bind: %v", err)
	}
	if err := s.RegisterWorkspace(listWorkspace("w2", "beta"), "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	return s
}
```

Append to `internal/cli/schema_version_test.go`:

```go
// The bind is an added field, not a schema break, so OutputSchemaVersion
// stays 2. Both halves of that claim are pinned here: the version is
// compared against the literal 2, and the complete pre-existing key set
// of a list row is asserted present, so a rename, a removal, or a
// retype fails here rather than reaching a consumer. An unbound row
// carries no bind key at all, which is the local nullable convention
// (list.go:41,43) and keeps the addition invisible to a v2 reader that
// never asked about binds.
func TestListRowsGainBindWithoutBreakingSchemaV2(t *testing.T) {
	installFakeStore(t, boundListStore(t))
	installLiveSessions(t, nil, nil)

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)

	var env struct {
		Workspaces []map[string]json.RawMessage `json:"workspaces"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding the envelope: %v\n%s", err, stdout)
	}
	if len(env.Workspaces) != 2 {
		t.Fatalf("%d rows, want 2:\n%s", len(env.Workspaces), stdout)
	}
	for _, key := range []string{
		"id", "slug", "repo_root", "session", "session_state",
		"recorded", "identity_conflict", "bind",
	} {
		if _, ok := env.Workspaces[0][key]; !ok {
			t.Errorf("bound row has no %q key:\n%s", key, stdout)
		}
	}
	var session, bind string
	if err := json.Unmarshal(env.Workspaces[0]["session"], &session); err != nil {
		t.Fatalf("session is not a string: %v", err)
	}
	if session != "feature-a" {
		t.Errorf("session = %q, want feature-a", session)
	}
	if err := json.Unmarshal(env.Workspaces[0]["bind"], &bind); err != nil {
		t.Fatalf("bind is not a string: %v", err)
	}
	if bind != "services/api" {
		t.Errorf("bind = %q, want services/api", bind)
	}
	if _, ok := env.Workspaces[1]["bind"]; ok {
		t.Errorf("unbound row carries a bind key:\n%s", stdout)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestListRowsGainBindWithoutBreakingSchemaV2`

Expected: FAIL — `bound row has no "bind" key`, followed by a fatal
`bind is not a string: unexpected end of JSON input` from unmarshalling the
absent field's nil `json.RawMessage`.

- [ ] **Step 3: Add `Bind` to `listRow` and populate it**

In `internal/cli/list.go`, append the field to `listRow` (last, so no existing
key moves) and populate it from the record. The struct at `list.go:35-47`
becomes:

```go
type listRow struct {
	ID               string               `json:"id"`
	Slug             string               `json:"slug"`
	RepoRoot         string               `json:"repo_root"`
	Session          string               `json:"session"`
	ProposedSession  string               `json:"proposed_session,omitempty"`
	ActualSession    *string              `json:"actual_session,omitempty"`
	SessionState     string               `json:"session_state"`
	LiveSession      *string              `json:"live_session,omitempty"`
	Container        *storedContainerInfo `json:"container,omitempty"`
	Recorded         bool                 `json:"recorded"`
	IdentityConflict bool                 `json:"identity_conflict"`
	// Bind is the session's base directory, repository-relative, absent
	// when the session opens at the repository root. list reports it
	// verbatim and never resolves it: a broken bind is status's business
	// (spec §5), and list resolves nothing.
	Bind *string `json:"bind,omitempty"`
}
```

and in the recorded loop (`list.go:114-123`) add one field to the literal:

```go
		row := listRow{
			ID:              rec.ID,
			Slug:            rec.Slug,
			RepoRoot:        rec.RepoRoot,
			Session:         rec.Session,
			ProposedSession: rec.ProposedSession,
			ActualSession:   rec.ActualSession,
			Container:       storedContainer(rec.Container),
			Bind:            rec.Bind,
			Recorded:        true,
		}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/cli/ -run 'TestList' -v`

Expected: PASS — `TestListRowsGainBindWithoutBreakingSchemaV2` ok, and every
pre-existing `TestList*` still ok.

- [ ] **Step 5: Commit**

```bash
git add internal/cli/list.go internal/cli/list_test.go internal/cli/schema_version_test.go
git commit -m "feat(cli): report a workspace's bind in list --json

An added field, not a schema break: OutputSchemaVersion stays 2, and the
test pins the row's whole pre-existing key set against the literal 2."
```

- [ ] **Step 6: Write the failing human-table test**

Append to `internal/cli/list_test.go`:

```go
// The workspace column renders the target a user would type: slug/session
// for a named session, bare slug for the repository's default one. A
// default session must never render with a trailing slash, which is what
// the "beta/" check rules out.
func TestListHumanRendersTheBindAndTheSessionTarget(t *testing.T) {
	installFakeStore(t, boundListStore(t))
	installLiveSessions(t, nil, nil)

	code, stdout, stderr := run(t, "list")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if !strings.Contains(stdout, "BIND") {
		t.Errorf("table has no BIND column:\n%s", stdout)
	}
	if !strings.Contains(stdout, "alpha/feature-a") {
		t.Errorf("named session does not render as slug/session:\n%s", stdout)
	}
	if !strings.Contains(stdout, "services/api") {
		t.Errorf("bind is not rendered:\n%s", stdout)
	}
	if strings.Contains(stdout, "beta/") {
		t.Errorf("the default session renders with a slash:\n%s", stdout)
	}
	// The five pre-existing columns survive the insertion.
	for _, column := range []string{"WORKSPACE", "SESSION", "TMUX", "CONTAINER", "NOTES"} {
		if !strings.Contains(stdout, column) {
			t.Errorf("table lost the %s column:\n%s", column, stdout)
		}
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestListHumanRendersTheBindAndTheSessionTarget`

Expected: FAIL — `table has no BIND column`, `named session does not render as
slug/session`, and `bind is not rendered`.

- [ ] **Step 8: Add the column and the two cell renderers**

In `internal/cli/list.go`, replace the header and row lines in
`writeListHuman` (`list.go:177-182`):

```go
	fmt.Fprintln(tw, "WORKSPACE\tSESSION\tBIND\tTMUX\tCONTAINER\tNOTES")
	for _, row := range env.Workspaces {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			listWorkspaceCell(row), listSessionCell(row), listBindCell(row),
			row.SessionState, listContainerCell(row.Container), listNotesCell(row))
	}
```

and add the two renderers above `listSessionCell` (`list.go:189`):

```go
// listWorkspaceCell renders the target a user would type for this row:
// slug/session for a named session, bare slug for the repository's
// default one, which carries the empty session component (spec §5).
func listWorkspaceCell(row listRow) string {
	if row.Slug != "" && row.Session != "" {
		return row.Slug + "/" + row.Session
	}
	return dashIfEmpty(row.Slug)
}

// listBindCell renders the stored bind. It is printed verbatim: list
// resolves nothing, so a bind that no longer exists still shows the
// value that has to be corrected.
func listBindCell(row listRow) string {
	if row.Bind == nil {
		return "-"
	}
	return dashIfEmpty(*row.Bind)
}
```

- [ ] **Step 9: Run the test to verify it passes**

Run: `go test ./internal/cli/ -run 'TestList' -v`

Expected: PASS — `TestListHumanRendersTheBindAndTheSessionTarget` ok, and
`TestListHumanNeverRendersRetainedBindingAsLive` (`list_test.go:176`) still ok:
its three assertions are substring checks on the session and container cells,
which the extra column shifts but does not alter.

- [ ] **Step 10: Commit**

```bash
git add internal/cli/list.go internal/cli/list_test.go
git commit -m "feat(cli): give list a BIND column and slug/session targets"
```

- [ ] **Step 11: Write the failing test for an unrecorded row's session**

A live session that no record claims still renders in `list`
(`list.go:146-164`), and it now carries a session component of its own from
`@dev_session`. Without it, a live `alpha/feature-a` that lost its record would
render as plain `alpha` — the wrong target to type.

Append to `internal/cli/list_test.go`:

```go
func TestListUnrecordedRowCarriesItsSessionComponent(t *testing.T) {
	installFakeStore(t, fake.NewStore())
	installLiveSessions(t, []controller.LiveSession{{
		Name:        "gamma--feature-b",
		WorkspaceID: "w9",
		Slug:        "gamma",
		Worktree:    "/w/gamma",
		Session:     "feature-b",
	}}, nil)

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeList(t, stdout)
	if len(env.Workspaces) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(env.Workspaces), env.Workspaces)
	}
	if env.Workspaces[0].Session != "feature-b" {
		t.Errorf("unrecorded row session = %q, want feature-b",
			env.Workspaces[0].Session)
	}
}
```

- [ ] **Step 12: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run TestListUnrecordedRowCarriesItsSessionComponent`

Expected: FAIL — `unrecorded row session = "", want feature-b`.

- [ ] **Step 13: Carry the session onto the unrecorded row**

In `internal/cli/list.go`, add one field to the extras literal
(`list.go:155-163`):

```go
		env.Workspaces = append(env.Workspaces, listRow{
			ID:               s.WorkspaceID,
			Slug:             s.Slug,
			RepoRoot:         s.Worktree,
			Session:          s.Session,
			SessionState:     "live",
			LiveSession:      &name,
			Recorded:         false,
			IdentityConflict: len(byID[s.WorkspaceID]) > 1,
		})
```

The row keeps no `Bind`: a bind is stored state, and this row has no record.

- [ ] **Step 14: Run the test to verify it passes**

Run: `go test ./internal/cli/ -run 'TestList' -v`

Expected: PASS — `TestListUnrecordedRowCarriesItsSessionComponent` ok, and
every pre-existing `TestList*` still ok.

- [ ] **Step 15: Commit**

```bash
git add internal/cli/list.go internal/cli/list_test.go
git commit -m "feat(cli): carry the session component onto unrecorded list rows"
```

- [ ] **Step 16: Write the failing status test**

Append to `internal/cli/status_test.go`:

```go
// status reports the stored bind, and reports when that bind cannot be
// used. Only Ensure computes the latter today, so status has to reach
// the same check without ensuring anything (spec §5).
func TestStatusReportsTheBindAndAnUnusableBind(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	bind := "services/gone"
	if err := s.SetBind(ws.ID, &bind, cliTestTime); err != nil {
		t.Fatalf("set bind: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if env.Stored == nil || env.Stored.Bind == nil || *env.Stored.Bind != bind {
		t.Fatalf("stored.bind = %+v, want %q", env.Stored, bind)
	}
	if env.BindWarning == "" {
		t.Fatal("bind_warning is empty; an unusable bind is invisible")
	}
	if !strings.Contains(env.BindWarning, bind) {
		t.Errorf("bind_warning = %q, want it to name the bind", env.BindWarning)
	}
	if !strings.Contains(stdout, `"schema_version": 2`) {
		t.Errorf("status envelope is no longer schema 2:\n%s", stdout)
	}
}

func TestStatusIsQuietWhenNoBindIsRecorded(t *testing.T) {
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
	env := decodeStatus(t, stdout)
	if env.Stored == nil || env.Stored.Bind != nil {
		t.Errorf("stored.bind = %+v, want none", env.Stored)
	}
	if env.BindWarning != "" {
		t.Errorf("bind_warning = %q, want empty", env.BindWarning)
	}
	if strings.Contains(stdout, "bind") {
		t.Errorf("an unbound workspace emits a bind key:\n%s", stdout)
	}
}
```

`services/gone` is never created, so `bindpath.Resolve` fails to canonicalize it
— the unusable case, without needing a symlink.

- [ ] **Step 17: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestStatus.*Bind'`

Expected: FAIL — the package does not compile:
`env.Stored.Bind undefined (type *storedInfo has no field or method Bind)` and
`env.BindWarning undefined (type statusEnvelope has no field or method BindWarning)`.

- [ ] **Step 18: Export the check and report it from status**

In `internal/controller/ensure.go`, add the exported wrapper beside
`resolveBindBase`:

```go
// BindWarning reports why a stored bind cannot be used, or "" when it is
// usable. It is exactly the check Ensure runs before rendering windows,
// exported so that status can report the condition without ensuring
// anything — one derivation, so the two commands cannot drift.
func BindWarning(repoRoot string, stored *state.Record) string {
	_, warning := resolveBindBase(repoRoot, stored)
	return warning
}
```

In `internal/cli/status.go`, add the two fields. `statusEnvelope`
(`status.go:28-40`) gains, after `NeedsRebuildReason`:

```go
	// BindWarning explains why the recorded bind cannot be used. It sits
	// beside the workspace rather than under stored, because it is a
	// verdict about the current filesystem, not something the store holds.
	BindWarning string `json:"bind_warning,omitempty"`
```

and `storedInfo` (`status.go:42-47`) gains:

```go
	Bind *string `json:"bind,omitempty"`
```

In `statusEnvelopeFrom`, add `Bind: rec.Bind,` to the `storedInfo` literal
(`status.go:303-307`):

```go
		env.Stored = &storedInfo{
			ProposedSession: rec.ProposedSession,
			ActualSession:   rec.ActualSession,
			Bind:            rec.Bind,
			RegisteredAt:    stamp(rec.RegisteredAt),
			UpdatedAt:       stamp(rec.UpdatedAt),
		}
```

and set the warning after the stored block closes (`status.go:322`), where
`snap.Stored` may be nil — `BindWarning` handles that:

```go
	env.BindWarning = controller.BindWarning(ws.RepoRoot, snap.Stored)
```

In `writeStatusHuman`, print both after the recorded-session block
(`status.go:369`):

```go
	if env.Stored != nil && env.Stored.Bind != nil {
		fmt.Fprintf(tw, "bind\t%s\n", *env.Stored.Bind)
	}
	if env.BindWarning != "" {
		fmt.Fprintf(tw, "bind warning\t%s\n", env.BindWarning)
	}
```

- [ ] **Step 19: Run the test to verify it passes**

Run: `go test ./internal/cli/ ./internal/controller/ -run 'TestStatus|TestEnsure' -v`

Expected: PASS — `TestStatusReportsTheBindAndAnUnusableBind` and
`TestStatusIsQuietWhenNoBindIsRecorded` ok, and every pre-existing `TestStatus*`
and `TestEnsure*` still ok.

- [ ] **Step 20: Commit**

```bash
git add internal/controller/ensure.go internal/cli/status.go internal/cli/status_test.go
git commit -m "feat(cli): report the bind and an unusable bind from status

controller.BindWarning exports the check Ensure already runs, so status
and open cannot drift on what counts as unusable or on how it reads."
```

- [ ] **Step 21: Write the failing open test**

Append to `internal/cli/open_test.go`, and make sure its import block contains
`"path/filepath"` (Task 8's `--cwd` tests may already have added it):

```go
// open reports the bind the session was actually planned from, which
// only the controller knows: it is read inside the lock, after any
// --cwd write and after the observation (spec §5).
func TestOpenReportsTheEffectiveBind(t *testing.T) {
	ws := openWorkspace(t)
	if err := os.MkdirAll(filepath.Join(ws.RepoRoot, "services", "api"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	bind := "services/api"
	if err := s.SetBind(ws.ID, &bind, cliTestTime); err != nil {
		t.Fatalf("set bind: %v", err)
	}
	installOpenStore(t, s)
	installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeOpen(t, stdout)
	if env.Bind == nil || *env.Bind != bind {
		t.Errorf("bind = %v, want %q", env.Bind, bind)
	}
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d", env.SchemaVersion)
	}
}

func TestOpenEmitsNoBindWhenUnbound(t *testing.T) {
	ws := openWorkspace(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if strings.Contains(stdout, `"bind"`) {
		t.Errorf("an unbound open emits a bind key:\n%s", stdout)
	}
}
```

- [ ] **Step 22: Run the test to verify it fails**

Run: `go test ./internal/cli/ -run 'TestOpen.*Bind|TestOpenEmitsNoBind'`

Expected: FAIL — the package does not compile:
`env.Bind undefined (type openEnvelope has no field or method Bind)`.

- [ ] **Step 23: Carry the bind out of Ensure and into the envelope**

In `internal/controller/ensure.go`, add the field to `EnsureResult` beside
`BindWarning`:

```go
	// Bind is the bind the session was planned from, repository-relative,
	// nil when the session opens at the repository root. It is reported
	// even when BindWarning is set: a caller correcting a broken bind
	// needs to see the value that is broken.
	Bind *string
```

Capture it at the same call site Task 7 added, from the record the observation
returned — that is the value after any `--cwd` write, inside the lock:

```go
	base, bindWarning := resolveBindBase(d.Workspace.RepoRoot, snap.Stored)
	var effectiveBind *string
	if rec := snap.Stored; rec != nil && rec.Bind != nil && *rec.Bind != "" {
		bind := *rec.Bind
		effectiveBind = &bind
	}
	windows, err := renderWindows(intents, d, base, containerObs, c.ContainerAct)
```

Then set it wherever Task 7 sets `BindWarning`: add `Bind: effectiveBind,` to
the `EnsureResult` literals on the `SessionActionNone` and `SessionActionAdopt`
paths, and on the create path beside it:

```go
	case SessionActionCreate:
		res, err := c.createSession(ctx, d, windows, containerObs)
		if err != nil {
			return EnsureResult{}, err
		}
		res.Container = containerObs
		res.BindWarning = bindWarning
		res.Bind = effectiveBind
		return res, nil
```

In `internal/cli/open.go`, add the field to `openEnvelope` (`open.go:34-43`),
last so no existing key moves:

```go
	Bind *string `json:"bind,omitempty"`
```

set it in the JSON literal (`open.go:81-93`), after `ContainerWindowsStale`:

```go
			ContainerWindowsStale: res.ContainerWindowsStale,
			Bind:                  res.Bind,
```

and report it in the human block, after the container line (`open.go:106`):

```go
	if res.Bind != nil {
		fmt.Fprintf(stdout, "bind %s\n", *res.Bind)
	}
```

- [ ] **Step 24: Run the test to verify it passes**

Run: `go test ./internal/cli/ ./internal/controller/ -run 'TestOpen|TestEnsure' -v`

Expected: PASS — `TestOpenReportsTheEffectiveBind` and
`TestOpenEmitsNoBindWhenUnbound` ok, and every pre-existing `TestOpen*` and
`TestEnsure*` still ok.

- [ ] **Step 25: Commit**

```bash
git add internal/controller/ensure.go internal/cli/open.go internal/cli/open_test.go
git commit -m "feat(cli): report the effective bind from open

EnsureResult carries it because only the controller, inside the lock and
after the observation, knows which bind the windows were planned from."
```

- [ ] **Step 26: Run the full verification**

```bash
gofmt -l internal/ cmd/
go vet ./...
go test -race ./...
```

Expected: `gofmt -l` prints nothing, `go vet` is silent, and every package
passes. In particular `internal/cli`'s seven `Test*EnvelopeIsSchemaV2` tests
(`schema_version_test.go:101-218`) still pass unchanged: nothing in this task
renames, removes, or retypes a key any of them asserts.

- [ ] **Step 27: Commit any formatting fallout**

```bash
git status --porcelain
git diff --stat
```

If the working tree is clean, there is nothing to commit and the task is done.
If `gofmt` changed anything above, commit it:

```bash
git add -A
git commit -m "style: gofmt"
```
### Task 12: Decision record for the bound directory and the session grammar

Decision 0001 exists because the design doc and plan behind #31 were removed, leaving the *why* nowhere. The same is about to happen here. Two choices in this slice are departures from the design of record and will read as arbitrary to anyone who finds them in the code later: the bind is the session's base directory rather than the directory the session opens in, and the session grammar is stricter than tmux's own. Both are cheap to reverse by accident and expensive to rediscover, so they get a record.

There is exactly one existing decision record, `0001-repository-scoped-workspaces.md`, so the next number is 0002 and the template is whatever 0001 does: an `# N. Title` heading, a `**Status:** Accepted — implemented in #NN` line, a short paragraph saying what the record is for and where the behavior itself is documented, then `## Context`, `## Decision`, `## Consequences worth knowing` (paragraphs with bolded lead-in sentences, not bullets), and `## Deferred`.

**Files:**
- Create: `docs/decisions/0002-session-targets-and-the-bound-directory.md`
- Test: none. Step 4 is the verification gate.

**Interfaces:**
- Consumes: the behavior built in tasks 1-11, read from the code rather than from the docs — the session grammar in `internal/target/target.go`, and the base-directory composition `controller.Ensure` performs via `bindpath.Resolve`. `docs/commands.md` is *not* an input here: it still describes a one-session-per-repository world until Task 13 rewrites it.
- Produces: the fixed reasoning behind two decisions, and the link target `docs/decisions/0002-session-targets-and-the-bound-directory.md` that Task 13 Step 15 writes into decision 0001's `## Deferred` section. Task 13's prose must agree with this record's `## Decision` section; where they differ, this record is the one that was checked against the code.

- [ ] **Step 1: Write the decision record**

Create `docs/decisions/0002-session-targets-and-the-bound-directory.md` with exactly this content:

```markdown
# 2. Session targets and the bound directory

**Status:** Accepted — implemented in #37 and #38.

Records the two places where session targets and `bind` depart from the design
of record, now that the spec and plan they came from have been removed. The
behavior itself is documented in `docs/commands.md` and `docs/worktrees.md`;
this is only the *why*, for the decisions a reader would otherwise have to
reconstruct from the code. It closes the "Deferred" section of
[decision 0001](0001-repository-scoped-workspaces.md).

## Context

Decision 0001 made the repository the unit and deferred two things: the
`<repo>/<session>` target form (#37) and a `bind` command giving a session its
own working directory (#38).

They shipped together because neither is worth much alone. #37 on its own
gives a repository two sessions that open the same directory and load the same
configuration — the same workspace twice under two names. The directory is
most of what a second session is *for*. So the two arrived as one slice, and
as one slice they raised two questions the design of record either answered
differently or did not answer at all.

## Decision

**A bind is the session's base directory.** Issue #38 words it as "the
directory the session opens in", which puts it in the same slot as a window's
`cwd` and leaves the two settings fighting over that slot. Instead the bind
prefixes whichever `cwd` wins: a session bound to `services/api` with a window
`cwd: cmd` opens `services/api/cmd`.

**The session component of a target is stricter than tmux's own naming
rules.** It must match `^[A-Za-z0-9][A-Za-z0-9_-]*$` and be at most 64
characters, which rejects names tmux itself would accept.

## Consequences worth knowing

**The base is computed once rather than prefixed at five sites.** A relative
directory becomes a real path in exactly one file, `controller/ensure.go`, but
at five places within it: the container pane command, the container window
command, the host window directory, the host pane directory, and the host-side
`-c` for container windows and panes. All five need the base, so it is derived
once — the repository root joined with the bind — and threaded through. Joining
at each site would work today and leave a sixth site free to forget it.

**A pane's directory replaces the window's rather than nesting inside it.**
That is pre-existing behavior, and it means the bind has to prefix whichever of
the two wins, not the window's alone. It is written down because "prefix the
window directory" is the natural reading and the wrong one.

**Containers needed no adapter change.** The container path already joins the
binding's workdir with the relative directory, so the same prefix that produces
`services/api/cmd` on the host produces `/workspaces/<repo>/services/api/cmd`
inside the container, with nothing added to the adapter.

**Containment is re-checked at every use, not only at bind time.** A bind is
validated when it is set — relative to the repository root, existing, inside
the repository once symlinks are resolved — but a path that passed can later be
replaced by a symlink pointing out of the repository, after which window
creation would follow it out. Every read re-canonicalizes and re-verifies, and
a bind that no longer resolves inside the repository is treated as missing.
`open` then falls back to the repository root and says so, because failing to
open a session over a stale directory is a worse outcome than opening it one
level up.

**A relative bind argument is read against the current directory, not against
the repository root.** It is *stored* relative to the repository root, which is
what makes the row portable, and that asymmetry is deliberate rather than an
oversight. `bind services/api` typed from inside `services/` would mean
`services/services/api` under a root-relative rule — the shell just completed
that path against the current directory, so reading it any other way makes tab
completion produce a wrong answer. The cost is the reverse case: the same
command typed from `services/` when `services/api` exists only at the root now
resolves to a path that is not there. That is why the "outside the repository"
error carries a `did you mean` line naming the root-relative interpretation
whenever it would have resolved. An error that names the path the user probably
meant is cheaper than a rule that silently picks the wrong one of two readings.

**A restrictive grammar converts a confusing error into an accurate one.**
`projectmux <target>` with no command is shorthand for `open`, so a mistyped
path — a tab-completed filename, `docs/commands.md` — reaches target parsing.
Under tmux's own rules it would parse, resolve as a repository name, and be
reported as an *unknown workspace*, exit 4, sending the reader to look for a
repository they never meant to name. Under this grammar it is a malformed
target, exit 2, and the message names the grammar. The cost is that a session
cannot be called `my.session` or `_wip`. That was judged cheaper than the
misdirected error, and the grammar can be widened later without invalidating
any name it accepts today.

**`/` is the separator because a git repository directory cannot contain
one.** No name that was a valid target before this change became ambiguous
after it.

**A fourth identity key was required, and neither issue named it.** Sessions
carried the workspace ID, the slug, and the repository root; none of them
recorded the session component, which is harmless while a repository has one
session and a correctness bug the moment it has two. `rebuild` re-derives a
live session's identity before comparing it, and with no session component
recorded a named session re-derives to the *default* workspace ID and is
reported as an identity conflict. A fourth key records it. An absent key reads
as the empty string, which is exactly what a `v0.5.0` default session is — so
no stored ID changed and nobody is forced to rebuild.

**`OutputSchemaVersion` stays 2.** `bind` is an added field on `list`,
`status`, and `open`, and `BIND` is an added column. Nothing was renamed,
retyped, or removed, so a consumer written against version 2 keeps working —
unlike the version 1 break decision 0001 records.

## Deferred

**Per-session configuration.** Configuration is still loaded by slug, so every
session on a repository reads the same `workspaces/<slug>.yaml`. Two sessions
bound to different directories run the same windows, each rooted at its own
base, which is the useful case; a session that needs *different* windows has
no way to say so yet.

**A `doctor` check for dangling binds.** `open` already reports an unusable
bind and falls back to the repository root, so a stale bind is visible at the
moment it matters rather than only in a diagnosis.
```

- [ ] **Step 2: Correct the issue numbers in the status line**

The status line above reads `**Status:** Accepted — implemented in #37 and #38.`, following decision 0001's form (`Accepted — implemented in #31, merged 2026-08-08.`). Confirm against the branch which PR actually carries this work and rewrite that one line to match — a single PR closing both issues becomes `Accepted — implemented in #NN, merged YYYY-MM-DD.`:

```bash
git log --oneline main..HEAD | tail -5
gh pr view --json number,title,mergedAt 2>/dev/null || echo "no PR yet; use the issue numbers"
```

If the PR is not open yet, leave `#37 and #38` — they are the real issue numbers from the spec and are accurate either way.

- [ ] **Step 3: Check the record against decision 0001's shape**

Read `docs/decisions/0001-repository-scoped-workspaces.md` and the new file side by side and confirm four things: the heading is `# N. Title` with the number matching the filename; the status line uses the same `**Status:** Accepted — …` vocabulary; the section headings are `## Context`, `## Decision`, `## Consequences worth knowing`, `## Deferred` in that order; and the consequences are paragraphs with bolded lead-in sentences rather than a bullet list.

```bash
grep -n '^#' docs/decisions/0001-repository-scoped-workspaces.md
grep -n '^#' docs/decisions/0002-session-targets-and-the-bound-directory.md
```

Expected: the same sequence of heading levels and section names in both files, with `0002` adding no section `0001` does not have.

- [ ] **Step 4: Verify the record matches the shipped behavior and reads on its own**

This task now runs *before* the user-facing documentation, so the record is
written against the code, not against `docs/commands.md` — which still says a
repository holds exactly one session until Task 13 rewrites it. Two concrete
checks:

1. **The record does not describe something else.** Each claim is checked
   against the implementation, which is the ground truth here. Confirm the
   grammar the record states is the one the parser enforces, and that the
   composition rule it states is the one `controller.Ensure` performs:

```bash
grep -n 'A-Za-z0-9' internal/target/target.go docs/decisions/0002-session-targets-and-the-bound-directory.md
grep -rn 'bindpath.Resolve' internal/controller/
```

Expected: the same character class and 64-character limit in both the parser
and the record, and the base-directory composition happening in
`controller.Ensure` before either actuator sees a path — the record's claim, in
code.

   The record also claims a relative bind argument is read against the current
   directory while the *stored* form is root-relative. That asymmetry is the one
   claim in the record a reader is most likely to assume is a typo, so check
   both halves against `internal/bindpath`:

```bash
grep -n 'filepath.Abs\|filepath.Rel' internal/bindpath/bindpath.go
grep -n 'Suggestion' internal/bindpath/bindpath.go
```

Expected: `filepath.Abs` resolving the argument (cwd-relative, per Task 4) and
`filepath.Rel` against the repository root producing the stored form, plus the
`Suggestion` field the record's `did you mean` sentence describes. If
`Suggestion` is absent, Task 4 shipped without the smart error and the record
overstates what the command does — fix the code, not the record.

2. **The reciprocal link resolves.** 0002 links back to 0001; the forward link
   from 0001 to 0002 is Task 13's Step 15 and does not exist yet.

```bash
grep -n '0001-' docs/decisions/0002-session-targets-and-the-bound-directory.md
ls docs/decisions/
```

Expected: `0001-repository-scoped-workspaces.md` present in both. Do **not**
grep 0001 for a `0002-` link at this point: it is legitimately absent until
Task 13, and Task 13's Step 16 is where that direction is gated.

- [ ] **Step 5: Commit**

```bash
git add docs/decisions/0002-session-targets-and-the-bound-directory.md
git commit -m "docs: record the bind-as-base-directory and session-grammar decisions"
```


---

### Task 13: User-facing documentation for targets, binds, and per-session worktrees

Nothing in `docs/` knows that a repository can hold more than one session. `docs/commands.md` calls the argument `<workspace>`, states that every tree of a project shares "one workspace, one session, and one container", and documents no `bind`. `docs/worktrees.md` tells the reader that opening a worktree as its own session is "Not supported". Decision 0001 still lists the `<repo>/<session>` form and per-session working directories as deferred. This task brings all three in line with what tasks 1-11 built. It is documentation only — no Go file is touched.

The transcripts in `docs/commands.md` are held to a stated contract, stated in its own first paragraph: "Every command below was run against a real installation to produce the output shown." The blocks written below are the expected form. The verification step at the end of this task requires actually running each new command against a scratch installation (`PROJECTMUX_CONFIG_ROOT`, `PROJECTMUX_STATE_ROOT`, and `PROJECTMUX_TMUX_SOCKET` pointed at a temporary directory, as the environment table in that file describes) and replacing any block whose real output differs — including column widths, which are computed from the data.

**Files:**
- Modify: `docs/commands.md` — table of contents (lines 7-15), `## Conventions` (lines 17-36), `## Exit codes` table (lines 64-74), the synopsis lines of `## projectmux config` (line 91), `## projectmux list` (line 173), `## projectmux status` (line 199), `## projectmux attach` (line 324), `## projectmux stop` (line 345), the `## projectmux list` transcript and prose (lines 178-194), the `## projectmux status` transcript (lines 206-218), all of `## projectmux open` (lines 287-320), the `## projectmux stop` sibling example (lines 359-370), and a new `## projectmux bind` section inserted between `## projectmux stop` and `## projectmux autostart`
- Modify: `docs/worktrees.md` — the opening paragraph (lines 3-5), `## The one thing to internalize` (lines 20-26), a new `## A session per worktree` section inserted after `## Getting a window into a worktree`, `## Stopping` (lines 89-102), and the `## Quick reference` table (lines 121-128)
- Modify: `docs/decisions/0001-repository-scoped-workspaces.md` — the `## Deferred` section (lines 80-87)
- Test: none. Documentation has no unit tests; Step 16 is the verification gate.

**Interfaces:**
- Consumes: the user-visible surface built in tasks 1-11 and documented verbatim here — `projectmux bind [--clear] [--json] [--compact] <target> [<path>]`; `projectmux open [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]`; the target grammar `<repo>` or `<repo>/<session>` with the session component matching `^[A-Za-z0-9][A-Za-z0-9_-]*$` and at most 64 characters; the tmux session names `<slug>` and `<slug>--<session>`; the `BIND` column on `list`; the `bind` field in the JSON envelopes of `list`, `status`, and `open`; `OutputSchemaVersion` still 2; and exit codes 2 (malformed target or invalid bind path), 3 (ambiguous), 4 (unknown repository).
- Produces: the user-facing prose that decision 0002 (Task 12, already written) points at. Task 12's `## Decision` section was checked against the code, so where this task's prose and that record disagree, the record wins and the prose here is what changes.

- [ ] **Step 1: Update the table of contents in `docs/commands.md`**

Replace lines 11-12, currently:

```markdown
- Lifecycle — [`open`](#projectmux-open), [`attach`](#projectmux-attach),
  [`stop`](#projectmux-stop)
```

with:

```markdown
- Lifecycle — [`open`](#projectmux-open), [`attach`](#projectmux-attach),
  [`stop`](#projectmux-stop), [`bind`](#projectmux-bind)
```

- [ ] **Step 2: Rewrite the naming convention in `docs/commands.md`**

Replace the whole block from `**Naming a workspace.**` through `still exit 2.` — currently lines 19-36:

```markdown
**Naming a workspace.** Commands that accept `<workspace>` resolve it two
ways. With no argument, the workspace is the one containing the current
directory. With an argument, it is looked up by name under the
`repository_roots` configured in `defaults.yaml`.

Either way the answer is a *repository*, never one of its trees: a linked
worktree — including the conventional `.worktrees/` and `.claude/worktrees/`
directories, and any tree `git worktree add` placed elsewhere on the disk —
is a separate working tree attached to the same repository, so working in one
resolves to the repository it belongs to, and it cannot be named on its own.
Every tree of a project therefore shares one workspace, one session, and one
container.

`projectmux <workspace>` with no command is shorthand for
`projectmux open <workspace>`. A mistyped *bare* command therefore resolves as
a workspace name and exits 4 when no worktree matches it, not 2 — a documented
trade for the shorthand. Flag-shaped tokens and bad arguments to real commands
still exit 2.
```

with:

```markdown
**Naming a target.** Commands that accept `<target>` take either `<repo>` or
`<repo>/<session>`. `<repo>` names the repository; `<session>` names one of
the sessions on it. `/` is the separator because it cannot appear in a git
repository directory name, so no bare repository name became ambiguous when
the second form was added.

Every repository has a default session, which is what a bare `<repo>` names.
Further sessions come into existence by being named —
`projectmux bind slabledger/feature-a .worktrees/feature-a`, or
`projectmux open slabledger/feature-a` — and they sit on the same repository,
share its container, and read the same `workspaces/<slug>.yaml`. The tmux
session is `<slug>` for the default session and `<slug>--<session>` for a
named one.

The session component must match `^[A-Za-z0-9][A-Za-z0-9_-]*$` and be at most
64 characters. That is deliberately stricter than tmux's own rules: it is what
makes a mistyped path fail as a malformed target instead of being looked up as
a repository name. `repo/` (no session), `/session` (no repository), `a/b/c`
(more than one separator), a session component starting with `-` or `_`, and
anything longer than 64 characters are all usage errors, and each message
names the grammar.

**Resolving a target.** With no argument, the repository is the one containing
the current directory, and the session is the one whose bound directory
contains that directory — the longest match wins, and the default session is
used when none matches. With an argument, the repository is looked up by name
under the `repository_roots` configured in `defaults.yaml`, and a bare
`<repo>` always means the default session: an explicit target is exact, and
the current directory gets no vote. That is also how you address the default
session while standing inside another session's bound directory.

Either way the answer is a *repository*, never one of its trees: a linked
worktree — including the conventional `.worktrees/` and `.claude/worktrees/`
directories, and any tree `git worktree add` placed elsewhere on the disk —
is a separate working tree attached to the same repository, so working in one
resolves to the repository it belongs to, and it cannot be named on its own.
Every tree of a project therefore shares one workspace and one container. A
session can still be pointed *at* a tree — see [`bind`](#projectmux-bind).

`projectmux <target>` with no command is shorthand for
`projectmux open <target>`. A mistyped *bare* command that the grammar accepts
therefore resolves as a repository name and exits 4 when no repository matches
it, not 2 — a documented trade for the shorthand. Anything the grammar
rejects, flag-shaped tokens, and bad arguments to real commands still exit 2.
```

- [ ] **Step 3: Widen the exit-code table in `docs/commands.md`**

Two rows of the table at lines 66-74 describe only the repository lookup. Replace these two lines:

```markdown
| 3 | the workspace name matched more than one repository |
| 4 | the workspace name matched no repository |
```

with:

```markdown
| 3 | the target matched more than one repository, or the current directory matched more than one session's bind |
| 4 | the target matched no repository |
```

No code is added: exit 2 already covers a malformed target and an invalid bind path, and no new exit code exists.

- [ ] **Step 4: Rename `<workspace>` to `<target>` in the remaining synopses**

Five synopsis blocks still say `<workspace>`. Change each one line, leaving the surrounding prose alone:

`## projectmux config`, line 91:

```text
projectmux config [--validate] [--json] [--compact] [<target>]
```

`## projectmux status`, line 199:

```text
projectmux status [--json] [--compact] [<target>]
```

`## projectmux attach`, line 324:

```text
projectmux attach [--json] [--compact] [<target>]
```

`## projectmux stop`, line 345:

```text
projectmux stop [--container] [--force] [--json] [--compact] [<target>]
```

`## projectmux list` (line 173) takes no argument and is unchanged. Leave the
`config --validate` prose alone: its argument names a workspace *file*, not a
target, and that distinction is the point of the mode.

- [ ] **Step 5: Document `--cwd` under `## projectmux open`**

Replace the synopsis at line 290:

```text
projectmux open [--no-attach] [--json] [--compact] [<workspace>]
```

with:

```text
projectmux open [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]
```

Then insert the following after the `--no-attach` paragraph ("`--no-attach` does everything except hand over the terminal, which is what scripts and the systemd unit want. Without it, `open` attaches on success.") and before the paragraph beginning "Open takes a per-workspace lock":

```markdown
`--cwd <path>` sets the session's bound directory as part of the same
operation. It is not [`bind`](#projectmux-bind) followed by `open`: the bind
is persisted inside the one critical section this command already holds, before
the observation the windows are planned from, so the windows this call creates
are the first ones built from the new bind and no second command can slip into
the gap. The path is relative to the repository root and must exist inside it.

```text
$ projectmux open --no-attach --cwd .worktrees/feature-a slabledger/feature-a
session slabledger--feature-a (created)
bind .worktrees/feature-a
```

The bind survives an open that fails afterwards. It is a declaration about the
session rather than a side effect of a successful open, so fixing whatever
failed and running `open` again keeps it.

If the bound directory is unusable when the session is opened — deleted,
pruned along with its worktree, or no longer resolving to a path inside the
repository — `open` falls back to the repository root and says so rather than
failing:

```text
$ projectmux open --no-attach slabledger/feature-a
session slabledger--feature-a (created)
bind .worktrees/feature-a unusable: no such directory; using the repository root
```
```

- [ ] **Step 6: Add the `## projectmux bind` section**

Insert this complete section into `docs/commands.md` between the end of `## projectmux stop` (after the paragraph "A partial failure — the session ended but the container did not — reports what succeeded and what did not on stdout, with a one-line summary on stderr and a non-zero exit.") and `## projectmux autostart`:

```markdown
## projectmux bind

```text
projectmux bind [--clear] [--json] [--compact] <target> [<path>]
```

Points a session at a directory inside its repository, creating the session if
it does not exist yet.

```text
$ projectmux bind slabledger/feature-a .worktrees/feature-a
bound slabledger/feature-a to .worktrees/feature-a
```

The bind is the session's **base directory**, not a one-off starting
directory. Every window's and pane's `cwd` composes on top of it: a session
bound to `services/api` with a window `cwd: cmd` opens `services/api/cmd`.
The same composition happens under the container mount, giving
`/workspaces/slabledger/services/api/cmd`, so a bind works the same for
`location: container` windows as for host ones.

The path is interpreted relative to the repository root, and it is stored
relative so that moving the repository does not invalidate it. It must exist
when you bind it, and it must lie inside the repository once symlinks are
resolved. Anything else is a usage error:

```text
$ projectmux bind slabledger/feature-a ../elsewhere
projectmux: bind: path must be inside the repository, got "../elsewhere"
```

Containment is re-checked at every *use*, not only when the bind is set: a
path that was inside the repository can later be replaced by a symlink
pointing out of it, and following that would put windows outside the
repository. A bind that no longer resolves inside the repository is treated as
missing rather than followed, which is the `unusable` fallback
[`open`](#projectmux-open) reports.

With no `<path>`, `bind` reports what the session is currently bound to.
`--clear` removes the bind and keeps the session:

```text
$ projectmux bind --clear slabledger/feature-a
cleared the bind on slabledger/feature-a
```

Binding is the natural way to declare a session before opening it. A session
that is bound but has never been opened is recorded with no applied
configuration, so the first `open` on it converges like any other drift.

Standalone `bind` takes the session's lock and not the repository's — it has
no container work to do — so binding one session never queues behind a
sibling's container starting. Use [`open --cwd`](#projectmux-open) when you
want the bind and the windows built from it in one operation.

Configuration is keyed on the repository, not the session: every session on a
repository reads the same `workspaces/<slug>.yaml`. Two sessions bound to
different directories therefore run the same windows, each rooted at its own
base.
```

- [ ] **Step 7: Add the `BIND` column to `## projectmux list`**

Replace the transcript at lines 178-182:

```markdown
```text
$ projectmux list
WORKSPACE   SESSION     TMUX  CONTAINER  NOTES
slabledger  slabledger  live  -          -
```
```

with a two-session example plus a paragraph explaining the two changed columns:

```markdown
```text
$ projectmux list
WORKSPACE             SESSION                TMUX  CONTAINER  BIND                  NOTES
slabledger            slabledger             live  -          -                     -
slabledger/feature-a  slabledger--feature-a  live  -          .worktrees/feature-a  -
```

`WORKSPACE` is the target you would type: a bare slug for a repository's
default session, `slug/session` for a named one. `SESSION` is the tmux session
name, which is `<slug>--<session>` for a named session. `BIND` is the
session's base directory relative to the repository root, or `-` when the
session opens at the root.
```

Then replace the closing paragraph of the section (lines 192-194):

```markdown
`TMUX` is `live`, `missing`, or `unknown`. `unknown` means the tmux server
could not be observed — it is not a synonym for absent, and the distinction is
deliberate throughout ProjectMux.
```

with:

```markdown
`TMUX` is `live`, `missing`, or `unknown`. `unknown` means the tmux server
could not be observed — it is not a synonym for absent, and the distinction is
deliberate throughout ProjectMux.

`--json` gains a `bind` field per row alongside the existing `session` field.
`schema_version` stays **2**: every change here is an added field or an added
column, and nothing was renamed, retyped, or removed.
```

- [ ] **Step 8: Report the bind in `## projectmux status`**

Replace the first transcript's identity block — the lines from `recorded session  slabledger` through `updated ...` inside the block at lines 206-218 — so the block reads:

```text
$ projectmux status
workspace         slabledger/feature-a
repository        /home/you/src/slabledger
id                d7142c2621eba1b47024261c980871d9e70d982e0e9fab5e0924100dcc300493
recorded session  slabledger--feature-a
bind              .worktrees/feature-a
registered        2026-08-06T05:53:54.037942782Z
updated           2026-08-06T05:53:54.181608075Z
tmux session      live (slabledger--feature-a, identity match)
container         none
config            in sync (desired sha256:40dd44f7…, applied sha256:40dd44f7…)
last operation    open ok at 2026-08-06T05:53:54.181608075Z
plan              session=none container=none reapply=false record-name=false
```

The `id` line above is the real ID of the *default* session and will differ for
a named one; Step 16 replaces this block with genuine output. Then insert this
paragraph directly after the existing sentence "`plan` is what `open` *would*
do right now. `session=none` means the session already matches the desired
state.":

```markdown
`bind` is the session's base directory relative to the repository root, and is
absent for a session that opens at the root. It is reported as recorded — a
bind that no longer resolves inside the repository still shows here, and it is
[`open`](#projectmux-open) that reports it as unusable and falls back.
```

- [ ] **Step 9: Confirm the `stop --container` example names a real sibling**

Read lines 359-370 of `docs/commands.md`. The example already reads:

```text
$ projectmux stop --container
projectmux: the container is shared with live session(s) slabledger--feature-a; refusing to stop it (use --force)
```

That is already a real sibling session name in the `<slug>--<session>` form, so no substitution is needed — confirm it and change nothing in the block. The situation it describes was unreachable before this slice, so add one sentence to the paragraph introducing it. Replace:

```markdown
A container belongs to a repository and is shared by every session on it, so
`stop --container` refuses with exit 6 when another session on the same
repository is live, and names them:
```

with:

```markdown
A container belongs to a repository and is shared by every session on it, so
`stop --container` refuses with exit 6 when another session on the same
repository is live, and names them. A repository gets siblings by having named
sessions — see [target conventions](#conventions):
```

- [ ] **Step 10: Commit `docs/commands.md`**

```bash
git add docs/commands.md
git commit -m "docs: document session targets, bind, and the BIND column"
```

- [ ] **Step 11: Correct the framing at the top of `docs/worktrees.md`**

Replace lines 3-5:

```markdown
ProjectMux does not create or manage worktrees — that is an explicit non-goal.
It opens repositories, and a repository is the unit: every tree of a project,
main or linked, shares one workspace, one tmux session, and one Dev Container.
```

with:

```markdown
ProjectMux does not create or manage worktrees — that is an explicit non-goal.
It opens repositories, and a repository is the unit: every tree of a project,
main or linked, shares one workspace and one Dev Container. What a repository
*can* have more than one of is sessions, and a session can be bound to a
directory inside the repository — which is how a worktree gets a session of
its own without getting a container of its own.
```

Then replace the closing sentence of `## The one thing to internalize` (lines 20-22):

```markdown
That session's windows start at `~/workspace/my-project` — the main working
tree. The worktree you typed the command in is not where you land, and it is
not a workspace of its own. `projectmux list` will never show it, and
`projectmux feature-a` exits 4: a linked worktree cannot be named.
```

with:

```markdown
That session's windows start at `~/workspace/my-project` — the main working
tree. The worktree you typed the command in is not where you land, and it is
not a workspace of its own. `projectmux list` will never show it as a
workspace, and `projectmux feature-a` exits 4: a linked worktree cannot be
named. What you can do instead is bind a *session* to it — see
[A session per worktree](#a-session-per-worktree) — after which `open` from
inside the worktree lands there.
```

- [ ] **Step 12: Add `## A session per worktree` to `docs/worktrees.md`**

Insert this section after `## Getting a window into a worktree` (that is, after the paragraph ending "Use `workspaces/<slug>.local.yaml` if you want the entry without committing it.") and before `## Stopping`:

```markdown
## A session per worktree

A repository has one workspace and one container, but it may have several
sessions, and each session can be bound to a directory inside the repository.
That is the supported way to give a worktree a session of its own:

```sh
cd ~/workspace/my-project
git worktree add .worktrees/feature-a -b feature-a
projectmux open --cwd .worktrees/feature-a my-project/feature-a
```

The tmux session is `my-project--feature-a`, and its windows start in
`.worktrees/feature-a` rather than at the repository root. The configuration
is still `workspaces/my-project.yaml` — sessions share it — so both sessions
run the same windows, each rooted at its own base directory. A window's `cwd`
composes *on top of* the bind rather than replacing it: `cwd: cmd` in a
session bound to `.worktrees/feature-a` opens `.worktrees/feature-a/cmd`.

Both sessions still share the repository's container, and the bind composes
inside it too, so a `location: container` window in the bound session opens
`/workspaces/my-project/.worktrees/feature-a` — which works only if the
worktree is under the repository root, the same rule as above.

From then on, `projectmux open` run from inside `.worktrees/feature-a` picks
that session, because the bind contains the current directory. Bind two
sessions to nested directories and the longest match wins; bind two to the
same directory and `open` exits 3 rather than guessing. `projectmux open
my-project` still opens the *default* session at the repository root even when
run from inside the worktree — an explicit target is exact.

To point a session that already exists at a worktree, or to move it, use
`bind` on its own:

```sh
projectmux bind my-project/feature-a .worktrees/feature-b
```

Removing the worktree does not remove the session. Until you run
`projectmux bind --clear my-project/feature-a`, `open` reports the bind as
unusable and falls back to the repository root rather than failing.
```

- [ ] **Step 13: Update `## Stopping` and the quick reference in `docs/worktrees.md`**

Replace the first paragraph of `## Stopping` (lines 91-93):

```markdown
`stop` ends the session. `stop --container` also ends the container — which
every tree of the repository shares, so it refuses with exit 6 if another
session on that repository is live, and names it. `--force` overrides.
```

with:

```markdown
`stop` ends one session. `stop --container` also ends the container — which
every session on the repository shares, so it refuses with exit 6 if a sibling
session is live, and names it. `--force` overrides. A repository with a
worktree session therefore has a sibling to trip over, which the default
session on its own never did.
```

Then replace the trailing sentence of that section (lines 101-102):

```markdown
Nothing recorded in ProjectMux referred to that tree, so there is nothing to
clean up.
```

with:

```markdown
Nothing recorded in ProjectMux refers to that tree — unless a session was
bound to it, in which case clear the bind or stop the session; an unusable
bind is reported and falls back to the repository root, never followed.
```

Finally replace these two rows of the `## Quick reference` table:

```markdown
| To open a worktree as its own session | Not supported; the repository is the unit |
```

```markdown
| To remove a worktree | `git worktree remove` — ProjectMux holds nothing to clean up |
```

with, respectively:

```markdown
| To give a worktree its own session | `projectmux open --cwd .worktrees/<name> <repo>/<name>` |
| To point an existing session at a worktree | `projectmux bind <repo>/<name> .worktrees/<name>` |
| To go back to the repository root | `projectmux bind --clear <repo>/<name>` |
```

```markdown
| To remove a worktree | `git worktree remove` — then clear the bind of any session pointing at it |
```

- [ ] **Step 14: Commit `docs/worktrees.md`**

```bash
git add docs/worktrees.md
git commit -m "docs: show one session per worktree in the worktree guide"
```

- [ ] **Step 15: Close out the Deferred section of decision 0001**

Replace the whole `## Deferred` section of `docs/decisions/0001-repository-scoped-workspaces.md` (lines 80-87):

```markdown
## Deferred

The design also specified a `<repo>/<session>` target form and a `bind` command
for per-session working directories. Neither shipped in #31. The identity was
built session-aware anyway — `sha256(repo_root + "\0" + session)` — so that
adding them later does not rewrite every stored ID a second time. Until then
`resolve.Resolve` hardcodes the empty session, and one repository has exactly
one session.
```

with:

```markdown
## Deferred

**Closed.** Both items shipped; see
[decision 0002](0002-session-targets-and-the-bound-directory.md).

The design also specified a `<repo>/<session>` target form and a `bind` command
for per-session working directories. Neither shipped in #31. The identity was
built session-aware anyway — `sha256(repo_root + "\0" + session)` — so that
adding them later would not rewrite every stored ID a second time. That is what
happened: `resolve.Resolve` now takes the session component instead of
hardcoding the empty one, a repository may hold several sessions, and each can
be bound to a directory inside the repository. The empty session component is
still what a repository's default session uses, so no ID recorded by #31
changed and no existing session was invalidated.
```

The link target already exists: Task 12 created
`docs/decisions/0002-session-targets-and-the-bound-directory.md` before this
task ran, which is why Step 16's `ls` of that path is a gate this task can
actually satisfy. If it is missing, Task 12 has not run and this task is out of
order.

- [ ] **Step 16: Verify the documentation against the real binary**

Three concrete checks, all of which must pass before committing:

1. **Every transcript is real.** Build the branch and run each new or changed example against a scratch installation, then paste the genuine output over the block — column widths in the `list` table and the `id` in the `status` block are computed from real data and will not match what was written by hand:

```bash
go build -o /tmp/pmux ./cmd/projectmux
export PROJECTMUX_CONFIG_ROOT=/tmp/pmux-docs/config
export PROJECTMUX_STATE_ROOT=/tmp/pmux-docs/state
export PROJECTMUX_TMUX_SOCKET=pmux-docs
/tmp/pmux bind slabledger/feature-a .worktrees/feature-a
/tmp/pmux bind --clear slabledger/feature-a
/tmp/pmux bind slabledger/feature-a ../elsewhere; echo "exit=$?"
/tmp/pmux open --no-attach --cwd .worktrees/feature-a slabledger/feature-a
/tmp/pmux list
/tmp/pmux status slabledger/feature-a
```

2. **Every flag shown exists.** Compare each synopsis line in `docs/commands.md` against the flags the binary actually registers, and confirm the two lines below match the `bind` and `open` synopses character for character:

```bash
grep -n '^projectmux \(bind\|open\)' docs/commands.md
/tmp/pmux bind --help
/tmp/pmux open --help
```

Expected: `projectmux bind [--clear] [--json] [--compact] <target> [<path>]` and `projectmux open [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]`, with no flag in the docs that `--help` does not list and none in `--help` that the docs omit.

3. **Every anchor resolves.** The new prose adds four intra-document links —
   `#projectmux-bind`, `#projectmux-open`, `#conventions`, and
   `#a-session-per-worktree` — plus a cross-file link to decision 0002. Confirm each heading exists:

```bash
grep -n '^## projectmux bind' docs/commands.md
grep -n '^## projectmux open' docs/commands.md
grep -n '^## Conventions' docs/commands.md
grep -n '^## A session per worktree' docs/worktrees.md
ls docs/decisions/0002-session-targets-and-the-bound-directory.md
```

Then re-read the rendered `## Conventions`, `## projectmux bind`, and `## A session per worktree` sections end to end and confirm the grammar stated in prose (`^[A-Za-z0-9][A-Za-z0-9_-]*$`, 64 characters) is the grammar the parser implements, and that no sentence still claims a repository has exactly one session.

- [ ] **Step 17: Commit the verified transcripts and the decision update**

```bash
git add docs/commands.md docs/worktrees.md docs/decisions/0001-repository-scoped-workspaces.md
git commit -m "docs: close decision 0001's deferred items and verify transcripts"
```

