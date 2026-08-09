# Design: `<repo>/<session>` targets and per-session working directories

Date: 2026-08-09
Issues: #37 (the `<repo>/<session>` target form), #38 (`bind`)
Status: approved, ready for planning

## Why these two together

Issue #37 lets a repository hold more than one session. Issue #38 lets a session
open somewhere other than the repository root. Shipped alone, #37 gives a user
two sessions that open the same directory and load the same configuration — the
same workspace twice under two names. The value the second session carries is
almost entirely the directory it points at, so the two issues are one unit of
work and get one spec.

Decision 0001 already keys identity on the repository:
`RepositoryID = sha256(repo_root)` and `Workspace.ID = sha256(repo_root + "\0" +
session)`. The session component has been an input to that derivation since #31
precisely so this slice would not rewrite every stored ID a second time. The
state store already carries `session TEXT NOT NULL DEFAULT ''` with
`UNIQUE (repository_id, session)`. Storage and identity are ready; the command
surface, the tmux identity keys, and the resolution path are not.

## 1. Command surface

```
projectmux open  [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]
projectmux bind  [--clear] [--json] [--compact] <target> [<path>]
projectmux attach|stop|status|config [<target>]
```

`<target>` is `<repo>` or `<repo>/<session>`. `/` is the separator because it
cannot appear in a git repository directory name, so no existing bare workspace
name becomes ambiguous.

The session component must match `^[A-Za-z0-9][A-Za-z0-9_-]*$` and be at most 64
characters. This is deliberately stricter than tmux's own rules. The reason is
the bare-workspace shorthand in `cli.go`: `projectmux docs/commands.md` — a
mistyped path, a tab-completed filename — must fail as a malformed target
(exit 2, naming the grammar) rather than be resolved as a workspace name and
reported as an unknown workspace (exit 4). A restrictive grammar converts a
confusing error into an accurate one.

Rejected with exit 2, each naming the grammar: `repo/` (empty session),
`/session` (empty repository), `a/b/c` (more than one separator), a session
component with a leading `-` or `_`, and any session component over 64
characters.

The tmux session name stays `<slug>--<session>`, as the design of record
specifies. `state.AllocateSessionName` already suffixes `-2` on a UNIQUE
violation, so a collision with a real repository literally named
`myrepo--feature-a` is handled by machinery that exists.

## 2. Identity: the fourth key

Sessions carry three tmux user options today: `@dev_workspace_id`, `@dev_slug`,
`@dev_worktree`. None of them records the session component. That is a
correctness bug the moment named sessions exist, and neither issue names it.

`rebuild`'s `worktreeResolver.Resolve` (`internal/rebuild/apply.go:120-136`,
`325-365`) re-derives a live session's identity by calling
`resolve.Resolve("", nil, <worktree>)` *unconditionally*, before comparing
identity. With no session component recorded, a live `myrepo--feature-a`
re-resolves to the *default* workspace ID and `rebuild` reports a false
`identityConflict`.

`status`'s `staleSessions` (`internal/cli/status.go:219-232`) does **not** have
this problem, for two independent reasons. It calls `resolve.Resolve` only after
`SessionBelongsTo` has already rejected the session (`status.go:222`), so once
the fourth key matches, a healthy named session leaves through the first guard
and is never re-resolved. Independently, `snap.Session.ByName` holds only
sessions occupying a *candidate name* for the queried workspace
(`controller/types.go:41-55`), and `myrepo--feature-a` is never a candidate name
for the default `myrepo` — a sibling never enters the loop at all. This is
recorded because an earlier draft of this design asserted the opposite; no
status change is required and no test should be written against a bug that
cannot occur.

Add `@dev_session` as a fourth key, written at session creation, carried on
`LiveSession`, and compared in `SessionBelongsTo`
(`internal/controller/plan.go:114-116`). An absent key reads as `""`, which is
exactly what a v0.5.0 default session is — so no existing ID changes, no
existing session is invalidated, and no user is forced to rebuild.

A side effect worth recording: `stop --container`'s check for sibling sessions
sharing the container has never been reachable, because one repository could
only ever have one session. This slice makes it reachable for the first time,
so it needs a test rather than being assumed correct.

`staleRepositoryRoots` (`status.go:249-266`) also calls `resolve.Resolve` with an
empty name, but compares only `RepoRoot` and never an ID, so it is unaffected.
It is named here so it is not "fixed" alongside `rebuild`, which does need
changing.

## 3. The target seam

`resolve` owns every git invocation and "neither reads configuration files nor
mutates any resource." Choosing a session for a bare `open` requires reading the
state store for a bind, which would break that property if pushed into
`resolve`.

