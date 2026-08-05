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
is in play; additive to schema version 1.

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
     (label verified present with the exact worktree path). The result
     is **never `present`**: discovery cannot supply the full binding
     (remoteUser/workdir are not in labels), and `present` is reserved
     for *fully bound and running*. One match (running or stopped) →
     `{missing, id}`; none → `{missing}`; **more than one running**
     match → error (ambiguity is uncertainty; no claimant is picked).
     The plan then says `start`, and the idempotent `up` acquires the
     authoritative binding — a running container costs one ~1s no-op.
- **`StartContainer(ctx, ws, cfg)`** (actuator) — `devcontainer up`
  bounded by `cfg.DevContainer.StartTimeout`; parses the **last stdout
  line** as the result JSON; `outcome != "success"`, unparseable
  output, or timeout → typed error carrying bounded stderr. Success →
  `ContainerObservation{Health: present, Kind: "devcontainer",
  ContainerID, ContainerUser: remoteUser, Workdir:
  remoteWorkspaceFolder}`.
- **`ExecCommand(binding, command, relDir string) string`** (actuator,
  pure) — renders the §5 execution request as a tmux window command
  string: `docker exec -i -t` + `-u <user>` when non-empty + `-w
  <path.Join(binding.Workdir, relDir)>` + id + `sh -lc '<command>'`
  (empty command → `sh -l`). Shell quoting through one tested
  single-quote-escaping function — the only place quoting happens.
  ExecCommand never inspects live state; a pane whose container died
  shows docker's own error.

## 4. Controller changes

```go
type ContainerActuator interface {
	StartContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (ContainerObservation, error)
	ExecCommand(binding state.ContainerBinding, command, relDir string) string
}
```

`Controller` gains `ContainerAct ContainerActuator`. Nil preserves
today's behavior exactly: any non-`none` container action refuses with
`ErrContainerActionUnsupported`.

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

- container `none` → skip (an applying-but-unbound running container
  surfaces as `start` via discovery, see §3);
- `start` → `StartContainer`; failure → failed op, typed error, no
  session mutation;
- `probe-first` (observation errored) → **one** re-probe via the
  observer: `present` → proceed (binding refreshed in the commit),
  `missing` → `StartContainer`, error → failed op, typed error. Open
  never mutates on uncertainty.
- The applied digest continues to assert the whole document was
  actuated — now including that container windows were rendered
  against a live binding.

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
  quotes, `$`, `;`); up-JSON last-line parsing (with stderr noise and
  multi-line stdout fixtures); probe/discovery classification tables
  (running/stopped/no-such-object case-insensitivity/daemon-down/
  ambiguous multi-match); intent derivation and the location-resolution
  matrix including both new validation errors; Ensure's container phase
  against a `fake.ContainerActuator` (start success/failure/timeout
  path, probe-first re-probe outcomes, container-window rendering only
  after binding, `location: container` without a container failing
  before actuation, nil-actuator behavior unchanged).
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
- Discovery never reports `present`: `present` means fully bound and
  running; acquisition always flows through the idempotent `up`
  (verified ~1s no-op on a running container), keeping `BuildPlan`
  unchanged.
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
