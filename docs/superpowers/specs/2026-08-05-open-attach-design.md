# Open/attach slice — the first mutating commands

**Status:** Approved design

**Date:** 2026-08-05

**Scope:** Design §13 step 3, second part: `projectmux open` (observe,
ensure, record, attach) and `projectmux attach`, host-side only. Adds the
per-workspace filesystem lock (§9), the tmux session actuator, the
controller's `Ensure`, and one store method (`AdoptSessionName`). The real
container adapter is the next slice; container-requiring workspaces refuse
honestly here.

## 1. Outcome

- **`internal/lock`** — the §9 per-workspace filesystem lock.
- **`internal/tmux`** — grows the actuator half: session/window creation
  and focus selection on the existing `Client` and runner.
- **`internal/controller`** — `Ensure(ctx, Desired) (EnsureResult, error)`
  implements the §9 convergence loop over the existing `Observe` and
  `BuildPlan`; a new `SessionActuator` interface is consumed here.
- **`internal/state`** — one new method, `AdoptSessionName`.
- **`internal/cli`** — `open` and `attach` commands, the bare
  `projectmux <workspace>` form, terminal attachment, exit code 6.

## 2. Observable behavior

### `projectmux open [<workspace>] [--no-attach] [--json] [--compact]`

Observe, ensure, record, attach (design §8). On success the workspace's
tmux session exists carrying the three `@dev_` identity keys; on creation
its windows come from the merged configuration; state is recorded; and the
terminal is attached. Drift on an already-live session is **reported, not
applied** — window reconciliation of live sessions is deliberately out of
scope (recorded decision), matching §12's "idempotent reopen".

- `--no-attach` skips terminal attachment (automation and autostart).
- `--json` implies `--no-attach` and emits a versioned envelope
  (`schema_version: 1`): the `config` command's `workspace` block, the
  action taken (`created` / `adopted` / `already-running`), the assigned
  session name, and a drifted flag.
- **Bare form:** `projectmux <workspace>` dispatches to `open`. Dispatch
  tries known commands first, then treats the token as a workspace name;
  a mistyped command therefore exits 4 (unknown workspace), not 2 — the
  §8-mandated trade, documented in help. The bare form takes no flags
  (a leading `-` is an unknown-command usage error); flagged invocations
  use the explicit `open` form, which — like every existing command —
  takes flags before the positional workspace.

### `projectmux attach [<workspace>] [--json] [--compact]`

Observes and attaches only; it never creates and never mutates: no
operation lock (§9 — observation-only commands do not take it) and no
store writes. It targets only the identity-matched session.

- Live matching session → attach (or `--json`: report without attaching).
- Confirmed absent → error "no live session for this workspace; run
  `projectmux open`", exit 1.
- Unobservable tmux → refusal, exit 6: attach cannot know, and does not
  guess.

### Terminal attachment

Outside tmux (`TMUX` unset): the process **exec**s
`tmux attach-session -t <name>` (execve of the resolved tmux path), so
the terminal becomes the session. Inside tmux: `tmux switch-client -t
<name>` via the runner (recorded decision). `--no-attach`/`--json` print
the session name instead.

### Exit code 6 — `ExitRefused`

Additive to the stable 0–5 contract: the plan refused (unobservable tmux,
foreign or keyless occupant on a candidate name, contradictory identity
keys) or attach faced an unobservable tmux. Automation distinguishes
"conflict — do not blindly retry" from generic failure (1). A refused
`open` records a failed `open` operation with the refusal text as its
bounded error summary, so `status` explains it afterwards.

## 3. `internal/lock`

`Acquire(dir, workspaceID string, timeout time.Duration) (*Lock, error)`:

- Lock file `<state-root>/locks/<workspace-id>.lock` (directory created
  as needed). `flock(LOCK_EX | LOCK_NB)` polled every ~100 ms until the
  timeout; a typed `ErrLockHeld` names the workspace when it expires
  (CLI: exit 1, "another projectmux operation holds this workspace").
- `Release()` unlocks and closes. The lock file is never deleted (unlink
  + flock races).
- **Close-on-exec is load-bearing:** PR 54's worst defect was a detached
  tmux server inheriting the lock fd and holding it forever (design §2).
  Go's `os.OpenFile` opens O_CLOEXEC on Linux; a test execs a child while
  the lock is held and proves the child does not hold it.
- Local filesystems only (state dir), same as SQLite itself.

## 4. tmux actuator

`controller.SessionActuator`, implemented by `*tmux.Client`:

```go
type WindowSpec struct {
	Name    string
	Command string // empty => default shell
	Dir     string // absolute working directory
	Focus   bool
}

type SessionSpec struct {
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string // also the default window dir
	Windows     []WindowSpec // at least one
}

type SessionActuator interface {
	CreateSession(ctx context.Context, spec SessionSpec) error
}
```

