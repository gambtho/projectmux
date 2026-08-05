# Doctor Slice Design

`projectmux doctor` — diagnose-only environment and state diagnostics per
design.md §8/§9/§11. The explicit state-rebuild action (backup + recreate
after confirmation) is deliberately deferred to a later slice; this slice
never mutates anything.

## 1. Command surface

```
projectmux doctor [--json] [--compact]
```

No workspace argument: doctor diagnoses the whole installation. It takes
no locks, records no operational metadata, and never mutates tmux,
Docker, or any file — **including the state database**. Unlike
`list`/`status`, doctor never calls `state.Open`: a doctor that reports
"migrations pending" must not perform them in the same breath, and a
corrupted database must never be opened read-write. All database access
goes through the read-only inspection path (§3 database), which is
verified never to create or mutate the file (Codex review finding). Exit codes:

- `0` — the diagnosis completed, regardless of what it found. Findings
  are report content, exactly as drift is for `list`/`status`.
- `2` — usage errors.
- Other failures (undeterminable config/state root) use the normal typed
  error paths with nothing on stdout.

JSON envelope (additive to `schema_version: 1`):

```json
{
  "schema_version": 1,
  "checks": [
    {
      "name": "dependencies",
      "status": "ok|warn|fail|unknown",
      "detail": "optional one-line summary",
      "items": [
        {"subject": "tmux", "status": "ok", "detail": "tmux 3.4"}
      ]
    }
  ]
}
```

`checks` is always the full fixed-order list (`dependencies`,
`configuration`, `database`, `orphaned-sessions`, `stale-bindings`); a
check that could not run reports `unknown` in place — one broken
dependency never truncates the report. `items` is per-subject detail and
may be empty. Human output renders one line per check
(`ok dependencies` style) with indented item lines.

Check status aggregation: a check's status is the worst of its items
(`fail` > `unknown` > `warn` > `ok`), unless the check itself could not
run at all (then `unknown` or `fail` as specified below).

## 2. Architecture

New package `internal/doctor`:

```go
type Status string // "ok" | "warn" | "fail" | "unknown"

type Item struct {
	Subject string
	Status  Status
	Detail  string
}

type Check struct {
	Name   string
	Status Status
	Detail string
	Items  []Item
}

type Report struct{ Checks []Check }

// ProbeResult carries the facts the dependency policy branches on —
// run.Run already produces them all; the seam must not flatten them
// (Codex review finding: daemon-down is exit 1 + EMPTY stdout + the
// reason on stderr, indistinguishable through a stdout-only seam).
type ProbeResult struct {
	Stdout   string // trimmed
	Stderr   string // trimmed, bounded by the runner's capture cap
	ExitCode int
	Found    bool // the binary resolved on PATH
}

type VersionRunner interface {
	// Probe runs one argv (never a shell) under the observation
	// timeout. err reports execution failures (timeout, permission);
	// a nonzero exit is a result, not an error, matching run.Run.
	Probe(ctx context.Context, argv ...string) (ProbeResult, error)
}

// SessionLister is the bulk enumeration doctor needs; *tmux.Client's
// existing Sessions method satisfies it.
type SessionLister interface {
	Sessions(ctx context.Context) ([]controller.LiveSession, error)
}

type Runner struct {
	ConfigRoot string
	Defaults   config.Layer
	StateRoot  string
	Store      Store        // read-only store view (§3 database)
	Sessions   SessionLister
	Containers controller.ContainerObserver
	Versions   VersionRunner
}

func (r *Runner) Diagnose(ctx context.Context) Report
```

`Store` is a narrow read-only interface (`Workspaces()`,
`Workspace(id)`) satisfied by `*state.ReadOnlyStore` (§3 database) and
the fake. The CLI wires the container observer seam the other commands
use plus the tmux client as `SessionLister`, `state.OpenReadOnly` for
the store view, and a real `VersionRunner` built on `internal/run` with
the 5s observation timeout. The controller is not involved: doctor is
store-wide and plan-free, which is exactly what the per-workspace
controller is not for.

The tri-state discipline binds throughout: uncertainty renders
`unknown`, never a finding. Only confirmed absence produces `warn`/
`fail` content.

## 3. The five checks

### dependencies

Probes, in order, via `VersionRunner` (structured argv, never a shell):

| subject | argv | verified output shape |
|---|---|---|
| tmux | `tmux -V` | `tmux 3.4` |
| git | `git --version` | `git version 2.54.0` |
| docker client | `docker --version` | `Docker version 29.7.1, build e9452d6` |
| docker daemon | `docker version --format {{.Server.Version}}` | `29.7.1`; daemon down: exit 1, **empty stdout**, stderr `failed to connect to the docker API …` |
| devcontainer | `devcontainer --version` | `0.86.1` |

Raw trimmed stdout is the version detail; no parsing. Statuses:

- tmux missing → item `fail` (core dependency).
- git missing → item `warn` (worktree resolution degrades).
- docker client or devcontainer missing → item `warn` (container
  features degrade; host-only workspaces are unaffected).
- docker daemon: probed only when the client exists. Reachable =
  exit 0 **and** non-empty trimmed stdout (verified: the template
  prints an empty line when the daemon is down). Unreachable → item
  `unknown` with the stderr summary — the daemon may simply be off.
- A probe execution error (timeout, permission) → item `unknown`.

### configuration

- `config.LoadDefaults` failure → check `fail` with the error, no items
  (per-workspace validation cannot run without defaults).
