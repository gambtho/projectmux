# Observation commands slice — `list`, `status`, and the tmux observer

**Status:** Approved design

**Date:** 2026-08-05

**Scope:** The first part of design §13 step 3: port the observation commands
`list` and `status`. Adds the design-§5 runner utility and a minimal
read-only tmux adapter implementing `controller.SessionObserver`. No
mutating commands, no real container adapter, no state writes.

## 1. Outcome

Three additions:

- **`internal/run`** — the design-§5 internal runner utility, sized to
  observer needs: execute one subprocess with a context, structured argv,
  bounded output capture, and retained exit status.
- **`internal/tmux`** — a read-only adapter that lists live sessions with
  their identity keys in **one** subprocess call and implements
  `controller.SessionObserver` on top of it.
- **`internal/cli`** — `projectmux list` and `projectmux status`, rendering
  stored state, live session observations, and `controller.BuildPlan`
  output. They never re-implement planning logic and never write to the
  store.

`internal/controller`, `internal/state`, and `internal/controller/fake`
APIs are unchanged.

## 2. Observable behavior

### `projectmux list [--json] [--compact]`

One summary over the union of stored workspaces and live tmux sessions
carrying the identity keys:

- Stored rows first, in store order (slug, then worktree), then live
  sessions with no stored record ("unrecorded"), by session name.