- `CreateSession` issues **one** tmux invocation with chained commands
  (verified on tmux 3.4): `new-session -d -s <name> -c <dir> -n <first>
  [cmd] \; set-option @dev_workspace_id … \; set-option @dev_slug … \;
  set-option @dev_worktree … \; new-window … \; select-window …`. One
  subprocess makes creation-with-identity near-atomic, closing most of
  the window in which a concurrent observer could see a keyless session.
- Window commands are passed as tmux's shell-command argument — tmux runs
  them in the pane's default shell; projectmux itself never interpolates
  into a shell (design §11). A window whose command exits immediately
  closes; that is tmux semantics, documented, not fought — agent and
  command windows are long-running by design.
- `WindowSpec` is derived in the CLI wiring from `config.Window`: `agent`
  and `command` become the command string; `shell: true` an empty one;
  relative `cwd` joins the worktree (already validated non-escaping);
  exactly one merged window carries focus (validation guarantees it).
  `location: container` never reaches the actuator this slice (§6).

## 5. `Ensure` — the §9 loop

`Controller` gains an `Actuator SessionActuator` field and:

```go
type EnsureAction string // "created", "adopted", "already-running"

type EnsureResult struct {
	Action  EnsureAction
	Session string // the actual session name
	Drifted bool
}

func (c *Controller) Ensure(ctx context.Context, d Desired, windows []WindowSpec, lockDir string, lockTimeout time.Duration) (EnsureResult, error)
```

Order of operations, all under the workspace lock:

1. `lock.Acquire(lockDir, d.Workspace.ID, timeout)`; defer release.
2. `RegisterWorkspace` (idempotent upsert — registration is a mutating
   command's job, never an observation command's).
3. Final observation: `Observe(ctx, d)` **under the lock** (§9).
4. `BuildPlan(snapshot)`.
5. **Refuse:** record operation `open`/`failed` with the refusal text,
   return typed `RefusalError` (exit 6). Refusal is global — nothing was
   or will be mutated.
6. **Container gate:** if the plan's container action ≠ `none` and the
   controller has no container actuator (this slice: always nil), record
   `open`/`failed` and return typed `ErrContainerActionUnsupported`
   before any mutation (exit 1). Capability-shaped: the container slice
   plugs in an actuator; `Ensure` does not change.
7. **Names are resolved per action, never pre-allocated:**
   - `create`: `AllocateSessionName` (UNIQUE constraint is the collision
     mechanism) → `CreateSession` with identity keys and windows →
     re-observe → commit `CommitReconciliation{AppliedDigest: &digest,
     Operation: open/ok}`. The applied digest is recorded only here —
     creation is the only moment configuration is fully applied.
   - `adopt`: `AdoptSessionName(id, liveName)` records the observed name
     (includes live Phase-1 Bash sessions — §13 step 7). No applied
     digest: adoption applies nothing, so drift stays honest. Commit
     operation `open`/`ok`.
   - `none`: commit operation `open`/`ok`.
8. Post-create re-observation failing (created but unconfirmable) commits
   `open`/`failed` with the observation error and returns it — the next
   `open` adopts via the identity keys (§9 crash recovery; the same path
   recovers a crash between create and commit).

`attach` never calls `Ensure`.

## 6. Host-only container gating

The CLI wires a `hostOnlyContainerObserver` replacing the observation
slice's always-erroring one **for open** (status keeps its existing
unprobed observer and rendering):

