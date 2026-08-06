# Two-pane windows by default

**Status:** Approved design

**Date:** 2026-08-06

**Scope:** Multi-pane window support in the tmux backend, with the default
layout becoming two panes per window: the window's primary pane (its
configured agent/command/shell) plus a secondary shell pane in the same
working directory and location. Originates in the Zellij backend
investigation (`2026-08-06-zellij-backend-design.md` §6): the capability,
not the backend, was the valuable part. The identity, adoption, and
exact-kill safety model is preserved unchanged.

## 1. Behavior after upgrade

Every window renders as **primary pane + shell pane, 50/50 side-by-side**
(`split-window -h`): the shell pane opens in the same directory — and the
same container, when one applies — as the primary, and focus stays on the
primary pane. The change takes effect on the next `ensure` that creates a
session. **Running sessions are untouched**: panes, like windows, are
create-only and never reconciled (§5). An already-live session reports
drift until it is stopped and reopened — see §3 for why and for the
migration path.

Opting a window back to single-pane is one line of YAML:

```yaml
windows:
  - name: dev
    agent: claude
    panes: []
```

This is acceptable for existing workspaces because nothing destructive
happens (no session is restarted, no pane is killed), the new pane costs
one shell process, and the escape hatch is a one-line, per-window opt-out.

## 2. Configuration schema

`Window` gains an optional `panes` list of *additional* panes; the window's
own fields continue to describe the primary pane unchanged. Each pane
mirrors the window's shape minus `location`:

```go
// Pane is one normalized additional pane of a window.
type Pane struct {
    Name string `json:"name"`
    // Exactly one of Agent, Command, or Shell describes how the pane runs.
    Agent   *string `json:"agent"`
    Command *string `json:"command"`
    Shell   bool    `json:"shell"`
    // Cwd is an optional worktree-relative working directory; absent
    // means the window's directory.
    Cwd   *string `json:"cwd"`
    Focus bool    `json:"focus"`
}
```

`Window` gains `Panes []Pane `json:"panes"``, appended after `Focus` so
existing field order is undisturbed.

### Examples

```yaml
# Default: nothing to write. The window gets its agent pane plus a
# shell pane in the window's directory.
windows:
  - name: dev
    agent: claude

# Opt out: single-pane window.
windows:
  - name: dev
    agent: claude
    panes: []

# Customized: replace the default shell pane with two named panes.
windows:
  - name: dev
    agent: claude
    panes:
      - name: logs
        command: tail -f log/dev.log
        cwd: services/api
      - name: shell
        shell: true
```

### Normalization and defaulting

- **Omitted `panes`** → normalization materializes the default
  `[{"name": "shell", "shell": true}]` into the normalized `Config`, so
  the digested document states the panes that will actually render.
- **`panes: []`** → normalizes and digests as an empty list.
- Pane `cwd` resolves like window `cwd` (worktree-relative); absent means
  the window's directory.

### Location

Panes have **no `location` field**: every pane inherits the window's
resolved location. In a container-located window (explicit or `auto`
resolving to container), a shell pane's command is rendered through the
existing container `ExecCommand` path with the same binding and workdir
as the primary; on a host window it is the default shell. `location:
auto` continues to resolve at the window level exactly as today.

### Focus

Two independent, composable rules:

- Window `focus` is unchanged: exactly one focused window per session
  selects which window is active.