- Columns: `WORKSPACE` (slug), `SESSION` (actual name; the proposed name
  marked `(unassigned)` when no actual name exists; the live name for
  unrecorded rows), `TMUX` (`live` / `absent` / `unknown`), `CONTAINER`
  (from the **stored** binding: health plus last-observed timestamp; `-`
  when never bound), `NOTES` (`unrecorded`, or `conflict` when a live
  session's identity keys contradict the stored row it matched by ID).
- Stored and live rows are matched by the `@dev_workspace_id` key. A
  matched session whose slug or worktree keys contradict the stored row
  renders the `conflict` note; deciding what to do about it is `status`'s
  and later slices' job.
- **Duplicate identity claims:** when more than one live session carries
  the same `@dev_workspace_id`, `list` reports uncertainty consistently
  with `ObserveSession`: the matching stored row renders `TMUX unknown`
  with no live session and the `conflict` note, and each claiming
  session additionally renders as an unrecorded-style row with the
  `conflict` note. `list` never picks one claimant.
- `list` loads **no** workspace configuration and resolves nothing: a
  broken workspace YAML cannot break the summary, and N workspaces cost
  one tmux subprocess, not N.
- If the session observation fails, every row renders `TMUX unknown` and
  the command still exits 0: a successful report of uncertainty. Store
  read failure exits 1.

### `projectmux status [<workspace>] [--json] [--compact]`

The full observation pipeline for one workspace, resolved from the
argument or the current directory exactly as `config` does: defaults →
resolve → workspace config → digest, then `state.Open`,
`controller.Observe`, `controller.BuildPlan`. Renders:

- **workspace** — slug, worktree, ID, proposed/actual session, primary
  flag, registered/updated timestamps; or "not registered" (a normal
  report, not an error).
- **session** — tri-state state; when live, the session's name and an
  identity verdict (`match`/`conflict`). No identity verdict is reported
  for `absent` or `unknown`: an unobserved session is never described as
  matching or mismatching.
- **container** — two clearly separated parts. The **stored binding**
  (kind, ID, user, workdir, health, observed-at), where `health=missing`
  or `unknown` must never read as live (design §8) — human output spells
  it out, e.g. `missing (last seen 2026-08-05T12:00:00Z)`. And the
  **current observation's outcome**: in this slice always "not probed
  (container probing is not implemented in this build)", so a stored
  `present` can never masquerade as a live probe result.
- **config** — desired digest, applied digest, drifted flag.
- **last operation** — name, outcome, exit status, error summary,
  finished-at, when recorded.
- **plan** — `BuildPlan` output verbatim: session action, container
  action, reapply and record-name flags, and the refusal text when
  refusing.

### Contract

- Both commands support `--json` / `--compact` (compact implies JSON) with
  the established envelope pattern: top-level
  `schema_version: OutputSchemaVersion (= 1)` and documented structures.
  Human output is not a contract.
- Exit codes: 0 on any successful report, including drift, refusal, and
  observation uncertainty — automation branches on JSON content, not exit
  codes. Existing codes keep their meanings: usage 2, ambiguous 3, unknown
  workspace 4, invalid config 5, everything else 1. No new codes.
- Neither command performs any **operational-record mutation** (no
  workspace, binding, or operation rows are written). Design §8 permits
  last-observed metadata updates, but this slice has nothing new to
  record: there is no real container probe yet and no session-liveness
  column. Tests assert zero mutations (design §12). `state.Open` may
  still create the database file and apply migrations on first use —
  that is store initialization, not workspace mutation.

## 3. JSON envelopes

`list`:

```json
{
  "schema_version": 1,
  "tmux_observed": true,
  "workspaces": [
    {
      "id": "…", "slug": "…", "worktree": "…", "is_primary": true,
      "proposed_session": "…", "actual_session": "…" ,
      "session_state": "live",
      "live_session": "…",
      "container": {"kind": "…", "container_id": "…", "health": "missing",
                     "observed_at": "…"},
      "recorded": true,
      "identity_conflict": false
    }
  ]
}
```

- `tmux_observed: false` reports a failed observation; every
  `session_state` is then `"unknown"` and `live_session` is absent.
- Unrecorded rows have `recorded: false`, identity fields from the tmux
  keys, and no `container`.
- Duplicate identity claims render per the behavior above: the stored
  row gets `session_state: "unknown"`, no `live_session`, and
  `identity_conflict: true`; each claimant appears as an unrecorded-style
  row with `identity_conflict: true`. `live_session` is only ever
  present when exactly one live session corresponds to the row.
- `actual_session` and `container` are omitted when absent (`omitempty`
  via pointers), mirroring the nullable store fields.

`status`:

```json
{
  "schema_version": 1,
  "workspace": {"id": "…", "slug": "…", "worktree": "…",
                 "session_name": "…", "is_primary": true},
  "registered": true,
  "stored": {"proposed_session": "…", "actual_session": "…",
              "registered_at": "…", "updated_at": "…"},
  "session": {"state": "live", "name": "…", "identity": "match"},
  "container": {
    "stored": {"kind": "…", "container_id": "…", "container_user": "…",
                "workdir": "…", "health": "missing", "observed_at": "…"},
    "observation": {"attempted": false, "reason": "probe-not-implemented"}
  },
  "config": {"desired_digest": "…", "applied_digest": "…", "drifted": true},
  "last_operation": {"operation": "…", "outcome": "failed",
                      "exit_status": 1, "error_summary": "…",
                      "finished_at": "…"},
  "plan": {"session": "refuse", "container": "none", "reapply": false,
            "record_name": false, "refusal": "…"}
}
```

- `stored`, `container.stored`, and `last_operation` are omitted when
  never recorded; `registered: false` accompanies a nil stored record.
- `session.identity` is an enum (`"match"` / `"conflict"`) present
  **only** when `state` is `"live"`; it is omitted for `absent` and
  `unknown`, so a consumer can never read a verified mismatch out of an
  unobserved session. `name` is likewise live-only.
- `container.observation` is always present when a container is in play
  and separates the live observation's outcome from the stored binding,
  so `stored.health: "present"` can never hide that the current
  observation failed or was unsupported. In this slice it is always
  `{"attempted": false, "reason": "probe-not-implemented"}`; the real
  container-adapter slice replaces it with attempted probes and their
  failure classification.
- `workspace` reuses the `config` command's `workspaceInfo` shape.

## 4. `internal/run`

A small utility, not a public package or domain boundary (design §5):

- `run.Command{Argv []string, Dir string, Timeout time.Duration}` executed
  by `run.Run(ctx, cmd) (run.Result, error)`.
- `Result{Stdout, Stderr []byte, ExitCode int}`. Stdout and stderr capture
  is bounded (64 KiB each); truncation is recorded on the result rather
  than silently dropped.
- A non-zero exit is **not** a Go error: the caller reads `ExitCode` and
  `Stderr`. The error return is reserved for failure to start, context
  cancellation, and timeout.
- Cancellation kills the child's **process group** (the child starts in
  its own group via `Setpgid`), not just the immediate process:
  descendants holding the stdout/stderr pipes would otherwise keep `Run`
  from returning at its deadline. A bounded `WaitDelay` backstops
  descendants that survive the kill. Tested with a helper that spawns a
  background grandchild.
- No shell interpretation anywhere; argv only (design §11) — including a
  test proving shell metacharacters in arguments stay literal.

## 5. `internal/tmux`

`tmux.Client{Socket string, Timeout time.Duration}` — `Socket` passes
`-L <name>` for isolated integration tests, empty meaning the default
server; `Timeout` bounds every subprocess, the zero value meaning a
5-second default, so a hung tmux cannot wedge unattended callers.
Timeout propagation through `Sessions` is tested.