Instead:

- `resolve.Resolve(name, session string, roots []string, cwd string)` gains a
  session parameter and stays pure. The hardcoded `session := ""` at
  `resolve.go:100` goes away.
- `resolve.WithSession(ws Workspace, session string) Workspace` re-derives the
  IDs and names for a different session component, also pure. `rebuild` and
  `status` use it to reconstruct a live session's identity from `@dev_session`
  without a second git call.
- A new `internal/target` package sits between the CLI and `resolve`. It parses
  the argument, resolves the repository, and then chooses the session.

Selection keys on **target presence**, not on whether a target happened to carry
a session. `resolve.Resolve` already distinguishes the two — an empty name means
"resolve the cwd", a non-empty name means "look this up by name"
(`resolve.go:79-88`), and `docs/commands.md:19-28` documents that distinction as
user-facing behavior. Collapsing them would make `open myrepo`, run from inside
a bound directory, silently open a *named* session the user did not ask for.

So there are exactly three cases:

1. **`<repo>/<session>`** — use that session. Explicit and final.
2. **`<repo>`** — use the default session, `""`. An explicit target names the
   workspace exactly; the cwd does not get a vote. This is also how a user
   addresses the default session while standing inside a bound directory, which
   would otherwise be unreachable.
3. **No target** — resolve the repository from the cwd, then run the bind
   lookup: open the state store read-only, find sessions on this repository
   whose bind contains the cwd, and take the longest match. Fall back to the
   default session when nothing matches.

The bind lookup matches on **path components, not string prefixes**. A raw
string comparison would match a bind of `services/api` against a cwd of
`services/apixyz`. Compare component-wise after canonicalizing both sides, the
way `resolve.canonicalize` (`resolve.go:167-182`) already establishes stable
paths.

Two sessions bound to the same directory produce `resolve.AmbiguousError`
(exit 3), listing both.

The bind lookup uses `state.OpenReadOnly`, whose failures are typed and are
deliberately *not* interchangeable (`readonly.go:14-73`). Collapsing them all to
"fall back to the default session" would let a corrupt database silently open
the wrong workspace. The rule is:

- **Fall back to the default session**, silently: the database does not exist,
  and the schema is older than this build (`PendingMigrationError` — a
  diagnosis, and resolution is not the command that should act on it).
- **Propagate the error**: integrity-check failure, a permission failure, and an
  indeterminate WAL state. These are uncertainty about *which* session is
  correct, and guessing is the one thing resolution must not do.

The plan binds these two lists to the concrete types and sentinels in
`readonly.go`; naming them here rather than in the spec keeps the rule from
drifting as that file evolves.

## 4. `bind`

Migration 0003 adds a nullable `bind TEXT` to the workspaces table. It is
additive and does not require a rebuild.

A bind is interpreted relative to the repository root, must lie inside the
repository after `EvalSymlinks`, and must exist at bind time — otherwise exit 2.
Storing it relative rather than absolute keeps it stable if the repository
moves.

Containment is re-checked at **every use**, not only at bind time. A stored
in-repository path can later be replaced by a symlink pointing outside the
repository, after which host window creation would follow the escaped path
through `filepath.Join` (`ensure.go:278-286`). Every read of a bind
canonicalizes it and re-verifies containment; a bind that no longer resolves
inside the repository is treated as missing (see the fallback below) rather than
followed.

### Who writes the bind, and under which lock

`Ensure` is `lock → RegisterWorkspace → observe → plan → mutate → commit` under
one `defer release()` (`ensure.go:78-89`), with `lockPhases` taking the
repository lock then the workspace lock (`locking.go:10-31`). The bind fits that
structure rather than working around it:

- **`open --cwd <path>` does not bind and then open.** The bind is a field on
  `Desired`, and `Ensure` persists it in the same critical section that
  registers the workspace — before the observation the windows are planned from.
  One continuous, globally ordered operation: no window is ever built from a
  bind that changed underneath it, and there is no two-command race.
- **Standalone `bind` takes the workspace lock only.** It has no container
  phase, and `lockPhases` documents an empty `repositoryID` as exactly that, so
  a bind must not queue behind a sibling's `devcontainer up`.
- **A new record initializes with the bind and no applied digest.** A
  bound-but-never-opened session is already representable: `BuildPlan` treats
  `snap.Stored.AppliedDigest == nil` as "reapply" (`plan.go:71-73`), so the
  first `open` on a bound session converges normally with no special case.
