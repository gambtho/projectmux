# Stop/autostart slice — the destructive command and boot-time containers

**Status:** Approved design

**Date:** 2026-08-05

**Scope:** Design §8's `projectmux stop` (the only destructive command)
and `projectmux autostart` (§10), plus the systemd user unit template.
Completes the §13 step 3 command vocabulary; the dotfiles installer and
cutover remain later work.

## 1. Observable behavior

### `projectmux stop [--container] [--json] [--compact] [<workspace>]`

The only destructive command, deliberately idempotent — stopping twice
is success, and nothing is ever destroyed on uncertainty:

- **Live identity-matched session** → `tmux kill-session -t '=<name>'`
  (exact-match target verified: a `slab-2` sibling survives killing
  `slab`; killing the last session exits the server, which subsequent
  observation correctly reads as absence). Recorded operation
  `stop`/`ok`.
- **Confirmed absent** → success no-op ("no live session"). A
  kill racing a dying session ("can't find session", exit 1) is also
  idempotent success — the goal state holds (verified classification).
- **Unobservable tmux** → refusal, exit 6. Foreign occupants are
  irrelevant: stop targets only the identity-matched session, never a
  name.
- **`--container`** → after the session, `docker stop` the *stored*
  bound container. Success (including already-stopped, exit 0 verified,
  and "No such container" — gone is the goal state) records
  `health=missing` on the retained binding (§7: confirmed absence; the
  binding is never cleared). No stored binding → note, not an error.
  A real stop failure → failed op with bounded stderr, exit 1, honestly
  reported as partial (the session kill already happened).
- **Unregistered workspace** → observes desired identity only; a live
  identity session is still killed (crash-recovery symmetry); with
  nothing live, success no-op.
- Runs under the workspace lock (§9). No registration is performed —
  stop never creates state for unknown workspaces.
- JSON envelope (`schema_version: 1`): `workspace` block,
  `session: {"stopped": bool, "name"?}`,
  `container: {"stopped": bool, "container_id"?}` present only when
  `--container` was requested and a binding existed.

### `projectmux autostart [--json] [--compact]`

No arguments. Iterates **registered primary** workspaces straight from
the store — no filesystem resolution; identity comes from the stored
record, and a vanished worktree simply fails that entry's config load:

