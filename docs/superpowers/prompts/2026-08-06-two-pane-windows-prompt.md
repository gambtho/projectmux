# Prompt: Two-Pane Windows by Default in ProjectMux Layouts

Paste this prompt into a fresh session in the ProjectMux repository.

---

You are working in the current ProjectMux repository checkout. Treat the
checkout, its git history, tests, and documentation as the only source of
truth. Ignore older descriptions not reflected in the current files.

## Objective

Design and plan multi-pane window support in ProjectMux's tmux backend, with
the default layout becoming **two panes per window**: the window's primary
pane (its configured agent/command/shell) plus a secondary shell pane in the
same working directory.

This idea originated in the Zellij backend investigation
(`docs/superpowers/specs/2026-08-06-zellij-backend-design.md` §6): Zellij
layouts express multi-pane tabs natively, and the capability — not the
backend — was the valuable part. Deliver it on tmux, where the existing
identity, adoption, and exact-kill safety model is preserved unchanged.

This is a design-and-plan task first. Do not implement until the design has
been reviewed and approved at the design gate below.

## Working rules

Follow the repository's established Superpowers workflow: inspect first, run
a blind-spot pass, present the design, stop for review, and only then plan.
Begin with `git status`, preserve all existing changes, and do feature writes
only in a linked worktree. Verify every tmux behavior you rely on against the
locally installed tmux (`tmux -V`) on an isolated `-L` socket; treat
remembered tmux syntax as a hypothesis until verified. Never touch the
user's real tmux server or sessions.

## Phase 1: Understand the current window model

Read at least: `internal/controller/types.go` (`WindowSpec`, `SessionSpec`),
`internal/controller/interfaces.go` (`WindowIntent`, env contract),
`internal/controller/ensure.go` (`renderWindows`), `internal/tmux/actuate.go`
(`createArgv`, the chained-argv creation, `escapeChainArg` and its
trailing-`;` rule, `envArgs`), `internal/cli/wiring.go` (`windowIntents`),
`internal/config/` (window schema, validation, merge-by-name, digest),
`internal/container/exec.go` (container exec command rendering), and the
open/attach spec under `docs/superpowers/specs/`.

Establish precisely: windows today are single-pane; observation is
session-level (panes and windows are not observed or reconciled after
creation); creation is one chained tmux invocation; environment reaches every
window via `-e` on `new-session`/`new-window`; the applied digest is the
drift signal.

## Phase 2: Design decisions to resolve

Lead with the decisions that change the design the most:

1. **Default policy.** The goal is two panes per window *by default*. A
   changed default alters the rendered session for every existing workspace
   and changes nothing in their YAML. Determine what that means for the
   configuration digest and drift reporting, and whether the default flips in
   one release or arrives opt-in first (`panes` schema plus a
   `defaults.yaml` policy) with the flip as a follow-up. Present the
   trade-off explicitly — do not silently choose.
2. **Schema.** How a window declares its panes (e.g. an optional `panes`
   list mirroring the window fields: command/shell, `cwd`, `focus`), what
   the implicit default second pane is (proposal: a shell pane in the
   window's directory), how "exactly one focused pane" interacts with
   "exactly one focused window", and how merge-by-name behaves for panes
   across config layers.
3. **Creation semantics.** Extend the single chained invocation with
   `split-window` (verify: `-t`, `-c`, `-e`, `-d` behavior, split direction
   and sizing, and the chain parser's trailing-`;` escaping for every new
   argument position, all on an isolated socket). Preserve near-atomicity:
   creation either completes or the post-create confirmation fails it.
4. **Reconciliation boundary.** Panes, like windows today, are created but
   not reconciled: a user closing the second pane must not create drift or
   repair actions. State this boundary explicitly in the design.
5. **Container windows.** Decide what the secondary pane runs in a
   container-located window: host shell or container exec shell (proposal:
   container exec, same binding, same workdir — verify the rendered command
   works as a pane command). Cover `location: auto`.
6. **Environment.** Confirm `-e` on `split-window` (verify against local
   tmux; it exists on new-session/new-window on 3.4) or determine the
   alternative that gets the configured environment into secondary panes.
7. **Stop/attach/status.** Confirm none of these need changes (they operate
   on sessions, not panes) and say so in the design.

## Phase 3: Required output

Present: the schema with examples (default case, opted-out single-pane case,
customized panes case); the default-flip compatibility analysis including
digest impact; the exact tmux argv rendering with verified transcripts; the
reconciliation boundary statement; container and environment behavior; and a
testing strategy (unit tests for argv rendering and escaping, config
merge/validation/digest tests, isolated-socket integration tests proving pane
count, cwd, env, and focus; existing tmux tests stay green).

## Design gate

Stop after presenting the design for review. On approval, write the spec to
`docs/superpowers/specs/<current-date>-two-pane-windows-design.md`,
self-review it for placeholders and contract weakening, and stop again before
producing the implementation plan.

## Quality bar

The design is complete only when it answers: what every existing workspace's
session looks like after upgrade and why that is acceptable; how a workspace
opts back to single-pane windows; how panes merge across config layers; the
verified argv for a two-pane, env-carrying, container-located window; and why
observation, adoption, stop, and the digest stay honest throughout.