- First item is always `defaults` itself, validated standalone: run the
  merge/normalize/validate pipeline over the defaults layer with no
  workspace layers (Codex review finding: validation lives only in
  `config.Load`, so a defaults.yaml with e.g. an unsupported `version:`
  would otherwise read healthy whenever no workspace files exist). A
  *missing* defaults.yaml is deliberately legal (`loadLayer` treats
  absence as an empty document) → item `ok` with detail
  "defaults.yaml absent".
- Then glob `<config root>/workspaces/*.yaml`; each slug loads via
  `config.Load(root, defaults, slug)`. Valid → item `ok`; an
  `InvalidConfigError` or load failure → item `fail` with the problems.
  `*.local.yaml` files are not separate items; `Load` already merges
  them under their slug.

### database

All verified against modernc/sqlite (probe transcript 2026-08-05):

- `os.Stat` the DB file first. Missing → check `ok`, detail
  "no state database yet" (fresh installations are healthy). Stat
  errors other than not-exist → `unknown`.
- Open with `?mode=ro` — verified to never create or mutate the file —
  bypassing `state.Open` (which creates, enables WAL, and migrates; a
  diagnose-only command must do none of that, Codex review finding).
  New helper `state.OpenReadOnly(root) (*ReadOnlyStore, Inspection,
  error)` owns the read-only open and pragmas so doctor never
  hand-builds a DSN. `Inspection` carries `UserVersion int` and
  `IntegrityErr error`; `ReadOnlyStore` exposes exactly the read
  methods doctor's `Store` interface needs (`Workspaces`, `Workspace`)
  over the same `mode=ro` connection, and is valid for queries **only
  when the integrity check passed and `UserVersion` equals the current
  schema version** — the store-backed checks (§orphaned-sessions,
  §stale-bindings) render `unknown` otherwise, with the reason
  ("database is corrupt" / "migrations pending; run any command to
  migrate" / "database is from a newer projectmux"). A **missing**
  database is different: nothing is registered, which is a confirmed
  fact — the store-backed checks run against an empty registered set
  (so a live identity session still reports "session not registered",
  and stale-bindings is trivially `ok`).
- `PRAGMA integrity_check`: the single row `"ok"` → healthy. A non-ok
  row, or the verified error shapes (`database disk image is malformed
  (11)`, `file is not a database (26)`) → check `fail` with the message.
  The file is reported, never touched; rebuild is the future slice.
- `PRAGMA user_version` compared to the current schema version: newer
  than the binary supports → `fail` ("database is from a newer
  projectmux"); older → `warn` ("migrations pending; they run on the
  next mutating command"); equal → `ok`.

### orphaned-sessions

One bulk enumeration through the `SessionLister` seam (§2), which
`*tmux.Client.Sessions` already satisfies — `ObserveSession` is
workspace-scoped and is not used here. For each live identity-carrying
session (non-empty `WorkspaceID`):

- workspace ID not in the store → item `warn`, "session not registered".
- registered but the recorded worktree no longer exists on disk —
  `errors.Is(err, os.ErrNotExist)` only — → item `warn`,
  "worktree no longer exists". Other stat errors → item `unknown`.
- tmux unobservable → check `unknown` with the error. No server → check
  `ok` (nothing live, nothing orphaned).

Sessions without identity keys are not projectmux's business and are
ignored.

### stale-bindings

For each stored workspace record with a container binding: one live
`ProbeContainer`. Confirmed absent (`missing`) → item `warn`,
"container no longer exists" with the retained ID. Probe error →
item `unknown` (daemon may be down; never converts to a finding).
Present → item `ok`. No bindings → check `ok`. Probes run sequentially;
each is bounded by the adapter's 5s timeout. When the dependencies
check already found no docker client, this check short-circuits to
`unknown` ("docker is not installed") without probing.

## 4. CLI

`internal/cli/doctor.go`: flag parsing and rendering only. Dispatch
case `doctor` + usage entry. Wiring: config root + defaults, state
root, `state.OpenReadOnly` (never `openStore`/`state.Open` — doctor is
the one command that must not create or migrate the database), the
tmux client as `SessionLister`, the container observer, and the real
`VersionRunner` on `internal/run`. When `OpenReadOnly` reports a
missing or unusable database, the store-backed checks render as §3
specifies; the CLI never falls back to a read-write open.

Human output example:

```
ok      dependencies
          ok      tmux    tmux 3.4
          warn    devcontainer    not installed
ok      configuration
fail    database    database disk image is malformed (11)
warn    orphaned-sessions
          warn    slab    worktree no longer exists: /w/gone
ok      stale-bindings
```

## 5. Testing

- Unit tests per check in `internal/doctor` against fakes: scripted
  `SessionLister`, `fake.ContainerObserver`, `fake.Store`, a scripted
  `VersionRunner`. Status aggregation table test.
- `state.OpenReadOnly` tests: healthy, corrupted (bytes overwritten mid-file
  — verified to yield malformed(11)), not-a-database, missing, and the
  file-untouched assertion (bytes identical after inspection).
- CLI tests: envelope shape, exit 0 with findings present, usage exit 2,
  and the no-mutation guarantee via the guarded fake store.
- Lifecycle test (real tmux): seed an identity session whose worktree is
  a deleted temp directory → `orphaned-sessions` reports the item; a
  healthy open workspace reports clean.
- Smoke: add `doctor --json` to the isolated end-to-end script; assert
  exit 0 and a five-check envelope.

## 6. Exclusions

- `--rebuild` / backup-and-recreate: next slice.
- No parsing or minimum-version enforcement of tool versions.
- No repair of orphans or stale bindings (stop/open already converge).
- No new dependencies; stdlib + existing modules only.