- **A bind persists when the open that carried it fails.** It is a declaration
  about the session, not a side effect of a successful open — and
  `RegisterWorkspace` already commits before planning, so this matches how the
  record behaves today. Retrying `open` after fixing the failure keeps the bind.

`bind --clear <target>` removes the bind and leaves the record. `bind` on a
session that does not yet exist creates it, so binding is the natural way to
declare a new session.

**The bind is the session's base directory.** This is a deliberate departure
from #38's wording, which reads as "the directory the session opens in." Making
it the base means every window's `dir:` composes on top of it, rather than the
two settings fighting over one slot.

The composition happens in `controller/ensure.go`, which is the only place a
`RelDir` becomes a path. Note that a pane's `RelDir` *replaces* the window's
rather than nesting inside it (`ensure.go:258-260`, `284-286`) — the bind must
prefix whichever `RelDir` wins, not only the window's. Five sites join or pass a
`RelDir` and all five take the prefix: the container pane command (264), the
container window command (271), the host window `dir` (280), the host pane `dir`
(286), and the host-side `-c` for container windows and panes (265, 272), which
is `Workspace.RepoRoot` today and becomes the bound directory.

Because `container/exec.go:31` does `path.Join(binding.Workdir, relDir)` with
`Workdir` set to `/workspaces/<repo>`, the same prefix produces the correct path
inside a container with **no change to the container adapter**. A session bound
to `services/api` with a window `dir: cmd` opens `services/api/cmd` on the host
and `/workspaces/<repo>/services/api/cmd` in the container.

Rather than prefixing at five call sites, the implementation should compute the
base once — `Workspace.RepoRoot` joined with the bind — and thread it through,
so a future site cannot forget the prefix.

If a bind is unusable at open time — the directory was deleted, a linked
worktree was pruned, or it no longer canonicalizes to a path inside the
repository — `open` falls back to the repository root and says so rather than
failing.

Configuration stays keyed on slug: `config.Load(root, defaults, ws.Slug)` is
unchanged, so both sessions on a repository share `workspaces/<slug>.yaml`.
Per-session configuration is out of scope.

## 5. Reporting

- `list` gains a `BIND` column and renders the target as `slug/session` for
  named sessions, bare `slug` for the default. `listRow` already carries
  `Session`.
- `bind` appears in the JSON for `list`, `status`, and `open`.
- `OutputSchemaVersion` stays 2. Every change is an added field or an added
  column; nothing is renamed, retyped, or removed.

Documentation to update: `docs/commands.md` (target conventions, `open`,
`list`, `status`, a new `bind` section, and the `stop --container` example which
should now show a real sibling such as `slabledger--feature-a`);
`docs/worktrees.md`; and the "Deferred" section of decision 0001, which this
slice closes out.

A new decision record captures the two departures from the design of record:
the bind-as-base-directory reading of #38, and the restrictive session grammar.

## 6. Errors

No new exit codes. The existing set covers every case: 2 for a malformed target
or an invalid bind path, 3 for an ambiguous bind, 4 for an unknown repository,
and 1 for a propagated state-store failure per §3.

## 7. Verification

The critical regression test is a golden test pinning
`sha256(repo_root + "\0" + "")` for the default session, so no refactor of the
derivation can silently change an existing user's stored IDs.

Alongside it:

- a table test over the target grammar, covering every exit-2 form in §1;
- target presence: `<repo>` selecting the default session even when the cwd sits
  inside a bound directory, and no-target selecting the bound one — the pair
  that distinguishes the two cases finding this design's first draft collapsed;
- bind lookup: nested binds resolving to the longest match, a tie producing
  exit 3, and `services/apixyz` **not** matching a bind of `services/api` — the
  component-wise comparison, which a string prefix would fail;
- read-only store outcomes: a missing database and a pending migration falling
  back to the default session, and an integrity failure, a permission failure,
  and an indeterminate WAL each propagating rather than falling back;
- `SessionBelongsTo` matching a v0.5.0 session that has no `@dev_session`;
- `rebuild` recovering a named session, with an unrecoverable bind reported
  rather than silently dropped;
- `open --cwd` holding one critical section: the bind is visible to the window
  planning in the same call, and it persists when the open fails afterward;
- a bind that canonicalizes outside the repository at open time being refused
  and falling back to the repository root, not followed;
- ensure-level base-directory joining, verified on both the host and the
  container path, and including a pane that sets its own `dir:` — the case where
  the pane replaces rather than nests;
- `stop --container`'s sibling check, now reachable for the first time;
- a migration 0003 test.

Explicitly out of scope: a `doctor` check for dangling binds, and per-session
configuration.
