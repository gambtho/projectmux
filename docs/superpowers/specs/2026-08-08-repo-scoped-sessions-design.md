# Repository-Scoped Sessions Design

ProjectMux currently treats every linked git worktree as its own workspace: its
own identity, its own tmux session, and — the part that breaks — its own Dev
Container. This slice makes the **repository** the unit of a workspace and
replaces worktree-derived identity with an explicit named session.

The change is motivated by a concrete failure, and it restores a non-goal the
current implementation quietly violates. Design.md §3 lists among the non-goals:

> Managing project source, creating worktrees, or replacing the Dev Containers
> CLI.

Worktree-aware resolution is not worktree *creation*, so it does not breach
that clause literally. But keying containers on worktrees does make ProjectMux
responsible for per-worktree source layout, which is the spirit of the clause.
After this slice, worktrees are ordinary directories that ProjectMux neither
resolves, creates, nor owns.

## 1. The defect

Opening two worktrees of one repository fails. Reported against
`double-holo-ui`:

```
projectmux: starting the container: devcontainer up failed (exit 1):
devcontainer up reported "error": Command failed: docker compose
-f .../.worktrees/1529/.devcontainer/docker-compose.yml
-f .../.worktrees/1529/.devcontainer/docker-compose.override.yml
--profile * config
```

The proximate cause is a missing file. Running that compose command by hand
gives:

```
open .../.worktrees/1529/.devcontainer/docker-compose.override.yml:
no such file or directory
```

`devcontainer.json` is tracked and declares
`"dockerComposeFile": ["docker-compose.yml", "docker-compose.override.yml"]`,
while `.gitignore` keeps the override untracked. The primary checkout has one;
a fresh worktree does not.

The **root** cause is one level up. `internal/container/adapter.go:150` starts
containers with `devcontainer up --workspace-folder <ws.Worktree>`, and
`adapter.go:95` finds existing ones with
`--filter label=devcontainer.local_folder=<ws.Worktree>`. Both key on the
worktree, so N worktrees demand N containers — N databases, N caches, N sets
of published ports. The missing override is the first symptom encountered, not
the disease. Seeding the file would produce a second container that then
collides on ports.

Once the container is keyed on the repository, the override is needed only at
the repository root, where it already exists. The compose mount is
`..:/workspaces/<repo>:cached` — the whole repository root — so every worktree
under it is already visible inside the single running container at
`/workspaces/<repo>/.worktrees/<name>`. Nothing new needs mounting.

## 2. Scope

In scope:

- Resolution lands on the repository (main worktree), never on a linked
  worktree.
- A `<repo>/<session>` argument form for multiple sessions on one repository.
- Identity derived from repository root plus session name.
- A state schema that separates repositories from sessions, and a
  repository-scoped lock for the container each now shares.
- An explicit per-session working directory (`bind`), replacing the implicit
  one that worktree identity used to supply.

Out of scope, deliberately:

- **Creating or removing worktrees.** Design.md §3 already excludes this.
  `git worktree add` / `remove` and the existing `git-worktree-gc` in dotfiles
  cover it.
- **Seeding untracked files into worktrees.** §1 explains why the problem
  disappears rather than needing a solution. Recorded as a rejected
  alternative in §10.
- **Per-worktree container isolation.** No `devcontainer.scope` key. A
  workspace that genuinely needs an isolated container is a separate
  repository checkout, which already works.

## 3. Command surface

```
projectmux open [--no-attach] [--cwd <path>] [--json] [--compact] [<target>]
projectmux bind [--clear] <target> [<path>]
```

`<target>` takes two forms:

| Form | Meaning | tmux session |
| --- | --- | --- |
| `<repo>` | the repository's default session | `<slug>` |
| `<repo>/<session>` | a named session on the same repository | `<slug>--<session>` |

With no argument, the target is the repository containing the current
directory, default session. Every command that accepts `<workspace>` today
accepts `<target>` instead: `config`, `status`, `stop`, `open`.