- Pane `focus: true` selects the active pane *within its own window*.
  At most one pane per window may set it; the default active pane is the
  primary. Validation enforces the per-window at-most-one rule alongside
  the existing focused-window rule; pane focus in an unfocused window is
  legal (it selects that window's active pane for when the user switches
  to it — verified: a chained no-`-d` split in a detached, non-current
  window sets that window's active pane without stealing window focus).

### Validation

- Pane mode exclusivity: exactly one of `agent`/`command`/`shell`, same
  rule and message shape as windows.
- Duplicate pane names within one window's list are a per-layer error,
  mirroring `duplicateWindows`.
- At most one `focus: true` per window's pane list.
- Problems use the field path `windows[<window>].panes[<pane>].<field>`
  with the same origin attribution machinery.

### Merge across layers

`panes` merges **as a unit**: a layer that states `panes` replaces the
whole list; an absent key inherits. This follows the established
"mode merges as a unit" precedent in `mergeWindows` and is what makes
`panes: []` a real opt-out even when a lower layer declared panes —
per-pane merge-by-name would make an empty overlay list merge nothing
and silently inherit. Attribution credits `windows[<name>].panes` to the
stating layer.

## 3. Compatibility and digest impact

The digest hashes the normalized `Config`'s canonical JSON
(`internal/config/digest.go`), and `Window` gains a `panes` member, so
**every workspace with configured windows sees its digest change exactly
once at this release**, YAML untouched or not. That signal is honest:
the rendered session genuinely differs (each window gains a pane), and
drift reporting should say so.

**Exception — zero-window workspaces.** A workspace whose YAML declares
no `windows:` digests an empty list (`internal/config/normalize.go`);
its implicit shell window is invented at derivation (`windowIntents`,
`internal/cli/wiring.go`), outside the digested document. That implicit
window gains the default pane too, so these workspaces change rendering
with **no digest change and no drift signal**. This is a knowing
exception: the implicit window already lives outside the digest — it is
the existing instance of the derivation-time-default pattern this spec
otherwise rejects — and pulling it into the normalized config would
change digest semantics for an unrelated reason. The default pane for
the implicit window is therefore supplied at derivation, and the spec
accepts that its arrival is digest-silent.

**When the drift flag clears.** Only a *creating* ensure records the
applied digest (`internal/controller/ensure.go`, creation commit); the
already-running path deliberately commits none. An existing live session
therefore reports drift from this release until it is stopped and
reopened. That is the migration path: `projectmux stop` then `open` (or
simply keep the running session and accept the drift flag until its
natural end). Nothing forces a restart, and no reconciliation touches
the live session.

This is why the default flips in one release rather than opt-in first:
shipping the schema already churns the digest, so a later default flip
would churn it a second time for the same end state. The alternative of
applying the default at derivation time (like the implicit shell window)
would avoid digest churn but hide pane policy from drift reporting;
honesty wins. (Decision recorded at the design gate.)

Schema version stays 1: the new key is optional, every existing document
remains valid, and its absence has a defined meaning.

## 4. Creation semantics

The single chained tmux invocation (`createArgv`) is extended in place;
creation remains one subprocess, preserving near-atomicity and the
post-create confirmation.

Per window, after its `new-session`/`new-window` segment (unchanged),
one segment per pane:

```
; split-window -h -d -t <session>:<window> -c <dir> [-e K=V]... [command]
```

- `-h` splits side-by-side; with one additional pane the result is 50/50.
- `-d` keeps the primary pane active — **omitted on exactly the pane
  whose `focus: true` is set**, because `split-window` without `-d`
  makes the new pane the window's active pane (verified chained). Focus
  selection therefore never targets a pane index: `pane-base-index` is a
  user-configurable server option, so any rendered `.<index>` target
  would be guessing. When no pane sets focus, every split carries `-d`
  and the primary stays active.
- `-e` carries the workspace environment, same sorted `envArgs` rendering
  as windows.
- An empty command means the default shell, as with windows.

For windows with **two or more** additional panes, a final:

```
; select-layout -t <session>:<window> even-horizontal
```

to equalize columns (with exactly one extra pane the 50/50 split is
already even, no segment emitted).

Every new argument position — `-t` targets, `-c` directories, `-e`
pairs, commands, `select-layout` targets — passes through `escapeChainArg`:
the trailing-`;` chain-parsing rule applies to any argv element, and the
composite targets are escaped as complete strings, matching the existing
`select-window` treatment.

### Partial failure

"Near-atomic" means one subprocess, not all-or-nothing. Verified on
tmux 3.4: a failing command mid-chain **aborts the remaining commands,
exits nonzero, and leaves everything already created alive** — including
the session, which carries its identity keys because they are set
immediately after `new-session`. `CreateSession` then returns the error,
ensure records the failure and returns without post-create confirmation,
and the next ensure observes an identity-matched live session and
accepts it as already-running (with the drift flag set, since no applied
digest was committed). The partial session is never repaired.

This is a pre-existing property of chained creation — a mid-chain
`new-window` failure behaves identically today — and the added
`split-window` segments widen the window without changing the shape.
The design keeps it, for two reasons:

- **Recovery works and is the normal path**: because the partial session
  is identity-tagged, `projectmux stop` finds and kills it and `open`
  recreates it. The alternative ordering (identity keys last) would
  leave an identity-*less* session squatting on the workspace's name,
  which planning treats as foreign and refuses to touch — strictly
  worse.
- **The added failure surface is small**: a nonexistent pane `cwd` does
  *not* fail — verified on tmux 3.4, `split-window -c` falls back to
  the home directory with exit 0 and the chain continues (pane `cwd`
  validation is lexical only, matching windows). Realistic mid-chain
  failures are tmux server faults, which fail creation loudly either
  way.

### Verified transcripts (tmux 3.4, isolated `-L` socket)

Chained split of a detached window created earlier in the same chain,
with `-c`, `-e`, and `-d`:

```
$ tmux -L test new-session -d -s t1 -n w1 \; \
    split-window -d -t t1:w1 -c /tmp -e FOO=bar \; \
    list-panes -t t1:w1 -F '#{pane_index} #{pane_current_path}'
1 <original cwd>
2 /tmp
(exit 0; `echo FOO=$FOO` in pane 2 prints FOO=bar)
```

Horizontal 50/50 with primary kept active:

```
$ tmux -L test split-window -h -d -t t3:w1 -c /tmp \; \
    list-panes -t t3:w1 -F '#{pane_index} w=#{pane_width} active=#{pane_active}'
1 w=100 active=1
2 w=99 active=0
```

Splitting a window that was created detached (`new-window -d`) works.
A no-`-d` split makes the new pane active without changing the active
window:

```
$ tmux -L test new-session -d -s t5 -n w1 \; \
    new-window -d -t t5 -n w2 \; \
    split-window -h -t t5:w2 -c /tmp \; \
    list-panes -t t5:w2 -F '#{pane_index} active=#{pane_active}'
1 active=0
2 active=1
(list-windows: w1 stays the active window)
```

Three panes equalized:

```
$ tmux -L test split-window -h -d -t t3:w1 -c /tmp \; \
    select-layout -t t3:w1 even-horizontal \; \
    list-panes -t t3:w1 -F '#{pane_index} w=#{pane_width} active=#{pane_active}'
1 w=66 active=1
2 w=66 active=0
3 w=66 active=0
```

A nonexistent `-c` directory does not fail the split:

```
$ tmux -L test new-session -d -s t6 -n w1 \; \
    split-window -h -d -t t6:w1 -c /nonexistent-dir-xyz \; \
    set-option -t t6 @after-split yes
(exit 0; pane 2's cwd falls back to the home directory; @after-split set)
```

A genuinely failing mid-chain command aborts the rest and leaves the
session alive:

```
$ tmux -L test new-session -d -s t7 -n w1 \; \
    split-window -h -d -t t7:no-such-window \; \
    set-option -t t7 @after-fail yes
can't find window: no-such-window
(exit 1; session t7 exists; @after-fail is unset — the chain aborted)
```

## 5. Reconciliation boundary

Panes are created and never observed, never reconciled — identical to
windows today. A user closing the shell pane (or splitting more of their
own) creates **no drift and no repair action**: observation stays
session-level, and drift remains purely configuration digest vs. applied
digest. This is a stated boundary, not an accident.

## 6. What does not change

- **Stop** kills by observed session ID; **attach** and **status**
  operate on sessions; **adoption** matches identity keys; **doctor**
  inspects session and store health. All session-scoped, all untouched.
- Identity keys, locking, autostart, and the container binding lifecycle
  are unchanged.
- Plumbing: `WindowIntent` and `WindowSpec` each gain a `Panes` field
  (a `PaneIntent`/`PaneSpec` mirroring the window fields minus location);
  `renderWindows` resolves each pane's command and directory — container
  windows render pane commands via `ExecCommand` with the pane's cwd —
  and `SessionSpec` is otherwise unchanged.

## 7. Testing strategy

- **Unit — argv rendering:** `createArgv` for the default two-pane case,
  opted-out single-pane, multi-pane with `even-horizontal`, pane focus
  (the focused pane's split omits `-d`), env-carrying panes, and
  semicolon-adversarial
  session/window/pane names exercising `escapeChainArg` at every new
  position.
- **Unit — config:** merge-as-unit (overlay replaces list; `panes: []`
  opts out over a lower layer that declared panes; absent inherits),
  validation (mode exclusivity, duplicate pane names, multi-focus with
  origin attribution), normalization materializing the default pane, and
  digest tests: adding the field changes the digest once for configured
  windows; a zero-window config's digest is unchanged (the documented
  §3 exception); identical pane config digests stably; `panes: []` vs
  omitted digest differently.
- **Unit — partial failure:** a `CreateSession` error leaves ensure
  reporting failure without a committed digest, and the following ensure
  accepts the identity-tagged session as already-running with the drift
  flag set (fake actuator; pins the §4 partial-failure semantics).
- **Integration (isolated `-L` socket):** created session has the
  expected pane count per window, pane cwd, pane environment (via
  `show-environment`/`send-keys` probe), active pane per window, and
  layout; a container-located window's pane command is asserted through
  the fake container actuator's rendered `ExecCommand`.
- Existing tmux, controller, config, and CLI tests stay green.

## 8. Recorded decisions

- Default flips in **one release**; digest churns once; honesty of the
  drift signal preferred over derivation-time invisibility.
- `panes` lists **additional** panes only; window fields keep describing
  the primary pane; no meaning of any existing YAML changes.
- Default layout is **50/50 side-by-side** (`split-window -h`).
- Container windows' panes run **container exec shells** (same binding,
  same workdir); panes have no `location` of their own.
- `panes` merges **as a unit** across layers.
- Panes are **not reconciled**; closing one is not drift.
- The **implicit window's default pane is digest-silent** (§3 exception):
  the implicit window already lives outside the digest, and this spec
  does not move it.
- **Partial creation is recoverable, not prevented**: identity keys stay
  first in the chain so `stop` + `open` always recovers a partial
  session; the drift flag on live sessions clears only through
  stop-and-reopen.
