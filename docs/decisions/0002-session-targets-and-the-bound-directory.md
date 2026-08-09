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