`/` is the separator because it cannot appear in a git repository directory
name, so `<repo>/<session>` is unambiguous without escaping. A `<session>`
containing `/` is a usage error (exit 2).

The bare `projectmux <target>` shorthand for `open` is retained, and with it
the documented trade recorded in `docs/commands.md`: a mistyped command
resolves as a target and exits 4 rather than 2.

## 4. Resolution

`internal/resolve` changes in three places.

**`fromDirectory` (`resolve.go:121`)** currently returns
`git rev-parse --show-toplevel`, which for a linked worktree is that
worktree's own root. It must return the **main** worktree instead — the first
entry of `git worktree list --porcelain`, which `slugFor` (`resolve.go:229`)
already parses for exactly this purpose. The two callers converge on one
helper.

Consequence: `cd .worktrees/1529 && projectmux open` opens the repository
session. Running ProjectMux from inside a worktree is no longer a way to get a
different workspace.

**`byName` (`resolve.go:130`)** stops scanning `nestedWorktreeDirs`
(`resolve.go:22`, `.worktrees` and `.claude/worktrees`). It searches each
configured root for directly-named repositories only.

**`Workspace` (`resolve.go:25`)** replaces `Worktree` with `RepoRoot`, and
gains `Session` (empty for the default). `ID` becomes the hex SHA-256 of
`RepoRoot` and the session name rather than of `Worktree`, and its doc comment
changes with it.

`IsPrimary` is removed from the resolved struct: every resolved workspace is
now the repository's main tree by construction, so the field is always true at
resolution time. It is **not** merely descriptive in the state layer, though —
autostart uses the stored flag to avoid starting a shared container once per
worktree. §6.3 covers what replaces it before the field is dropped.

Exit 3 (`the workspace name matched more than one worktree`) survives with a
narrower meaning: two configured roots containing repositories of the same
name. Its message changes from "worktree" to "repository". Exit 4 is unchanged.

## 5. Identity and state

### 5.1 Identity

Design.md §7 states:

> The workspace ID is derived from the canonical worktree path and is stable
> for that path. The human-facing session name is derived from the slug and
> linked worktree basename.

Both sentences change. The ID is derived from the canonical **repository root
path plus the session name**, and is stable for that pair. The session name is
derived from the slug and the **session name**, not a worktree basename. The
default session's name is the bare slug, preserving today's naming for the
common case.

Design.md §7 is amended in place as part of this slice; a spec that contradicts
the design document without amending it leaves the next reader with two
answers.

The tmux user options `@dev_workspace_id`, `@dev_slug`, and `@dev_worktree`
retain their names — they are a compatibility surface with live sessions, and
renaming `@dev_worktree` would strand every currently-running session from
`rebuild`. Its **value** becomes the repository root. This is recorded here
because the name will otherwise read as a mistake.

### 5.2 Schema

The current schema cannot express this model, and no renaming makes it fit.
`workspaces.worktree` is `NOT NULL UNIQUE`
(`internal/state/migrations/0001_initial.sql:8`), so two sessions on one
repository collide on insert. `container_bindings.workspace_id` is the primary
key and the foreign key (`0001_initial.sql:23`), so a container is owned by one
session — but §6 makes containers shared, and cascade-deleting one session
would drop a binding its siblings still use. `last_operations` is keyed the same
way (`0001_initial.sql:33`).

An earlier draft of this section proposed keeping the `worktree` column and
changing its meaning. That is unimplementable: the uniqueness constraint is
precisely the assumption this slice invalidates.

Migration `0002` splits the entity in two:

```
CREATE TABLE repositories (
    id          TEXT PRIMARY KEY,   -- hex SHA-256 of repo_root
    slug        TEXT NOT NULL,
    repo_root   TEXT NOT NULL UNIQUE,
    ...
);

CREATE TABLE workspaces (
    id               TEXT PRIMARY KEY,   -- hex SHA-256 of repo_root + session
    repository_id    TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    session          TEXT NOT NULL,      -- '' for the default session
    proposed_session TEXT NOT NULL,
    actual_session   TEXT UNIQUE,
    bound_path       TEXT,               -- §7, relative to repo_root
    ...
    UNIQUE (repository_id, session)
);
```

`container_bindings` re-keys to `repository_id`, which is what makes one
container per repository representable at all rather than merely intended.
`last_operations` stays keyed on `workspace_id`: an operation is performed by a
session, not by a repository.

`slug` moves to `repositories` because it is a property of the repository, and
storing it per session would let two sessions on one repository disagree.

`is_primary` is dropped from the schema. Every row in `repositories` is a main
worktree by construction, so the flag is always true and carries no
information — see §6 for the autostart behavior it used to carry.

### 5.3 JSON envelopes

The affected surface is wider than the three commands an earlier draft named.
`workspaceInfo` (`internal/cli/config.go:51`) is shared, and carries both
`worktree` and `is_primary`; `rebuild` declares the same two fields
independently (`internal/cli/rebuild.go:54-55`). All envelopes share a single
`OutputSchemaVersion` (`internal/cli/config.go:19`), so one bump moves all of
them together.

| Command | Carrier | Change |
| --- | --- | --- |
| `config` | `workspaceInfo` | `worktree`→`repo_root`, `is_primary` dropped, `session` added |
| `open` | `workspaceInfo` (`open.go:83`) | same |
| `attach` | `workspaceInfo` (`attach.go:136`) | same |
| `stop` | `workspaceInfo` (`stop.go:118`) | same |
| `status` | `workspaceInfo` | same |
| `list` | own fields (`list.go:38-39`) | same |
| `rebuild` | own fields (`rebuild.go:54-55`) | same |

`OutputSchemaVersion` goes 1 → 2, once, covering all seven. The README's alpha
notice already reserves this:

> Expect breaking changes to the configuration schema and the command surface
> while the version remains below 1.0.

Renaming the JSON field to `repo_root` rather than keeping `worktree` with a
new meaning is deliberate: a consumer that breaks loudly on a missing field is
better than one that silently reads a repository root as a worktree path. The
tmux option keeps its name for the opposite reason — it has live sessions
depending on it, and JSON consumers at this version do not.

## 6. Containers

No new configuration. `devcontainer up --workspace-folder` and the
`devcontainer.local_folder` filter both receive `ws.RepoRoot`, which is now the
only path a workspace has. Every session on a repository therefore shares one
container, and `adapter.go` changes only in which field it reads.

Container **working directory** is where sessions differ: a window's cwd inside
the container is the container's workspace folder joined with the session's
bound path relative to the repository root (§7). A session bound to
`.worktrees/1529` runs at `/workspaces/<repo>/.worktrees/1529`.

### 6.1 Locking

A shared container needs a shared lock, and today's lock does not provide one.
`Ensure`, `Stop`, and `StartWorkspaceContainer` all acquire on
`d.Workspace.ID` (`internal/controller/ensure.go:79`,
`internal/controller/stop.go:28`, `internal/controller/autostart.go:27`) — an
ID this slice makes **per-session**. Two sessions on one repository would no
longer exclude each other, which breaks the lock's stated guarantee of
serializing observation through mutation. Concretely: two `open`s race
`devcontainer up`, or the §6.2 sibling check passes just as a sibling opens,
and the container is stopped out from under it.

Container work therefore locks on the **repository ID**, session work on the
workspace ID:

| Phase | Lock |
| --- | --- |
| container observe/start/stop | `repository_id` |
| session observe/create/kill, state commit | `workspace_id` |

Where a command needs both — `open` and `stop --container` — it acquires
**repository first, then workspace**, and releases in reverse. A single global
ordering is what keeps two commands on the same repository from deadlocking;
it is stated here because the current code has only one lock and no ordering to
inherit.

