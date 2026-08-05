# State slice — SQLite schema, store, and observe/plan foundations

**Status:** Approved design

**Date:** 2026-08-05

**Scope:** Design §13 step 2 of `docs/design.md`: the SQLite schema, the
current-state store, the controller-facing interfaces with fakes, and the pure
observe-and-plan core. No CLI surface changes; the slice is verified entirely
by tests. Commands that make it visible (`list`, `status`) are the next slice.

## 1. Outcome

Two new packages:

- **`internal/state`** owns the database. It resolves the state directory,
  applies embedded migrations, and issues every line of SQL in the
  application. It exposes coarse, transactional domain methods; transaction
  boundaries are decided inside the store, never by callers.
- **`internal/controller`** owns the domain types and the interfaces it
  consumes, implements `Observe` (assemble a snapshot) and `Plan` (pure
  function from snapshot to typed actions), and ships in-memory fakes in
  `internal/controller/fake` for every interface, exported for reuse by the
  observation-commands slice.

Existing packages are unchanged except as inputs: `controller` consumes
`config.Effective` and `resolve.Workspace`.

## 2. State directory and database lifecycle

- Directory: `$XDG_STATE_HOME/projectmux`, falling back to
  `~/.local/state/projectmux`; `PROJECTMUX_STATE_ROOT` overrides it, mirroring
  `PROJECTMUX_CONFIG_ROOT`. The database file is `state.db`.
- `state.Open(root)` creates the directory and file as needed, configures the
  pool, applies pending migrations, and returns the store. `Close` closes the
  pool.
- Connection policy (design §11): WAL, a five-second busy timeout, and
  `foreign_keys=ON` are applied to **every pooled connection** via
  `modernc.org/sqlite` DSN `_pragma` parameters, not by a once-per-pool
  `Exec`. A test opens at least two concurrent connections and asserts the
  pragmas on each.
- Migrations are embedded SQL files applied in order, each in its own
  `BEGIN IMMEDIATE` transaction, with `PRAGMA user_version` recording the
  version inside that same transaction. The pending set is computed from a
  `user_version` **re-read inside each transaction**, so two store handles
  opening the same database concurrently serialize on the write lock and the
  second finds nothing left to apply — a migration is never replayed. Opening
  a database whose `user_version` is **newer** than the binary supports fails
  with a typed error naming both versions; it never misreads or "repairs".

## 3. Schema (migration 0001)

Timestamps are RFC3339 UTC text supplied by the caller (the controller owns
the clock), never `CURRENT_TIMESTAMP`.

```sql
CREATE TABLE workspaces (
    id               TEXT PRIMARY KEY,          -- hex SHA-256 of canonical worktree
    slug             TEXT NOT NULL,
    worktree         TEXT NOT NULL UNIQUE,
    is_primary       INTEGER NOT NULL CHECK (is_primary IN (0, 1)),
    proposed_session TEXT NOT NULL,
    actual_session   TEXT UNIQUE,               -- NULL until assigned
    desired_digest   TEXT,
    applied_digest   TEXT,
    registered_at    TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

CREATE TABLE container_bindings (
    workspace_id   TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    container_id   TEXT NOT NULL,
    container_user TEXT,
    workdir        TEXT,
    health         TEXT NOT NULL CHECK (health IN ('present', 'missing', 'unknown')),
    observed_at    TEXT NOT NULL
);

CREATE TABLE last_operations (
    workspace_id  TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    operation     TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN ('ok', 'failed')),
    exit_status   INTEGER,
    error_summary TEXT,                          -- truncated to 4 KiB by the store
    finished_at   TEXT NOT NULL
);
```

Design-§7 invariants made structural:

- `actual_session TEXT UNIQUE` — SQLite `UNIQUE` ignores NULLs, so uniqueness
  holds exactly over assigned names.
- An absent `container_bindings` row **is** "no binding has ever been
  recorded"; `health` cannot be null or invalid while a binding exists.
- `missing` and `unknown` are health values only; no code path clears
  `container_id` short of successful replacement or workspace deletion
  (deletion itself is a later slice).

Digest columns store `config.Effective.Digest` (`"sha256:" + hex`) as opaque
strings; drift is string inequality.

