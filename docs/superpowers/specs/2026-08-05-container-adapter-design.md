# Container-adapter slice — real devcontainer/Docker support

**Status:** Approved design

**Date:** 2026-08-05

**Scope:** The design-§5 container adapter, made real: probe and
discovery via Docker, startup via `devcontainer up`, container-located
windows via `docker exec` built from the stored binding, wired into
`Ensure`'s existing capability gate. Also lands the recorded deferrals
that live in the touched code: exported identity predicate,
numeric-window-name validation, and the static
`enabled: false` + `location: container` contradiction. `stop` and
autostart remain later slices.

## 1. Outcome

- **`internal/container`** — the one package invoking `docker` and
  `devcontainer` (design §5), on the existing runner, with
  `dockerBinary`/`devcontainerBinary` test seams mirroring `tmuxBinary`.
  Implements `controller.ContainerObserver` and the new
  `controller.ContainerActuator`.
- **`internal/controller`** — `ContainerActuator` interface and
  `Controller.ContainerAct` field; window derivation split into pure
  intents (CLI) and post-binding rendering (Ensure); the container
  phase of `Ensure` executes plans instead of refusing when an actuator
  is present; `SessionBelongsTo` exported and reused.
- **`internal/cli`** — `open` wires the real adapter (replacing
  `hostOnlyContainerObserver`), `status`/`attach` wire the real prober
  (replacing `unprobedObserver`); `open --json` gains an optional
  `container` block; window derivation produces intents.
- **`internal/config`** — two validation additions.

## 2. Observable behavior

### `open` on a container workspace

`enabled: true`, or `auto` with a devcontainer configuration on disk:

- Under the workspace lock, the **container is ensured before the
  session** (its windows need the binding): plan `start` runs
  `devcontainer up --workspace-folder <worktree> [--config <path>]`
  bounded by the config's `start_timeout` (default 5m). The §9 split
  holds: the filesystem lock spans the start; no SQLite transaction
  does.
- `up`'s result JSON — verified shape
  `{"outcome":"success","containerId":…,"remoteUser":…,"remoteWorkspaceFolder":…}`,
  emitted as the **last line of stdout** (logs go to stderr) — becomes
  the binding observation, committed atomically with the operation via
  the existing `CommitReconciliation.Container` field.
- Failures: `up` non-success or timeout → failed `open` operation with
  bounded stderr, exit 1, **stored binding never cleared** (§7). Docker
  unreachable at observation → health `unknown` → plan `probe-first` →
  one re-probe; still failing → failed op, exit 1: open never starts or
  guesses on uncertainty (§9).
- Idempotent reopen with a live container: probe `present` → container
  action `none` → no `up`. A running container with **no** stored
  binding costs one fast no-op `up` (~1s, verified) to acquire the
  authoritative user/workdir — see §4 discovery.

### Window placement