The sibling check in §6.2 and the container stop that follows it must occur
under one continuous hold of the repository lock. A check released before the
stop is the race it exists to prevent.

### 6.2 Stopping a shared container

Because a container is now shared, `stop --container` stops a container other
live sessions may be using. It must refuse when another session on the same
repository is live, naming them, and exit 6 (the documented "refused: conflict"
code). `--force` overrides.

### 6.3 Autostart

`is_primary` was not merely descriptive: autostart iterates every stored
workspace and skips non-primary rows (`internal/cli/autostart.go:96-97`),
precisely so a shared parent container is not started once per worktree
(`docs/commands.md:350,358`). Dropping the flag without a replacement would
make autostart start the same container once per session.

The §5.2 split supplies the replacement directly: autostart iterates
`repositories`, not `workspaces`. One row per repository means one container
start per repository, structurally rather than by filtering — the same
guarantee the flag provided, now enforced by the schema.

Its report is per repository, keyed by repository ID and slug. This is a
further `--json` change on top of §5.3, and rides the same version bump. The
existing regression test asserting that a recovered `is_primary` keeps
autostart from restarting a container (`internal/cli/lifecycle_test.go:495`)
is rewritten against the repository iteration rather than deleted; the behavior
it protects still matters.

## 7. Session working directory

Sessions start at the repository root. A session may bind a directory:

```
projectmux bind myrepo/feature-a .claude/worktrees/xyz
projectmux bind --clear myrepo/feature-a
projectmux open --cwd .claude/worktrees/xyz myrepo/feature-a
```

Rules:

- The path resolves relative to the repository root, and must lie inside it
  after symlink resolution. A path outside is a usage error (exit 2) — a bind
  cannot point a session out of its own repository, which would reintroduce
  the container/cwd mismatch this slice removes.
- The bind is stored per session, and is applied to every window that does not
  set its own `cwd`. An explicit window `cwd` still wins.
- On open, a bound path that no longer exists falls back to the repository
  root and **says so** in the report. A worktree deleted after `git-worktree-gc`
  runs degrades to a working session rather than a failure.
- `--cwd` on `open` binds and opens in one step, so the common case is one
  command.

**Durability.** A bind is user intent stored in a database design.md documents
as disposable — "the database is rebuildable" (`docs/design.md:253`), and
`rebuild` already declines to restore container bindings and other lost state
(`docs/commands.md:398`). The policy is explicit rather than left to be
discovered:

> A bind does not survive loss of the state database. `rebuild` restores
> sessions at the repository root, and reports which sessions had a bind it
> could not recover.

Two things make this acceptable rather than merely tolerated. A bind is one
command to restore, and the fallback is a working session at the repository
root rather than a failure. And the alternative — writing binds to the tmux
session as a user option, the one store that outlives the database — cannot
work for the case the bind exists to serve: the session may not be running when
the bind is set, and a bind must survive the session being killed and
recreated, which is exactly when it is needed.

Recording the bind in **both** places, database as the source of truth and tmux
as a recovery hint, is a reasonable later refinement. It is out of scope here
because it adds a second writer and a reconciliation question for a case that
one command fixes.

**Why explicit rather than observed.** An earlier draft recorded each window's
`pane_current_path` opportunistically on every command that already observes
tmux. Two observations on a live server killed it:

- Container-backed windows report nothing. `tmux list-panes -a -F
  '#{pane_current_command} | #{pane_current_path}'` returns
  `docker | ` — an empty path — because the pane's process is `docker`.
  ProjectMux workspaces are predominantly container-backed, so capture would
  be blind in exactly the sessions it was meant to serve.
- An agent that changes its own working directory need not move the pane's
  process. A live `claude` process was observed with cwd at a repository root
  while a sibling shell sat inside `.claude/worktrees/…`.

