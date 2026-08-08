# 1. Repository-scoped workspaces and containers

**Status:** Accepted — implemented in #31, merged 2026-08-08.

Records the reasoning behind the change, now that the design doc and plan it
came from have been removed. The behavior itself is documented in `README.md`
and `docs/commands.md`; this is only the *why*, for the decisions a reader
would otherwise have to reconstruct from the code.

## Context

ProjectMux keyed a workspace — its identity, its tmux session, and its Dev
Container — on the git worktree. Opening a second worktree of one repository
therefore demanded a second container: N databases, N caches, N sets of
published ports.

It first surfaced as a missing file. `devcontainer.json` declares
`docker-compose.override.yml`, `.gitignore` keeps it untracked, so a fresh
worktree does not have one and `devcontainer up` fails. That is the symptom,
not the disease — seeding the file would have produced a second container that
then collided on ports.

It also quietly breached a stated non-goal. Keying containers on worktrees made
ProjectMux responsible for per-worktree source layout, which is what
`docs/design.md` §3 excludes.

## Decision

The **repository** is the unit. Resolution returns the main worktree, never a
linked one, so every tree of a project shares one workspace and one container.
The compose mount is already the repository root, so every worktree is visible
inside the single container at `/workspaces/<repo>/.worktrees/<name>` with
nothing new to mount.

## Consequences worth knowing

**Seeding untracked files was rejected, not deferred.** Three variants were
measured on a real repository: a naive copy of every gitignored file moves
multiple GB; skipping ignored directories brings it to 608 files / 124 MB; a
1 MiB cap to 602 / 38 MB. All were rejected because "gitignored" and "required
by a worktree" are different sets that merely overlap — the heuristic copies
`.claude/` partially, varies with the per-machine unversioned
`.git/info/exclude`, and a size cap turns a too-large required file into a
runtime failure far from its cause.

**Two locks, globally ordered.** A shared container needs a shared lock.
Container work locks on the repository ID, session work on the workspace ID,
and a command needing both takes **repository first, then workspace**. That
ordering is what keeps two commands on one repository from deadlocking; it is
written down because the code it replaced had a single lock and no ordering to
inherit. The sibling check before `stop --container` and the stop itself must
happen under one continuous hold — a check released before the stop is the
race it exists to prevent.

**Autostart iterates repositories, not workspaces.** The dropped `is_primary`
flag was load-bearing: autostart skipped non-primary rows so a shared container
would not be started once per worktree. One row per repository restores that
guarantee structurally rather than by filtering.

**The migration deliberately over-counts.** Telling a main worktree from a
linked one requires asking git, which is resolver work, not SQL. So `0002` is
pure SQL that moves every stored row verbatim, treating each path as a
repository root, and `rebuild` — which *can* ask git — collapses the duplicates
afterward. This preserves the property that matters: a migration never fails
because the filesystem changed. The intermediate state is over-counted, never
wrong. `rebuild` is a required upgrade step, not an optional one.

**Existing workspace IDs changed.** The session name is an input to the hash,
and it does not reduce to the old value even when empty. Because IDs appear in
`@dev_workspace_id` on live sessions, `rebuild` matches such a session by its
`@dev_worktree` value and rewrites the option instead of treating the mismatch
as a different workspace.

**`OutputSchemaVersion` went to 2** — the first break since the compatibility
contracts were withdrawn below 1.0 (#29). It renamed the workspace path field
from `worktree` to `repo_root`, dropped `is_primary`, and added `session`. The
rename is deliberate: a consumer that breaks loudly on a missing field is
better than one that silently reads a repository root as a worktree path.

## Deferred

The design also specified a `<repo>/<session>` target form and a `bind` command
for per-session working directories. Neither shipped in #31. The identity was
built session-aware anyway — `sha256(repo_root + "\0" + session)` — so that
adding them later does not rewrite every stored ID a second time. Until then
`resolve.Resolve` hardcodes the empty session, and one repository has exactly
one session.