The config contract (config.go's `Location` comment) becomes real:

- unset `location` → **container when one applies to the workspace,
  host otherwise**;
- `location: host` → host, always;
- `location: container` → requires a container: statically contradictory
  with `enabled: false` (validation error, §6); with `auto` that
  resolved to none, a typed error inside the locked loop — before any
  actuation, with a failed `open` operation recorded so `status` can
  explain it (§4).
- Container windows run
  `docker exec -it [-u <container_user>] -w <workdir> <container_id> …`
  rendered from the **stored binding** (the §7 columns' purpose).
  Relative `cwd` joins the binding's container workdir with POSIX
  `path.Join`, never the host worktree. `sh -lc '<command>'` for
  agent/command windows, `sh -l` for shell windows. The pane dies when
  the container does; `docker exec` on a stopped container prints a
  clear "is not running" error in the pane (verified).

### `status` and `attach`

Both wire the real prober. `status`'s container `observation` becomes
honest live data: `{"attempted": true, "health": …, "error": …?}` after
a probe or discovery, retaining `{"attempted": false, "reason": …}` for
not-applicable cases (devcontainer disabled). A stored `present` can now
be contradicted by a live `missing`/`unknown` in one report. `list`
stays stored-only (its contract is last-observed state; batched live
probing is a recorded follow-up). `attach` remains lock-free and
mutation-free.

### `open --json`

Gains an optional `container` block —
`{"kind": …, "container_id": …, "health": …}` — present when a container
is in play, plus a `container_windows_stale: true` flag on the
replacement-into-live-session case (§4); both additive to schema
version 1.

## 3. `internal/container` adapter

`container.Adapter{Timeout, StartTimeout time.Duration}` — `Timeout`
(zero → 5s) bounds probes and discovery; `StartTimeout` (zero → the
config value passed per call; the field is a test override) bounds `up`.

- **`ProbeContainer(ctx, binding)`** — one
  `docker inspect -f '{{.State.Running}}' <id>`:
  - `true` → `present`;
  - `false` (exists, stopped) → `missing`;
  - exit 1 with stderr containing `no such object`
    (**case-insensitive** — docker 29 lowercases it, older daemons
    capitalize) → `missing`;
  - anything else — daemon unreachable, timeout, unrecognized output —
    → `unknown`. Uncertainty never converts to absence.
  - Paused and restarting containers also classify as `missing` — not
    running is what matters to open, and the idempotent `up` reconciles
    them; a finer distinction is deliberately out of scope.
- **`DiscoverContainer(ctx, ws, cfg)`** — for `auto` resolution and
  post-rebuild reacquisition:
  1. Devcontainer configuration on disk? Explicit `devcontainer.config`
     path, else `.devcontainer/devcontainer.json`, else
     `.devcontainer.json`. Absent and `enabled: auto` → `(nil, nil)`
     (no container applies). Stat errors that are not IsNotExist →
     error (unknown funnel). `enabled: true` skips this check —
     a container always applies.
  2. Configuration applies →
     `docker ps -a --filter label=devcontainer.local_folder=<worktree>`
     (label verified present with the exact worktree path). Health stays
     truthful — `missing` means confirmed absence (design §9), so a
     running match is **never** reported missing:
     - one **running** match → `{present, id}` with **no user/workdir**
       (labels cannot supply them): a truthful but *incomplete* binding
       observation. The plan turns this into `acquire` (§4), and the
       idempotent `up` (~1s no-op on a running container, verified)
       supplies the authoritative binding. `status` on such a workspace
       reports health `present` — never a false `missing`.
     - one **stopped** match → `{missing, id}`; none → `{missing}`.
     - **more than one running** match → error (ambiguity is
       uncertainty; no claimant is picked).
- **`Applies(ctx, ws, cfg) (bool, error)`** (observer) — the
  applicability check alone: `enabled: true` → always true; `auto` →
  the devcontainer-configuration stat of step 1 (IsNotExist → false;
  other stat errors → error). `Observe` consults it **before probing a
  stored binding** under `auto` (§4) — deleting `devcontainer.json`
  must stop the workspace being container-shaped, stored binding or
  not.
- **`StartContainer(ctx, ws, cfg)`** (actuator) — `devcontainer up`
  bounded by `cfg.DevContainer.StartTimeout`; parses the **last stdout
  line** as the result JSON. Failures return a typed
  `*StartError{ExitCode int, Stderr string, TimedOut bool}` so the
  recorded operation preserves the real exit status and bounded stderr
  (design §9): non-`success` outcome, unparseable output, start
  failure, or timeout. A `success` result is **validated before use**:
  `containerId` and `remoteWorkspaceFolder` must be non-empty
  (`remoteUser` may be empty → `-u` omitted); missing load-bearing
  fields are a `StartError`, and nothing downstream — window rendering
  or session mutation — sees an invalid binding. Valid success →
  `ContainerObservation{Health: present, Kind: "devcontainer",
  ContainerID, ContainerUser: remoteUser, Workdir:
  remoteWorkspaceFolder}`.
- **`ExecCommand(binding, command, relDir string, env map[string]string) string`**
  (actuator, pure) — renders the §5 execution request as a tmux window
  command string: `docker exec -i -t` + `-u <user>` when non-empty +
  `-w <path.Join(binding.Workdir, relDir)>` + one `-e KEY=VALUE` per
  configured environment entry in sorted key order + id +
  `sh -lc '<command>'` (empty command → `sh -l`). The environment must
  be passed explicitly: tmux's `-e` sets pane variables on the **host**
  side only, and `docker exec` forwards nothing into the container —
  without these flags the every-window environment contract
  (open/attach spec §4) would silently break while the digest still
  recorded as applied. Shell quoting through one tested
  single-quote-escaping function — the only place quoting happens.
  ExecCommand never inspects live state; a pane whose container died
  shows docker's own error.

## 4. Controller changes

```go
type ContainerActuator interface {
	StartContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (ContainerObservation, error)
	ExecCommand(binding state.ContainerBinding, command, relDir string, env map[string]string) string
}
```

`Controller` gains `ContainerAct ContainerActuator`. Nil preserves
today's behavior exactly: any **mutating** container action
(`start`/`acquire`) refuses with `ErrContainerActionUnsupported` after
the read-only probe-first retry resolves what it can.

**`ContainerObserver` gains `Applies(ctx, ws, cfg) (bool, error)`**, and
`Observe`'s container step changes for `enabled: auto`: applicability is
checked **before** any stored binding is probed. Not applicable → the
container snapshot is empty (exactly like `enabled: false`); the stored
binding is retained in the store but ignored, and unset-location windows
resolve to the host. An applicability error is the usual unknown funnel.
`enabled: true` skips the check. This closes the hole where deleting
`devcontainer.json` left an old binding — and container-shaped windows —
applicable forever (the pre-existing observe path probed the binding
first, observe.go:109).

**A fourth container action, `acquire`**, keeps `BuildPlan` pure while
handling discovery's truthful present-but-incomplete observation:
`containerAction` returns `acquire` when the observation is `present`
with an empty `Workdir` (the discovery shape — probes of stored bindings
and `up` results always carry one). `acquire` executes as the idempotent
`StartContainer`. A refusing plan still carries container action `none`
(the global-refusal invariant is unchanged).

**Window intents replace pre-rendered specs as Ensure's input.** The
CLI derives, purely from configuration:

```go
type WindowIntent struct {
	Name     string
	Command  string // empty => shell window
	RelDir   string // config cwd, relative; "" => workspace root
	Focus    bool
	Location WindowLocation // host | container | auto (unset)
}
```

`Ensure(ctx, d, intents, lockDir, lockTimeout)` renders concrete
`WindowSpec`s **after** the container phase:

- host / auto-without-container → `Dir = filepath.Join(worktree,
  RelDir)`, command unchanged (today's behavior);
- container / auto-with-container → `Command =
  ContainerAct.ExecCommand(binding, cmd, relDir)`, `Dir = worktree`
  (the pane's host cwd is irrelevant);
- `location: container` while no container applies → typed error
  before any actuation, failed op recorded.

**Ensure order** (all under the lock): register → observe → plan →
**refuse (global, first, unchanged)** → container phase → render
windows → session action → single commit (container observation +
operation + applied digest only on confirmed creation):

- container `none` → skip;
- `start` and `acquire` → `StartContainer` (idempotent `up`); failure →
  failed op recording the `StartError`'s **real exit status** and
  bounded stderr (`Operation.ExitStatus` — design §9's requirement),
  typed error, no session mutation;
- `probe-first` (observation errored) → **one** retry of the same
  observation kind: a stored binding retries `ProbeContainer`; an
  unbound workspace retries `DiscoverContainer` (probe requires a
  binding that does not exist in that case). Retry outcomes: `present`
  with a complete binding → proceed; `present` incomplete → acquire;
  `missing` → start; `(nil, nil)` (auto resolved to none on retry) →
  proceed container-free; error → failed op, typed error. Both retry
  paths are tested. Open never mutates on uncertainty.
- The applied digest continues to assert the whole document was
  actuated — now including that container windows were rendered
  against a live binding.

**Container replacement and live sessions — an explicit, narrowed
promise:** when the session action is `none`/`adopt` (live session, no
reapply — the open/attach decision stands) and the container phase
started or replaced a container, `open` does **not** recreate container
windows in the live session: panes bound to the dead container have
died or show docker's error, and new windows are created only at
session creation. `open` says so instead of implying repair — human
output and the JSON envelope carry a `container_windows_stale` note
recommending `stop`-and-reopen (or killing the session) to rebuild.
Live-window reconciliation remains the recorded follow-up; silent
partial repair is worse than a truthful limitation.

**Runner contract change:** `run.Run` currently discards captured
output when the context expires (run.go:87 returns an empty `Result`).
It now returns the partially captured stdout/stderr **alongside** the
timeout/cancellation error, so `StartContainer` can preserve a bounded
stderr summary for timed-out `up` runs. Additive: existing callers
ignore the `Result` on error today; the runner tests pin the new
behavior.

**`SessionBelongsTo(s LiveSession, ws resolve.Workspace) bool`** is
exported (the three-key predicate); `plan.go`, `status.go`, and
`attach.go` use it — closing the drift-risk deferral from #6/#7.

## 5. CLI wiring

- `open`: `hostOnlyContainerObserver` is deleted; the observer and
  actuator are `container.Adapter` seams
  (`newContainerObserver`/`newContainerActuator` package vars). The
  windowSpecs derivation becomes windowIntents (same validations,
  location resolved to the tri-state instead of refusing container).
- `status`/`attach`: `unprobedObserver` is deleted; the real adapter
  observes. `status`'s `containerObservationInfo` gains `health` and
  keeps `reason` for not-applicable cases.
- The `errUnprobed` constant and its rendering disappear with their
  observers.

## 6. `internal/config` validation additions

- Window names must not be fully numeric (`^[0-9]+$` rejected): tmux
  resolves numeric tokens as window **indexes** before names in
  targets, so a numeric name can focus the wrong window (deferral from
  #7's final review).
- `location: container` on any window while `devcontainer.enabled` is
  `false` is a validation error: statically contradictory desired
  state. (`auto` stays legal — resolvable only at open time.)
- Both are breaking-change-tolerant per the project charter (alpha;
  breaking allowed), and neither invalidates the shipped defaults.

## 7. Testing

- **Unit:** ExecCommand rendering and shell quoting (commands with
  quotes, `$`, `;`; sorted `-e KEY=VALUE` env flags present for every
  configured entry; `-u` omitted for an empty user); up-JSON last-line
  parsing (with stderr noise and multi-line stdout fixtures) **and
  field validation** (missing/empty `containerId` or
  `remoteWorkspaceFolder` → `StartError`, nothing rendered); probe/
  discovery classification tables (running/stopped/no-such-object
  case-insensitivity/daemon-down/ambiguous multi-match; a running
  unbound match reports `present`-incomplete, never `missing`);
  `Applies` matrix (true/auto-with-config/auto-without/stat-error);
  `runner` timeout returning partially captured output alongside the
  error; intent derivation and the location-resolution matrix including
  both new validation errors; `BuildPlan` `acquire` on
  present-with-empty-workdir; Ensure's container phase against a
  `fake.ContainerActuator` (start/acquire success, failure persisting
  the `StartError` exit status into `Operation.ExitStatus`, timeout
  path with bounded stderr, **both** probe-first retry paths — bound
  re-probe and unbound re-discover — and each retry outcome,
  container-window rendering only after a validated binding,
  `location: container` without a container failing before actuation,
  auto-with-deleted-config ignoring the stored binding, the
  `container_windows_stale` note on replacement into a live session,
  nil-actuator behavior unchanged).
- **Fake tooling (CI-safe, design §12):** fake `docker`/`devcontainer`
  script binaries through the seams: `up` success/failed-outcome/
  timeout (timeout proves the failed op and retained binding), probe
  classifications, discovery label output parsing.
- **Real Docker integration** (runners have Docker): a busybox
  container — probe `present` while running, `missing` after stop and
  after remove; an exec round-trip through a rendered ExecCommand.
- **Real devcontainer integration** (local-only; skips when the CLI is
  absent — GitHub runners lack it): the minimal alpine fixture —
  `up`, JSON parsed, probe present, idempotent re-up, discovery by
  label.
- **Lifecycle (real tmux + fake container tooling, §12 verbatim):**
  open a container workspace end to end — fake `up` runs once, binding
  recorded with user/workdir, the container window's tmux command is
  the rendered exec string, reopen is idempotent (no second `up`),
  failing `up` → failed op and no session creation, `status` reports
  the live probe.
- Gates: `gofmt -l .` empty, `go vet ./...`,
  `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`.

## 8. Exclusions

No `stop` (session or container), no autostart, no live-session window
reconciliation, no `list` live probing (recorded follow-up), no doctor,
no image/container GC, no compose-specific logic beyond what the
devcontainer CLI abstracts, no Windows.

## 9. Decisions recorded

- One adapter package for both CLIs; §5 describes one boundary.
- Exec mechanism is `docker exec` built from the stored binding — the
  §7 user/workdir columns are load-bearing; devcontainer-CLI exec
  rejected for pane-spawn latency and a permanent runtime dependency.
- Discovery keeps health truthful: a running unbound match is
  `present` with an incomplete binding (empty workdir), never a false
  `missing`; the new pure `acquire` plan action routes it through the
  idempotent `up` (verified ~1s no-op on a running container).
- Applicability (`Applies`) is checked before probing stored bindings
  under `auto`, so deleting the devcontainer configuration
  de-containerizes the workspace without touching the retained binding.
- Configured environment reaches container windows via explicit
  `docker exec -e` flags — tmux pane env is host-side only and exec
  forwards nothing.
- Container replacement into a live session does not recreate windows;
  it is reported (`container_windows_stale`) with a stop-and-reopen
  recommendation. Truthful limitation over silent partial repair;
  reconciliation stays a follow-up.
- `probe-first` retries the observation kind that failed: probe when
  bound, discover when not.
- `StartError` carries the real exit status, bounded stderr, and a
  timeout flag; the recorded operation persists the exit status
  (design §9). The runner returns partial capture alongside
  timeout/cancel errors to make that possible.
- `up` success is validated (non-empty `containerId`,
  `remoteWorkspaceFolder`) before any window rendering or session
  mutation.
- `up`'s result is the last stdout line (verified; logs on stderr);
  parse exactly that.
- Probe classification is narrow and case-insensitive on
  `no such object` (docker 29 lowercases); everything unrecognized is
  `unknown`, never absence.
- Ambiguous discovery (multiple running label matches) is an error —
  no claimant is picked, mirroring session identity claims.
- Container-relative cwds join `remoteWorkspaceFolder` with POSIX
  `path.Join`.
- Probes/discovery use a short 5s default timeout; only `up` gets
  `start_timeout`.
- Window derivation splits into pure intents (CLI) and post-binding
  rendering (Ensure): rendering requires the binding, which exists only
  inside the locked loop.
- The container phase runs after refusal and before session actuation;
  probe-first performs exactly one re-probe.
- `status`/`attach` probe live; `list` stays last-observed (follow-up
  recorded for batched probing).
- `SessionBelongsTo` exported; numeric window names and the
  `enabled: false`+`location: container` contradiction rejected at
  validation (deferrals from #6/#7 closed).
- Real-devcontainer tests are local-only; CI relies on fake tooling
  plus real-Docker probe/exec tests.