- **Eligible** = `is_primary` AND config `autostart: true` AND a
  container applies (`enabled: true`, or `auto` with a devcontainer
  configuration on disk — the adapter's `Applies`).
- Per eligible workspace, under its lock: the existing container phase
  (observe container → plan → `start`/`acquire` via the adapter →
  commit). **No tmux session is created or observed** — see §3; §8/§10:
  "It does not create tmux sessions merely to satisfy boot behavior."
- Workspaces run **sequentially** (no boot-time Docker stampede; ordered
  report). One failure never stops the loop.
- Per-workspace outcomes: `started`, `already-running`,
  `skipped` (+reason: not primary is filtered before reporting;
  autostart false; no container applies), or `failed` (+error — config
  load failures count as failed so boot logs surface them, not as
  silent skips). Recorded operations use the name `autostart`.
- Exit 0 when every *eligible* workspace succeeded; 1 when any failed.
  JSON envelope: `{"schema_version": 1, "workspaces": [{"id", "slug",
  "outcome", "reason"?, "container_id"?}]}`.

### Systemd unit template

`contrib/systemd/projectmux-autostart.service` — `Type=oneshot`,
`ExecStart=%h/.local/bin/projectmux autostart` (comment: adjust to the
install location; user units do not search PATH portably),
`WantedBy=default.target`, and a header comment noting Docker must
already be running and that enabling the unit is the installer's
(dotfiles') decision, not ProjectMux's (§10). No enablement logic
anywhere in this repository.

## 2. Controller: `Stop`

```go
type StopResult struct {
	SessionStopped   bool
	SessionName      string // the killed session's name, when stopped
	ContainerStopped bool
	ContainerID      string // the stopped container, when stopped
}

func (c *Controller) Stop(ctx context.Context, d Desired, stopContainer bool, lockDir string, lockTimeout time.Duration) (StopResult, error)
```

Under the lock: read the stored record (absence is fine — no
registration), observe the session (candidates as in `Observe`), then:

- session `unknown` → record `stop`/`failed` with the refusal text (only
  when the workspace is registered — an unregistered workspace has no
  record to write), return `RefusalError`.
- live identity match → `Actuator.KillSession(ctx, name)`; a
  "can't find session" failure is treated as success (idempotency, the
  race above); other failures record `stop`/`failed`, return the error.
- `stopContainer` and a stored binding exists →
  `ContainerAct.StopContainer(ctx, id)`; success → the final commit
  carries `Container: {Health: missing}` (retained binding, §7);
  failure → `stop`/`failed` with bounded stderr, typed error, exit 1.
- Single final commit: `CommitReconciliation`/`RecordOperation` with
  operation `stop`/`ok` (+ the missing observation when the container
  was stopped), only for registered workspaces.

Identity comparison uses `SessionBelongsTo`; a live session matched by
ID whose other keys contradict refuses exactly as `BuildPlan` would
(`stop` reuses `refusalFor`-equivalent checks via a small helper rather
than `BuildPlan` itself, since no plan actions are wanted).

## 3. Controller: `StartWorkspaceContainer` (autostart's engine)

```go
type ContainerStartOutcome string // "started", "already-running", "none-applies"

func (c *Controller) StartWorkspaceContainer(ctx context.Context, d Desired, lockDir string, lockTimeout time.Duration) (ContainerStartOutcome, *ContainerObservation, error)
```

**Container-only observation — tmux is never consulted.** At boot there
is no tmux server; going through `Observe`/`BuildPlan` would make the
global session-`unknown` refusal block every container start. Instead:
lock → stored record → `observeContainer` (the existing method: Applies
gate under `auto`, probe stored binding or discover) → `containerAction`
→ the existing `ensureContainer` phase (including the probe-first
retry) → commit (operation `autostart` + the observation). Outcomes:
`none` with a present observation → `already-running`; `none` without →
`none-applies` (reported as skipped); `start`/`acquire` → `started`
after the idempotent `up`.

**Operation-name plumbing (a prerequisite refactor):** the container
phase's persistence helpers currently hard-code the operation name
`"open"` (`recordFailure`, `recordStartFailure`, `commitOutcome` in
ensure.go). `ensureContainer` and those helpers gain an operation-name
parameter (or the name becomes a field threaded through the call):
`Ensure` passes `"open"`, `Stop` passes `"stop"`, and
`StartWorkspaceContainer` passes `"autostart"`, so every recorded
operation — including retry and start failures inside the shared
container phase — carries the command that ran it. Reusing
`ensureContainer` "verbatim" is explicitly not possible without this.

Autostart (CLI) iterates `store.Workspaces()`, filters `IsPrimary`,
builds `Desired` from the stored record (`resolve.Workspace{ID, Slug,
Worktree, SessionName: ProposedSession, IsPrimary}`), and per workspace:

1. **Stat the stored worktree first.** Config loading reads only
   config-root files (load.go) and the adapter's `Applies` treats
   absent devcontainer files under `auto` as "does not apply" — so a
   vanished worktree would otherwise sail through as a silent skip.
   A missing worktree is reported as `failed` ("worktree no longer
   exists"), giving boot logs the visibility this spec promises.
2. Load config by slug (failure → `failed` entry).
3. Check `autostart: true` (false → skipped).
4. Call `StartWorkspaceContainer`.

## 4. Adapter additions

- `SessionActuator.KillSession(ctx context.Context, name string) error` —
  `tmux kill-session -t '=<name>'`; "can't find session" on stderr with
  exit 1 → nil (idempotent success, verified); other failures → error
  with bounded stderr.
- `ContainerActuator.StopContainer(ctx context.Context, containerID string) error` —
  `docker stop <id>` with a **30-second** subprocess timeout
  (`docker stop`'s default SIGTERM grace is 10s before SIGKILL —
  verified 10s on busybox — so the probe-class 5s default would
  false-fail routinely). Exit 0 (including already-stopped) → nil;
  "No such container" → nil (gone is the goal state); other failures →
  error with bounded stderr.
- Fakes: `fake.SessionActuator` gains `Killed []string` + `KillErr`;
  `fake.ContainerActuator` gains `Stopped []string` + `StopErr`.

## 5. CLI

- `stop.go` / `autostart.go` following the established command shape
  (flag conventions, envelope patterns, seams). `stop` resolves like
  `open` (name or cwd); `autostart` takes no workspace.
- Dispatch cases + usage entries; the bare-form fallback is unaffected
  (`stop`/`autostart` are matched before the workspace fallback).
- Exit codes unchanged (0–6); autostart's any-failure → 1.
- **Deliberate amendment to the no-stdout-on-failure contract**
  (cli.go's `Main` doc: "writes nothing to stdout for a failing
  command"): that invariant assumed single-operation commands, where a
  failure means the output would be garbage. Autostart is a
  multi-workspace batch and `stop --container` can partially succeed —
  for these two commands the structured report (JSON or human) **is**
  the output and is written to stdout even when the command exits
  nonzero; the failure detail lives inside the report. Mechanically:
  the command writes its report, then returns a typed sentinel error
  (`errPartialFailure`-style, carrying only a one-line summary) that
  `Main` prints to stderr and maps to exit 1 — nothing further is
  written to stdout after the report. `Main`'s doc comment is amended
  to state the exception explicitly, and every single-operation command
  keeps the old contract unchanged. Automation contract: check the exit
  code, then parse stdout — which now works for both total success and
  partial failure.

## 6. Testing

- **Fakes:** the stop matrix (live/absent/unknown/unregistered ×
  `--container` × bound/unbound/stop-failure): refusal on unknown (6),
  idempotent no-ops, kill-race success, partial-failure reporting,
  commit contents (missing recorded only on successful container stop;
  no writes for unregistered workspaces); autostart eligibility matrix
  (non-primary filtered, autostart false skipped, none-applies skipped,
  invalid config failed, one-failure-continues, sequential order,
  **zero SessionActuator/SessionObserver calls** — the fake session
  observer asserts it was never consulted).
- **Real tmux:** stop kills exactly the identity session while a
  same-prefix sibling survives; stop of the last session (server exit)
  followed by reopen.
- **Real docker (busybox):** `StopContainer` stops a running container
  within the timeout; the next probe classifies `missing`; already-
  stopped and removed containers are success.
- **Lifecycle:** open → stop → reopen (fresh create); open → stop
  `--container` (fake tooling) → binding retained, health missing;
  autostart over a seeded store with fake tooling (one eligible started,
  one skipped, one failed → exit 1).
- Gates: `gofmt -l .` empty, `go vet ./...`,
  `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`.

## 7. Exclusions

No workspace deletion/unregistration, no doctor, no `list` live probing,
no live-window reconciliation, no unit enablement or installer work, no
parallel autostart.

## 8. Decisions recorded

- Stop defaults to session-only; `--container` extends it. Conservative
  default for the one destructive command.
- Stop is idempotent end to end: absent/vanished sessions and
  stopped/removed containers are success, verified against real tmux
  and docker error shapes ("can't find session", "No such container").
- Nothing is destroyed on uncertainty: unknown tmux refuses (6).
- Stop performs no registration; unregistered workspaces get identity-
  only observation, and no operations are recorded for them.
- A successful container stop records `missing` on the retained binding
  — confirmed absence by our own hand.
- Autostart's engine is container-only observation: `Observe`/
  `BuildPlan` are deliberately bypassed because boot-time tmux absence
  would globally refuse; no session is observed or created.
- Autostart is sequential, config-load failures are `failed` (exit 1)
  rather than silent skips, and eligibility is primary + `autostart:
  true` + container-applies.
- `docker stop` gets a 30s subprocess timeout (its default SIGTERM
  grace is 10s — verified; the 5s probe default would false-fail).
- The systemd template ships without enablement logic; `%h/.local/bin`
  path with an adjust-comment (user units don't search PATH portably).
- The container phase's persistence helpers take an operation name
  (open/stop/autostart) — hard-coded `"open"` made verbatim reuse
  impossible (Codex review finding).
- Autostart stats the stored worktree before config load: a vanished
  worktree is a `failed` boot-log entry, never a silent skip — config
  loading never touches the worktree, and `auto`'s applicability check
  would misread absence as "does not apply" (Codex review finding).
- The no-stdout-on-failure contract is deliberately amended for the two
  commands whose report is the output (autostart, partial stop): report
  on stdout, typed summary error to stderr, exit 1 (Codex review
  finding).
