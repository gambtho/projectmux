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
it is idempotent (§8).

`--dry-run` performs every read-only step of the real run — classification,
resolution, identity verification, configuration loading — and stops before
the lock and the two writes. It is a preview rather than a partial pass: a
dry run that reports a conflict is the conflict the real run would report,
and a dry run that says "would register" has already established every fact
registration depends on except the outcome of the writes themselves (§6).
`list` remains a cheaper read-only view of the same drift.

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
2. **Rebuild gets an integrity check for free, and it checks all three keys.**
   `resolve` derives the ID as `sha256(canonical worktree)`, along with the
   slug and the canonical worktree path. Rebuild accepts a session only when
   the resolved workspace agrees with the live session on *all three* keys —
   the `controller.SessionBelongsTo` predicate (`plan.go:107-114`), whose own
   comment states the reason: "a session with the right workspace ID but a
   contradictory slug or worktree is evidence of corruption or collision, not
   a match". Checking the derived ID alone would admit a session carrying a
   stale or hand-set `@dev_slug`, or a non-canonical worktree spelling: it
   would be registered from resolved values that silently disagree with the
   live keys, and the *next* rebuild would then report that row as a case-4
   conflict instead of a clean no-op. Rebuild calls `SessionBelongsTo` rather
   than restating the rule.
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

The vocabulary comes from `internal/cli/list.go:124-142`, which already
distinguishes claimant count and slug/worktree mismatch. The classification
below is nevertheless new work rather than reuse: `buildList` never compares a
sole claimant's name against the recorded `actual_session`, and never detects
that a live name is some *other* record's `actual_session`, so cases 0, 2, 3,
and 6 are rebuild's own semantics. Sorted by what rebuild would do:

| # | Situation | Action |
| --- | --- | --- |
| 0 | Row exists, `actual_session` already equals the live name | Nothing; not reported |
| 1 | Live session, no row at all | **Register + adopt** |
| 2 | Row exists, `actual_session` nil, one live session claims it | **Adopt only** |
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

**Fill-only is a property of which primitive each case calls, not of the
primitives themselves.** `RegisterWorkspace` is an upsert whose conflict
branch overwrites `slug`, `worktree`, `is_primary`, `proposed_session`, and
`desired_digest` (`store.go:43-49`) — correct for re-registration, wrong for
repair. Rebuild therefore calls it in **case 1 only**, where no row exists and
the insert branch runs. Case 2 already has a row and calls `AdoptSessionName`
alone; it never re-registers, so no recorded field can be rewritten. Cases 3-6
write nothing, and case 0 has nothing to write.

The one path that could still reach the upsert branch is a race: another
process registers the workspace between classification and the write. The
final observation under the lock (§6) closes it — a case-1 candidate whose row
appeared in the gap is re-classified there as case 2 and adopts instead of
registering.

Under those rules, **rebuild never overwrites a recorded value. It only fills
in what is missing.** A mistaken run therefore costs nothing that was already
known.

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
- **Corrupt.** Detected *before* the read-write open, not by it.

**`state.Open` is not a corruption test.** It calls `os.MkdirAll`, opens the
pool, and migrates (`state.go:57-75`); against a database already at the
current schema, `migrate` needs only a successful `PRAGMA user_version`
(`migrate.go:64`). Damage elsewhere in the file passes that and surfaces later
as a generic read failure — mid-run, after rebuild has begun writing.

Rebuild therefore classifies the database the way doctor does, before opening
it read-write: `state.OpenReadOnly(root)`, which runs `PRAGMA
integrity_check` (`readonly.go:163-194`), then `Inspection.Usable`.

| `OpenReadOnly` result | Rebuild |
| --- | --- |
| `IsMissingDatabase` | Proceed; `state.Open` creates it |
| `IntegrityErr` set | Refuse, exit 1, with the message below |
| `FutureSchemaError` | Refuse, exit 1: a newer build wrote this database |
| `PendingMigrationError` | Proceed; `state.Open` migrates |
| Incomplete WAL | Proceed; `state.Open` recovers the log |
| Any other error | Refuse, exit 1: uncertainty, never a guess |

Two rows differ from doctor's handling, both deliberately. A pending migration
is doctor's finding but rebuild's normal path — doctor must not migrate, and
rebuild is a mutating command.

The incomplete-WAL case is the harder one. `readonly.go:154-161` refuses a
`-wal` with no `-shm`, which means a writer died without checkpointing —
precisely the crash rebuild exists to recover from, so refusing there would
refuse the main case. But it is currently an untyped `fmt.Errorf`,
indistinguishable from a permission failure. **This slice adds a typed
`state.IncompleteWALError`** beside the existing typed errors in `readonly.go`
and returns it from `incompleteWAL`. Rebuild treats it as proceed; every other
error is a refusal. Doctor's behavior is unchanged — it reports the message
either way.

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

**Application** performs, per candidate, in this order:

1. `resolve.Resolve("", nil, worktree)`.
2. `controller.SessionBelongsTo(session, workspace)` — all three keys (§3).
3. `config.Load` for the desired digest — for CaseRegister only, the only case
   that writes a digest. A workspace whose configuration is broken can still
   have its live session adopted, because adoption does not depend on the
   digest. A failure here is carried to step 5 rather than ending the candidate:
   the case may become an adoption once the lock-held re-classification runs,
   and a candidate that no longer writes a digest is not blocked by one it could
   not load.
