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
Docker, or user files. The one standing carve-out (identical to
`list`/`status`, PR #6): `state.Open` may create and migrate the
database file itself — and the `database` check inspects the file
*before* that open, so pre-existing corruption is still reported
faithfully. Exit codes:

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

type VersionRunner interface {
	// Output runs one probe argv and returns trimmed stdout, whether
	// the binary was found, and the execution error if any.
	Output(ctx context.Context, argv ...string) (out string, found bool, err error)
}

type Runner struct {
	ConfigRoot string
	Defaults   config.Layer
	StateRoot  string
	Store      Store               // read-only store view (below)
	Sessions   controller.SessionObserver
	Containers controller.ContainerObserver
	Versions   VersionRunner
}

func (r *Runner) Diagnose(ctx context.Context) Report
```

`Store` is a narrow read-only interface (`Workspaces()`,
`Workspace(id)`) satisfied by both `*state.Store` and the fake. The CLI
wires the same seams every other command uses (`newSessionObserver`,
`newContainerObserver`, `openStore`) plus a real `VersionRunner` built on
`internal/run` with the 5s observation timeout. The controller is not
involved: doctor is store-wide and plan-free, which is exactly what the
per-workspace controller is not for.

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
- Otherwise glob `<config root>/workspaces/*.yaml`; each slug loads via
  `config.Load(root, defaults, slug)`. Valid → item `ok`; an
  `InvalidConfigError` or load failure → item `fail` with the problems.
  No workspace files → check `ok`, detail "no workspace configuration".

### database

All verified against modernc/sqlite (probe transcript 2026-08-05):

- `os.Stat` the DB file first. Missing → check `ok`, detail
  "no state database yet" (fresh installations are healthy). Stat
  errors other than not-exist → `unknown`.
- Open with `?mode=ro` — verified to never create or mutate the file —
  bypassing `state.Open` (which migrates). New helper
  `state.Inspect(root) (user_version int, integrityErr error, err error)`
  owns the read-only open and pragmas so doctor never hand-builds a DSN.
- `PRAGMA integrity_check`: the single row `"ok"` → healthy. A non-ok
  row, or the verified error shapes (`database disk image is malformed
  (11)`, `file is not a database (26)`) → check `fail` with the message.
  The file is reported, never touched; rebuild is the future slice.
- `PRAGMA user_version` compared to the current schema version: newer
  than the binary supports → `fail` ("database is from a newer
  projectmux"); older → `warn` ("migrations pending; they run on the
  next mutating command"); equal → `ok`.

### orphaned-sessions

One `Sessions.Sessions(ctx)`-equivalent enumeration via the observer
(`ObserveSession` is workspace-scoped; doctor uses the tmux client's
session listing through a small `SessionLister` interface —
`Sessions(ctx) ([]controller.LiveSession, error)` — which
`*tmux.Client` already implements). For each live identity-carrying
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
case `doctor` + usage entry. Wiring: config root + defaults, state root,
`openStore()` (which may create/migrate the DB file exactly as `list`
does — the `database` check inspects the file *before* the store opens,
so doctor still reports pre-existing corruption faithfully; order:
run `state.Inspect` first, then open the store for the two
store-backed checks, and if the open fails render `orphaned-sessions`
and `stale-bindings` as `unknown`).

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
- `state.Inspect` tests: healthy, corrupted (bytes overwritten mid-file
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