## 4. Store API

Every method is one SQLite transaction internally. Reads return typed
not-found (`ErrNotFound` via `errors.Is`), never sentinel strings.

- `Open(root string) (*Store, error)`, `Close() error`.
- `RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error`
  — upsert by ID: insert the identity row, or refresh slug, worktree,
  is_primary, proposed_session, desired_digest, updated_at on
  re-registration. Idempotent; rebuilding the database is re-running
  registration.
- `AllocateSessionName(workspaceID string, now time.Time) (string, error)` —
  one `BEGIN IMMEDIATE` transaction: if an actual name is already assigned,
  return it; otherwise try the proposed name, then `<proposed>-2`, `-3`, …
  until the `UNIQUE` constraint accepts one. The constraint is the
  collision-prevention mechanism; there is no SELECT-then-INSERT check.
- `RecordContainerObservation(workspaceID string, obs state.ContainerObservation, now time.Time) error`
  — upsert the binding. `present` with a new ID replaces the binding;
  `missing`/`unknown` update health and observed_at while retaining the
  stored ID and connection metadata.
- `RecordOperation(workspaceID string, op Operation, now time.Time) error` —
  upsert `last_operations`; the store truncates `error_summary` to 4096
  bytes.
- `CommitReconciliation(workspaceID string, r ReconciliationResult, now time.Time) error`
  — design-§9 step 5 as one transaction: optional applied digest, optional
  container observation, and operation outcome together. `AppliedDigest` is a
  pointer field set only when the desired configuration was fully applied; a
  failed or partial reconciliation commits its outcome (and any container
  observation) while leaving `applied_digest` untouched, so drift is never
  cleared by a failure.
- `Workspace(id string) (Record, error)` and `Workspaces() ([]Record, error)`
  — joined record: identity plus optional binding plus optional last
  operation, absence of the optional parts expressed as nil fields.

## 5. Controller interfaces, observe, and plan

Type ownership and import direction: `internal/state` defines the stored-
record types (`state.Record`, `state.ContainerBinding`,
`state.ContainerObservation`, `state.Operation`, `state.ReconciliationResult`)
and knows nothing of the controller. `internal/controller` imports `state`,
reuses those types in its `Store` interface (so `*state.Store` satisfies it
without adapters), and defines the live-observation types
(`controller.SessionObservation`, `controller.ContainerObservation`) that
future tmux/container adapters will produce. The dependency arrow only ever
points controller → state.

Interfaces are defined in `internal/controller`, where they are consumed:

```go
type Store interface { /* the subset of state methods the controller uses */ }

type SessionObserver interface {
    // ObserveSession reports both the live tmux session carrying the
    // workspace's identity keys (@dev_workspace_id, @dev_slug, @dev_worktree)
    // and any live session occupying one of the candidate names, so planning
    // can distinguish adoption from a foreign session squatting on a name the
    // workspace would use.
    ObserveSession(ctx context.Context, q SessionQuery) (SessionObservation, error)
}

// SessionQuery carries the workspace identity plus the candidate session
// names (proposed and, when assigned, actual).
type SessionQuery struct {
    WorkspaceID    string
    CandidateNames []string
}

// SessionObservation reports ByIdentity (the session matching the identity
// keys, if any) and ByName (live sessions holding a candidate name, with
// whatever identity keys they carry, possibly none).

type ContainerObserver interface {
    // ProbeContainer checks the liveness of an existing binding.
    ProbeContainer(ctx context.Context, binding state.ContainerBinding) (ContainerObservation, error)
    // DiscoverContainer looks for a container for a workspace with no stored
    // binding — post-rebuild reacquisition and devcontainer.enabled: auto
    // resolution both need the workspace and its effective configuration.
    DiscoverContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (ContainerObservation, error)
}

type Clock interface{ Now() time.Time }
```

The three tmux identity keys are declared as exported constants here; their
spelling is load-bearing for Phase 1 adoption (design §13 step 7).
`SessionObserver`'s granularity is provisional until the tmux adapter exists;
it is internal and cheap to renegotiate.

