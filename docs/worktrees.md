# Working with git worktrees

ProjectMux does not create or manage worktrees — that is an explicit non-goal.
It opens repositories, and a repository is the unit: every tree of a project,
main or linked, shares one workspace, one tmux session, and one Dev Container.

This guide is the practical consequence of that rule. For the reasoning, see
[decision 0001](decisions/0001-repository-scoped-workspaces.md).

## The one thing to internalize

`open` from inside a worktree opens the **repository**, not the worktree.

```sh
cd ~/workspace/my-project/.worktrees/feature-a
projectmux open
```

That session's windows start at `~/workspace/my-project` — the main working
tree. The worktree you typed the command in is not where you land, and it is
not a workspace of its own. `projectmux list` will never show it, and
`projectmux feature-a` exits 4: a linked worktree cannot be named.

This is not a limitation to work around. One repository means one container,
one port allocation, and one set of dependencies, however many branches you
have checked out at once.

## Put worktrees inside the repository

The container mounts the repository root. A worktree placed under it is
therefore already visible inside the container, with nothing new to mount:

```sh
cd ~/workspace/my-project
git worktree add .worktrees/feature-a -b feature-a
```

Inside the container that tree is at `/workspaces/my-project/.worktrees/feature-a`.
`.claude/worktrees/` works the same way.

A worktree created **outside** the repository root still resolves correctly on
the host — `open` from it finds the right repository — but it is outside the
mount, so it does not exist inside the container. If your windows run with
`location: container`, keep worktrees under the repository root.

### Relative worktree links

A linked worktree records its repository in a `gitdir:` line. Absolute, that
line names a *host* path, which does not exist inside the container — so git
fails in the worktree even though the files are mounted. Set:

```sh
git config --global worktree.useRelativePaths true
```

Two things to know before you do:

- It needs git ≥ 2.48. Older git parses the key, ignores it, and writes an
  absolute link anyway — indistinguishable from success until a worktree fails
  inside a container.
- It implies `extensions.relativeWorktrees`, which raises the repository to
  `core.repositoryformatversion = 1` once it has created a relative worktree.
  That requirement then follows the repository everywhere it is opened,
  including into container images with an older git.

Worktrees created before you set it keep their absolute links; only new ones
are relative.

## Getting a window into a worktree

Since every window starts at the repository root, point one at a worktree with
`cwd`, which is relative to that root:

```yaml
windows:
  - name: feature-a
    shell: true
    cwd: .worktrees/feature-a
```

`cwd` may not escape the repository root — another reason to keep worktrees
inside it.

Because the config is per repository (`workspaces/<slug>.yaml`), a worktree you
expect to be short-lived is usually better handled by `cd` in a shell window
than by a config entry you will have to remove. Use
`workspaces/<slug>.local.yaml` if you want the entry without committing it.

## Stopping

`stop` ends the session. `stop --container` also ends the container — which
every tree of the repository shares, so it refuses with exit 6 if another
session on that repository is live, and names it. `--force` overrides.

Removing a worktree is git's job, and ProjectMux neither notices nor cares:

```sh
git worktree remove .worktrees/feature-a
```

Nothing recorded in ProjectMux referred to that tree, so there is nothing to
clean up.

## Upgrading from before `v0.5.0`

Releases before `v0.5.0` keyed workspaces on the worktree, so each tree had its
own workspace and its own container. After installing `v0.5.0`, run:

```sh
projectmux rebuild
```

once. It is required, not optional. The schema migration moves each stored row
verbatim, treating its recorded path as a repository root, because telling a
main worktree from a linked one means asking git and a migration must never
fail because a directory moved. `rebuild` collapses the linked worktrees into
their parents. Until you run it, `status` reports the repository as needing it.

## Quick reference

| You want | Do this |
| --- | --- |
| A worktree reachable in the container | `git worktree add .worktrees/<name>` from the repository root |
| To open the project from inside a worktree | `projectmux open` — it resolves to the repository |
| To open a worktree as its own session | Not supported; the repository is the unit |
| A window sitting in a worktree | `cwd: .worktrees/<name>` in `workspaces/<slug>.yaml` |
| Git to work in a worktree inside the container | `git config --global worktree.useRelativePaths true` (git ≥ 2.48) |
| To remove a worktree | `git worktree remove` — ProjectMux holds nothing to clean up |