Observation is therefore unreliable here in a way that is invisible until a
session is recreated and lands in the wrong place. An explicit bind fails
loudly at bind time instead.

## 8. Companion changes in dotfiles

These are not ProjectMux changes; they are recorded here because the slice is
incomplete without them.

**`git mv bin/dev bin/pm`.** Verified safe: no script, test, systemd unit, or
Makefile target invokes `bin/dev`. The only reference is prose in
`tools/projectmux/README.md`. Design.md §3 goal 2 says "`dev` remains a
dotfiles-only alias" — the alias stays dotfiles-only, under a name that does
not read as "run the dev server".

**`git config --global worktree.useRelativePaths true`.** Load-bearing, and
easy to omit. A linked worktree's `.git` file holds an absolute host path:

```
gitdir: /home/tng/workspace/double-holo-ui/.git/worktrees/1529
```

That path does not exist inside the container. Before this slice it did not
matter, because a worktree got its own container mounted at its own root.
After it, worktrees are reached *through* the shared container, so git inside
one would break. `worktree.useRelativePaths` (git 2.54 confirmed to support
it) makes every future worktree — created by hand or by an agent — link
relatively and work from both sides. Existing worktrees convert with
`git worktree repair`.

**A PostToolUse hook that calls `projectmux bind`.** Mirrors the existing
`ai/claude/hooks/worktree-guard.sh`: reads the tool payload from stdin with
`jq`, no-ops when `$TMUX` is unset, resolves the session with
`tmux display-message -p '#{session_name}'`. It closes the gap where an agent
creates a worktree the user never named, so reopening the session returns to
it.

This is glue on the dotfiles side by design. ProjectMux exposes the generic
`bind` verb and depends on nothing that exists only on one machine.

## 9. Migration

Existing state rows carry worktree-derived IDs and a `worktree` column that may
name either a main or a linked worktree. Under §5.2 the two now belong in
different tables, and telling them apart requires asking git — which is
resolver work, not SQL.

**Ownership.** Migrations today are embedded SQL executed directly in a
transaction (`internal/state/migrate.go:75,87`), and classifying a main
worktree belongs to `internal/resolve`. Rather than teach the migration runner
to call git — which would give a schema migration a dependency on the
filesystem and on git's exit status — the work splits:

- **`0002` (SQL only)** creates the new tables and moves every existing row
  into `repositories` + `workspaces` **verbatim**, treating each stored
  `worktree` as a repository root. It is pure SQL, atomic, and cannot fail on a
  missing directory or an absent git binary.
- **`rebuild` (Go, resolver-aware)** corrects the result. Rows whose recorded
  path is a linked worktree resolve to their parent repository and collapse
  into it; rows whose path no longer exists are dropped.

This keeps the property that matters: **a migration never fails because the
filesystem changed.** A repository deleted or moved since the last run makes
`rebuild` skip a row, not the upgrade abort. The intermediate state between
`0002` and `rebuild` is over-counted, never wrong — extra repository rows that
resolve to the same real repository, which `rebuild` merges.

**ID stability.** A default-session workspace keeps its ID only if the ID
derivation for the empty session name reduces to today's hash of the repository
path. It does not, since the session name is now an input. Existing IDs
therefore change, including for repositories that were already main worktrees
and are otherwise unaffected. Because IDs appear in `@dev_workspace_id` on live
sessions, `rebuild` matches such a session by its `@dev_worktree` value and
rewrites the option, rather than treating an ID mismatch as a different
workspace.

Live sessions created before the change carry `@dev_worktree` pointing at a
linked worktree. `rebuild` maps such a session to its repository and proposes
the linked worktree's basename as the session name — recovering
`myrepo--feature-a` under the new model with the same name it already has.