- `Sessions(ctx) ([]controller.LiveSession, error)` uses **two-phase
  observation with raw single-value transport**, so no in-band framing
  exists for a value to forge (identity values are not newline-free —
  the resolver imposes no such restriction, and tmux emits option
  values verbatim in formats; all verified on tmux 3.4):

  1. `tmux [-L socket] list-sessions -F '#{session_id}'` — one line per
     session. Every line must match `^\$[0-9]+$` exactly, with no
     duplicates; anything else is an observation error. Session IDs are
     tmux-generated and cannot be influenced by identity values.
  2. Per session ID, four `tmux display-message -p -t <id> '<format>'`
     calls, one each for `#{session_name}`, `#{@dev_workspace_id}`,
     `#{@dev_slug}`, `#{@dev_worktree}`. The **entire output** of one
     call is one raw value plus exactly one trailing newline added by
     tmux; the decoder strips exactly that one newline. Embedded
     newlines, tabs, and anchor-shaped content round-trip unmodified
     because no parsing grammar exists to collide with.

  Validation: an empty session name is an observation error (real tmux
  sessions cannot have empty names; a session vanishing between the two
  phases surfaces this way — verified: a dead `-t` target can exit 0
  with empty output). Observation therefore costs `1 + 4N` subprocesses,
  each individually bounded by the client timeout; N is the live session
  count and small in practice, and the trade is deliberate: correctness
  of attribution over subprocess count.
- **No server is absence, not failure — matched narrowly:** exit 1 with
  stderr containing `no server running` (older tmux), or containing both
  `error connecting to` **and** `No such file or directory` (3.x),
  returns an empty list and nil error. `error connecting to` alone is
  **not** absence: tmux emits that prefix for permission and other
  socket failures (`Operation not permitted`), which must stay errors so
  a denied socket never reads as "no sessions" and never lets planning
  propose creation. Any other non-zero exit, start failure, or
  validation failure returns an error, which `Observe` renders as
  `SessionUnknown` (a tmux outage is not absence — design §9).
- `ObserveSession(ctx, q)` implements `controller.SessionObserver` by
  filtering `Sessions` output in-process: `ByIdentity` is the session
  whose `@dev_workspace_id` equals `q.WorkspaceID`; `ByName` is every live
  session occupying a candidate name. **More than one** session claiming
  the workspace ID is an observation error (→ unknown → refuse), not a
  choice between them. Three-key contradiction checking stays in
  `BuildPlan`, unchanged.

## 6. CLI wiring

- `list.go`: `state.Open(state.Root())` → `Workspaces()`, plus
  `tmux.Client.Sessions(ctx)`; union, render. No config, no resolver, no
  controller.
- `status.go`: the `config` command's pipeline (`config.Root` →
  `LoadDefaults` → `resolve.Resolve` → `config.Load`) builds
  `controller.Desired`; then
  `controller.Controller{Store, Sessions: tmux client, Containers:
  unprobed observer, Clock: system clock}.Observe` and
  `controller.BuildPlan` produce the rest.
- The **unprobed container observer** is a cli-local
  `controller.ContainerObserver` whose `ProbeContainer` and
  `DiscoverContainer` return a typed "container probing is not implemented"
  error. The snapshot then honestly carries `health=unknown` (plan:
  `probe-first`) while the **rendered** container facts come from the
  stored binding, and the rendered observation outcome states that no
  probe was attempted. It is never presented as a live probe.
- Commands take a `context.Context` cancelled on SIGINT/SIGTERM for
  interactive interruption; the hang defense for unattended callers is
  the client's finite subprocess timeout. `run` propagates both.
- Test seam: package-level constructor variables in `cli` (for the session
  observer and store opening), overridden in tests with
  `internal/controller/fake` implementations — complementing, not
  replacing, the existing env-override conventions
  (`PROJECTMUX_CONFIG_ROOT`, `PROJECTMUX_STATE_ROOT`).

## 7. Failure behavior

- Store open/read failure: exit 1, nothing on stdout (existing `Main`
  contract). A fresh machine gets an empty database from `state.Open`;
  `list` then shows only live identity sessions.
- tmux binary missing or failing: `list` renders `unknown` and exits 0;
  `status` shows session `unknown` and the plan's refusal.
- `status` for a stored workspace whose worktree no longer exists fails in
  resolution (exit 4) before the store is read; explaining stale records
  is `doctor`'s job (a recorded exclusion, not an accident).
- Invalid workspace configuration fails `status` with exit 5, as `config`
  does today.

## 8. Testing

