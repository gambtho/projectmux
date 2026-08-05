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
  cancellation, and timeout. Cancellation kills the child process.
- No shell interpretation anywhere; argv only (design §11).

## 5. `internal/tmux`

`tmux.Client{Socket string, Timeout time.Duration}` — `Socket` passes
`-L <name>` for isolated integration tests, empty meaning the default
server; `Timeout` bounds every subprocess, the zero value meaning a
5-second default, so a hung tmux cannot wedge unattended callers.
Timeout propagation through `Sessions` is tested.

- `Sessions(ctx) ([]controller.LiveSession, error)` runs exactly one
  subprocess, with a **newline-safe keyed format** (four lines per
  session; tmux formats expand user options — verified on tmux 3.4):

  ```text
  tmux [-L socket] list-sessions -F '#{session_id}\tname\t#{session_name}
  #{session_id}\twsid\t#{@dev_workspace_id}
  #{session_id}\tslug\t#{@dev_slug}
  #{session_id}\ttree\t#{@dev_worktree}'
  ```

  Identity values are **not** newline-free (the resolver imposes no such
  restriction, and tmux emits option values verbatim — verified: a
  worktree containing a newline splits a naive record). The decoder is
  defined as:

  - a line matching `^\$[0-9]+\t(name|wsid|slug|tree)\t` starts a field;
    `#{session_id}` is tmux-generated (`$N`) and cannot contain the
    delimiter, and each line carries exactly one free-form value, last;
  - any other line is a continuation of the previous field's value with
    the `\n` restored — embedded newlines round-trip;
  - every session must yield exactly one of each key; duplicate or
    missing keys, or a session id appearing with inconsistent grouping,
    is an observation error. A value line that itself matches the anchor
    pattern would collide with the real session's keys and be caught by
    this rule; the residual forgery window (a path injecting a complete
    phantom session) is out of threat model — identity values come from
    the local filesystem and configuration, not remote input.
- **No server is absence, not failure:** exit 1 with stderr matching
  `no server running` (older tmux) or `error connecting to` (3.x) returns
  an empty list and nil error. Any other non-zero exit, start failure, or
  parse failure returns an error, which `Observe` renders as
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
  context cancellation kills the child, argv-only execution.
- `internal/tmux`: pure decoder tests over fixture output (well-formed;
  embedded newlines in every free-form field round-tripping; malformed,
  duplicate-key, and missing-key records erroring; duplicate-identity
  claims); no-server stderr classification for both known variants;
  timeout propagation through `Sessions` (a hung fake subprocess is
  killed at the client timeout); real-tmux integration tests on an
  isolated `-L` socket (skipped when tmux is absent; CI's ubuntu-latest
  ships tmux): create sessions carrying the three `@dev_` user options —
  including a worktree value containing a newline — observe, assert
  identity and name matching and value round-trips.
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
- One bulk `list-sessions -F` call with user-option format expansion is
  the whole tmux surface; `ObserveSession` filters in-process.
- The tmux format is a keyed multi-line frame anchored on
  tmux-generated `#{session_id}`, with a defined decoder that restores
  embedded newlines: identity values are not constrained to be
  newline-free, and one odd-but-legal path must not poison the whole
  observation. (Verified: tmux emits option values verbatim; `q:` does
  not escape newlines.)
- Every tmux subprocess has a finite timeout (`Client.Timeout`, default
  5s when zero); signal cancellation alone is not the hang defense.
- No-server (both known stderr variants) is absence; every other tmux
  failure is unknown. Unmatched failure never converts to absence.
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