`Observe(ctx, desired, ws)` assembles a `Snapshot` from the stored record and
both observations. Observer failures become typed uncertainty inside the
snapshot, never a guess in either direction: a failed container probe is
`health=unknown` (design §9: probe failure never converts to loss), and a
failed session observation marks the session state `unknown` — a tmux outage
is not absence. Both session and container knowledge in the snapshot are
tri-state: `live`/`absent`/`unknown` and `present`/`missing`/`unknown`.

`Plan(snapshot)` is pure and returns:

- **Session action:** `none` (live, identity matches, recorded), `adopt`
  (live with matching keys but absent/stale record), `create` (session state
  confirmed `absent`), or `refuse` — plus a record-name flag when no actual
  name is assigned yet. Session state `unknown` always plans `refuse`: no
  mutating action may be derived from an unobservable tmux.
- **Container action:** `none` (devcontainer disabled, or health `present`),
  `start` (enabled and `missing` or never bound), `probe-first` (enabled and
  `unknown`).
- **Drift and refusal:** desired digest ≠ applied digest sets a reapply flag;
  a live session occupying a candidate name whose identity keys contradict
  the workspace (or that carries no identity keys) plans `refuse` with a
  typed explanation, never silent adoption or renaming (design §7's
  cross-workspace guard).

Execution of plans (`ensure`/`stop`), the per-workspace filesystem lock, and
real adapters are later slices.

## 6. Testing

- Migration: fresh create reaches the latest version; a too-new
  `user_version` is refused with the typed error; two independent store
  handles opening the same database concurrently both succeed and apply
  migration 0001 exactly once. (Version-N→latest tests begin when a second
  migration exists.)
- Concurrency: a real-SQLite test under `-race` runs concurrent
  `AllocateSessionName` calls for workspaces sharing a proposed name and
  proves the `UNIQUE` constraint yields distinct assigned names.
- Tri-state: probe failure records `unknown` retaining the ID; confirmed
  absence records `missing` retaining the ID; successful replacement
  overwrites; a never-bound workspace has no binding row.
- Pragmas: at least two concurrent pooled connections each report WAL, the
  5s busy timeout, and foreign-keys enabled.
- Store round-trips for every method, including error-summary truncation and
  `ErrNotFound`; `CommitReconciliation` with a failed outcome (nil
  `AppliedDigest`) leaves `applied_digest` untouched.
- Table-driven `Plan` tests over the snapshot matrix (session
  live/absent/unknown × identity match/mismatch/none × name occupied by a
  foreign session × binding health × digest drift) using the fakes,
  including: session `unknown` plans `refuse`; a foreign session on a
  candidate name plans `refuse`.
- Fakes conform to the interfaces via compile-time assertions.

## 7. Exclusions

No CLI changes, no real tmux/container adapters, no ensure/stop execution, no
filesystem lock, no doctor, no autostart, no workspace deletion, no data
migration from the Bash implementation.

## 8. Decisions recorded

- Two-table split (`workspaces` + `container_bindings`) so §7's binding
  invariants are schema-enforced rather than conventions.
- Coarse transactional store methods; callers never see transactions. One
  composite `CommitReconciliation` covers §9 step 5.
- `PRAGMA user_version` + embedded ordered SQL files for migrations; too-new
  databases are refused, never repaired.
- Timestamps injected by callers (controller clock), stored as RFC3339 UTC
  text.
- Error summaries bounded at 4096 bytes in the store.
- Fakes live in the exported `internal/controller/fake` package for reuse by
  the next slice.
- Session observation is a query (identity + candidate names) returning both
  identity-matched and name-occupying sessions, so the cross-workspace guard
  is decidable at plan time.
- Session knowledge is tri-state; `unknown` (observation failure) always
  plans `refuse` — a tmux outage never reads as absence.
- Container observation is split into `ProbeContainer` (existing binding) and
  `DiscoverContainer` (workspace + effective config), covering post-rebuild
  reacquisition and `enabled: auto`.
- Migrations serialize via `BEGIN IMMEDIATE` with `user_version` re-read
  inside the transaction; concurrent `Open` is tested.
- `CommitReconciliation.AppliedDigest` is optional; failure never clears
  drift.