- `internal/run`: exit-status retention, bounded-capture truncation,
  timeout and cancellation killing the whole process group (including a
  backgrounded grandchild holding the pipes), shell metacharacters in
  argv staying literal.
- `internal/tmux`: session-ID validation tests (well-formed, malformed,
  duplicate, empty); value round-trips through the raw single-value
  transport, including embedded newlines and anchor-shaped content;
  empty-session-name rejection; no-server stderr classification —
  both absence variants accepted, `error connecting to … (Operation not
  permitted)` and other failures rejected as errors; duplicate-identity
  claims; timeout propagation through `Sessions` (a hung fake subprocess
  is killed at the client timeout); real-tmux integration tests on an
  isolated `-L` socket (skipped when tmux is absent; CI's ubuntu-latest
  ships tmux): create sessions carrying the three `@dev_` user options —
  including a worktree value containing a newline and an anchor-shaped
  line — observe, assert identity and name matching and value
  round-trips.
- `internal/cli`: command tests over `Main` with fakes via the test seam:
  union rendering and ordering, unrecorded and conflict notes,
  duplicate-claim rendering (stored row unknown, claimants listed, no
  claimant picked), missing/unknown bindings never rendering as live,
  the stored-vs-observation container split, `session.identity` present
  only for live states, refusal text passthrough, drift flag, JSON
  envelope shapes, `--compact` implying `--json`, exit codes, and —
  design §12 — an assertion that a full
  `list`/`status` run performs **zero** store mutations (the fake store
  records every call).
- Gates: `gofmt -l .` empty, `go vet ./...`,
  `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`.

## 9. Exclusions

No `open`/`attach`/`stop`, no workspace registration or any store write,
no real container adapter, no per-workspace filesystem lock, no `doctor`,
no autostart, no schema changes, no daemon. Stale-record diagnosis
(worktree deleted, orphaned sessions) is deferred to `doctor`.

## 10. Decisions recorded

- `list` enumerates stored workspaces ∪ live identity-keyed sessions, so
  the command is useful before `open` exists and during side-by-side
  validation with the Bash implementation; observation commands must not
  register workspaces (design §8).
- A minimal real tmux observer ships in this slice; the container
  observer remains unimplemented and is injected as an honest
  "not implemented" error, never a fake probe.
- Container facts render from the stored binding with its observed-at
  timestamp; plan output stays truthful (`probe-first` on unknown).
- Successful reports exit 0 regardless of drift or refusal; automation
  reads JSON. No new exit codes.
- No operational-record mutations this slice: §8's "may update
  last-observed metadata" is declined because nothing new is observed
  that the schema stores. `state.Open` may still initialize/migrate the
  database file.
- The tmux surface is two-phase: a strictly validated
  `list-sessions -F '#{session_id}'` enumeration, then per-field
  `display-message -p` calls whose entire output is one raw value.
  In-band framing was rejected twice: a naive tab/newline frame splits
  on legal newline-bearing values, and a keyed anchor frame is forgeable
  by values containing anchor-shaped lines. Raw single-value transport
  has no grammar to forge. (Verified on tmux 3.4: option values are
  emitted verbatim in formats; `q:` does not escape newlines; the `s/…/`
  modifier cannot match newlines; `show-options` quoting is
  undocumented and inconsistent; session names are control-character-
  sanitized by tmux at creation.) `ObserveSession` filters in-process.
- Every tmux subprocess has a finite timeout (`Client.Timeout`, default
  5s when zero); signal cancellation alone is not the hang defense. One
  observation costs `1 + 4N` subprocesses, each bounded.
- No-server is absence only when matched narrowly: `no server running`,
  or `error connecting to` together with `No such file or directory`.
  Permission failures (`Operation not permitted`) and every other
  failure stay unknown — a denied socket never reads as absence, so
  planning can never propose creation from it.
- Runner cancellation kills the child's process group with a bounded
  `WaitDelay` backstop, so descendants holding pipes cannot wedge a
  deadline.
- Duplicate live claims on one workspace ID are an observation error in
  `ObserveSession` and render as consistent per-row uncertainty in
  `list`; no code path picks a claimant.
- `status` separates the stored container binding from the current
  observation's outcome in both human and JSON output; a stored
  `present` is never presented as a live probe result.
- Session identity verdicts are reported only for observed-live
  sessions; absent/unknown states carry no match/mismatch claim.
- `list` renders a `conflict` note when a matched session's slug/worktree
  keys contradict the stored row; refusal semantics remain in `BuildPlan`.
- The design-§5 runner is introduced now (`internal/run`), sized to
  observer needs; the container-adapter slice extends rather than invents
  it.