**This is a documented upgrade step, not a silent one.** `0002` alone leaves
state that is structurally valid and semantically stale; `rebuild` is what
completes it. An earlier draft left "whether the migration can avoid a
`rebuild` run" open — it cannot, because the classification needs git. The
release note says so, and `status` reports a repository whose root is not a
main worktree as needing `rebuild`.

## 10. Rejected alternatives

**Seed untracked files into new worktrees.** Three variants were considered:
copying every gitignored file; copying gitignored files while skipping
gitignored directories and capping file size; and an explicit `seed_files:`
list. Measured on `double-holo-ui`, the naive copy moves multiple GB
(`node_modules/`, `.next/`, `.pnpm-store/`); skipping ignored directories
brings it to 608 files / 124 MB; a 1 MiB cap brings it to 602 files / 38 MB.

All three were rejected because "gitignored" and "required by a worktree" are
different sets that merely overlap. The heuristic copies `.claude/` partially
(`settings.local.json` yes, `worktrees/` no), silently varies with
`.git/info/exclude`, which is per-machine and unversioned, and a size cap turns
a too-large required file into a runtime failure far from its cause. The
explicit list avoids the guessing but still treats a symptom that §1's change
removes.

**`projectmux new` and `projectmux done`.** Rejected against design.md §3's
non-goal. `git worktree add` and `git-worktree-gc` already cover it, and a
`done` that stopped sessions would duplicate `git-worktree-gc`'s
squash-merge-aware sweep.

**`done` delegating to `git-worktree-gc`.** Rejected twice over: a published
Go binary cannot depend on a bash script in one user's dotfiles, and `gc`
removes only merged branches, so `done` on an abandoned experiment would report
success having removed nothing.

**Auto-creating a worktree when `open` is given an unknown name.** Rejected
because bare `projectmux <target>` is shorthand for `open`, so `projectmux
stauts` would create a branch named `stauts`. It would also make exit 4
unreachable in practice.

## 11. Open questions

- **PostToolUse payload.** Whether the hook fires for the tool that creates a
  worktree, and whether its payload carries the resulting path, is unverified.
  A spike answers this before §8's hook is written. If the payload lacks the
  path, the hook falls back to reading it from the tool input.
- **Worktrees created via a shell command** rather than a dedicated tool would
  not match that hook. Whether to also match shell invocations of
  `git worktree add` is deferred until the first case is working.
- **Lock granularity under `autostart`.** §6.1 has autostart take the
  repository lock. Whether boot-time autostart of many repositories should hold
  them concurrently or serialize is an implementation question; the ordering
  rule makes either safe.

## 12. Verification

- `internal/resolve` tests already construct real repositories with linked
  worktrees (`resolve_test.go:44`). They invert: a linked worktree must now
  resolve to its parent, and nested-directory lookup must no longer find
  anything.
- A test asserting two targets on one repository produce one container
  identity and two session identities — the defect in §1, stated as a test.
- Two sessions on one repository registering successfully, which the pre-`0002`
  schema cannot represent (`0001_initial.sql:8`). This is the §5.2 finding as a
  test, and it fails against a column rename.
- Concurrent `open` of two sessions on one repository issues exactly one
  `devcontainer up` — the §6.1 race, driven through the fake container
  actuator with both goroutines released together.
- `stop --container` refusal while a sibling session is live, with exit 6, and
  the same case with `--force` proceeding.
- Autostart over a repository with several registered sessions starts its
  container once, replacing the `is_primary` regression test at
  `lifecycle_test.go:495`.
- Bind rejection for a path outside the repository root, including via
  symlink.
- Bind fallback when the bound path is deleted, asserting the report says so.
- Every `--json` envelope in the §5.3 table reports `schema_version: 2` and
  omits `is_primary`, asserted per command so a missed carrier fails.
- The socket-isolated integration test (`socket_integration_test.go`) covers
  the migration path against a database written by the previous schema:
  `0002` alone must succeed with the filesystem entirely absent, and a
  following `rebuild` must collapse a linked-worktree row into its parent.
