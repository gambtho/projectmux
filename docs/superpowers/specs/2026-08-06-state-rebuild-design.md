# State Rebuild Slice Design

`projectmux rebuild` — recover lost workspace registrations from live tmux
sessions. This is the explicit state-rebuild action design.md §8 promises
("State rebuilding is explicit rather than an automatic response to
corruption") and the counterpart `internal/doctor` defers to
(`doctor.go:132-134`: "It is reported, never repaired: rebuilding is a
separate, explicit command").

Design.md §7 states the guarantee this slice implements:

> The database is rebuildable. Repository roots rediscover source worktrees,
> tmux user options rediscover live sessions, and a later open can reacquire a
> container binding. Rebuilding may lose diagnostic timestamps and last errors,
> which is acceptable because they are operational metadata rather than desired
> state.

This slice implements the **tmux user options** half. Repository-roots
rediscovery is deliberately excluded (§9).

## 1. Scope

In scope: a live tmux session carries `@dev_workspace_id`, `@dev_slug`, and
`@dev_worktree`, but the state database has no row for it. Rebuild registers
the workspace and adopts the live session name.

Out of scope, deliberately:

- **Pruning records whose worktree is gone.** Requires a delete primitive the
  store does not have and a confirmation story; `stop` remains the only
  destructive command (§8).
- **Rediscovering worktrees from `repository_roots`.** It would register trees
  the user never opened, changing what registration means. The database
  currently holds exactly what was opened.
- **Restoring container bindings.** §7 already assigns this to a later `open`.

The `2026-08-05-doctor-design.md` header anticipated this slice as "backup +
recreate after confirmation". That is deliberately narrowed here: rebuild
refuses on a corrupt database rather than moving it aside (§5). The narrower
command needs no confirmation prompt because it can only add rows.

## 2. Command surface

```
projectmux rebuild [--dry-run] [--json] [--compact]
```

No workspace argument: rebuild works over the whole installation, like
`doctor` and `list`.

It applies by default. That is safe because of two properties established
below: rebuild only ever inserts state that was missing (§4), and re-running
it is idempotent (§8). `--dry-run` classifies and reports without writing.
`list` already serves as a read-only preview of the same drift.

The name follows §7/§8's own vocabulary rather than introducing a new term,
so roots rediscovery can extend this command later instead of forcing a
rename. Because "rebuild" promises more than this slice delivers, the help
text and `docs/commands.md` must state plainly that it recovers registrations
from live sessions and does **not** rediscover worktrees from
`repository_roots` or restore container bindings.

## 3. What can and cannot be recovered

The constraint that shapes the whole design: **tmux carries three identity
keys, but registration needs five fields plus a digest.**

`controller.LiveSession` carries `@dev_workspace_id`, `@dev_slug`, and
`@dev_worktree`. `Store.RegisterWorkspace` takes a `resolve.Workspace{ID,
Slug, Worktree, SessionName, IsPrimary}` plus a `desiredDigest`. Two fields
are in neither place:

- **`IsPrimary`**, which is not cosmetic: `autostart` starts containers only
  for registered primary worktrees, so a guessed value silently changes boot
  behavior.
- **`SessionName`** (the *proposed* name), derived as `slug` or
  `slug--<basename>` depending on `IsPrimary`.

Rebuild therefore cannot synthesize a row from the tmux keys alone. It
re-derives identity the way every other command does — `resolve.Resolve("",
nil, worktree)` against the session's `@dev_worktree`, then `config.Load` for
the digest. Three consequences:

1. **A session whose worktree no longer exists cannot be re-registered.** Git
   cannot run there, so `IsPrimary` and the slug are unknowable. That session
   is reported as a conflict and skipped, consistent with doctor already
   flagging it as `worktree no longer exists`.
2. **Rebuild gets an integrity check for free.** `resolve` derives the ID as
   `sha256(canonical worktree)`. A derived ID that does not equal the
   session's `@dev_workspace_id` means the keys are internally inconsistent —
   a stale key after a tree moved, or a hand-set option. Rebuild refuses that
   row rather than registering identity it cannot confirm.
3. **The digest comes from current configuration.** Registering with today's
   `desired_digest` and a nil `applied_digest` means the next `open` sees
   drift and reconciles — the correct outcome, since the configuration was
   never applied to *this* database. This is exactly the loss §7 sanctions.

Not recovered, all operational metadata rather than desired state: container
bindings, `applied_digest`, `registered_at` (set to now), and last-operation
history.

`resolve.Resolve` needs no `repository_roots` here: roots feed only `byName`,
and rebuild resolves *from a directory*. `LoadDefaults` is still required, for
the digest.

## 4. Classification: fill-only

`internal/cli/list.go:124-142` already distinguishes the cases rebuild meets.
Sorted by what rebuild would do:

| # | Situation | Action |
| --- | --- | --- |
| 0 | Row exists, `actual_session` already equals the live name | Nothing; not reported |
| 1 | Live session, no row at all | **Register + adopt** |
| 2 | Row exists, `actual_session` nil, one live session claims it | **Adopt** |
| 3 | Row exists with `actual_session` set, live name differs | Conflict, skip |
| 4 | Live `@dev_worktree`/`@dev_slug` differ from the recorded ones | Conflict, skip |
| 5 | Two live sessions carry the same `@dev_workspace_id` | Conflict, skip |
| 6 | Live name is another workspace's `actual_session` | Conflict, skip |

Case 0 is what makes a second run a clean no-op, and it is silent rather than
reported: a fully recovered installation must produce an empty report and
exit 0 (§8).

**Precedence, since a session can match several rows at once.** Evaluate 5,
then 6, then 4, then 0, then 2, then 1. Uncertainty wins over action, so two
sessions sharing one unregistered workspace ID are a case-5 conflict and are
never registered. A candidate that survives classification can still become a
conflict during application (§6) — most commonly a worktree that no longer
exists (§3), which cannot be detected during classification because
classification performs no I/O.

**Rebuild never overwrites a recorded value. It only fills in what is
missing.** A mistaken run therefore costs nothing that was already known.

This extends rules the codebase already follows rather than inventing one.
Case 6 is forced by §7 ("Collision resolution and assignment happen in one
transaction"), and `SessionNameConflictError` is documented as "a refusal,
never an overwrite". Case 5 matches `ObserveSession`, which already treats
multiple claimants as uncertainty and picks none. Fill-only extends the same
discipline to cases 3 and 4 — the doctor slice's rule that uncertainty is
reported, never resolved by guessing.

Case 4 is the one where an overwrite would do real damage: it would rewrite a
workspace's identity to point at the wrong tree.

Sessions carrying no `@dev_workspace_id` belong to someone else and are
ignored entirely, as in `buildList` and `orphanedSessions`.

## 5. Corrupt and missing databases

The two damaged-database cases behave differently in the existing code:

- **Missing.** `openStore` → `state.Open(root)` creates the file and runs
  migrations. Rebuild proceeds against a fresh empty database. Nothing is
  destroyed. This is the primary recovery path.
- **Corrupt.** `state.Open` fails (SQLITE_CORRUPT / SQLITE_NOTADB). Rebuild
  cannot obtain a handle.

**Rebuild refuses on a corrupt database.** It does not move the file aside.
That keeps this slice purely additive and keeps `stop` the only destructive
command; the operator performs one inspectable `mv` and re-runs.

The refusal message is the deliverable, so it must be specific: name the
`state.db` path, name the `-wal` and `-shm` sidecars explicitly, and say to
move all three aside and re-run. Naming the sidecars is not pedantry —
per `2026-08-05-doctor-design.md`, moving `state.db` alone leaves a stale WAL
that a freshly created database would inherit.

Automatic relocation behind a `--replace-corrupt` flag remains available as a
follow-up if the manual step proves annoying in practice.

## 6. Architecture

New package `internal/rebuild`, wired by `internal/cli/rebuild.go` — the same
split as the doctor slice.

**Classification is a pure function.** No I/O, no clock, no git:

```go
// Plan is one classification pass over live sessions and stored records.
type Plan struct {
	Candidates []Candidate
	Conflicts  []Conflict
}

func classify(live []controller.LiveSession, records []state.Record) Plan
```

This is where §4's cases are sorted, and it is fully testable from
literals — which matters, because the case analysis is the part most likely
to be wrong.

**Application** performs, per candidate: `resolve.Resolve("", nil, worktree)`
→ verify the derived ID equals the session's key → `config.Load` for the
digest → take the per-workspace lock → `RegisterWorkspace` →
`AdoptSessionName`. A failure at any step becomes a conflict in the report
rather than aborting the batch, matching how `autostart` treats one
workspace's failure.

**Report** is `Report{Registered []Registered, Conflicts []Conflict}`,
deterministically ordered by slug then session name.

Dependencies are interfaces so each half is testable without a live
environment, and the `cli` wiring uses package variables as in
`wiring.go:28-44` so command tests can substitute fakes.

**Flow:** load defaults → open store (corrupt refuses here, §5) → list live
sessions → read records → classify → if `--dry-run`, render and stop →
otherwise apply each candidate under its own lock → render.

**This slice adds no new state mutations.** `RegisterWorkspace` and
`AdoptSessionName` both already exist and are already covered;
`internal/controller/fake` already implements all three methods rebuild
needs. Rebuild is new composition of existing primitives.

### Concurrency

Classification is a snapshot, and the workspace lock is taken per candidate at
apply time, so another process can register in the gap. This degrades safely
without a global rebuild lock:

- `RegisterWorkspace` is an upsert, so a duplicate registration is idempotent
  and preserves `registered_at`, the assigned session name, the applied
  digest, and any binding (`store.go:26-30`).
- `AdoptSessionName` resolves the name in one transaction and returns
  `SessionNameConflictError` if another workspace won the race — already a
  conflict rebuild reports.

The store's transactional guarantees cover the race; a global lock would add a
new failure mode for no gain.

## 7. Exit codes and output

Reusing the existing table rather than adding a code:

| Condition | Code |
| --- | --- |
| Registered everything, or nothing to do | 0 |
| Any conflict or skipped row | 6 |
| tmux unobservable | 6 |
| `defaults.yaml` will not load | 5 |
| One workspace's configuration will not load | 6, batch continues |
| Corrupt database | 1 |
| Usage error | 2 |

Exit 6 is "declined to act because it could not establish what state the world
was in — an ambiguous session" (`docs/commands.md`), which is precisely cases
3-6. A tmux outage is exit 6 for the same reason: it is not "no live
sessions", and registering nothing while reporting success would be the
tri-state violation doctor exists to prevent.

A corrupt database is exit **1**, not 6: every other command exits 1 when the
store will not open, and exit 6 denotes uncertainty about the world, whereas a
corrupt file is a diagnosed, definite condition. The value is in the message
(§5), not in a distinct code.

An unloadable `defaults.yaml` is fatal (exit 5) because the digest is
underivable for every workspace — mirroring doctor's fatal `DefaultsErr`
branch. A single workspace's configuration failure is that workspace's
conflict, not the batch's.

**Stdout contract.** Rebuild's report *is* its output, so it follows the
stop/autostart §5 amendment: the report goes to stdout even when the command
exits 6, with a one-line summary to stderr via `reportedError`. Anything else
would hide the conflicts in exactly the case they need reading.

**`--dry-run` uses the same codes.** Conflicts found in a dry run still exit
6: the exit code describes the state of the world, not whether anything was
written. Otherwise `--dry-run` would report a clean 0 for a situation the real
run refuses.

JSON envelope (additive to `schema_version: 1`):

```json
{
  "schema_version": 1,
  "dry_run": false,
  "registered": [
    {
      "id": "…",
      "slug": "projectmux",
      "worktree": "/home/u/src/projectmux",
      "is_primary": true,
      "session": "projectmux"
    }
  ],
  "conflicts": [
    {"subject": "projectmux--wt", "reason": "two live sessions claim workspace …"}
  ]
}
```

Both arrays are always present, empty rather than absent, matching doctor's
always-full `checks`. Human output renders one line per registration and one
per conflict; that layout is not a compatibility contract.

## 8. Testing

Three layers.

**Classification — table tests, pure, exhaustive.** All seven cases from
literals, plus the precedence order. Every case needs a test that fails if it
is classified into the wrong bucket, including the two easiest to get
backwards: a live session whose ID matches a record with `actual_session`
already set to a *different* name (skip, §4), and the same session name
recorded for a different workspace (skip, §7 of design.md). Combinations too:
two sessions sharing one ID where one also conflicts on name must resolve as
case 5, not case 6.

**Application — real git, fake store.** `resolve.Resolve` shells out to git,
so these need real repositories; `internal/resolve/resolve_test.go:14-45`
already has `git`/`makeRepo`/`addWorktree` helpers to lift. Mocking git would
test the mock. Cases: primary versus linked worktree registering with the
correct `IsPrimary`; the derived-ID mismatch refusal; a vanished worktree
becoming a conflict; one workspace's configuration failure not aborting the
batch.

**Lifecycle — real tmux, end to end.** One new test in
`internal/cli/lifecycle_test.go` performing the actual disaster: `open` a
workspace, delete the state database, run `rebuild`, assert the workspace is
registered again with the live session adopted and `is_primary` correct. Then
run `rebuild` a second time and assert it reports nothing and exits 0. That
second run is the idempotence claim and the one most likely to regress.

**Verification before any completion claim:** `gofmt -l .`, `go vet ./...`,
`CGO_ENABLED=0 go build ./cmd/projectmux`, `go test ./... -count=1 -race`.
Plus mutation testing of the load-bearing assertions — specifically, inverting
the fill-only rule so classification overwrites case 3, and confirming a test
actually fails. A test that passes when the safety rule is inverted is not
testing the safety rule.

## 9. Documentation

- `docs/commands.md` gains a `projectmux rebuild` section.
- The usage block in `internal/cli/cli.go` gains its entry.
- Both state what rebuild does not do: no roots rediscovery, no container
  bindings.

## 10. Risks and follow-ups

- **The name overpromises** relative to what ships. Mitigated by explicit
  documentation (§9) and by leaving the command open to roots rediscovery
  later.
- **Case 3 may prove common** — a recorded `actual_session` disagreeing with
  the live one. If so, a `--force` overwrite is the clean follow-up; the
  fill-only default should not change.
- **Corrupt-database recovery stays manual** (§5). `--replace-corrupt` is the
  follow-up if that proves annoying.
- **Record pruning and roots rediscovery** remain open, each deserving its own
  slice.
