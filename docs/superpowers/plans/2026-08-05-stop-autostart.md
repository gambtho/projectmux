# Stop/Autostart Implementation Plan

> **Status note:** this slice was implemented and fully verified during
> the plan's materialize-verify pass (all gates green under `-race`,
> real-tmux lifecycle tests, and an isolated end-to-end smoke of
> open → stop → reopen → stop --container → autostart with every exit
> code asserted), then committed directly rather than re-transcribed
> task-by-task. The committed code is authoritative where it and the
> summaries below diverge. Spec:
> `docs/superpowers/specs/2026-08-05-stop-autostart-design.md`.

**Goal:** `projectmux stop` (the only destructive command: end the
workspace session, and with `--container` its bound container) and
`projectmux autostart` (start containers for eligible registered primary
worktrees at boot), plus the systemd user unit template.

**Architecture:** The controller gains two entry points that reuse the
existing observation and persistence machinery under the per-workspace
lock: `Stop` (session observation → refusal on uncertainty →
exact-match kill → optional container stop from the stored binding) and
`StartWorkspaceContainer` (container-only observation that deliberately
bypasses `Observe`/`BuildPlan`, whose global session-unknown refusal
would block every container start when no tmux server exists at boot).
Actuators gain `KillSession` and `StopContainer`, both idempotent on
confirmed absence. The CLI adds `stop` and `autostart` with the
deliberate no-stdout-contract amendment: when the structured report IS
the output (autostart's batch, a partially failed stop), the report goes
to stdout and a `reportedError` carries only a one-line summary to
stderr.

**Tech Stack:** Go stdlib only. Verified tool behaviors (tmux 3.4,
docker 29.x) recorded in the spec §2 are binding: `kill-session -t
"=name"` for exact matching, "can't find session" as idempotent success,
`docker stop` with a 30s `stopTimeout`, "No such container" as success.

## Global Constraints

- No new module dependencies; stdlib only. Linux/WSL only.
- `gofmt -l .` empty, `go vet ./...`, `go test ./... -count=1 -race`,
  `CGO_ENABLED=0 go build ./cmd/projectmux` before every commit.
- Exit codes 0–6 unchanged; JSON envelope changes are additive to schema
  version 1.
- Uncertainty never converts to absence; nothing is destroyed on an
  unknown observation (refusal → exit 6, recorded only for registered
  workspaces).
- The stored container binding is retained after `stop --container`,
  re-recorded with health `missing` — it is the identity autostart and
  reopen probe from.
- Amended stdout contract (spec §5): report-is-the-output commands write
  the report to stdout even on failure and return a `reportedError`
  whose one-line summary is the only stderr text; nothing further
  reaches stdout after the report.
- Operation names are threaded through the container-phase persistence
  helpers (`opName` parameter) so autostart's records say `autostart`,
  not `open`.
- Commit messages and code comments must not mention Claude or AI
  assistance.

---

### Task 1: Thread the operation name through container-phase persistence

**Files:** Modify `internal/controller/ensure.go`.

**Interfaces (produced):**

```go
func (c *Controller) recordFailure(workspaceID, opName, summary string)
func (c *Controller) recordStartFailure(workspaceID, opName string, err error)
func (c *Controller) commitOutcome(workspaceID, opName string, obs *ContainerObservation) error
func (c *Controller) ensureContainer(ctx context.Context, d Desired, snap Snapshot, action ContainerAction, opName string) (*ContainerObservation, bool, error)
```

`Ensure` and `createSession` pass `const opName = "open"`; the new
entry points pass their own names. Existing tests prove the open path
unchanged. Commit: `Thread the operation name through container-phase
persistence helpers`.

### Task 2: KillSession and StopContainer actuator methods

**Files:** Modify `internal/controller/interfaces.go`,
`internal/tmux/actuate.go`, `internal/container/adapter.go`,
`internal/controller/fake/fake.go`; tests in
`internal/tmux/actuate_test.go`, `internal/container/adapter_test.go`.

**Interfaces (produced):**

```go
// SessionActuator
KillSession(ctx context.Context, name string) error
// ContainerActuator
StopContainer(ctx context.Context, containerID string) error
```

- tmux: `kill-session -t "="+name`; a lowercased "can't find session"
  stderr is idempotent success (verified: exact-match `=` is valid for
  target-session commands). Killing the last session exits the server —
  also success.
- docker: `docker stop` under `const stopTimeout = 30 * time.Second`
  (SIGTERM grace is docker's default 10s); "no such container"
  (case-insensitive) is idempotent success.
- Fakes gain `KillErr`/`Killed []string` and `StopErr`/`Stopped
  []string`.

Commit: `Add KillSession and StopContainer to the session and container
actuators`.

### Task 3: Controller Stop

**Files:** Create `internal/controller/stop.go`,
`internal/controller/stop_test.go`.

**Interfaces (produced):**

```go
type StopResult struct {
	SessionStopped   bool
	SessionName      string
	ContainerStopped bool
	ContainerID      string
}

func (c *Controller) Stop(ctx context.Context, d Desired, stopContainer bool, lockDir string, lockTimeout time.Duration) (StopResult, error)
```

Semantics (spec §4), all under the workspace lock:

1. Load the stored record; `ErrNotFound` is tolerated and a
   `registered` flag gates every subsequent write — stopping an
   unregistered workspace writes nothing.
2. Observe sessions over the candidate set (desired name ∪ stored
   actual name). An unknown observation is a `RefusalError` (exit 6),
   recorded as failed only if registered; nothing is killed.
3. A live session must satisfy `SessionBelongsTo` — a foreign squatter
   under our name is a refusal, never a kill.
4. Kill the identity session (exact match). Absence is idempotent
   success.
5. With `stopContainer`, stop the container named by the stored
   binding. A failure after the session kill returns the partial
   `StopResult` alongside the error and records `failed`.
6. On success, re-record the binding with health `missing` via
   `commitOutcome` under `const opName = "stop"` — the binding is
   retained as the probe identity.

Ten controller tests cover the matrix, including partial failure and
the unregistered-writes-nothing invariant. Commit: `Add controller Stop
with idempotent destructive semantics`.

### Task 4: StartWorkspaceContainer (autostart engine)

**Files:** Create `internal/controller/autostart.go`,
`internal/controller/autostart_test.go`.

**Interfaces (produced):**

```go
type ContainerStartOutcome string

const (
	ContainerStarted        ContainerStartOutcome = "started"
	ContainerAlreadyRunning ContainerStartOutcome = "already-running"
	ContainerNoneApplies    ContainerStartOutcome = "none-applies"
)

func (c *Controller) StartWorkspaceContainer(ctx context.Context, d Desired, lockDir string, lockTimeout time.Duration) (ContainerStartOutcome, *ContainerObservation, error)
```

Container-only observation under the lock: `observeContainer` →
`ensureContainer(..., containerAction(snap), "autostart")` → commit
guarded by `stored != nil`. tmux is never consulted — tests assert zero
session queries — because at boot the global session-unknown refusal in
`BuildPlan` would block every container start. `ContainerStartError`
exit statuses persist exactly as on the open path. Commit: `Add
StartWorkspaceContainer for boot-time container ensuring`.

### Task 5: `projectmux stop` CLI

**Files:** Create `internal/cli/stop.go`, `internal/cli/stop_test.go`;
modify `internal/cli/cli.go` (dispatch case, usage entry, `reportedError`
type, `Main` doc-comment amendment).

**Interfaces (produced):**

```go
type stopEnvelope struct {
	SchemaVersion int                `json:"schema_version"`
	Workspace     workspaceInfo      `json:"workspace"`
	Session       stopSessionInfo    `json:"session"`
	Container     *stopContainerInfo `json:"container,omitempty"`
	Error         string             `json:"error,omitempty"`
}

type reportedError struct{ msg string }
```

Stop resolves identity only (`Root` → `LoadDefaults` → `Resolve`) —
deliberately no workspace config load, so a broken workspace YAML can
never block stopping. Error routing: a failure with nothing stopped
takes the normal no-stdout error path; after partial progress the
envelope (with `Error` populated) is emitted and a `reportedError`
returns the one-line stderr summary with exit 1. Seven CLI tests.
Commit: `Add projectmux stop with the partial-failure report contract`.

### Task 6: `projectmux autostart` CLI

**Files:** Create `internal/cli/autostart.go`,
`internal/cli/autostart_test.go`; modify `internal/cli/cli.go`
(dispatch case, usage entry).

Envelope: `{schema_version, workspaces: [{id, slug, outcome, reason?,
container_id?}]}` with outcome ∈ started | already-running | skipped |
failed. Sequential iteration over `Store.Workspaces()` filtered to
primaries; per workspace, in order:

1. `os.Stat(rec.Worktree)` **before** the config load — a vanished
   worktree is a visible `failed` entry, never a silent skip (auto's
   applicability check would misread absence as "does not apply";
   Codex review finding 3).
2. `config.Load` failure → `failed` with the load error.
3. `!cfg.Autostart` → `skipped`.
4. `StartWorkspaceContainer`; `ContainerNoneApplies` → `skipped`,
   errors → `failed`, otherwise the outcome string with the container
   ID.

One failure never stops the batch. Any `failed` entry → the report goes
to stdout, then `reportedError` → exit 1. Tests: the eligibility
matrix (non-primary filtered, autostart-off skipped, vanished-worktree
failed while the rest proceed, started with container ID and
`autostart`-named operation records), all-healthy exit 0, argument
rejection. Commit: `Add projectmux autostart for boot-time container
starts`.

### Task 7: systemd user unit template

**Files:** Create `contrib/systemd/projectmux-autostart.service`.

`Type=oneshot`, `ExecStart=%h/.local/bin/projectmux autostart` with an
adjust-the-path comment, `WantedBy=default.target`. Comments record the
two operational caveats: user units cannot order against the system
`docker.service`, and enablement is the dotfiles' decision — installing
the binary never enables it. Commit: `Add the systemd user unit
template for autostart`.

### Task 8: Lifecycle tests and final sweep

**Files:** Append to `internal/cli/lifecycle_test.go`.

Real tmux on an isolated socket, real SQLite store, fake container
tooling:

- `TestLifecycleOpenStopReopen` — open → stop kills the real session →
  second stop is an idempotent no-op → reopen creates fresh.
- `TestLifecycleStopKillsExactlyTheIdentitySession` — a same-prefix
  sibling (`<name>-scratch`) survives the kill; tmux's default prefix
  matching would have taken it out without the pinned `=` exact match.
- `TestLifecycleStopContainerThenAutostart` — `stop --container` kills
  the session, stops the bound container, retains the binding as
  missing; `autostart` skips without the config flag, then (flag
  enabled) restarts the container from the stored binding without
  touching tmux.

Final sweep, all run on the finished branch:

```bash
gofmt -l .            # empty
go vet ./...
go test ./... -count=1 -race
CGO_ENABLED=0 go build ./cmd/projectmux
```

Isolated end-to-end smoke (temp state/config/tmux roots, throwaway git
repo): open → stop (reports stopped) → stop again (no-op) → reopen
(`created`) → `stop --container` (container block present) →
`autostart` (skips without the flag) → usage error exits 2. All
asserted; all green. Commit: `Add stop and autostart lifecycle tests
against real tmux`.

---

## Self-review notes

- Spec §2 verified behaviors → Task 2; §3 autostart engine and
  worktree-stat ordering → Tasks 4, 6; §4 stop semantics → Tasks 3, 5;
  §5 contract amendment → Tasks 5, 6 and the `Main` doc comment; §6
  systemd → Task 7; §7 testing rows → Tasks 3–6, 8.
- All three Codex findings are implemented: op-name plumbing (Task 1),
  the reportedError stdout/exit resolution (Task 5), the
  vanished-worktree visibility check (Task 6).
- Discovery during Task 8: a fake `ProbeResult` left at its zero value
  (health `""`) makes `ensureContainer`'s single probe-first retry
  resolve to probe-first again and surface "unexpected container
  action" — a test-fixture bug, not a controller one; real probes
  return present/missing/error. The lifecycle fixture now probes
  missing after the stop.