4. Take the per-workspace lock.
5. **Re-observe under the lock:** `ObserveSession(SessionQuery{WorkspaceID,
   CandidateNames: []string{session.Name}})`, and re-read the workspace's row.
6. Re-classify from that observation with the same `classify`.
7. Write what the re-classification says: case 1 → `RegisterWorkspace` then
   `AdoptSessionName`; case 2 → `AdoptSessionName` only; anything else →
   report a conflict and write nothing.

Steps 1-3 are read-only, which is what lets `--dry-run` stop after step 3 and
still predict the real run's verdict (§2). A failure at any step becomes a
conflict in the report rather than aborting the batch, matching how
`autostart` treats one workspace's failure.

**Partial application.** `RegisterWorkspace` and `AdoptSessionName` are
separate transactions, so a case-1 candidate can register and then fail to
adopt, leaving a row with a nil `actual_session`. That state needs no new
atomic primitive: it is exactly case 2, so the next rebuild completes it. It
is reported as a conflict at the time, naming both what was written and what
was not — the operator is never told a workspace was registered when only half
of it was.

**Report** is `Report{Registered []Registered, Conflicts []Conflict}`,
deterministically ordered by slug then session name.

Dependencies are interfaces so each half is testable without a live
environment, and the `cli` wiring uses package variables as in
`wiring.go:28-44` so command tests can substitute fakes.

**Flow:** load defaults → inspect the database read-only and classify it (§5)
→ open the store read-write → list live sessions → read records → classify →
if `--dry-run`, run steps 1-3 per candidate, render, and stop → otherwise
apply each candidate through steps 1-7 under its own lock → render.

**This slice adds no new state mutations.** `RegisterWorkspace` and
`AdoptSessionName` both already exist and are already covered;
`internal/controller/fake` already implements all three methods rebuild
needs. Its only change to an existing package is the typed
`state.IncompleteWALError` (§5), which adds no behavior of its own. Rebuild is
new composition of existing primitives.

### Concurrency

The first classification is a snapshot taken outside any lock, so another
process can register a workspace, rename a session, or kill one in the gap.
What closes that gap is the rule the lock package states for every mutating
command — take the lock *before* the final observation and hold it through the
resulting state commit (`lock.go:1-5`). Rebuild follows it: the observation in
step 5 above is taken after the lock, and it is the one the writes are decided
from. The initial pass is a work list, not evidence.

Two residual races remain, both benign:

- **The session dies between step 5 and the write.** The row then records a
  session name that is no longer live — the ordinary state every crash
  produces, which `list`, `doctor`, and the next `open` already handle.
- **Another workspace wins the name.** `AdoptSessionName` resolves the name in
  one transaction and returns `SessionNameConflictError` — already a conflict
  rebuild reports.

A global rebuild lock would add a new failure mode for no gain: rebuild's
writes are per-workspace, and per-workspace locking is what every other
mutating command uses.

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
written. That holds only because a dry run performs the read-only steps 1-3
(§6) — a vanished worktree fails in `resolve`, and a broken workspace
configuration fails in `config.Load`, so both reach the dry run's exit code
too. A dry run that stopped after pure classification would report a clean 0
for situations the real run refuses, which is a preview that misleads in
exactly the case worth previewing.

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

Five layers.

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
correct `IsPrimary`; a session whose `@dev_slug` or `@dev_worktree` disagrees
with the resolved workspace being refused even though the derived ID matches
(§3); a vanished worktree becoming a conflict; one workspace's configuration
failure not aborting the batch; and a `--dry-run` over those same fixtures
reporting the identical conflicts and exit code as the real run, with the
store observing no writes.

**Lock-time reclassification — fake observer, real store.** The step-5
observation is where the fill-only guarantee actually lives, so it needs its
own tests: an observer whose second call differs from its first must change
the write. A case-1 candidate whose row appears in the gap must adopt rather
than register, leaving every pre-existing field byte-identical — the
assertion that would fail if application skipped straight to
`RegisterWorkspace`. A session that vanishes between the two observations must
become a conflict, not a registration. A register-then-failed-adopt must be
reported as a conflict naming both halves, and a second run over that row must
complete it as case 2.

**Corruption — real files.** Overwrite `state.db` with garbage and assert
rebuild refuses with exit 1, names all three paths, and left the file
untouched. Separately, a `-wal` with no `-shm` must *proceed* rather than
refuse (§5) — the crash case rebuild exists for.

**Lifecycle — real tmux, end to end.** One new test in
`internal/cli/lifecycle_test.go` performing the actual disaster: `open` a
workspace, delete the state database, run `rebuild`, assert the workspace is
registered again with the live session adopted and `is_primary` correct. Then
run `rebuild` a second time and assert it reports nothing and exits 0. That
second run is the idempotence claim and the one most likely to regress.

**Verification before any completion claim:** `gofmt -l .`, `go vet ./...`,
`CGO_ENABLED=0 go build ./cmd/projectmux`, `go test ./... -count=1 -race`.
Plus mutation testing of the load-bearing assertions — inverting the fill-only
rule so classification overwrites case 3, and separately making case 2 call
`RegisterWorkspace` instead of adopting, then confirming a test actually fails
in each. A test that passes when the safety rule is inverted is not testing
the safety rule.

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
- **Registration and adoption are not atomic** (§6). The half-written row is
  benign and self-healing, but it is a state a reader can observe. If it turns
  out to be common, a combined `RegisterAndAdopt` transaction in `state` is
  the clean fix; adding one speculatively is not.
- **Record pruning and roots rediscovery** remain open, each deserving its own
  slice.