- `DiscoverContainer`: stat the workspace's devcontainer config —
  explicit `devcontainer.config` path, else `.devcontainer/devcontainer.json`,
  else `.devcontainer.json`. Absent **and** `enabled` is `auto` →
  `(nil, nil)`: no container applies; the plan's container action is
  `none` and plain repositories work end to end. Present, or
  `enabled: true` → return an error (→ snapshot `unknown` → plan
  `probe-first` → `Ensure`'s container gate refuses with "container
  support is not implemented in this build"). Docker is never touched.
- `ProbeContainer` (stored binding exists): error, same funnel. No
  binding can exist in practice before the container slice.

## 7. `AdoptSessionName`

`AdoptSessionName(workspaceID, name string, now time.Time) error` — one
immediate transaction, patterned on `AllocateSessionName`
(store.go:200): sets `actual_session = name` for the workspace. The
UNIQUE constraint still governs: a violation (the name is recorded for
another workspace) returns a typed conflict error, which `Ensure`
surfaces as a refusal — never an overwrite. Adopting the name a
workspace already has recorded is a no-op success. Idempotent
re-registration and rebuild flows reuse it.

## 8. CLI wiring

- `open.go`: flag parsing per existing conventions; builds `Desired` via
  the `status` pipeline (defaults → resolve → load); derives
  `[]WindowSpec`; calls `Ensure` with `state.Root()`-derived lock dir and
  a 10 s lock timeout; then attaches (exec / switch-client / none). JSON
  envelope written before any attach can replace the process (`--json`
  implies `--no-attach` anyway).
- `attach.go`: observation-only: resolve, `Observe` (no lock),
  identity-matched live session or the errors of §2, then attach.
- `cli.go`: dispatch cases `open`/`attach`; bare-form fallback in the
  `default` arm (token without leading `-` → `runOpen`); usage entries;
  `ExitRefused = 6`; `RefusalError`, `ErrContainerActionUnsupported`,
  and `lock.ErrLockHeld` mapped in `exitCode`.
- Attach seams for tests: the exec and switch-client steps sit behind
  package variables (the established seam pattern in `wiring.go`); a real
  terminal attach cannot run in CI.

## 9. Testing

- **`internal/lock`:** mutual exclusion under `-race` (two goroutines,
  one holder at a time), timeout → `ErrLockHeld`, release-then-reacquire,
  and the child-inheritance test: exec a child while locked; a second
  process must still be blocked from acquiring only until `Release` —
  i.e. the child holds nothing (O_CLOEXEC, the PR-54 failure class).
- **`internal/state`:** `AdoptSessionName` round-trip, conflict with an
  existing assignment on another workspace (typed error, nothing
  changed), no-op re-adoption, `ErrNotFound` for unregistered workspaces.
- **`internal/controller`:** table-driven `Ensure` over plan outcomes
  with fakes (a new `fake.SessionActuator` recording specs): create
  commits applied digest + ok; adopt records the live name and no
  digest; none commits ok; refuse records failed and mutates nothing
  (actuator sees zero calls); container gate fires before any actuator
  call; post-create observation failure commits failed and the next
  Ensure adopts (crash-window recovery); lock held across observe →
  mutate → commit (fake lock dir; second Ensure blocks).
- **`internal/tmux`:** real-tmux integration on isolated sockets:
  `CreateSession` yields the session with identity keys, named windows,
  working dirs, and focus (verified via the existing observer); the
  chained single-invocation form is exercised end to end.
- **`internal/cli`:** `open --json`/`--no-attach` envelopes and exit
  codes (0, 4 for unknown workspace, 6 for refusal, 2 for bare-form flag
  misuse); bare-workspace dispatch; attach's never-creates guarantee
  (fake actuator asserts zero calls, guarded store asserts no writes for
  attach); attach absent → 1 with the hint; attach unknown → 6;
  drift reported on `already-running`.
- **Lifecycle (real tmux + fake containers), design §12:** open →
  idempotent reopen → attach; adoption of a pre-seeded session carrying
  the three Phase-1 keys without creating, renaming, or wrong-session
  attachment (§13 step 7); foreign-squatter refusal; two concurrent
  `open`s of one workspace serialize on the lock with exactly one
  creation.
- Gates: `gofmt -l .` empty, `go vet ./...`,
  `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`.

## 10. Exclusions

No real container adapter (`devcontainer up`, Docker probes,
container-located windows, `exec` wiring), no window reconciliation of
live sessions, no `stop`, no autostart, no doctor, no daemon, no
workspace deletion. Store API grows only `AdoptSessionName`.

## 11. Decisions recorded

- Host-only first: the container adapter is its own next slice;
  container-requiring workspaces refuse with a typed capability error
  before any mutation.
- The capability gate lives in `Ensure` (nil container actuator ⇒ any
  non-`none` container action refuses), so the container slice plugs in
  without controller rework.
- Inside tmux, attaching means `switch-client`; outside, an exec of
  `attach-session`. Nested attach is never produced.
- Live sessions are never re-laid-out this slice; drift is reported, and
  the applied digest is written only at creation.
- Session names resolve per plan action (create → allocate, adopt →
  `AdoptSessionName`); nothing pre-allocates.
- `AdoptSessionName` conflicts are refusals, never overwrites.
- Session creation is one chained tmux invocation (verified on 3.4),
  making creation-with-identity near-atomic.
- Window commands are tmux shell-command arguments; a fast-exiting
  command closes its window (tmux semantics, documented).
- Exit 6 (`ExitRefused`) is additive; refused opens record a failed
  operation so `status` can explain them.
- `attach` takes no lock and writes nothing; unknown tmux refuses (6),
  confirmed absence fails with a hint (1).
- The bare `projectmux <workspace>` form is sugar for `open` with no
  flags; mistyped commands exit 4 by design (§8 trade, documented).
- The lock file lives under the state root, is polled non-blocking with
  a 10 s default timeout, is never unlinked, and must never be
  inheritable by children (O_CLOEXEC, tested).
