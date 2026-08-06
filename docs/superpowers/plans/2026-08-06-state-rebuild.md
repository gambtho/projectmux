# State Rebuild Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `projectmux rebuild`, the explicit state-repair command that
recovers lost workspace registrations by reading identity back out of live tmux
sessions.

**Architecture:** A new `internal/rebuild` package holds a pure classifier
(`Classify`, no I/O) and an applier that resolves, verifies identity, takes the
per-workspace lock, re-observes under it, re-classifies, and only then writes.
`internal/cli/rebuild.go` is a thin command that wires production adapters into
that applier, renders a versioned report, and maps the outcome to an exit code.
The one change to an existing package is a typed `state.IncompleteWALError`, so
rebuild can tell "a writer crashed" — the case it exists to recover — apart from
a database it must refuse.

**Tech Stack:** Go 1.25, `modernc.org/sqlite` (WAL), tmux user options for
identity, `internal/lock` flock for mutual exclusion, `text/tabwriter` for human
output.

**Design spec:** `docs/superpowers/specs/2026-08-06-state-rebuild-design.md`.
Section references below (§4, §5, §6…) point into it.

## Global Constraints

- Module `github.com/gambtho/projectmux`, Go 1.25. `CGO_ENABLED=0` must keep
  building — no cgo-dependent dependency may be added.
- **Fill-only.** Rebuild only ever adds information the database lacked. It
  never overwrites a recorded value. This is a property of *which* primitive
  each case calls, not of the primitives themselves: `RegisterWorkspace` is an
  upsert whose conflict branch overwrites `slug`, `worktree`, `is_primary`,
  `proposed_session`, and `desired_digest` (`internal/state/store.go:43-49`),
  so only case 1 may call it.
- **Tri-state discipline.** ok / unknown / fail. Uncertainty is reported, never
  resolved by guessing. Rebuild refuses rather than writing on a hunch.
- **Identity is three keys**, not one: `@dev_workspace_id`, `@dev_slug`,
  `@dev_worktree`, compared by `controller.SessionBelongsTo`
  (`internal/controller/plan.go:107-114`).
- **Lock before the final observation.** A mutating command takes the
  per-workspace lock *before* the observation it acts on and holds it through
  the commit (`internal/lock/lock.go:1-5`).
- **No stdout on failure**, with one deliberate exception: `reportedError`
  (`internal/cli/cli.go:78`), for commands whose report *is* their output.
  Rebuild uses it.
- Exit codes are fixed by `internal/cli/cli.go`: 0 ok, 1 error, 2 usage,
  3 ambiguous, 4 unknown workspace, 5 invalid config, 6 refused.
- JSON output is additive to `schema_version: 1`
  (`cli.OutputSchemaVersion`). Arrays are always present, never absent when
  empty. Human output is not a compatibility contract.
- Verification gate, all four, before any completion claim: `gofmt -l .` (empty
  output), `go vet ./...`, `CGO_ENABLED=0 go build ./cmd/projectmux`,
  `go test ./... -count=1 -race`.

## File Structure

| File | Responsibility | Task |
| --- | --- | --- |
| `internal/state/readonly.go` (modify) | Typed `IncompleteWALError` + `IsIncompleteWAL`, so a crashed writer is distinguishable from a database rebuild must refuse | 1 |
| `internal/rebuild/classify.go` (create) | Pure case analysis: live sessions × stored records → candidates and conflicts. No I/O. | 2 |
| `internal/rebuild/apply.go` (create) | Resolve, verify identity, lock, re-observe, re-classify, write. All dependencies are interfaces. | 3 |
| `internal/cli/wiring.go` (modify) | `rebuildDatabaseCheck` — the read-only pre-flight that decides whether the database is safe to open at all | 4 |
| `internal/cli/rebuild.go` (create) | Flag parsing, production adapters, report rendering, exit-code mapping | 5 |
| `internal/cli/cli.go` (modify) | Usage entry and dispatch case | 5 |
| `docs/commands.md` (modify) | The user-facing `projectmux rebuild` section, including what it does *not* do | 5 |

Tests live beside their subjects: `classify_test.go`, `apply_test.go`,
`rebuild_check_test.go`, `rebuild_test.go`, plus an end-to-end case in the
existing `internal/cli/lifecycle_test.go` (task 6).

The split between `classify.go` and `apply.go` is the load-bearing one: the
case analysis is where the fill-only rule is decided, and keeping it a pure
function means every case and every precedence pair can be tested without a
store, a lock, or a tmux socket.

---

### Task 1: Typed `state.IncompleteWALError`, and a WAL state that means it

Spec §5. `readonly.go` refuses a database whose `-wal` has no `-shm` beside it,
because reading such a log would recover it into the state directory and an
inspection must not write. That refusal is currently an untyped `fmt.Errorf`,
indistinguishable from a permission failure — but the two callers want opposite
things. Doctor must stop; rebuild must *proceed*, because an unrecovered log is
exactly the crash `rebuild` exists to recover from, and `state.Open` recovers it.

**The type alone is not enough, and this is the load-bearing half of the task.**
`walIncomplete` today is four situations wearing one name
(`internal/state/readonly.go:140-152`): a `-wal` whose stat failed for a reason
other than absence, a `-wal` that is not a regular file, a `-shm` whose stat
failed for any reason at all — and the one case the name describes, a regular
`-wal` with `-shm` confirmed absent. Doctor may overload them because it maps the
whole bucket to a single *unknown*, which is honest. Rebuild may not: it reads
this classification as **proceed**, so an unexaminable sidecar would be silently
promoted from "we could not look" to "we looked and it is fine". That is the
tri-state violation this slice's Global Constraints forbid, committed by the very
first task. So `walStateOf` gains a fourth state, and only the confirmed case
becomes `IncompleteWALError`.

Doctor's behavior on the confirmed case must not change. It prints the message
either way, so that message text stays byte-identical and the test asserts it
byte for byte. The unexaminable cases get a new, different message — doctor
still refuses on them, which is all it did before, so no golden output moves.

**Files:**
- Modify: `internal/state/readonly.go:14-26` (add the type beside `PendingMigrationError`)
- Modify: `internal/state/readonly.go:87-96` (a fourth arm in `OpenReadOnly`'s switch)
- Modify: `internal/state/readonly.go:118-152` (add `walUnknown`; narrow `walStateOf`)
- Modify: `internal/state/readonly.go:154-161` (replace the `incompleteWAL` helper)
- Modify: `internal/state/readonly.go:222-225` (add `IsIncompleteWAL` beside `IsMissingDatabase`)
- Test: `internal/state/readonly_test.go` (this file exists — **add** two functions and
  **amend** `TestWalStateOfClassifiesSidecars` at :299-318; do not create the file)

**Interfaces:**
- Consumes: nothing.
- Produces:
```go
type IncompleteWALError struct{ Path string }

func (e *IncompleteWALError) Error() string
func IsIncompleteWAL(err error) bool
```

The unexaminable case gets no exported type and no predicate. Task 4 reaches it
through its `default:` arm — "any other error, refuse" — which is exactly the
behavior wanted, and an exported symbol nobody names is a symbol that invites
someone to special-case it later.

A note on the test fixture, since staging this state is the awkward part and the
repository already solved it: `internal/state/readonly_test.go:231` defines
`seedUnrecoveredWAL(t)`, which copies the `-wal` aside while a writer still holds
it and restores both files after that writer closes (a clean `Close` checkpoints
the log away, so the pre-checkpoint database bytes have to be restored too). It
asserts `walStateOf(path) == walIncomplete` before returning. Reuse it; do not
write a second fixture. That assertion stays true after this task — the fixture
stages precisely the confirmed case.

- [ ] **Step 1: Write the failing tests**

Append to `internal/state/readonly_test.go`:

```go
// TestOpenReadOnlyUnrecoveredWALIsTyped pins the one refusal a mutating
// command must tell apart from all the others. An unrecovered log is the
// crash case rebuild exists to recover — state.Open recovers it — while a
// permission failure is uncertainty rebuild must stop on. Untyped, the two
// are the same error value. The message is asserted byte for byte because
// doctor prints it unchanged and this change must not touch its output.
func TestOpenReadOnlyUnrecoveredWALIsTyped(t *testing.T) {
	root := seedUnrecoveredWAL(t)
	path := DBPath(root)

	ro, _, err := OpenReadOnly(root)
	if err == nil {
		ro.Close()
		t.Fatal("an unrecovered write-ahead log opened cleanly")
	}
	var walErr *IncompleteWALError
	if !errors.As(err, &walErr) {
		t.Fatalf("OpenReadOnly error = %T (%v), want *IncompleteWALError", err, err)
	}
	if walErr.Path != path {
		t.Errorf("Path = %q, want %q", walErr.Path, path)
	}
	if !IsIncompleteWAL(err) {
		t.Error("IsIncompleteWAL = false on an unrecovered write-ahead log")
	}

	want := "the state database at " + path + " has a write-ahead log with no shared-memory index, " +
		"which means a writer did not shut down cleanly; reading it would require " +
		"recovering the log into the state directory, which an inspection must not do. " +
		"The next mutating command recovers it"
	if got := err.Error(); got != want {
		t.Errorf("message changed:\n got %q\nwant %q", got, want)
	}

	// The predicate must be as narrow as the type: every other refusal is
	// still a refusal, and a missing database is the nearest neighbour.
	if IsMissingDatabase(err) {
		t.Error("an unrecovered write-ahead log was reported as a missing database")
	}
	if _, _, missing := OpenReadOnly(t.TempDir()); IsIncompleteWAL(missing) {
		t.Error("a missing database was reported as an unrecovered write-ahead log")
	}
}

// TestOpenReadOnlyUnexaminableSidecarsAreNotTypedAsAnIncompleteWAL is the
// reason walUnknown exists. Every case here is "the filesystem would not
// tell us", which is uncertainty; the typed error means "we looked, and a
// writer crashed", which a mutating command is licensed to proceed past.
// Collapsing the two would let rebuild open a database it never examined.
func TestOpenReadOnlyUnexaminableSidecarsAreNotTypedAsAnIncompleteWAL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, path string)
	}{
		{
			name: "a -wal that is not a regular file",
			stage: func(t *testing.T, path string) {
				if err := os.Mkdir(path+"-wal", 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
		},
		{
			name: "a -shm that cannot be stat'ed",
			stage: func(t *testing.T, path string) {
				if err := os.WriteFile(path+"-wal", []byte("log"), 0o600); err != nil {
					t.Fatalf("writing the -wal: %v", err)
				}
				// A -shm inside an unsearchable directory: stat fails with
				// EACCES rather than ENOENT, which is "we cannot tell"
				// rather than "it is absent".
				blocked := filepath.Join(t.TempDir(), "blocked")
				if err := os.Mkdir(blocked, 0o000); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
				if _, err := os.Stat(filepath.Join(blocked, "probe")); errors.Is(err, fs.ErrNotExist) {
					t.Skip("this filesystem or user ignores directory permissions")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := seedDatabase(t)
			path := DBPath(root)
			tc.stage(t, path)

			ro, _, err := OpenReadOnly(root)
			if err == nil {
				ro.Close()
				t.Fatal("an unexaminable sidecar opened cleanly")
			}
			if IsIncompleteWAL(err) {
				t.Errorf("uncertainty was typed as a recoverable crash: %v", err)
			}
		})
	}
}
```

The second case needs a `-shm` whose stat fails for a reason other than absence,
and the portable way to get one is an unsearchable parent directory. It cannot be
the *state* root's parent — `OpenReadOnly` stats the database itself first
(`internal/state/readonly.go:79`) and would fail there instead, testing nothing.
So the case is staged as a skip-guarded probe: if this filesystem or this user
ignores directory permissions (running as root does), the sub-test skips rather
than passing vacuously. The first case needs no such guard and carries the
finding on its own.

Amend `TestWalStateOfClassifiesSidecars` (`internal/state/readonly_test.go:299-318`).
Its final assertion currently expects a directory `-wal` to be `walIncomplete`;
that expectation is the bug. Replace lines 311-317 with:

```go
	// A sidecar that cannot be examined is not an absent one — and it is
	// not a crashed writer either. walIncomplete means "we looked, and a
	// writer left a log behind"; this is "we could not look".
	if err := os.Mkdir(clean+"-wal", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := walStateOf(clean); got != walUnknown {
		t.Errorf("an unexaminable -wal = %v, want walUnknown", got)
	}
```

and update its doc comment from "the three states" to "the four states".

Add `"io/fs"` and `"path/filepath"` to the test file's imports if they are not
already there; `errors`, `os`, and `testing` already are.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/state/ -run 'TestOpenReadOnlyUnrecoveredWALIsTyped|TestOpenReadOnlyUnexaminableSidecars|TestWalStateOfClassifiesSidecars' -v`

Expected: a build failure, not a test failure —
`undefined: IncompleteWALError`, `undefined: IsIncompleteWAL`, `undefined: walUnknown`.

- [ ] **Step 3: Write the implementation**

In `internal/state/readonly.go`, insert after the `PendingMigrationError` block
(after line 26, before `// Inspection is what a read-only open learned...`):

```go
// IncompleteWALError reports a database whose write-ahead log cannot be
// read without writing: a -wal with no -shm beside it, which is what a
// writer that did not shut down cleanly leaves behind. It is typed rather
// than a bare message because its two readers want opposite things — an
// inspection must refuse, since recovering the log would alter the state
// root, while a mutating command is precisely what recovers it.
//
// It is deliberately narrower than "the sidecars were not readable". A
// stat that failed is uncertainty, and a mutating command that treated
// uncertainty as this error would open a database it never examined.
// walStateOf keeps the two apart; see walUnknown.
type IncompleteWALError struct {
	Path string
}

func (e *IncompleteWALError) Error() string {
	return fmt.Sprintf(
		"the state database at %s has a write-ahead log with no shared-memory index, "+
			"which means a writer did not shut down cleanly; reading it would require "+
			"recovering the log into the state directory, which an inspection must not do. "+
			"The next mutating command recovers it", e.Path)
}
```

Replace `OpenReadOnly`'s switch (currently lines 87-96) with:

```go
	switch walStateOf(path) {
	case walComplete:
		// The sidecars are already there, so the ordinary path adds
		// nothing to the state root — and it is the only one that sees
		// what the -wal holds. Immutable reads of a live WAL database
		// silently omit committed rows.
		return inspect(dsn)
	case walIncomplete:
		return nil, Inspection{}, &IncompleteWALError{Path: path}
	case walUnknown:
		return nil, Inspection{}, unexaminableSidecars(path)
	}
```

Replace the `walState` constants and `walStateOf` (currently lines 118-152) with:

```go
// walState is what the files beside a database say about its write-ahead
// log. The last two cases exist because a -wal alone does not describe a
// readable WAL state, and because failing to examine the sidecars is a
// different answer from examining them and finding a crash.
type walState int

const (
	// walNone: no write-ahead log, so nothing is uncheckpointed.
	walNone walState = iota
	// walComplete: a -wal and the -shm index that reads it, both present.
	walComplete
	// walIncomplete: a regular -wal with -shm confirmed absent. Neither
	// DSN serves this state — the ordinary path creates the missing -shm
	// and keeps it, and an immutable read silently omits every row the
	// -wal holds. This is what an unclean shutdown leaves, and the one
	// state a mutating command may proceed through: opening read-write
	// recovers the log.
	walIncomplete
	// walUnknown: the sidecars could not be examined — a stat that failed
	// for any reason other than absence, or a -wal that is not a regular
	// file. Everyone refuses. Reporting this as walIncomplete would tell
	// a mutating command it may proceed, on the strength of a question
	// that was never answered.
	walUnknown
)

// walStateOf classifies the sidecars beside path. A stat that fails for
// any reason other than absence leaves the question open, and an open
// question is never resolved as "no log" — that is the one reading under
// which a silent immutable read looks safe — nor as "a writer crashed",
// which is the one reading under which a read-write open looks safe.
func walStateOf(path string) walState {
	wal, err := os.Stat(path + "-wal")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return walNone
	case err != nil:
		return walUnknown
	case !wal.Mode().IsRegular():
		// A directory, device, or socket named state.db-wal is not a log
		// this code understands, whatever else it may be.
		return walUnknown
	}
	if _, err := os.Stat(path + "-shm"); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return walIncomplete
		}
		return walUnknown
	}
	return walComplete
}
```

Replace the `incompleteWAL` helper (lines 154-161, including its comment) with
the message for the case that has no type:

```go
// unexaminableSidecars reports sidecars the filesystem would not describe.
// It is not IncompleteWALError: that one asserts a writer crashed, which
// is a licence to recover the log, and nothing here established that.
func unexaminableSidecars(path string) error {
	return fmt.Errorf(
		"the state database at %s has write-ahead log files that could not be examined, "+
			"so whether the log needs recovering is unknown; check the permissions and "+
			"contents of the state directory", path)
}
```

Add beside `IsMissingDatabase` at the end of the file:

```go
// IsIncompleteWAL reports whether an OpenReadOnly error means the database
// has a write-ahead log no reader can open without recovering it. Alone
// among an inspection's refusals it is not a reason for a mutating command
// to stop: opening the database read-write is what recovers the log. It is
// false for sidecars that could not be examined, which stop everyone.
func IsIncompleteWAL(err error) bool {
	var e *IncompleteWALError
	return errors.As(err, &e)
}
```

`errors`, `fmt`, `io/fs`, and `os` are all already imported by `readonly.go`; no
import changes are needed.

Leave the post-read re-check at `internal/state/readonly.go:107` alone. It asks
`walStateOf(path) != walNone`, which still means "a log appeared while we were
reading" for every state including the new one, and still ends in the same
refusal. The message names a mid-inspection write, which for `walUnknown` is a
guess about a race that has already made the answer unknowable — but it is a
refusal either way, and narrowing it would add a fourth message for a case no
one can reach deliberately.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/state/ ./internal/doctor/ ./internal/cli/ -count=1`

All three new or amended tests must pass, and so must the pre-existing
`TestOpenReadOnlyUnrecoveredWALIsUncertainty` — that one is the guarantee that
the state root is still left untouched. Doctor's and the CLI's tests must pass
unchanged; if any doctor golden output moved, the confirmed-case message was
altered and step 3 is wrong. Doctor's output for the *unexaminable* cases does
change, which is intended and covered by no golden test — it refused before and
refuses now, with a message that no longer claims a crash it did not observe.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/state
go vet ./internal/state/
git add internal/state/readonly.go internal/state/readonly_test.go
git commit -m "feat(state): type the incomplete write-ahead log refusal

An unrecovered -wal is the one inspection refusal a mutating command
should proceed through, since opening read-write recovers the log.
IncompleteWALError and IsIncompleteWAL let rebuild tell it apart from a
permission failure; the message is unchanged, so doctor's output is too.

walStateOf gains walUnknown to make that distinction real. It previously
returned walIncomplete for a failed stat and for a -wal that was not a
regular file, neither of which establishes that a writer crashed. Typing
that bucket as recoverable would have let a mutating command open a
database whose sidecars it never managed to examine."
```


---

### Task 2: `internal/rebuild` classification (pure function)

Spec §4 and §8. This is the case analysis the whole slice rests on, so it is a
pure function — no I/O, no clock, no git — and it is tested exhaustively from
literals.

The rule the tests exist to defend is **fill-only**: rebuild writes only what is
missing and never overwrites a recorded value. Classification enforces it by
which bucket a session lands in, so every test asserts the bucket, and the
conflict tests assert the exact user-facing reason string.

**Precedence is the subtle part.** One live session can match several rows at
once, and uncertainty always wins over action. Evaluate in this order:
`CaseDuplicateID`, `CaseNameTaken`, `CaseIdentityMismatch`, `CaseSettled`,
`CaseAdopt`, `CaseRegister` — with `CaseSessionMismatch` as the residual when a
row exists and already records a *different* session name.

`CaseSettled` produces nothing at all: not a candidate, not a conflict. That
silence is what makes a second `rebuild` run a clean no-op that exits 0, which
spec §8 calls the claim most likely to regress.

**Files:**
- Create: `internal/rebuild/classify.go`
- Test: `internal/rebuild/classify_test.go`

**Interfaces:**
- Consumes: `controller.LiveSession` (`internal/controller/types.go:33`),
  `state.Record` (`internal/state/types.go:34`). Neither is modified.
- Produces (later tasks are written against these exact names — do not rename):
```go
type Case int

const (
	CaseSettled Case = iota
	CaseRegister
	CaseAdopt
	CaseSessionMismatch
	CaseIdentityMismatch
	CaseDuplicateID
	CaseNameTaken
)

func (c Case) String() string

type Candidate struct {
	Case    Case
	Session controller.LiveSession
	Record  *state.Record
}
type Conflict struct {
	Subject string
	Reason  string
}
type Plan struct {
	Candidates []Candidate
	Conflicts  []Conflict
}

func Classify(live []controller.LiveSession, records []state.Record) Plan
```

`Candidate.Record` is nil for `CaseRegister` and non-nil for `CaseAdopt`. It
points into a copy `Classify` owns, so a caller cannot mutate the slice it
passed in through it.

`Conflict` carries no case discriminant by design — its two fields are what the
report renders. Tests therefore identify a conflict's case by its exact reason
string, which is worth pinning anyway: those sentences are the deliverable when
rebuild declines to act.

---

- [ ] **Step 1: Write the failing test — the seven cases**

Create `internal/rebuild/classify_test.go`:

```go
package rebuild

import (
	"reflect"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// live builds a session carrying the three tmux identity keys.
func live(name, id, slug, worktree string) controller.LiveSession {
	return controller.LiveSession{
		ID:          "$0",
		Name:        name,
		WorkspaceID: id,
		Slug:        slug,
		Worktree:    worktree,
	}
}

// stored builds a registered row. actual is the recorded actual_session,
// empty meaning nil — the state a workspace is in when no session has been
// adopted for it.
func stored(id, slug, worktree, actual string) state.Record {
	rec := state.Record{
		ID:              id,
		Slug:            slug,
		Worktree:        worktree,
		IsPrimary:       true,
		ProposedSession: slug,
	}
	if actual != "" {
		rec.ActualSession = &actual
	}
	return rec
}

// onlyCandidate asserts the plan holds exactly one candidate in the
// expected case and no conflicts, and returns it.
func onlyCandidate(t *testing.T, plan Plan, want Case) Candidate {
	t.Helper()
	if len(plan.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", plan.Conflicts)
	}
	if len(plan.Candidates) != 1 {
		t.Fatalf("%d candidates, want 1: %+v", len(plan.Candidates), plan.Candidates)
	}
	if got := plan.Candidates[0].Case; got != want {
		t.Fatalf("case = %s, want %s", got, want)
	}
	return plan.Candidates[0]
}

// onlyConflict asserts the plan holds exactly one conflict with the
// expected subject and reason, and no candidates. The reason is compared
// in full: it is what the operator reads when rebuild declines to act.
func onlyConflict(t *testing.T, plan Plan, subject, reason string) {
	t.Helper()
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none: this session must not be written for", plan.Candidates)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("%d conflicts, want 1: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	got := plan.Conflicts[0]
	if got.Subject != subject {
		t.Errorf("subject = %q, want %q", got.Subject, subject)
	}
	if got.Reason != reason {
		t.Errorf("reason =\n %q\nwant\n %q", got.Reason, reason)
	}
}

// TestClassifyRegistersAnUnrecordedSession is the primary recovery path:
// the database lost the row, tmux still has the session.
func TestClassifyRegistersAnUnrecordedSession(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		nil,
	)
	cand := onlyCandidate(t, plan, CaseRegister)
	if cand.Session.Name != "slab" {
		t.Errorf("session = %q, want %q", cand.Session.Name, "slab")
	}
	if cand.Record != nil {
		t.Errorf("Record = %+v, want nil: there is no row to carry", cand.Record)
	}
}

// TestClassifyAdoptsARowWithNoSession covers the half-recovered state a
// partial application leaves behind, which the next run completes.
func TestClassifyAdoptsARowWithNoSession(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "")},
	)
	cand := onlyCandidate(t, plan, CaseAdopt)
	if cand.Record == nil {
		t.Fatal("Record = nil, want the stored row an adoption updates")
	}
	if cand.Record.ID != "id-1" {
		t.Errorf("Record.ID = %q, want %q", cand.Record.ID, "id-1")
	}
}

// TestClassifySettledSessionIsSilent is the idempotence claim. A fully
// recovered installation must produce an empty report, not a list of
// things that were already fine.
func TestClassifySettledSessionIsSilent(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "slab")},
	)
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty: a settled session is neither a candidate nor a conflict", plan)
	}
}

// TestClassifySessionMismatchIsAConflict is fill-only at its sharpest: a
// recorded session name is a recorded value, so rebuild reports the
// disagreement rather than replacing it.
func TestClassifySessionMismatchIsAConflict(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "slab-old")},
	)
	onlyConflict(t, plan, "slab",
		`workspace id-1 (slab) already records session "slab-old", but the live `+
			`session carrying its identity keys is named "slab"; rebuild fills in `+
			`missing state and never overwrites a recorded session name.`)
}

// TestClassifyIdentityMismatchIsAConflict is the case where an overwrite
// would do real damage: it would repoint a workspace at the wrong tree.
func TestClassifyIdentityMismatchIsAConflict(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "other", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/slab", "")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" carries slug "other" and worktree "/w/slab", but workspace `+
			`id-1 is recorded as slug "slab" and worktree "/w/slab"; that `+
			`contradiction is evidence of corruption or collision rather than a `+
			`match, so nothing is written.`)
}

// TestClassifyDuplicateIDIsAConflictForEverySession matches ObserveSession,
// which already treats multiple claimants as uncertainty and picks none.
// Both sessions are reported, because the operator has to look at both.
func TestClassifyDuplicateIDIsAConflictForEverySession(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("slab", "id-1", "slab", "/w/slab"),
			live("slab--wt", "id-1", "slab", "/w/slab"),
		},
		nil,
	)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none: neither claimant may be registered", plan.Candidates)
	}
	want := `sessions "slab" and "slab--wt" all carry workspace ID id-1, so rebuild ` +
		`cannot tell which one is the workspace; none of them is registered.`
	if len(plan.Conflicts) != 2 {
		t.Fatalf("%d conflicts, want 2: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	for i, subject := range []string{"slab", "slab--wt"} {
		if plan.Conflicts[i].Subject != subject {
			t.Errorf("conflict %d subject = %q, want %q", i, plan.Conflicts[i].Subject, subject)
		}
		if plan.Conflicts[i].Reason != want {
			t.Errorf("conflict %d reason =\n %q\nwant\n %q", i, plan.Conflicts[i].Reason, want)
		}
	}
}

// TestClassifyNameTakenIsAConflict follows design §7: collision resolution
// happens in one transaction and a name conflict is a refusal, never an
// overwrite.
func TestClassifyNameTakenIsAConflict(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-2", "other", "/w/other", "slab")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" is already recorded as the session of workspace id-2 `+
			`(other), so rebuild will not also adopt it for workspace id-1.`)
}

// TestClassifyIgnoresSessionsWithoutAWorkspaceID keeps rebuild out of
// sessions that are not ours, as buildList and orphanedSessions already do.
// They are not conflicts either: there is nothing to report about them.
func TestClassifyIgnoresSessionsWithoutAWorkspaceID(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("someones-editor", "", "", ""),
			live("scratch", "", "scratch", "/w/scratch"),
		},
		nil,
	)
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty: foreign sessions are ignored entirely", plan)
	}
}

// TestClassifyEmptyInput covers the fresh installation with no tmux server.
func TestClassifyEmptyInput(t *testing.T) {
	if plan := Classify(nil, nil); !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty", plan)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/rebuild/ -v`

Expected: `no required module provides package .../internal/rebuild` or, once the
directory exists, build failures — `undefined: Classify`, `undefined: Plan`,
`undefined: CaseRegister`. Every listed test is red because nothing is written yet.

- [ ] **Step 3: Write the implementation**

Create `internal/rebuild/classify.go`:

```go
// Package rebuild recovers lost workspace registrations from live tmux
// sessions. Classification is pure — no I/O, no clock, no git — because
// the case analysis is the part most likely to be wrong, and a pure
// function is exhaustively testable from literals. Everything that has to
// touch the world happens in application, against the classification a
// second, lock-held pass produces.
package rebuild

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/state"
)

// Case is what rebuild would do about one live session.
type Case int

const (
	// CaseSettled: a row exists and already records this session name.
	// It produces nothing at all — no candidate and no conflict — which
	// is what makes a second run a silent no-op that exits 0.
	CaseSettled Case = iota
	// CaseRegister: a live session with no row at all. The primary
	// recovery path, and the only case that inserts.
	CaseRegister
	// CaseAdopt: a row exists with no actual session, and one live
	// session claims it. Adoption alone; the row is never rewritten.
	CaseAdopt
	// CaseSessionMismatch: the row records a different session name.
	CaseSessionMismatch
	// CaseIdentityMismatch: the live slug or worktree contradicts the
	// row. Acting here would repoint a workspace at the wrong tree.
	CaseIdentityMismatch
	// CaseDuplicateID: two live sessions carry one workspace ID.
	CaseDuplicateID
	// CaseNameTaken: the live name is another workspace's actual session.
	CaseNameTaken
)

// String names the case for test failures and debugging. It is not part of
// any output contract; the report renders Conflict.Reason.
func (c Case) String() string {
	switch c {
	case CaseSettled:
		return "settled"
	case CaseRegister:
		return "register"
	case CaseAdopt:
		return "adopt"
	case CaseSessionMismatch:
		return "session-mismatch"
	case CaseIdentityMismatch:
		return "identity-mismatch"
	case CaseDuplicateID:
		return "duplicate-id"
	case CaseNameTaken:
		return "name-taken"
	}
	return fmt.Sprintf("Case(%d)", int(c))
}

// Candidate is a session rebuild would write for: CaseRegister or
// CaseAdopt only. Every other case writes nothing, so it has no candidate.
type Candidate struct {
	Case    Case
	Session controller.LiveSession
	// Record is the stored row, nil for CaseRegister. It points into a
	// copy Classify owns, so a caller cannot reach its own input through
	// it.
	Record *state.Record
}

// Conflict is a session rebuild declines to act on, with the reason a
// reader needs. Subject is the live session name.
type Conflict struct {
	Subject string
	Reason  string
}

// Plan is one classification pass over live sessions and stored records.
type Plan struct {
	Candidates []Candidate
	Conflicts  []Conflict
}

// Classify sorts live sessions into what rebuild would do about each.
//
// Precedence matters, because one session can match several rows at once:
// duplicate ID, then name taken, then identity mismatch, then settled,
// then adopt, then register — with a session mismatch as the residual when
// a row exists and already names a different session. Uncertainty wins
// over action every time, which is what keeps rebuild fill-only: no case
// that could overwrite a recorded value reaches a candidate.
//
// Sessions carrying no workspace ID belong to someone else and are ignored
// entirely — neither candidate nor conflict, as in buildList.
//
// The output is deterministically ordered and does not depend on the order
// tmux happened to list sessions in: candidates by session slug then name,
// conflicts by subject.
func Classify(live []controller.LiveSession, records []state.Record) Plan {
	// A copy sorted by ID, so the indexes below are built in an order
	// that does not depend on the caller's, and so the pointers handed
	// out in candidates cannot reach the caller's slice. Records are
	// unique by ID: the column is the primary key.
	rows := slices.Clone(records)
	slices.SortFunc(rows, func(a, b state.Record) int { return cmp.Compare(a.ID, b.ID) })

	byID := make(map[string]*state.Record, len(rows))
	byActualSession := make(map[string]*state.Record, len(rows))
	for i := range rows {
		row := &rows[i]
		byID[row.ID] = row
		if row.ActualSession != nil {
			byActualSession[*row.ActualSession] = row
		}
	}

	// Claimant names per workspace ID, sorted so the duplicate-ID reason
	// reads the same however tmux ordered its output.
	claimants := make(map[string][]string, len(live))
	for _, s := range live {
		if s.WorkspaceID == "" {
			continue
		}
		claimants[s.WorkspaceID] = append(claimants[s.WorkspaceID], s.Name)
	}
	for id := range claimants {
		slices.Sort(claimants[id])
	}

	var plan Plan
	conflict := func(s controller.LiveSession, reason string) {
		plan.Conflicts = append(plan.Conflicts, Conflict{Subject: s.Name, Reason: reason})
	}
	for _, s := range live {
		if s.WorkspaceID == "" {
			continue
		}
		row := byID[s.WorkspaceID]
		owner := byActualSession[s.Name]
		switch {
		case len(claimants[s.WorkspaceID]) > 1:
			conflict(s, duplicateIDReason(s, claimants[s.WorkspaceID]))
		case owner != nil && owner.ID != s.WorkspaceID:
			conflict(s, nameTakenReason(s, owner))
		case row != nil && (row.Slug != s.Slug || row.Worktree != s.Worktree):
			conflict(s, identityMismatchReason(s, row))
		case row != nil && row.ActualSession != nil && *row.ActualSession == s.Name:
			// Settled. Deliberately silent.
		case row != nil && row.ActualSession == nil:
			plan.Candidates = append(plan.Candidates, Candidate{
				Case: CaseAdopt, Session: s, Record: row,
			})
		case row != nil:
			conflict(s, sessionMismatchReason(s, row))
		default:
			plan.Candidates = append(plan.Candidates, Candidate{
				Case: CaseRegister, Session: s,
			})
		}
	}

	slices.SortFunc(plan.Candidates, func(a, b Candidate) int {
		if c := cmp.Compare(a.Session.Slug, b.Session.Slug); c != 0 {
			return c
		}
		return cmp.Compare(a.Session.Name, b.Session.Name)
	})
	slices.SortFunc(plan.Conflicts, func(a, b Conflict) int {
		return cmp.Compare(a.Subject, b.Subject)
	})
	return plan
}

// duplicateIDReason names every claimant, because the operator's next step
// is to look at those sessions and kill or rename one.
func duplicateIDReason(s controller.LiveSession, names []string) string {
	return fmt.Sprintf(
		"sessions %s all carry workspace ID %s, so rebuild cannot tell which one "+
			"is the workspace; none of them is registered.",
		quotedList(names), s.WorkspaceID)
}

// nameTakenReason names the workspace that already holds the name, since
// that is where the operator has to look to free it.
func nameTakenReason(s controller.LiveSession, owner *state.Record) string {
	return fmt.Sprintf(
		"session %q is already recorded as the session of workspace %s (%s), so "+
			"rebuild will not also adopt it for workspace %s.",
		s.Name, owner.ID, owner.Slug, s.WorkspaceID)
}

// identityMismatchReason prints both identities side by side, because the
// disagreement is the whole finding.
func identityMismatchReason(s controller.LiveSession, row *state.Record) string {
	return fmt.Sprintf(
		"session %q carries slug %q and worktree %q, but workspace %s is recorded "+
			"as slug %q and worktree %q; that contradiction is evidence of "+
			"corruption or collision rather than a match, so nothing is written.",
		s.Name, s.Slug, s.Worktree, row.ID, row.Slug, row.Worktree)
}

// sessionMismatchReason states the fill-only rule outright, since this is
// the case where an operator is most likely to expect an overwrite.
func sessionMismatchReason(s controller.LiveSession, row *state.Record) string {
	return fmt.Sprintf(
		"workspace %s (%s) already records session %q, but the live session "+
			"carrying its identity keys is named %q; rebuild fills in missing "+
			"state and never overwrites a recorded session name.",
		row.ID, row.Slug, *row.ActualSession, s.Name)
}

// quotedList renders names as prose: "a", "a" and "b", or "a", "b", and "c".
func quotedList(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name)
	}
	switch len(quoted) {
	case 0:
		return ""
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " and " + quoted[1]
	}
	return strings.Join(quoted[:len(quoted)-1], ", ") + ", and " + quoted[len(quoted)-1]
}
```

`slices` is imported by `classify.go` only for now; the test file picks it up in
step 8.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/rebuild/ -v`

Expected: all nine tests pass. If a reason string differs by a character the
test names both sides — copy the implementation's wording into the test only
after checking the sentence actually reads better; these strings are output.

- [ ] **Step 5: Commit**

```bash
gofmt -l internal/rebuild
go vet ./internal/rebuild/
git add internal/rebuild/classify.go internal/rebuild/classify_test.go
git commit -m "feat(rebuild): classify live sessions against stored records

The seven cases of the rebuild design, as a pure function over literals.
Fill-only lives here: only a missing row and a missing session name reach
a candidate, and everything ambiguous becomes a reported conflict."
```

- [ ] **Step 6: Write the precedence tests**

These pin the order of the switch in `Classify`. They will pass immediately —
step 3 already implements the order — so step 7 proves they are load-bearing by
inverting the rule and watching them fail. A test that passes when the safety
rule is inverted is not testing the safety rule.

Append to `internal/rebuild/classify_test.go`:

```go
// TestClassifyDuplicateIDBeatsNameTaken is the combination spec §8 calls
// out by name: two sessions share one workspace ID and one of them also
// collides with another workspace's recorded name. Duplicate ID is the
// broader uncertainty, so both sessions must report it — reporting the
// name collision instead would suggest that freeing the name is enough.
func TestClassifyDuplicateIDBeatsNameTaken(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("slab", "id-1", "slab", "/w/slab"),
			live("dup", "id-1", "slab", "/w/slab"),
		},
		[]state.Record{stored("id-2", "other", "/w/other", "slab")},
	)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none", plan.Candidates)
	}
	want := `sessions "dup" and "slab" all carry workspace ID id-1, so rebuild ` +
		`cannot tell which one is the workspace; none of them is registered.`
	if len(plan.Conflicts) != 2 {
		t.Fatalf("%d conflicts, want 2: %+v", len(plan.Conflicts), plan.Conflicts)
	}
	for _, got := range plan.Conflicts {
		if got.Reason != want {
			t.Errorf("conflict %q reason =\n %q\nwant\n %q", got.Subject, got.Reason, want)
		}
	}
}

// TestClassifyNameTakenBeatsIdentityMismatch: the session both contradicts
// its own row's identity and squats another workspace's recorded name. The
// name collision is reported, because it is the one that would make a
// write fail outright.
func TestClassifyNameTakenBeatsIdentityMismatch(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "wrong", "/w/slab")},
		[]state.Record{
			stored("id-1", "slab", "/w/slab", ""),
			stored("id-2", "other", "/w/other", "slab"),
		},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" is already recorded as the session of workspace id-2 `+
			`(other), so rebuild will not also adopt it for workspace id-1.`)
}

// TestClassifyIdentityMismatchBeatsSettled is the one that would otherwise
// pass silently. The row already records this session name, so a
// name-first reading calls it settled and reports nothing — while the
// worktree it points at disagrees, which is exactly the corruption a
// rebuild run is there to surface.
func TestClassifyIdentityMismatchBeatsSettled(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "slab", "/w/other", "slab")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" carries slug "slab" and worktree "/w/slab", but workspace `+
			`id-1 is recorded as slug "slab" and worktree "/w/other"; that `+
			`contradiction is evidence of corruption or collision rather than a `+
			`match, so nothing is written.`)
}

// TestClassifyIdentityMismatchBeatsAdopt: an empty actual_session is an
// invitation to adopt, but not into a row whose identity contradicts the
// session. Adopting here would attach the workspace to the wrong tree.
func TestClassifyIdentityMismatchBeatsAdopt(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{live("slab", "id-1", "slab", "/w/slab")},
		[]state.Record{stored("id-1", "renamed", "/w/slab", "")},
	)
	onlyConflict(t, plan, "slab",
		`session "slab" carries slug "slab" and worktree "/w/slab", but workspace `+
			`id-1 is recorded as slug "renamed" and worktree "/w/slab"; that `+
			`contradiction is evidence of corruption or collision rather than a `+
			`match, so nothing is written.`)
}

// TestClassifySettledSessionIsNotNameTaken guards the near miss in the
// name-taken rule: a settled workspace owns its own recorded name, so the
// rule must compare owners rather than merely finding the name recorded.
// Getting this wrong makes every second run report every workspace.
func TestClassifySettledSessionIsNotNameTaken(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("slab", "id-1", "slab", "/w/slab"),
			live("mux", "id-2", "mux", "/w/mux"),
		},
		[]state.Record{
			stored("id-1", "slab", "/w/slab", "slab"),
			stored("id-2", "mux", "/w/mux", "mux"),
		},
	)
	if !reflect.DeepEqual(plan, Plan{}) {
		t.Fatalf("plan = %+v, want empty: a fully recovered installation reports nothing", plan)
	}
}
```

- [ ] **Step 7: Run the precedence tests, then invert the rule to prove they bite**

Run: `go test ./internal/rebuild/ -run TestClassify -v` — all pass.

Now the mutation check spec §8 requires. In `Classify`, move the
`case row != nil && (row.Slug != s.Slug || ...)` arm *below* the settled arm and
re-run:

Expected: `TestClassifyIdentityMismatchBeatsSettled` FAILs with
`0 conflicts, want 1`. Restore the order.

Second mutation: change the adopt arm to also accept a row whose
`ActualSession` is set, by dropping the `row.ActualSession == nil` condition:

Expected: `TestClassifySessionMismatchIsAConflict` FAILs with
`candidates = [...], want none: this session must not be written for` — the
assertion that would otherwise let case 2 quietly become an overwrite. Restore.

Re-run `go test ./internal/rebuild/ -count=1` and confirm green before moving on.

- [ ] **Step 8: Write the determinism and ordering tests**

Live sessions arrive in whatever order tmux listed them, and records in whatever
order the query returned. The report must not move.

Append to `internal/rebuild/classify_test.go`, and add `"slices"` to its import
block (the standard-library group, above `"testing"`):

```go
// permutations returns every ordering of s. The inputs here are small
// enough (4! = 24) to enumerate exhaustively, which beats a seeded shuffle:
// it cannot pass by luck.
func permutations[T any](s []T) [][]T {
	if len(s) <= 1 {
		return [][]T{slices.Clone(s)}
	}
	var out [][]T
	for i := range s {
		rest := make([]T, 0, len(s)-1)
		rest = append(rest, s[:i]...)
		rest = append(rest, s[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]T{s[i]}, tail...))
		}
	}
	return out
}

// TestClassifyIsIndependentOfInputOrder feeds one mixed installation —
// a registration, an adoption, a settled row, and a conflict — in every
// possible order and asserts the plan never moves. Map iteration inside
// Classify is the thing this would catch.
func TestClassifyIsIndependentOfInputOrder(t *testing.T) {
	sessions := []controller.LiveSession{
		live("fresh", "id-fresh", "fresh", "/w/fresh"),
		live("adoptme", "id-adopt", "adoptme", "/w/adoptme"),
		live("settled", "id-settled", "settled", "/w/settled"),
		live("drifted", "id-drift", "drifted", "/w/drifted"),
	}
	records := []state.Record{
		stored("id-adopt", "adoptme", "/w/adoptme", ""),
		stored("id-settled", "settled", "/w/settled", "settled"),
		stored("id-drift", "drifted", "/w/drifted", "drifted-old"),
		stored("id-gone", "gone", "/w/gone", ""),
	}

	want := Classify(sessions, records)
	if len(want.Candidates) != 2 || len(want.Conflicts) != 1 {
		t.Fatalf("fixture produced %d candidates and %d conflicts, want 2 and 1: %+v",
			len(want.Candidates), len(want.Conflicts), want)
	}

	for _, liveOrder := range permutations(sessions) {
		for _, recordOrder := range permutations(records) {
			if got := Classify(liveOrder, recordOrder); !reflect.DeepEqual(got, want) {
				t.Fatalf("order changed the plan:\n got %+v\nwant %+v", got, want)
			}
		}
	}
}

// TestClassifyOrdersCandidatesBySlugThenName pins the primary sort key.
// The names here sort opposite to the slugs, so a name-first
// implementation produces the reverse of this list.
func TestClassifyOrdersCandidatesBySlugThenName(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("zeta", "id-1", "alpha", "/w/alpha"),
			live("alpha", "id-2", "beta", "/w/beta"),
			live("beta", "id-3", "beta", "/w/beta2"),
		},
		nil,
	)
	var got []string
	for _, cand := range plan.Candidates {
		got = append(got, cand.Session.Slug+"/"+cand.Session.Name)
	}
	want := []string{"alpha/zeta", "beta/alpha", "beta/beta"}
	if !slices.Equal(got, want) {
		t.Fatalf("candidate order = %v, want %v", got, want)
	}
}

// TestClassifyOrdersConflictsBySubject keeps the report stable for a
// reader diffing two runs.
func TestClassifyOrdersConflictsBySubject(t *testing.T) {
	plan := Classify(
		[]controller.LiveSession{
			live("zulu", "id-1", "zulu", "/w/zulu"),
			live("alfa", "id-2", "alfa", "/w/alfa"),
			live("mike", "id-3", "mike", "/w/mike"),
		},
		[]state.Record{
			stored("id-1", "zulu", "/w/zulu", "zulu-old"),
			stored("id-2", "alfa", "/w/alfa", "alfa-old"),
			stored("id-3", "mike", "/w/mike", "mike-old"),
		},
	)
	if len(plan.Candidates) != 0 {
		t.Fatalf("candidates = %+v, want none", plan.Candidates)
	}
	var got []string
	for _, c := range plan.Conflicts {
		got = append(got, c.Subject)
	}
	if want := []string{"alfa", "mike", "zulu"}; !slices.Equal(got, want) {
		t.Fatalf("conflict order = %v, want %v", got, want)
	}
}
```

Note the `id-gone` record in the determinism fixture: a stored workspace with no
live session at all. Rebuild has nothing to say about it — pruning records is
explicitly out of scope (§1) — so it must produce neither a candidate nor a
conflict, which the candidate and conflict counts assert.

- [ ] **Step 9: Run the full package and verify**

```bash
go test ./internal/rebuild/ -count=1 -race -v
gofmt -l internal/rebuild
go vet ./internal/rebuild/
```

Expected: every test passes, `gofmt -l` prints nothing.

- [ ] **Step 10: Commit**

```bash
git add internal/rebuild/classify_test.go
git commit -m "test(rebuild): pin classification precedence and ordering

Precedence is where fill-only is actually enforced, so each rule has a
test that fails when the arm is reordered — verified by inverting the
identity-mismatch and adopt arms in turn. Input order is covered by
enumerating every permutation rather than a seeded shuffle."
```

---

**Handoff to later tasks.** After task 2, `rebuild.Classify` is available for the
lock-held re-classification of spec §6 step 6: application calls it a second time
with the single live session it observed under the lock and the single row it
re-read, and writes according to the case it returns — `CaseRegister` →
`RegisterWorkspace` then `AdoptSessionName`, `CaseAdopt` → `AdoptSessionName`
alone, anything else → a conflict. `Conflict` has no case field, so application
code that needs to branch must read `Candidate.Case`; a plan with no candidates
means the write is off.

---

### Task 3: The rebuild applier — turn a classified plan into fill-only store writes

**Files:**
- Create: `internal/rebuild/apply.go`
- Test: `internal/rebuild/apply_test.go`
- Modify: `docs/superpowers/specs/2026-08-06-state-rebuild-design.md` (final step only)

**Interfaces:**

- Consumes (from task 2, `internal/rebuild/classify.go`, same package — do not redeclare):
```go
type Case int

const (
	CaseSettled Case = iota
	CaseRegister
	CaseAdopt
	CaseSessionMismatch
	CaseIdentityMismatch
	CaseDuplicateID
	CaseNameTaken
)

type Candidate struct {
	Case    Case
	Session controller.LiveSession
	Record  *state.Record // nil for CaseRegister
}

type Conflict struct {
	Subject string
	Reason  string
}

type Plan struct {
	Candidates []Candidate
	Conflicts  []Conflict
}

func Classify(live []controller.LiveSession, records []state.Record) Plan
```

- Consumes (existing, verified against the repository):
```go
// internal/controller/plan.go:112
func controller.SessionBelongsTo(s controller.LiveSession, ws resolve.Workspace) bool

// internal/controller/interfaces.go:40-42
type controller.SessionObserver interface {
	ObserveSession(ctx context.Context, q controller.SessionQuery) (controller.SessionObservation, error)
}

// internal/controller/interfaces.go:97-99
type controller.Clock interface{ Now() time.Time }

// internal/state/types.go:27, :95
var state.ErrNotFound error
type state.SessionNameConflictError struct{ Name string }
```

- Produces (task 5, the CLI, is written against these in parallel — do not rename):
```go
type Resolver interface {
	Resolve(worktree string) (resolve.Workspace, error)
}

type ConfigLoader interface {
	Digest(slug string) (string, error)
}

type Store interface {
	Workspace(id string) (state.Record, error)
	Workspaces() ([]state.Record, error)
	RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error
	AdoptSessionName(workspaceID, name string, now time.Time) error
}

type Locker interface {
	Lock(ctx context.Context, workspaceID string) (func(), error)
}

type Registered struct {
	ID        string
	Slug      string
	Worktree  string
	IsPrimary bool
	Session   string
}

type Report struct {
	DryRun     bool
	Registered []Registered
	Conflicts  []Conflict
}

type Applier struct {
	Store    Store
	Sessions controller.SessionObserver
	Resolver Resolver
	Config   ConfigLoader
	Locker   Locker
	Clock    controller.Clock
	DryRun   bool
}

func (a *Applier) Apply(ctx context.Context, plan Plan) Report
```

Notes for the implementer before you start:

- `*state.Store` satisfies `Store` here (`RegisterWorkspace`, `AdoptSessionName`,
  `Workspace`, `Workspaces` all exist with exactly these signatures —
  `internal/state/store.go:31`, `:69`, `:73`, `:420`). So does
  `*fake.Store` (`internal/controller/fake/fake.go:101`, `:236`, `:248`, `:315`).
- The package doc comment belongs on `classify.go` (task 2). Do not write a
  second one here; `apply.go` starts with a bare `package rebuild`.
- These tests need no git, no tmux, and no SQLite. Every dependency is an
  interface satisfied by a fake in the test file.

---

- [ ] **Step 1: Write the failing tests for the straight-line applier**

Create `internal/rebuild/apply_test.go` with the fakes, the harness, and the
four tests that exercise application when nothing changes under the lock.

```go
package rebuild

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var testTime = time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)

// scriptedObserver returns a different observation per call.
// fake.SessionObserver cannot serve here: it returns one canned
// observation for every call, so a batch of two candidates cannot give
// each its own, and "the session vanished before the lock" cannot be
// expressed alongside a live one in the same run.
type scriptedObserver struct {
	results []controller.SessionObservation
	errs    []error
	calls   int
	queries []controller.SessionQuery
}

func (o *scriptedObserver) ObserveSession(_ context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	i := o.calls
	o.calls++
	o.queries = append(o.queries, q)
	if i < len(o.errs) && o.errs[i] != nil {
		return controller.SessionObservation{}, o.errs[i]
	}
	if i < len(o.results) {
		return o.results[i], nil
	}
	return controller.SessionObservation{}, fmt.Errorf("scriptedObserver: unscripted call %d", i+1)
}

// mapResolver stands in for resolve.Resolve, which shells out to git. A
// missing entry is the vanished-worktree case; errs models a tree that
// exists but will not resolve.
type mapResolver struct {
	byWorktree map[string]resolve.Workspace
	errs       map[string]error
}

func (r *mapResolver) Resolve(worktree string) (resolve.Workspace, error) {
	if err := r.errs[worktree]; err != nil {
		return resolve.Workspace{}, err
	}
	ws, ok := r.byWorktree[worktree]
	if !ok {
		return resolve.Workspace{}, fmt.Errorf("no worktree at %s", worktree)
	}
	return ws, nil
}

type mapConfig struct {
	digests map[string]string
	errs    map[string]error
}

func (c *mapConfig) Digest(slug string) (string, error) {
	if err := c.errs[slug]; err != nil {
		return "", err
	}
	digest, ok := c.digests[slug]
	if !ok {
		return "", fmt.Errorf("no configuration for slug %s", slug)
	}
	return digest, nil
}

type countingLocker struct {
	locked   []string
	released int
	err      error
}

func (l *countingLocker) Lock(_ context.Context, workspaceID string) (func(), error) {
	if l.err != nil {
		return nil, l.err
	}
	l.locked = append(l.locked, workspaceID)
	return func() { l.released++ }, nil
}

// countingStore counts writes so a dry run can be asserted to have made
// none, rather than inferred to have made none from the resulting rows.
type countingStore struct {
	*fake.Store
	registers int
	adopts    int
}

func (s *countingStore) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	s.registers++
	return s.Store.RegisterWorkspace(ws, desiredDigest, now)
}

func (s *countingStore) AdoptSessionName(workspaceID, name string, now time.Time) error {
	s.adopts++
	return s.Store.AdoptSessionName(workspaceID, name, now)
}

// adoptFailStore fails the first adoption only, so one store can serve
// both the half-applied run and the second run that completes it.
type adoptFailStore struct {
	*fake.Store
	err error
}

func (s *adoptFailStore) AdoptSessionName(workspaceID, name string, now time.Time) error {
	if s.err != nil {
		err := s.err
		s.err = nil
		return err
	}
	return s.Store.AdoptSessionName(workspaceID, name, now)
}

type harness struct {
	fakeStore *fake.Store
	store     Store
	observer  *scriptedObserver
	resolver  *mapResolver
	config    *mapConfig
	locker    *countingLocker
	dryRun    bool
}

func newHarness() *harness {
	fs := fake.NewStore()
	return &harness{
		fakeStore: fs,
		store:     fs,
		observer:  &scriptedObserver{},
		resolver:  &mapResolver{byWorktree: map[string]resolve.Workspace{}, errs: map[string]error{}},
		config:    &mapConfig{digests: map[string]string{}, errs: map[string]error{}},
		locker:    &countingLocker{},
	}
}

// know teaches the resolver and the configuration loader about one
// workspace, the way a real git tree and a real defaults.yaml would.
func (h *harness) know(ws resolve.Workspace, digest string) {
	h.resolver.byWorktree[ws.Worktree] = ws
	h.config.digests[ws.Slug] = digest
}

func (h *harness) applier() *Applier {
	return &Applier{
		Store:    h.store,
		Sessions: h.observer,
		Resolver: h.resolver,
		Config:   h.config,
		Locker:   h.locker,
		Clock:    &fake.Clock{Time: testTime},
		DryRun:   h.dryRun,
	}
}

func workspace(id, slug, worktree, sessionName string, primary bool) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        slug,
		Worktree:    worktree,
		SessionName: sessionName,
		IsPrimary:   primary,
	}
}

func projectmux() resolve.Workspace {
	return workspace(
		"1111111111111111111111111111111111111111111111111111111111111111",
		"projectmux", "/src/projectmux", "projectmux", true)
}

// liveSession is a session carrying identity keys that agree with the
// workspace. Tests that need disagreement overwrite one field after.
func liveSession(ws resolve.Workspace, name string) controller.LiveSession {
	return controller.LiveSession{
		ID:          "$1",
		Name:        name,
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.Worktree,
	}
}

func observing(s controller.LiveSession) controller.SessionObservation {
	return controller.SessionObservation{ByIdentity: &s, ByName: []controller.LiveSession{s}}
}

func TestApplyRegistersAndAdoptsAWorkspaceWithNoRow(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", report.Conflicts)
	}
	want := []Registered{{
		ID:        ws.ID,
		Slug:      "projectmux",
		Worktree:  "/src/projectmux",
		IsPrimary: true,
		Session:   "projectmux",
	}}
	if !reflect.DeepEqual(report.Registered, want) {
		t.Fatalf("Registered = %+v, want %+v", report.Registered, want)
	}

	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	if !rec.IsPrimary {
		t.Errorf("IsPrimary = false, want true — it comes from the resolver, not the session keys")
	}
	if rec.ProposedSession != "projectmux" {
		t.Errorf("ProposedSession = %q, want %q", rec.ProposedSession, "projectmux")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:desired" {
		t.Errorf("DesiredDigest = %v, want %q", rec.DesiredDigest, "sha256:desired")
	}
	if rec.AppliedDigest != nil {
		t.Errorf("AppliedDigest = %q, want nil so the next open reconciles", *rec.AppliedDigest)
	}
	if got := h.locker.locked; !reflect.DeepEqual(got, []string{ws.ID}) {
		t.Errorf("locked = %v, want [%s]", got, ws.ID)
	}
	if h.locker.released != 1 {
		t.Errorf("lock released %d times, want 1", h.locker.released)
	}
}

func TestApplyResolverFailureIsAConflictAndTheBatchContinues(t *testing.T) {
	ws := projectmux()
	good := liveSession(ws, "projectmux")
	gone := controller.LiveSession{
		ID:          "$2",
		Name:        "vanished",
		WorkspaceID: "2222222222222222222222222222222222222222222222222222222222222222",
		Slug:        "vanished",
		Worktree:    "/src/vanished",
	}
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.resolver.errs["/src/vanished"] = errors.New("worktree /src/vanished does not exist")
	h.observer.results = []controller.SessionObservation{observing(good)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{
			{Case: CaseRegister, Session: gone},
			{Case: CaseRegister, Session: good},
		},
	})

	if len(report.Registered) != 1 || report.Registered[0].Slug != "projectmux" {
		t.Fatalf("Registered = %+v, want only the resolvable workspace", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if report.Conflicts[0].Subject != "vanished" {
		t.Errorf("Subject = %q, want %q", report.Conflicts[0].Subject, "vanished")
	}
	if !strings.Contains(report.Conflicts[0].Reason, "/src/vanished") {
		t.Errorf("Reason = %q, want it to name the worktree", report.Conflicts[0].Reason)
	}
}

func TestApplyConfigFailureIsOneWorkspacesConflictNotTheBatchs(t *testing.T) {
	ws := projectmux()
	other := workspace(
		"3333333333333333333333333333333333333333333333333333333333333333",
		"other", "/src/other", "other", true)
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.know(other, "sha256:other")
	h.config.errs["other"] = errors.New(`other.yaml: unknown field "widnows"`)
	good := liveSession(ws, "projectmux")
	h.observer.results = []controller.SessionObservation{observing(good)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{
			{Case: CaseRegister, Session: liveSession(other, "other")},
			{Case: CaseRegister, Session: good},
		},
	})

	if len(report.Registered) != 1 || report.Registered[0].Slug != "projectmux" {
		t.Fatalf("Registered = %+v, want only the workspace whose configuration loaded", report.Registered)
	}
	if len(report.Conflicts) != 1 || report.Conflicts[0].Subject != "other" {
		t.Fatalf("Conflicts = %+v, want one for %q", report.Conflicts, "other")
	}
	if !strings.Contains(report.Conflicts[0].Reason, "widnows") {
		t.Errorf("Reason = %q, want the underlying configuration error preserved", report.Conflicts[0].Reason)
	}
	if _, err := h.fakeStore.Workspace(other.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace %s err = %v, want ErrNotFound: a configuration failure writes nothing", other.ID, err)
	}
}

func TestApplyOrdersRegistrationsBySlugThenSessionAndPassesPlanConflictsThrough(t *testing.T) {
	alpha := workspace(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"alpha", "/src/alpha", "alpha", true)
	alphaWT := workspace(
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"alpha", "/src/alpha/.worktrees/wt", "alpha--wt", false)
	zulu := workspace(
		"cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		"zulu", "/src/zulu", "zulu", true)

	h := newHarness()
	h.know(alpha, "sha256:a")
	h.know(alphaWT, "sha256:a")
	h.know(zulu, "sha256:z")

	zuluSess := liveSession(zulu, "zulu")
	wtSess := liveSession(alphaWT, "alpha--wt")
	alphaSess := liveSession(alpha, "alpha")
	// The observer is scripted in candidate order, not report order.
	h.observer.results = []controller.SessionObservation{
		observing(zuluSess), observing(wtSess), observing(alphaSess),
	}

	planConflict := Conflict{Subject: "ghost", Reason: "two live sessions claim workspace dddd"}
	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{
			{Case: CaseRegister, Session: zuluSess},
			{Case: CaseRegister, Session: wtSess},
			{Case: CaseRegister, Session: alphaSess},
		},
		Conflicts: []Conflict{planConflict},
	})

	var sessions []string
	for _, r := range report.Registered {
		sessions = append(sessions, r.Session)
	}
	want := []string{"alpha", "alpha--wt", "zulu"}
	if !reflect.DeepEqual(sessions, want) {
		t.Errorf("registered sessions = %v, want %v (slug, then session name)", sessions, want)
	}
	if !reflect.DeepEqual(report.Conflicts, []Conflict{planConflict}) {
		t.Errorf("Conflicts = %+v, want the classification conflict passed through unchanged", report.Conflicts)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/rebuild/ -run TestApply -v`

Expected: a compile failure — `undefined: Applier`, `undefined: Registered`,
`undefined: Report`. Nothing in the package declares them yet.

- [ ] **Step 3: Write the straight-line applier**

Create `internal/rebuild/apply.go`. This step covers resolution, the digest,
the lock, and the two writes — but not identity verification, not `--dry-run`,
and not the lock-time re-observation. Those arrive in later steps with the
tests that force them.

The per-candidate work goes in its own function rather than in `Apply`'s loop
body: `defer release()` written directly inside a `for` would hold every lock
until the whole batch finished.

```go
package rebuild

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// Resolver derives a workspace from a worktree directory. It is an
// interface rather than a direct call to resolve.Resolve so application
// is testable without a real git repository.
type Resolver interface {
	Resolve(worktree string) (resolve.Workspace, error)
}

// ConfigLoader returns the desired digest for a workspace slug.
type ConfigLoader interface {
	Digest(slug string) (string, error)
}

// Store is the slice of the state store rebuild writes through. It is
// deliberately narrower than controller.Store: rebuild adds no new state
// mutation, it only composes the two that already exist.
type Store interface {
	Workspace(id string) (state.Record, error)
	Workspaces() ([]state.Record, error)
	RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error
	AdoptSessionName(workspaceID, name string, now time.Time) error
}

// Locker takes the per-workspace lock and returns its release. The
// release is a plain func so the caller cannot forget which lock it
// belongs to.
type Locker interface {
	Lock(ctx context.Context, workspaceID string) (func(), error)
}

// Registered is one workspace rebuild recovered.
type Registered struct {
	ID        string
	Slug      string
	Worktree  string
	IsPrimary bool
	Session   string
}

// Report is what one rebuild run did and declined to do. Both slices are
// non-nil even when empty: the JSON envelope always carries both arrays.
type Report struct {
	DryRun     bool
	Registered []Registered
	Conflicts  []Conflict
}

// Applier turns a classified Plan into store writes. Every dependency is
// an interface so the whole of application is testable without git,
// tmux, or SQLite.
type Applier struct {
	Store    Store
	Sessions controller.SessionObserver
	Resolver Resolver
	Config   ConfigLoader
	Locker   Locker
	Clock    controller.Clock
	DryRun   bool
}

// Apply works the plan candidate by candidate. One candidate's failure
// is that candidate's conflict, never the batch's, matching how
// autostart treats one workspace's failure.
func (a *Applier) Apply(ctx context.Context, plan Plan) Report {
	report := Report{
		DryRun:     a.DryRun,
		Registered: []Registered{},
		Conflicts:  append([]Conflict{}, plan.Conflicts...),
	}
	for _, cand := range plan.Candidates {
		reg, conflict := a.applyCandidate(ctx, cand)
		switch {
		case conflict != nil:
			report.Conflicts = append(report.Conflicts, *conflict)
		case reg != nil:
			report.Registered = append(report.Registered, *reg)
		}
	}
	slices.SortFunc(report.Registered, func(a, b Registered) int {
		if c := cmp.Compare(a.Slug, b.Slug); c != 0 {
			return c
		}
		return cmp.Compare(a.Session, b.Session)
	})
	return report
}

// applyCandidate is a function rather than the body of Apply's loop so
// each candidate's lock release can be deferred: a bare defer inside the
// loop would hold every lock until the batch finished.
func (a *Applier) applyCandidate(ctx context.Context, cand Candidate) (*Registered, *Conflict) {
	sess := cand.Session

	ws, err := a.Resolver.Resolve(sess.Worktree)
	if err != nil {
		return nil, conflictf(sess.Name,
			"resolving the workspace from %s failed: %v; a session whose worktree is gone cannot be re-registered",
			sess.Worktree, err)
	}

	digest, err := a.Config.Digest(ws.Slug)
	if err != nil {
		return nil, conflictf(sess.Name,
			"loading the configuration for %q failed: %v", ws.Slug, err)
	}

	release, err := a.Locker.Lock(ctx, ws.ID)
	if err != nil {
		return nil, conflictf(sess.Name, "taking the workspace lock: %v", err)
	}
	defer release()

	now := a.Clock.Now()
	if err := a.Store.RegisterWorkspace(ws, digest, now); err != nil {
		return nil, conflictf(sess.Name, "registering workspace %s: %v", ws.Slug, err)
	}
	if err := a.Store.AdoptSessionName(ws.ID, sess.Name, now); err != nil {
		return nil, conflictf(sess.Name, "adopting session name %q: %v", sess.Name, err)
	}
	return registeredFor(ws, sess.Name), nil
}

func registeredFor(ws resolve.Workspace, session string) *Registered {
	return &Registered{
		ID:        ws.ID,
		Slug:      ws.Slug,
		Worktree:  ws.Worktree,
		IsPrimary: ws.IsPrimary,
		Session:   session,
	}
}

func conflictf(subject, format string, args ...any) *Conflict {
	return &Conflict{Subject: subject, Reason: fmt.Sprintf(format, args...)}
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/rebuild/ -run TestApply -v`

Expected: all four PASS.

- [ ] **Step 5: Commit**

`git add internal/rebuild/apply.go internal/rebuild/apply_test.go && git commit -m "Add the rebuild applier: resolve, register, adopt under the workspace lock"`

---

- [ ] **Step 6: Write the failing test for identity verification**

Append to `internal/rebuild/apply_test.go`:

```go
func TestApplyRefusesASessionWhoseIdentityKeysContradictTheWorkspace(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	// The derived workspace ID still matches; the slug does not. Checking
	// the ID alone would register the workspace from resolved values that
	// silently disagree with the live keys.
	sess.Slug = "stale-slug"

	h := newHarness()
	h.know(ws, "sha256:desired")
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "stale-slug") {
		t.Errorf("Reason = %q, want it to name the contradictory key", report.Conflicts[0].Reason)
	}
	if len(h.locker.locked) != 0 {
		t.Errorf("locked = %v, want none: identity is verified before the lock", h.locker.locked)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound: a refused session writes nothing", err)
	}
}
```

- [ ] **Step 7: Run the test to verify it fails**

Run: `go test ./internal/rebuild/ -run TestApplyRefusesASession -v`

Expected: FAIL — `Registered = [...], want none`. The applier registers the
workspace because it never compares the live keys against the resolved ones.

- [ ] **Step 8: Add the `SessionBelongsTo` check**

In `applyCandidate`, immediately after the successful `Resolve` and before the
digest:

```go
	// All three identity keys, not just the derived ID (spec §3). A
	// session carrying a stale or hand-set @dev_slug would otherwise be
	// registered from resolved values that silently disagree with it, and
	// the next rebuild would report that row as a mismatch conflict
	// instead of a clean no-op.
	if !controller.SessionBelongsTo(sess, ws) {
		return nil, conflictf(sess.Name,
			"session carries workspace %s, slug %q, worktree %q, but %s resolves to "+
				"workspace %s, slug %q, worktree %q; refusing to register it",
			sess.WorkspaceID, sess.Slug, sess.Worktree,
			sess.Worktree, ws.ID, ws.Slug, ws.Worktree)
	}
```

- [ ] **Step 9: Run the tests to verify they pass**

Run: `go test ./internal/rebuild/ -run TestApply -v`

Expected: all five PASS.

- [ ] **Step 10: Commit**

`git commit -am "Verify all three session identity keys before rebuild registers a workspace"`

---

- [ ] **Step 11: Write the failing test for `--dry-run`**

Append to `internal/rebuild/apply_test.go`:

```go
func TestApplyDryRunMatchesTheRealRunAndWritesNothing(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	gone := controller.LiveSession{
		ID:          "$2",
		Name:        "vanished",
		WorkspaceID: "2222222222222222222222222222222222222222222222222222222222222222",
		Slug:        "vanished",
		Worktree:    "/src/vanished",
	}
	planConflict := Conflict{Subject: "ghost", Reason: "two live sessions claim workspace dddd"}
	newPlan := func() Plan {
		return Plan{
			Candidates: []Candidate{
				{Case: CaseRegister, Session: gone},
				{Case: CaseRegister, Session: sess},
			},
			Conflicts: []Conflict{planConflict},
		}
	}

	actual := newHarness()
	actual.know(ws, "sha256:desired")
	actual.resolver.errs["/src/vanished"] = errors.New("worktree /src/vanished does not exist")
	actual.observer.results = []controller.SessionObservation{observing(sess)}
	actualReport := actual.applier().Apply(context.Background(), newPlan())

	preview := newHarness()
	preview.know(ws, "sha256:desired")
	preview.resolver.errs["/src/vanished"] = errors.New("worktree /src/vanished does not exist")
	counting := &countingStore{Store: preview.fakeStore}
	preview.store = counting
	preview.dryRun = true
	previewReport := preview.applier().Apply(context.Background(), newPlan())

	if !previewReport.DryRun {
		t.Errorf("dry run DryRun = false, want true")
	}
	if actualReport.DryRun {
		t.Errorf("real run DryRun = true, want false")
	}
	// The verdict is the deliverable: a dry run that says "would register"
	// has established every fact registration depends on except the
	// outcome of the writes themselves.
	if !reflect.DeepEqual(previewReport.Registered, actualReport.Registered) {
		t.Errorf("dry Registered = %+v, real = %+v; they must be identical",
			previewReport.Registered, actualReport.Registered)
	}
	if !reflect.DeepEqual(previewReport.Conflicts, actualReport.Conflicts) {
		t.Errorf("dry Conflicts = %+v, real = %+v; they must be identical",
			previewReport.Conflicts, actualReport.Conflicts)
	}
	if counting.registers != 0 || counting.adopts != 0 {
		t.Errorf("dry run wrote: %d registers, %d adopts; want 0 and 0",
			counting.registers, counting.adopts)
	}
	if len(preview.locker.locked) != 0 {
		t.Errorf("dry run locked %v, want nothing", preview.locker.locked)
	}
	if preview.observer.calls != 0 {
		t.Errorf("dry run called ObserveSession %d times, want 0", preview.observer.calls)
	}
	recs, err := preview.fakeStore.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("dry run left %d records, want 0", len(recs))
	}
}
```

- [ ] **Step 12: Run the test to verify it fails**

Run: `go test ./internal/rebuild/ -run TestApplyDryRun -v`

Expected: FAIL — `dry run wrote: 1 registers, 1 adopts; want 0 and 0`. `DryRun`
is currently a field nothing reads.

- [ ] **Step 13: Stop the dry run before the lock**

In `applyCandidate`, between the digest and the lock:

```go
	if a.DryRun {
		// Everything above is read-only, which is exactly what lets a dry
		// run predict the real run's verdict and exit code (spec §2). A
		// preview that stopped after pure classification would report a
		// clean 0 for a vanished worktree the real run refuses.
		return registeredFor(ws, sess.Name), nil
	}
```

- [ ] **Step 14: Run the tests to verify they pass**

Run: `go test ./internal/rebuild/ -run TestApply -v`

Expected: all six PASS.

- [ ] **Step 15: Commit**

`git commit -am "Add rebuild --dry-run: run every read-only step, stop before the lock"`

---

- [ ] **Step 16: Write the failing tests for lock-time re-observation and re-classification**

This is the load-bearing step: the writes must be decided from the observation
taken *after* the lock, not from the classification pass. Append to
`internal/rebuild/apply_test.go`:

```go
// seedRecorded registers a row whose every overwritable field disagrees
// with what the resolver and the configuration loader would supply.
// RegisterWorkspace's conflict branch overwrites slug, worktree,
// is_primary, proposed_session, and desired_digest
// (internal/state/store.go:43-49), so any of these changing proves the
// applier re-registered a workspace it should only have adopted.
//
// Slug and worktree deliberately match: a row disagreeing on those is an
// identity mismatch, which classification refuses before it ever reaches
// application.
func seedRecorded(t *testing.T, store *fake.Store, ws resolve.Workspace) {
	t.Helper()
	recorded := workspace(ws.ID, ws.Slug, ws.Worktree, "recorded-proposed", false)
	if err := store.RegisterWorkspace(recorded, "sha256:recorded", testTime); err != nil {
		t.Fatalf("seeding the recorded row: %v", err)
	}
}

func assertRecordedFieldsUntouched(t *testing.T, rec state.Record, ws resolve.Workspace) {
	t.Helper()
	if rec.IsPrimary {
		t.Errorf("IsPrimary = true, want the recorded false: adoption must not re-register")
	}
	if rec.ProposedSession != "recorded-proposed" {
		t.Errorf("ProposedSession = %q, want %q", rec.ProposedSession, "recorded-proposed")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:recorded" {
		t.Errorf("DesiredDigest = %v, want %q", rec.DesiredDigest, "sha256:recorded")
	}
	if rec.Slug != ws.Slug {
		t.Errorf("Slug = %q, want %q", rec.Slug, ws.Slug)
	}
	if rec.Worktree != ws.Worktree {
		t.Errorf("Worktree = %q, want %q", rec.Worktree, ws.Worktree)
	}
}

func TestApplyAdoptsOnlyAndLeavesEveryRecordedFieldUntouched(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	// A broken workspace configuration must not block adoption: only
	// registration writes a digest.
	h.config.errs["projectmux"] = errors.New("projectmux.yaml is unreadable")
	seedRecorded(t, h.fakeStore, ws)
	h.observer.results = []controller.SessionObservation{observing(sess)}

	// Candidate.Record is left nil on purpose: the applier re-reads the
	// row under the lock and must never trust the first pass's copy.
	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseAdopt, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", report.Conflicts)
	}
	if len(report.Registered) != 1 {
		t.Fatalf("Registered = %+v, want one", report.Registered)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	assertRecordedFieldsUntouched(t, rec, ws)
}

func TestApplyReclassifiesARegisterCandidateWhoseRowAppearedBeforeTheLock(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	// Classification saw no row; another process registered the workspace
	// in the gap. The lock-time re-read must turn this into an adoption.
	seedRecorded(t, h.fakeStore, ws)
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none", report.Conflicts)
	}
	if len(report.Registered) != 1 {
		t.Fatalf("Registered = %+v, want one", report.Registered)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	assertRecordedFieldsUntouched(t, rec, ws)

	if h.observer.calls != 1 {
		t.Fatalf("ObserveSession called %d times, want 1 (under the lock)", h.observer.calls)
	}
	wantQuery := controller.SessionQuery{
		WorkspaceID:    ws.ID,
		CandidateNames: []string{"projectmux"},
	}
	if !reflect.DeepEqual(h.observer.queries[0], wantQuery) {
		t.Errorf("query = %+v, want %+v", h.observer.queries[0], wantQuery)
	}
}

// The mirror image of the reclassification above, and the reason a digest
// failure is carried forward rather than returned where it happens.
// Classification saw no row, so a digest was loaded and the load failed —
// but by the time the lock was held the row existed, which makes this an
// adoption, and adoption writes no digest. Refusing at the load would
// turn a recoverable workspace into a conflict on the strength of a
// requirement it no longer has.
func TestApplyAdoptsARegisterCandidateWhoseDigestFailedButWhoseRowAppeared(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.config.errs["projectmux"] = errors.New("projectmux.yaml is unreadable")
	seedRecorded(t, h.fakeStore, ws)
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Conflicts) != 0 {
		t.Fatalf("Conflicts = %+v, want none: adoption does not need a digest", report.Conflicts)
	}
	if len(report.Registered) != 1 {
		t.Fatalf("Registered = %+v, want one", report.Registered)
	}
	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	assertRecordedFieldsUntouched(t, rec, ws)
}

// Carrying the failure must not swallow it. A register that is still a
// register under the lock writes a row recording a desired digest, and
// there is no digest to record: inventing one would be a guess, so this
// is a conflict and nothing is written.
func TestApplyRegisterWithAnUnreadableConfigurationIsAConflict(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.config.errs["projectmux"] = errors.New("projectmux.yaml is unreadable")
	h.observer.results = []controller.SessionObservation{observing(sess)}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "projectmux.yaml is unreadable") {
		t.Errorf("Reason = %q, want it to carry the configuration error",
			report.Conflicts[0].Reason)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound: nothing may be written", err)
	}
}

func TestApplySessionThatVanishedBeforeTheLockIsAConflictWithNoWrite(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	// The session died between classification and the lock: the lock-time
	// observation finds nothing carrying the workspace's identity keys.
	h.observer.results = []controller.SessionObservation{{}}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", report.Conflicts)
	}
	if !strings.Contains(report.Conflicts[0].Reason, "no longer live") {
		t.Errorf("Reason = %q, want it to say the session was no longer live",
			report.Conflicts[0].Reason)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound: a dead session writes nothing", err)
	}
	if h.locker.released != 1 {
		t.Errorf("lock released %d times, want 1 even on the conflict path", h.locker.released)
	}
}

func TestApplyReportsAnObservationFailureAsAConflict(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.observer.errs = []error{errors.New("no server running on /tmp/tmux-1000/default")}

	report := h.applier().Apply(context.Background(), Plan{
		Candidates: []Candidate{{Case: CaseRegister, Session: sess}},
	})

	if len(report.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none", report.Registered)
	}
	if len(report.Conflicts) != 1 ||
		!strings.Contains(report.Conflicts[0].Reason, "no server running") {
		t.Fatalf("Conflicts = %+v, want one preserving the tmux error", report.Conflicts)
	}
	if _, err := h.fakeStore.Workspace(ws.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("workspace err = %v, want ErrNotFound", err)
	}
}
```

- [ ] **Step 17: Run the tests to verify they fail**

Run: `go test ./internal/rebuild/ -run TestApply -v`

Expected: five failures.
- `TestApplyAdoptsOnlyAndLeavesEveryRecordedFieldUntouched`: FAIL at
  `Conflicts = [...], want none` — the digest is loaded unconditionally and
  `projectmux.yaml is unreadable` aborts the candidate.
- `TestApplyReclassifiesARegisterCandidateWhoseRowAppearedBeforeTheLock`: FAIL
  at `IsPrimary = true, want the recorded false` — the unconditional
  `RegisterWorkspace` took the upsert branch and overwrote the row.
- `TestApplyAdoptsARegisterCandidateWhoseDigestFailedButWhoseRowAppeared`: FAIL
  at `Conflicts = [...], want none` — the digest failure returns where it is
  loaded, before the lock-time re-classification can find the row.
- `TestApplySessionThatVanishedBeforeTheLockIsAConflictWithNoWrite`: FAIL at
  `Registered = [...], want none` — the applier never re-observes.
- `TestApplyReportsAnObservationFailureAsAConflict`: same, FAIL at
  `Registered = [...], want none`.

`TestApplyRegisterWithAnUnreadableConfigurationIsAConflict` passes already: the
unconditional load refuses it today. It is written now because Step 18 is what
could break it — carrying the error forward is only correct if something still
consults it.

- [ ] **Step 18: Make the digest conditional and decide the writes under the lock**

Three edits to `internal/rebuild/apply.go`.

First, in `applyCandidate`, replace the unconditional digest load with:

```go
	// Only registration writes a digest, so only registration needs one.
	// A workspace whose configuration is broken can still have its live
	// session adopted: adoption does not depend on the digest.
	//
	// The failure is carried rather than returned. The case decided here
	// is a work list, not a verdict — if a row appeared in the meantime
	// this candidate becomes an adoption under the lock, which needs no
	// digest at all. Refusing at the load would make the outcome depend
	// on a requirement the candidate may no longer have.
	var digest string
	var digestErr error
	if cand.Case == CaseRegister {
		digest, digestErr = a.Config.Digest(ws.Slug)
	}
```

Second, extend the dry-run gate added in Step 13 to consult it. A dry run stops
before the lock and so has only the pre-lock case to reason from; on that
evidence this candidate registers, and registering is what the missing digest
blocks:

```go
	if a.DryRun {
		// Everything above is read-only, which is exactly what lets a dry
		// run predict the real run's verdict and exit code (spec §2). A
		// preview that stopped after pure classification would report a
		// clean 0 for a vanished worktree the real run refuses.
		if digestErr != nil {
			return nil, conflictf(sess.Name,
				"loading the configuration for %q failed: %v", ws.Slug, digestErr)
		}
		return registeredFor(ws, sess.Name), nil
	}
```

Third, replace everything after `defer release()` with a call to a new
function, and add that function:

```go
	return a.writeUnderLock(ctx, ws, cand, digest, digestErr)
}

// writeUnderLock re-observes, re-reads, and re-classifies with the lock
// held, then writes what that says rather than what the first pass said.
// The lock package's rule is that the observation a mutation is decided
// from must be taken after the lock (internal/lock/lock.go:1-5): the
// classification pass is a work list, not evidence.
//
// digestErr is the configuration failure from before the lock, if any. It
// is consulted only where a digest is actually written, so that it stops
// exactly the candidates that still need one.
func (a *Applier) writeUnderLock(ctx context.Context, ws resolve.Workspace, cand Candidate, digest string, digestErr error) (*Registered, *Conflict) {
	sess := cand.Session

	obs, err := a.Sessions.ObserveSession(ctx, controller.SessionQuery{
		WorkspaceID:    ws.ID,
		CandidateNames: []string{sess.Name},
	})
	if err != nil {
		return nil, conflictf(sess.Name, "re-observing tmux under the workspace lock: %v", err)
	}
	if obs.ByIdentity == nil {
		return nil, conflictf(sess.Name,
			"the session was no longer live when the workspace lock was taken; nothing was written")
	}
	live := *obs.ByIdentity

	var records []state.Record
	rec, err := a.Store.Workspace(ws.ID)
	switch {
	case errors.Is(err, state.ErrNotFound):
		// No row: the register case, unless the re-classification says
		// otherwise.
	case err != nil:
		return nil, conflictf(live.Name, "re-reading the workspace record under the lock: %v", err)
	default:
		records = []state.Record{rec}
	}

	final := Classify([]controller.LiveSession{live}, records)
	if len(final.Candidates) != 1 {
		reason := "the workspace no longer needs recovery: something else completed it " +
			"before the lock was taken; nothing was written"
		if len(final.Conflicts) > 0 {
			reason = final.Conflicts[0].Reason
		}
		return nil, &Conflict{Subject: live.Name, Reason: reason}
	}

	now := a.Clock.Now()
	switch final.Candidates[0].Case {
	case CaseRegister:
		if cand.Case != CaseRegister {
			// The row the first pass saw has since disappeared. The store
			// has no delete primitive, so this needs an external actor —
			// load the digest now rather than registering an empty one.
			digest, digestErr = a.Config.Digest(ws.Slug)
		}
		// This is the one branch that writes a digest, so it is the one
		// branch the failure stops — whether it came from before the lock
		// or from the re-load just above.
		if digestErr != nil {
			return nil, conflictf(live.Name,
				"loading the configuration for %q failed: %v", ws.Slug, digestErr)
		}
		if err := a.Store.RegisterWorkspace(ws, digest, now); err != nil {
			return nil, conflictf(live.Name, "registering workspace %s: %v", ws.Slug, err)
		}
		if err := a.Store.AdoptSessionName(ws.ID, live.Name, now); err != nil {
			return nil, conflictf(live.Name, "adopting session name %q: %v", live.Name, err)
		}
	case CaseAdopt:
		// Never RegisterWorkspace here. It is an upsert whose conflict
		// branch overwrites slug, worktree, is_primary, proposed_session,
		// and desired_digest (internal/state/store.go:43-49), which is the
		// exact opposite of the fill-only guarantee. Fill-only is a
		// property of which primitive each case calls, not of the
		// primitives themselves.
		if err := a.Store.AdoptSessionName(ws.ID, live.Name, now); err != nil {
			return nil, conflictf(live.Name, "adopting session name %q: %v", live.Name, err)
		}
	default:
		return nil, conflictf(live.Name,
			"the workspace state changed under the lock and no longer calls for recovery; nothing was written")
	}
	return registeredFor(ws, live.Name), nil
}
```

Add `"errors"` to the import block.

- [ ] **Step 19: Run the tests to verify they pass**

Run: `go test ./internal/rebuild/ -run TestApply -v`

Expected: all twelve PASS.

- [ ] **Step 20: Commit**

`git commit -am "Decide rebuild's writes from the observation taken under the workspace lock"`

---

- [ ] **Step 21: Write the failing test for partial application**

`RegisterWorkspace` and `AdoptSessionName` are separate transactions, so a
register can succeed and the adopt then fail. Append to
`internal/rebuild/apply_test.go`:

```go
func TestApplyRegisterThenFailedAdoptNamesBothHalvesAndASecondRunCompletesIt(t *testing.T) {
	ws := projectmux()
	sess := liveSession(ws, "projectmux")
	h := newHarness()
	h.know(ws, "sha256:desired")
	h.store = &adoptFailStore{
		Store: h.fakeStore,
		err:   &state.SessionNameConflictError{Name: "projectmux"},
	}
	h.observer.results = []controller.SessionObservation{observing(sess), observing(sess)}
	plan := Plan{Candidates: []Candidate{{Case: CaseRegister, Session: sess}}}

	first := h.applier().Apply(context.Background(), plan)

	if len(first.Registered) != 0 {
		t.Fatalf("Registered = %+v, want none: only half the work landed", first.Registered)
	}
	if len(first.Conflicts) != 1 {
		t.Fatalf("Conflicts = %+v, want exactly one", first.Conflicts)
	}
	// The operator must never be told the workspace was registered when
	// only half of it was, nor that nothing happened when a row now
	// exists. The reason names both halves.
	reason := first.Conflicts[0].Reason
	for _, want := range []string{
		"was registered",
		"adopting session name",
		"already recorded for another workspace",
		"later rebuild will complete it",
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("Reason = %q, want it to contain %q", reason, want)
		}
	}

	rec, err := h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("the row must exist after a successful register: %v", err)
	}
	if rec.ActualSession != nil {
		t.Fatalf("ActualSession = %q, want nil: the adoption failed", *rec.ActualSession)
	}

	// The half-written row is exactly the adopt case, so the next run
	// completes it rather than needing a new atomic primitive.
	second := h.applier().Apply(context.Background(), plan)

	if len(second.Conflicts) != 0 {
		t.Fatalf("second run Conflicts = %+v, want none", second.Conflicts)
	}
	if len(second.Registered) != 1 {
		t.Fatalf("second run Registered = %+v, want one", second.Registered)
	}
	rec, err = h.fakeStore.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "projectmux" {
		t.Errorf("ActualSession = %v, want %q", rec.ActualSession, "projectmux")
	}
	if rec.DesiredDigest == nil || *rec.DesiredDigest != "sha256:desired" {
		t.Errorf("DesiredDigest = %v, want the digest written by the first run", rec.DesiredDigest)
	}
}
```

- [ ] **Step 22: Run the test to verify it fails**

Run: `go test ./internal/rebuild/ -run TestApplyRegisterThenFailedAdopt -v`

Expected: FAIL — `Reason = "adopting session name \"projectmux\": session name
\"projectmux\" is already recorded for another workspace", want it to contain
"was registered"`, and again for `"later rebuild will complete it"`. The
current message reports the adoption failure without saying a row was written.

- [ ] **Step 23: Name both halves in the partial-application conflict**

In `writeUnderLock`, in the `CaseRegister` branch, replace the adopt error
return with:

```go
		if err := a.Store.AdoptSessionName(ws.ID, live.Name, now); err != nil {
			// Registration and adoption are separate transactions, so this
			// leaves a row with no recorded session. That is not
			// corruption — it is precisely the adopt case, which the next
			// run completes. Both halves are named because reporting only
			// the failure would tell the operator nothing was written.
			return nil, conflictf(live.Name,
				"workspace %s was registered, but adopting session name %q failed: %v; "+
					"the row has no recorded session and a later rebuild will complete it",
				ws.Slug, live.Name, err)
		}
```

- [ ] **Step 24: Run the whole package**

Run: `go test ./internal/rebuild/ -count=1 -v`

Expected: every test in the package PASSes, including task 2's classification
tests.

- [ ] **Step 25: Verify formatting, vet, and the race detector**

Run:
```
gofmt -l internal/rebuild
go vet ./internal/rebuild/
go test ./internal/rebuild/ -count=1 -race
```

Expected: `gofmt -l` prints nothing, `go vet` is silent, tests pass under
`-race`.

- [ ] **Step 26: Commit**

`git commit -am "Report a rebuild that registered but could not adopt as a conflict naming both halves"`

---

- [ ] **Step 27: Amend the design spec's §6 step 3**

Application loads the digest for the register case only, which is a deliberate
refinement of what §6 wrote. The spec must say so rather than leave the code
silently diverging.

In `docs/superpowers/specs/2026-08-06-state-rebuild-design.md`, replace line
264:

```
3. `config.Load` for the desired digest.
```

with:

```
3. `config.Load` for the desired digest — for CaseRegister only, the only case
   that writes a digest. A workspace whose configuration is broken can still
   have its live session adopted, because adoption does not depend on the
   digest. A failure here is carried to step 5 rather than ending the candidate:
   the case may become an adoption once the lock-held re-classification runs,
   and a candidate that no longer writes a digest is not blocked by one it could
   not load.
```

The surrounding claim that steps 1-3 are read-only, and that `--dry-run` may
therefore stop after step 3, is unaffected.

- [ ] **Step 28: Commit the documentation amendment with the code**

`git add docs/superpowers/specs/2026-08-06-state-rebuild-design.md && git commit -m "Narrow the design's digest step to the register case, matching the applier"`

---

### Task 4: Database pre-flight classification

Spec §5. `state.Open` is not a corruption test: it calls `os.MkdirAll`, opens the
pool, and migrates (`internal/state/state.go:57-75`), and against a database
already at the current schema `migrate` needs only a successful
`PRAGMA user_version`. Damage elsewhere in the file passes that and surfaces
later as a generic read failure — mid-run, after rebuild has begun writing.
Rebuild therefore classifies the file read-only first, the way doctor does
(`internal/cli/wiring.go:181-201`), using `PRAGMA integrity_check`.

| `OpenReadOnly` result | rebuild | why |
| --- | --- | --- |
| missing (`state.IsMissingDatabase`) | proceed | `state.Open` creates it; the primary recovery path |
| `IntegrityErr` set | refuse | the contents are not a usable database |
| `*state.FutureSchemaError` | refuse | a newer build wrote this database — its data is good, so the refusal must not advise destroying it |
| `*state.PendingMigrationError` | proceed | `state.Open` migrates, as any mutating command does |
| `state.IncompleteWALError` (Task 1) | proceed | `state.Open` recovers the log; this is the crash case rebuild exists for |
| any other error | refuse | uncertainty, never a guess |

The refusal is a plain error, so it exits 1 through `exitCode`'s default branch
(`internal/cli/cli.go:181-183`). That is deliberate: exit 6 denotes uncertainty
about the world, while a corrupt file is a diagnosed, definite condition.

**Refusing is one decision; what to advise is another.** The two refusals in that
table are opposite situations wearing the same verdict. A corrupt database holds
bytes nobody can read, so the advice is to move all three files aside and rebuild
from live tmux sessions. A future schema holds *perfectly good data* — richer
than this build understands — written by a newer projectmux the operator very
likely still has installed. Telling them to move it aside would destroy a working
installation to fix a version mismatch, and it would do so on the strength of a
sentence that says "cannot be read", which in that case is false: it was read
fine, and it said it was newer. So the two get separate messages, and only the
corrupt one mentions moving files.

**Files:**
- Modify: `internal/cli/wiring.go:181-201` (add `rebuildDatabaseCheck`,
  `corruptDatabaseError`, and `futureSchemaError` immediately after the
  `inspectDatabase` seam)
- Test: `internal/cli/rebuild_check_test.go` (create)

The seam goes in `wiring.go` because that file already owns every substitutable
package-var dependency and, in `inspectDatabase`, the other read-only view of
the same database — the two belong side by side. Its tests get their own file so
Task 5's `rebuild_test.go` stays about the command.

**Interfaces:**
- Consumes: `state.OpenReadOnly(root) (*state.ReadOnlyStore, state.Inspection, error)`;
  `state.Inspection.Usable() error`; `state.DBPath(root string) string`;
  `state.IsMissingDatabase(err error) bool`;
  `state.IsIncompleteWAL(err error) bool` (Task 1);
  `*state.PendingMigrationError`; `*state.FutureSchemaError`;
  `state.SchemaVersion`
- Produces: `var rebuildDatabaseCheck = func(root string) error` — Task 5 calls
  it before `openStore()`, and its tests substitute it.

---

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/rebuild_check_test.go`:

```go
package cli

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"

	// The driver is registered by internal/state already; naming it here
	// keeps this file's sql.Open honest about what it depends on.
	_ "modernc.org/sqlite"
)

// healthyStateRoot creates a state root holding a database this build
// wrote and closed cleanly, and returns the root and the database path.
func healthyStateRoot(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	st, err := state.Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-1", Slug: "slab", Worktree: "/w/slab", SessionName: "slab", IsPrimary: true,
	}, "sha256:abc", time.Now()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return root, state.DBPath(root)
}

// A corrupt database is the one case rebuild refuses outright. The
// message is the deliverable: it must name the database and both
// sidecars, because moving state.db alone leaves a stale write-ahead log
// that a freshly created database would inherit.
func TestRebuildDatabaseCheckRefusesACorruptDatabase(t *testing.T) {
	root, path := healthyStateRoot(t)
	if err := os.WriteFile(path, []byte("this is not a database at all"), 0o600); err != nil {
		t.Fatalf("corrupting: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	err = rebuildDatabaseCheck(root)
	if err == nil {
		t.Fatal("rebuildDatabaseCheck accepted a corrupt database")
	}
	for _, want := range []string{path, path + "-wal", path + "-shm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "aside") {
		t.Errorf("refusal %q does not say to move the files aside", err)
	}

	// Rebuild refuses; it never relocates or repairs. The operator
	// performs one inspectable mv.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the check modified the database file")
	}
}

// A database a newer projectmux wrote is refused, like a corrupt one —
// but for the opposite reason, and the message must say so. Its contents
// are good; they are simply richer than this build understands. Telling
// the operator to move them aside would destroy a working installation to
// resolve a version mismatch, so this test asserts the refusal does not
// say that.
func TestRebuildDatabaseCheckRefusesAFutureSchemaWithoutAdvisingDestruction(t *testing.T) {
	root, path := healthyStateRoot(t)

	// Claim a schema this build does not know. The pragma write goes
	// through the ordinary driver; a clean close checkpoints it into the
	// database file and removes the sidecars.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", state.SchemaVersion+1)); err != nil {
		t.Fatalf("bumping user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err = rebuildDatabaseCheck(root)
	if err == nil {
		t.Fatal("rebuildDatabaseCheck accepted a database from a newer build")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("refusal %q does not name the database", msg)
	}
	if !strings.Contains(msg, "newer") {
		t.Errorf("refusal %q does not say a newer build wrote it", msg)
	}
	// The corrupt-database advice must not leak here. These bytes are the
	// operator's state, intact.
	for _, forbidden := range []string{"aside", "cannot be read"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("refusal %q contains %q, which would advise destroying good data", msg, forbidden)
		}
	}
}

// A missing database is the primary recovery path, not a failure:
// state.Open creates it and rebuild proceeds against a fresh one.
func TestRebuildDatabaseCheckProceedsWhenTheDatabaseIsMissing(t *testing.T) {
	if err := rebuildDatabaseCheck(t.TempDir()); err != nil {
		t.Errorf("rebuildDatabaseCheck on a fresh installation = %v, want nil", err)
	}
}

func TestRebuildDatabaseCheckProceedsOnAHealthyDatabase(t *testing.T) {
	root, _ := healthyStateRoot(t)
	if err := rebuildDatabaseCheck(root); err != nil {
		t.Errorf("rebuildDatabaseCheck = %v, want nil", err)
	}
}

// A -wal with no -shm beside it means a writer died without
// checkpointing — precisely the crash rebuild exists to recover from.
// Refusing here would refuse the main case, so this test is load-bearing
// rather than an edge case.
func TestRebuildDatabaseCheckProceedsWithAnUnrecoveredWAL(t *testing.T) {
	root, path := healthyStateRoot(t)

	// Stage the crash: capture the database and its log while a writer
	// holds them, then restore both after that writer's clean close has
	// removed the sidecars. What is left is a pre-checkpoint database
	// with an orphaned -wal and no -shm.
	st, err := state.Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-2", Slug: "other", Worktree: "/w/other", SessionName: "other", IsPrimary: true,
	}, "sha256:def", time.Now()); err != nil {
		t.Fatalf("register: %v", err)
	}
	dbBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	walBytes, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.WriteFile(path, dbBytes, 0o600); err != nil {
		t.Fatalf("restore db: %v", err)
	}
	if err := os.WriteFile(path+"-wal", walBytes, 0o600); err != nil {
		t.Fatalf("restore wal: %v", err)
	}

	if err := rebuildDatabaseCheck(root); err != nil {
		t.Fatalf("rebuildDatabaseCheck refused an unrecovered log: %v", err)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run TestRebuildDatabaseCheck -v`

Expected: FAIL to compile — `undefined: rebuildDatabaseCheck`. (If Task 1 has
not landed, `undefined: state.IsIncompleteWAL` appears once Step 3 is written;
Task 4 depends on Task 1.)

- [ ] **Step 3: Write the implementation**

Append to `internal/cli/wiring.go`, directly after `inspectDatabase`:

```go
// rebuildDatabaseCheck decides whether rebuild may open the database
// read-write. A nil error means proceed.
//
// state.Open is not a corruption test: against a database already at the
// current schema it needs only a successful PRAGMA user_version, so
// damage elsewhere in the file would surface mid-run, after rebuild had
// begun writing. Rebuild therefore classifies the file the way doctor
// does — read-only, through PRAGMA integrity_check — before opening it
// to write (spec §5).
//
// Two verdicts differ from doctor's, both deliberately. A pending
// migration is doctor's finding but rebuild's normal path, because
// rebuild is a mutating command. An unrecovered write-ahead log is the
// crash rebuild exists to recover from, so refusing it would refuse the
// main case; state.Open recovers the log.
var rebuildDatabaseCheck = func(root string) error {
	path := state.DBPath(root)

	ro, insp, err := state.OpenReadOnly(root)
	if err != nil {
		switch {
		case state.IsMissingDatabase(err):
			// A fresh installation. state.Open creates it and rebuild
			// proceeds against an empty database; nothing is destroyed.
			return nil
		case state.IsIncompleteWAL(err):
			return nil
		default:
			return corruptDatabaseError(path, err)
		}
	}
	defer ro.Close()

	usable := insp.Usable()
	if usable == nil {
		return nil
	}
	var pending *state.PendingMigrationError
	if errors.As(usable, &pending) {
		return nil
	}
	// A newer build's database is refused, but its contents are good and
	// the message must not tell anyone to move them aside.
	var future *state.FutureSchemaError
	if errors.As(usable, &future) {
		return futureSchemaError(path, usable)
	}
	// What is left is confirmed damage, and not something to guess past.
	return corruptDatabaseError(path, usable)
}

// corruptDatabaseError is the refusal message, which is the deliverable
// here. Naming the sidecars is not pedantry: moving state.db alone
// leaves a stale write-ahead log that a freshly created database would
// inherit.
func corruptDatabaseError(path string, cause error) error {
	return fmt.Errorf(
		"the state database at %s cannot be read: %w\n"+
			"rebuild will not move it aside for you. Move all three of %s, %s, and %s "+
			"aside, then run projectmux rebuild again to recover what live tmux "+
			"sessions still describe",
		path, cause, path, path+"-wal", path+"-shm")
}

// futureSchemaError refuses a database this build is too old to write,
// and says nothing about moving files. The data is intact and a newer
// projectmux — probably still installed — reads it correctly; rebuilding
// over it would discard everything the newer schema records that this one
// has no column for. The wrapped error already names both versions and
// says to upgrade, so this adds only the path and the reason not to
// reach for the corrupt-database remedy.
func futureSchemaError(path string, cause error) error {
	return fmt.Errorf(
		"the state database at %s was written by a newer projectmux: %w\n"+
			"its contents are intact, and rebuilding with this build would discard "+
			"what the newer schema records",
		path, cause)
}
```

`errors`, `fmt`, and `state` are already imported by `wiring.go`; no import
changes are needed.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run TestRebuildDatabaseCheck -v`

Expected: PASS, five tests.

Note on the corrupt-database case: whether SQLite reports the garbage file as
`SQLITE_NOTADB` (so `OpenReadOnly` returns an `Inspection` with `IntegrityErr`
set) or the driver fails the connection some other way (so `OpenReadOnly`
returns an error), both paths land on `corruptDatabaseError`. The test asserts
the refusal and its message rather than which branch produced it, so it is
stable across driver versions.

- [ ] **Step 5: Commit**

`git commit -m "Classify the state database read-only before rebuild opens it"`

---

### Task 5: The `projectmux rebuild` command

Spec §2, §6 (flow), §7, §9. This is the CLI half of the slice: flag parsing, the
flow, the exit-code mapping, the JSON envelope, the human renderer, the three
production adapters for `internal/rebuild`'s interfaces, and the documentation
that keeps the command's name from overpromising.

**Files:**
- Create: `internal/cli/rebuild.go`
- Create: `internal/cli/rebuild_test.go`
- Modify: `internal/cli/cli.go:59-61` (the `usage` const gains a `rebuild` entry
  between `doctor` and `version`) and `internal/cli/cli.go:149-151` (the
  `dispatch` switch gains `case "rebuild"` after `case "doctor"`)
- Modify: `docs/commands.md:13-14` (table of contents) and a new
  `## projectmux rebuild` section inserted before `## projectmux version`
  (currently line 361)

**Interfaces:**
- Consumes, from `internal/rebuild` (Tasks 2-3):
  `rebuild.Classify(live []controller.LiveSession, records []state.Record) rebuild.Plan`;
  `rebuild.Applier{Store, Sessions, Resolver, Config, Locker, Clock, DryRun}`;
  `(*rebuild.Applier).Apply(ctx context.Context, plan rebuild.Plan) rebuild.Report`;
  `rebuild.Report{DryRun bool; Registered []rebuild.Registered; Conflicts []rebuild.Conflict}`.
  `Apply` carries `Plan.Conflicts` into `Report.Conflicts` unchanged, so a
  conflict found during classification reaches the report without any candidate
  being applied.
- Consumes, from Task 4: `rebuildDatabaseCheck(root string) error`.
- Consumes, existing: `config.Root()`, `config.LoadDefaults(root) (config.Source, error)`,
  `config.Load(root string, defaults config.Source, slug string) (config.Effective, error)`,
  `state.Root()`, `resolve.Resolve(name string, roots []string, cwd string) (resolve.Workspace, error)`,
  `lock.Acquire(ctx, dir, workspaceID string, timeout time.Duration) (*lock.Lock, error)`,
  `(*lock.Lock).Release() error`, `lockTimeout` (`internal/cli/open.go:32`),
  `openStore`, `liveSessions`, `newSessionObserver` (`internal/cli/wiring.go:31-44`),
  `writeJSON` (`internal/cli/config.go:146`), `cells` (`internal/cli/doctor.go:152`),
  `OutputSchemaVersion` (`internal/cli/config.go:22`),
  `controller.RefusalError{Reason string}` (`internal/controller/ensure.go:36`),
  `reportedError{msg, err}` (`internal/cli/cli.go:78`).
- Produces, for Task 6: `rebuildEnvelope` and `decodeRebuild(t, stdout)`.

**Exit codes** (spec §7):

| condition | code | mechanism |
| --- | --- | --- |
| registered everything, or nothing to do | 0 | `nil` |
| any conflict | 6 | `&reportedError{msg, err: &controller.RefusalError{}}`, report on stdout |
| tmux unobservable | 6 | plain `&controller.RefusalError{}`, nothing on stdout |
| `defaults.yaml` will not load | 5 | `*config.InvalidConfigError` propagates |
| one workspace's configuration will not load | 6, batch continues | that workspace becomes a conflict |
| corrupt database | 1 | Task 4's plain error |
| usage error | 2 | `usagef` |

`--dry-run` uses the same codes. The exit code describes the state of the world,
not whether anything was written — which holds only because a dry run still
performs the read-only resolution and configuration load (Task 3).

**Adapter placement.** The three adapters (`worktreeResolver`, `configDigests`,
`workspaceLocker`) go in `rebuild.go`, not `wiring.go`: `wiring.go` holds
substitutable seams shared across commands, and these are fixed production
adapters used by exactly one command.

---

- [ ] **Step 1: Write the failing test for the empty happy path**

Create `internal/cli/rebuild_test.go`:

```go
package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
)

func decodeRebuild(t *testing.T, stdout string) rebuildEnvelope {
	t.Helper()
	var env rebuildEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding rebuild JSON: %v\n%s", err, stdout)
	}
	return env
}

// rebuildEnv wires every rebuild seam to a benign fake — a real git
// repository with valid configuration, an isolated state root, a store
// the command may mutate, and a scripted session observer that fails the
// test if it is consulted — so a test can perturb exactly one of them.
// It returns the resolved workspace the repository represents.
func rebuildEnv(t *testing.T, s *fake.Store, live []controller.LiveSession) resolve.Workspace {
	t.Helper()
	ws := openWorkspace(t)
	installOpenStore(t, s)
	installLiveSessions(t, live, nil)
	installScriptedSessions(t) // exhausts on any call
	return ws
}

// installRebuildDatabaseCheck substitutes the pre-flight classification
// so a command test can exercise the refusal without a corrupt file.
func installRebuildDatabaseCheck(t *testing.T, err error) {
	t.Helper()
	orig := rebuildDatabaseCheck
	t.Cleanup(func() { rebuildDatabaseCheck = orig })
	rebuildDatabaseCheck = func(string) error { return err }
}

// A fully recovered installation produces an empty report and exits 0.
// That is what makes applying by default safe: a needless run costs
// nothing and says so.
func TestRebuildReportsNothingWhenEverythingIsRecorded(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	env := decodeRebuild(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, OutputSchemaVersion)
	}
	if env.DryRun {
		t.Error("dry_run is set without --dry-run")
	}
	if len(env.Registered) != 0 || len(env.Conflicts) != 0 {
		t.Errorf("report = %+v, want empty", env)
	}
}

func TestRebuildRejectsArguments(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, stderr := run(t, "rebuild", "slabledger")
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "slabledger") {
		t.Errorf("stderr %q should name the unexpected argument", stderr)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/cli/ -run TestRebuildReportsNothing -v`

Expected: FAIL to compile — `undefined: rebuildEnvelope`. After Step 3 defines
the envelope but before dispatch is wired, the run instead exits 2: `rebuild` is
not a command yet, so the design-§8 shorthand routes `rebuild --json` to `open`,
which rejects two positional arguments.

- [ ] **Step 3: Write the command skeleton, the envelope, and the renderers**

Create `internal/cli/rebuild.go`:

```go
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/lock"
	"github.com/gambtho/projectmux/internal/rebuild"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

const rebuildHelp = `usage: projectmux rebuild [--dry-run] [--json] [--compact]

Recover workspace registrations the state database has lost, using the
identity keys live tmux sessions carry. A live session no stored row
matches is registered again and its session name adopted.

Rebuild only fills in what is missing: it never overwrites a recorded
value, and anything ambiguous is reported as a conflict and skipped.

It does NOT rediscover worktrees from repository_roots — only workspaces
whose tmux session is still alive can be recovered — and it does NOT
restore container bindings, which the next open reacquires.

  --dry-run  perform every read-only step and report what would change,
             writing nothing
  --json     emit the versioned JSON envelope instead of human text
  --compact  emit the JSON on a single line (implies --json)
`

// rebuildEnvelope is the versioned JSON structure for projectmux
// rebuild. Registered and Conflicts are always arrays, empty rather than
// absent, matching doctor's always-full checks. The envelope is written
// to stdout even when the command exits 6 — the report is the output
// (stop/autostart spec §5).
type rebuildEnvelope struct {
	SchemaVersion int                 `json:"schema_version"`
	DryRun        bool                `json:"dry_run"`
	Registered    []rebuildRegistered `json:"registered"`
	Conflicts     []rebuildConflict   `json:"conflicts"`
}

type rebuildRegistered struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	Worktree  string `json:"worktree"`
	IsPrimary bool   `json:"is_primary"`
	Session   string `json:"session"`
}

type rebuildConflict struct {
	Subject string `json:"subject"`
	Reason  string `json:"reason"`
}

func runRebuild(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("rebuild")
	dryRun := fs.Bool("dry-run", false, "report what would change, writing nothing")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, rebuildHelp)
			return nil
		}
		return usagef("rebuild: %s", err)
	}
	if fs.NArg() > 0 {
		// Rebuild works over the whole installation; there is no
		// workspace to scope it to.
		return usagef("rebuild: unexpected argument %q", fs.Arg(0))
	}
	if *compact {
		*asJSON = true
	}

	report, err := buildRebuild(ctx, *dryRun)
	if err != nil {
		return err
	}
	env := rebuildEnvelopeFrom(report)

	if *asJSON {
		if err := writeJSON(stdout, env, *compact); err != nil {
			return err
		}
	} else if err := writeRebuildHuman(stdout, env); err != nil {
		return err
	}

	if n := len(env.Conflicts); n > 0 {
		// The report already went to stdout, so only a one-line summary
		// reaches stderr. The wrapped refusal is what makes this exit 6:
		// a conflict is uncertainty about the world, not a failure of
		// the command.
		return &reportedError{
			msg: fmt.Sprintf(
				"rebuild left %d conflict(s) unresolved; details are in the report above", n),
			err: &controller.RefusalError{
				Reason: "rebuild found conflicts it will not resolve by guessing",
			},
		}
	}
	return nil
}

func rebuildEnvelopeFrom(report rebuild.Report) rebuildEnvelope {
	env := rebuildEnvelope{
		SchemaVersion: OutputSchemaVersion,
		DryRun:        report.DryRun,
		Registered:    []rebuildRegistered{},
		Conflicts:     []rebuildConflict{},
	}
	for _, r := range report.Registered {
		env.Registered = append(env.Registered, rebuildRegistered{
			ID:        r.ID,
			Slug:      r.Slug,
			Worktree:  r.Worktree,
			IsPrimary: r.IsPrimary,
			Session:   r.Session,
		})
	}
	for _, c := range report.Conflicts {
		env.Conflicts = append(env.Conflicts, rebuildConflict{
			Subject: c.Subject,
			Reason:  c.Reason,
		})
	}
	return env
}

// writeRebuildHuman renders one line per registration and one per
// conflict. This layout is not a compatibility contract; automation
// should use --json.
func writeRebuildHuman(w io.Writer, env rebuildEnvelope) error {
	if len(env.Registered) == 0 && len(env.Conflicts) == 0 {
		fmt.Fprintln(w, "nothing to rebuild: every live session is already recorded")
		return nil
	}
	registered := "registered"
	if env.DryRun {
		registered = "would-register"
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, r := range env.Registered {
		fmt.Fprintln(tw, cells(registered, r.Slug, r.Session))
	}
	for _, c := range env.Conflicts {
		fmt.Fprintln(tw, cells("conflict", c.Subject, c.Reason))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
```

- [ ] **Step 4: Write the flow and the production adapters**

Append to `internal/cli/rebuild.go`:

```go
// buildRebuild is spec §6's flow: load defaults, classify the database
// read-only, open the store read-write, list live sessions, read
// records, classify, apply.
func buildRebuild(ctx context.Context, dryRun bool) (rebuild.Report, error) {
	configRoot, err := config.Root()
	if err != nil {
		return rebuild.Report{}, err
	}
	// An unloadable defaults layer makes the digest underivable for
	// every workspace, so it is fatal (exit 5) rather than one
	// workspace's conflict — mirroring doctor's fatal DefaultsErr
	// branch.
	defaults, err := config.LoadDefaults(configRoot)
	if err != nil {
		return rebuild.Report{}, err
	}

	stateRoot, err := state.Root()
	if err != nil {
		return rebuild.Report{}, err
	}
	if err := rebuildDatabaseCheck(stateRoot); err != nil {
		return rebuild.Report{}, err
	}

	st, err := openStore()
	if err != nil {
		return rebuild.Report{}, err
	}
	defer st.Close()

	live, err := liveSessions(ctx)
	if err != nil {
		// A tmux outage is not "no live sessions". Registering nothing
		// and reporting success would be exactly the tri-state
		// violation doctor exists to prevent, so this is a refusal.
		return rebuild.Report{}, &controller.RefusalError{
			Reason: "cannot observe tmux sessions, so there is nothing to rebuild from: " +
				err.Error(),
		}
	}
	records, err := st.Workspaces()
	if err != nil {
		return rebuild.Report{}, fmt.Errorf("reading stored workspaces: %w", err)
	}

	applier := &rebuild.Applier{
		Store:    st,
		Sessions: newSessionObserver(),
		Resolver: worktreeResolver{},
		Config:   configDigests{root: configRoot, defaults: defaults},
		Locker:   workspaceLocker{dir: filepath.Join(stateRoot, "locks")},
		Clock:    systemClock{},
		DryRun:   dryRun,
	}
	return applier.Apply(ctx, rebuild.Classify(live, records)), nil
}

// worktreeResolver re-derives a workspace's identity the way every other
// command does: from the directory, never from the tmux keys. That is
// what recovers IsPrimary and the proposed session name, neither of
// which tmux carries (spec §3), and it is what lets rebuild verify the
// keys it was handed.
type worktreeResolver struct{}

func (worktreeResolver) Resolve(worktree string) (resolve.Workspace, error) {
	// No name and no roots: roots feed only lookup by name, and rebuild
	// resolves from a directory.
	return resolve.Resolve("", nil, worktree)
}

// configDigests supplies the desired digest from current configuration.
// Registering today's desired digest with no applied digest means the
// next open sees drift and reconciles — correct, since the configuration
// was never applied to this database (spec §3).
type configDigests struct {
	root     string
	defaults config.Source
}

func (c configDigests) Digest(slug string) (string, error) {
	effective, err := config.Load(c.root, c.defaults, slug)
	if err != nil {
		return "", err
	}
	return effective.Digest, nil
}

// workspaceLocker is the per-workspace filesystem lock every mutating
// command takes before its final observation and holds through the
// resulting state commit.
type workspaceLocker struct{ dir string }

func (w workspaceLocker) Lock(ctx context.Context, workspaceID string) (func(), error) {
	l, err := lock.Acquire(ctx, w.dir, workspaceID, lockTimeout)
	if err != nil {
		return nil, err
	}
	return func() { _ = l.Release() }, nil
}
```

- [ ] **Step 5: Wire dispatch and the usage block**

In `internal/cli/cli.go`, add to the `usage` const between the `doctor` entry
(lines 59-60) and the `version` entry (line 61):

```go
  rebuild [--dry-run] [--json] [--compact]
        re-register workspaces the state database lost, from the identity
        keys their live tmux sessions carry; does not rediscover worktrees
        from repository_roots and does not restore container bindings
```

And in `dispatch`, after `case "doctor":` (lines 149-150):

```go
	case "rebuild":
		return runRebuild(ctx, rest, stdout)
```

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./internal/cli/ -run 'TestRebuildReportsNothing|TestRebuildRejectsArguments' -v`

Expected: PASS.

- [ ] **Step 7: Write the failing exit-code and stdout-contract tests**

Append to `internal/cli/rebuild_test.go`:

```go
// mismatchedSession builds the fixture for spec §4 case 3: a recorded
// workspace whose actual_session is "old-name" while a live session
// carrying its identity keys answers to "new-name". Fill-only means
// rebuild reports that and writes nothing.
func mismatchedSession(t *testing.T) (*fake.Store, resolve.Workspace) {
	t.Helper()
	s := fake.NewStore()
	ws := rebuildEnv(t, s, nil)
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.AdoptSessionName(ws.ID, "old-name", cliTestTime); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	installLiveSessions(t, []controller.LiveSession{{
		Name:        "new-name",
		WorkspaceID: ws.ID,
		Slug:        ws.Slug,
		Worktree:    ws.Worktree,
	}}, nil)
	return s, ws
}

// The report goes to stdout even when the command exits 6. Hiding the
// conflicts in exactly the case that needs reading is what reportedError
// exists to prevent.
func TestRebuildReportsConflictsOnStdoutAndExitsRefused(t *testing.T) {
	s, ws := mismatchedSession(t)

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitRefused, stderr)
	}
	if stdout == "" {
		t.Fatal("the report did not reach stdout")
	}
	env := decodeRebuild(t, stdout)
	if len(env.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly one", env.Conflicts)
	}
	if len(env.Registered) != 0 {
		t.Errorf("registered = %+v, want nothing written on a conflict", env.Registered)
	}
	if stderr == "" {
		t.Error("stderr should carry the one-line summary")
	}

	// Fill-only: the recorded name is untouched.
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "old-name" {
		t.Errorf("actual_session = %v, want %q unchanged", rec.ActualSession, "old-name")
	}
}

// A tmux outage is uncertainty, not an empty installation. It refuses
// with nothing on stdout: there is no report to write.
func TestRebuildRefusesWhenTmuxIsUnobservable(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)
	installLiveSessions(t, nil, errors.New("tmux exploded"))

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d", code, ExitRefused)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "tmux") {
		t.Errorf("stderr %q does not explain the outage", stderr)
	}
}

// An unloadable defaults layer makes the digest underivable for every
// workspace, so it is fatal rather than one workspace's conflict.
func TestRebuildExitsFiveWhenDefaultsWillNotLoad(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": "version: 1\nautostrt: true\n"})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitInvalidConfig {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitInvalidConfig, stderr)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
}

// A corrupt database is exit 1, not 6: exit 6 denotes uncertainty about
// the world, and a corrupt file is a diagnosed, definite condition. The
// value is in the message.
func TestRebuildExitsOneOnACorruptDatabase(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)
	installRebuildDatabaseCheck(t, errors.New(
		"the state database at /state/state.db cannot be read: malformed"))

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitError {
		t.Fatalf("exit = %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("a failing command wrote to stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "state.db") {
		t.Errorf("stderr %q does not name the database", stderr)
	}
}
```

Add `"errors"` to the test file's imports; `"strings"` is already there.

- [ ] **Step 8: Run them and confirm they pass**

Run: `go test ./internal/cli/ -run TestRebuild -v`

Expected: PASS. These exercise code written in Steps 3-5; if any fails, the flow
in `buildRebuild` is wrong, not the test — check the ordering (defaults before
the database check, the database check before `openStore`).

- [ ] **Step 9: Write the envelope-shape, `--compact`, `--dry-run`, and help tests**

Append to `internal/cli/rebuild_test.go`:

```go
// Both arrays are always present and never null, matching doctor's
// always-full checks: a consumer never branches on absence.
func TestRebuildEnvelopeAlwaysCarriesBothArrays(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, _ := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	var raw struct {
		SchemaVersion *int               `json:"schema_version"`
		DryRun        *bool              `json:"dry_run"`
		Registered    *[]json.RawMessage `json:"registered"`
		Conflicts     *[]json.RawMessage `json:"conflicts"`
	}
	if err := json.Unmarshal([]byte(stdout), &raw); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, stdout)
	}
	if raw.SchemaVersion == nil || raw.DryRun == nil {
		t.Errorf("envelope is missing schema_version or dry_run: %s", stdout)
	}
	if raw.Registered == nil {
		t.Errorf("registered is null rather than an empty array: %s", stdout)
	}
	if raw.Conflicts == nil {
		t.Errorf("conflicts is null rather than an empty array: %s", stdout)
	}
}

func TestRebuildCompactImpliesJSON(t *testing.T) {
	rebuildEnv(t, fake.NewStore(), nil)

	code, stdout, stderr := run(t, "rebuild", "--compact")
	if code != ExitOK {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Errorf("--compact should emit one line:\n%s", stdout)
	}
	decodeRebuild(t, stdout)
}

// A dry run is a preview, not a partial pass: it reports the conflicts
// the real run would report and exits on them the same way, because the
// exit code describes the state of the world rather than whether
// anything was written.
func TestRebuildDryRunReportsTheSameConflictsAndCode(t *testing.T) {
	s, ws := mismatchedSession(t)

	code, stdout, stderr := run(t, "rebuild", "--dry-run", "--json")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d; stderr = %q", code, ExitRefused, stderr)
	}
	env := decodeRebuild(t, stdout)
	if !env.DryRun {
		t.Error("dry_run is false in a --dry-run report")
	}
	if len(env.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want the same conflict the real run reports", env.Conflicts)
	}
	if len(env.Registered) != 0 {
		t.Errorf("registered = %+v, want nothing", env.Registered)
	}

	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != "old-name" {
		t.Errorf("actual_session = %v, want %q unchanged", rec.ActualSession, "old-name")
	}
}

// The name overpromises relative to what ships, so the help text is the
// mitigation and is pinned by a test.
func TestRebuildHelpStatesWhatItDoesNotDo(t *testing.T) {
	code, stdout, _ := run(t, "rebuild", "--help")
	if code != ExitOK {
		t.Fatalf("exit = %d", code)
	}
	for _, want := range []string{
		"usage: projectmux rebuild", "--dry-run", "repository_roots", "container bindings",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("rebuild help does not mention %q:\n%s", want, stdout)
		}
	}
}

// Human output is the default and must not be JSON.
func TestRebuildHumanOutputNamesEachRow(t *testing.T) {
	mismatchedSession(t)

	code, stdout, _ := run(t, "rebuild")
	if code != ExitRefused {
		t.Fatalf("exit = %d, want %d", code, ExitRefused)
	}
	if strings.HasPrefix(strings.TrimSpace(stdout), "{") {
		t.Error("human output should not be JSON by default")
	}
	if !strings.Contains(stdout, "conflict") {
		t.Errorf("human output does not name the conflict:\n%s", stdout)
	}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if strings.TrimRight(line, " ") != line {
			t.Errorf("line ends in whitespace: %q", line)
		}
	}
}
```

The write-suppression guarantee itself — that a dry run reaches no store method
at all — is Task 3's test's job, against a store that records every call. This
test pins the CLI's flag threading, the `dry_run` field, and the exit code.

- [ ] **Step 10: Run the whole package**

Run: `go test ./internal/cli/ -count=1`

Expected: PASS.

- [ ] **Step 11: Document the command**

In `docs/commands.md`, extend the table of contents at lines 13-14:

```markdown
- Operations — [`autostart`](#projectmux-autostart),
  [systemd](#running-autostart-from-systemd), [`rebuild`](#projectmux-rebuild)
```

And insert this section immediately before `## projectmux version`:

````markdown
## projectmux rebuild

```text
projectmux rebuild [--dry-run] [--json] [--compact]
```

Recovers workspace registrations the state database has lost. Every live
projectmux session carries three identity keys — the workspace ID, the slug,
and the worktree — so a session that outlived its database still describes the
workspace it belongs to. Rebuild reads those keys, re-derives the rest of the
registration from the worktree itself, and writes the row back.

```text
$ projectmux rebuild
registered  slabledger  slabledger
```

**What it does not do.** Rebuild recovers registrations *from live sessions*
only. It does not rediscover worktrees from `repository_roots` — a workspace
with no live session stays unregistered until the next `open` — and it does not
restore container bindings, which the next `open` reacquires. The name is
broader than the command.

**It only fills in what is missing.** Rebuild never overwrites a recorded
value. A workspace already recorded with a different session name, two live
sessions claiming the same workspace, a session whose keys disagree with the
worktree they name — each is reported as a conflict and skipped, and the run
exits 6. Nothing that was already known is lost by running it, which is why it
applies by default rather than requiring confirmation.

Running it a second time over a recovered installation reports nothing and
exits 0.

`--dry-run` performs every read-only step — classification, resolution,
identity verification, configuration loading — and stops before the writes. It
is a preview rather than a partial pass: a dry run that reports a conflict is
the conflict the real run would report, and it exits on that conflict with the
same code, because the exit code describes the state of the world rather than
whether anything was written.

| Situation | Exit |
| --- | --- |
| Registered everything, or nothing to do | 0 |
| Any conflict | 6, with the report on stdout |
| tmux could not be observed | 6 |
| `defaults.yaml` will not load | 5 |
| The state database is corrupt | 1 |

The report is the output, so it goes to stdout even on exit 6, with a one-line
summary on stderr.

A **corrupt** state database is refused rather than repaired, and the message
names the database and both of its sidecars. Move all three aside and run
rebuild again; a **missing** database needs no such step, since that is the
case rebuild is built for.
````

- [ ] **Step 12: Commit**

`git commit -m "Add projectmux rebuild"`

---

### Task 6: End-to-end lifecycle test, mutation testing, and final verification

Spec §8. Everything before this task was tested against fakes at some seam. This
task performs the actual disaster against a real tmux server and the real SQLite
store, then deliberately breaks each load-bearing safety rule to confirm a test
notices.

**Files:**
- Modify: `internal/cli/lifecycle_test.go:39-47` (`lifecycleRig` also points the
  `liveSessions` seam at the isolated socket) and append one test at the end of
  the file

**Interfaces:**
- Consumes: `lifecycleRig(t, label) (resolve.Workspace, string)`,
  `openJSON(t) openEnvelope`, `run(t, args...) (int, string, string)`,
  `decodeRebuild(t, stdout) rebuildEnvelope` (Task 5),
  `state.Open(root) (*state.Store, error)`, `state.DBPath(root) string`,
  `(*state.Store).Workspace(id) (state.Record, error)`
- Produces: nothing further consumes this task.

---

- [ ] **Step 1: Extend `lifecycleRig` to route bulk session listing to the test socket**

`lifecycleRig` currently substitutes `newSessionObserver` and
`newSessionActuator` (`internal/cli/lifecycle_test.go:39-46`) but not
`liveSessions`, because no existing lifecycle test reaches a command that
enumerates sessions in bulk. `rebuild` does, and without this it would talk to
the developer's real tmux server.

Insert after the `newSessionActuator` assignment and before
`installContainerObserver`:

```go
	origLive := liveSessions
	t.Cleanup(func() { liveSessions = origLive })
	liveSessions = func(ctx context.Context) ([]controller.LiveSession, error) {
		return (&tmux.Client{Socket: socket}).Sessions(ctx)
	}
```

`context`, `controller`, and `tmux` are already imported by the file. No
existing test changes behavior, because none of them reaches `liveSessions`.

- [ ] **Step 2: Write the failing end-to-end test**

Append to `internal/cli/lifecycle_test.go`:

```go
// TestLifecycleRebuildAfterLosingTheDatabase performs the disaster this
// slice exists for, against a real tmux server and the real store: the
// state database is destroyed while the session it described is still
// running. Rebuild re-registers the workspace from the session's
// identity keys, recovering the two fields tmux does not carry —
// is_primary and the proposed session name — and adopts the live name.
// The second run is the idempotence claim, and the one most likely to
// regress.
func TestLifecycleRebuildAfterLosingTheDatabase(t *testing.T) {
	ws, socket := lifecycleRig(t, "rebuild")

	if env := openJSON(t); env.Action != "created" {
		t.Fatalf("open = %+v", env)
	}

	// The disaster: the database and its sidecars are gone; the tmux
	// session is not.
	stateRoot := os.Getenv("PROJECTMUX_STATE_ROOT")
	path := state.DBPath(stateRoot)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing %s: %v", p, err)
		}
	}

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("rebuild exit %d, stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	env := decodeRebuild(t, stdout)
	if len(env.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", env.Conflicts)
	}
	if len(env.Registered) != 1 {
		t.Fatalf("registered = %+v, want exactly one workspace", env.Registered)
	}
	got := env.Registered[0]
	if got.ID != ws.ID || got.Slug != ws.Slug || got.Worktree != ws.Worktree ||
		got.Session != ws.SessionName || !got.IsPrimary {
		t.Fatalf("registered = %+v, want %s at %s as primary session %q",
			got, ws.Slug, ws.Worktree, ws.SessionName)
	}

	// The row is real, and it carries the two fields tmux never held.
	st, err := state.Open(stateRoot)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	rec, err := st.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != ws.SessionName {
		t.Errorf("actual_session = %v, want %q adopted", rec.ActualSession, ws.SessionName)
	}
	if !rec.IsPrimary {
		t.Error("is_primary was not recovered; autostart would stop starting this container")
	}
	if rec.ProposedSession != ws.SessionName {
		t.Errorf("proposed_session = %q, want %q", rec.ProposedSession, ws.SessionName)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Idempotence: a fully recovered installation has nothing to do and
	// says so.
	code, stdout, stderr = run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("second rebuild exit %d, stderr: %s", code, stderr)
	}
	second := decodeRebuild(t, stdout)
	if len(second.Registered) != 0 || len(second.Conflicts) != 0 {
		t.Errorf("second rebuild = %+v, want an empty report", second)
	}

	// The session was adopted, never recreated or renamed.
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != ws.SessionName || live[0].WorkspaceID != ws.ID {
		t.Errorf("live = %+v; rebuild created or renamed sessions", live)
	}
}
```

- [ ] **Step 3: Run it**

Run: `go test ./internal/cli/ -run TestLifecycleRebuildAfterLosingTheDatabase -v`

Expected: PASS on a machine with tmux; SKIP (`tmux is not installed`) otherwise,
via `lifecycleRig`'s existing guard. A failure here means the flow is wrong
end to end — most likely the `liveSessions` seam from Step 1, the resolver
adapter, or the lock-time re-observation.

- [ ] **Step 4: Mutation-test safety rule (a) — classification must not overwrite a recorded session name**

Spec §4 case 3: a row whose `actual_session` is already set and disagrees with
the live name is a conflict, never an overwrite. Case 4 is where an overwrite
would do real damage, but case 3 is the one an implementer is most likely to
"helpfully" repair.

Exact edit, in `internal/rebuild` (Task 2's classification): in the branch that
returns `CaseSessionMismatch` when the record's `ActualSession` is non-nil and
differs from the live session's `Name`, change the produced case to `CaseAdopt`.

Run: `go test ./internal/cli/ ./internal/rebuild/ -count=1`

Expected: FAIL, specifically
`TestRebuildReportsConflictsOnStdoutAndExitsRefused` (Task 5) — it would exit 0
with an empty conflicts array instead of 6 — and the case-3 row of Task 2's
classification table test.

If nothing fails, the fill-only rule is not tested and no further work on this
task is valid until it is. Revert the edit before continuing.

- [ ] **Step 5: Mutation-test safety rule (b) — `CaseAdopt` must not re-register**

Spec §4: `RegisterWorkspace` is an upsert whose conflict branch overwrites
`slug`, `worktree`, `is_primary`, `proposed_session`, and `desired_digest`.
Fill-only is a property of which primitive each case calls, so case 2 — a row
that exists with no `actual_session` — must call `AdoptSessionName` alone.

Exact edit, in `internal/rebuild` (Task 3's application): in the switch over the
re-classified case, change the `CaseAdopt` arm to call
`a.Store.RegisterWorkspace(workspace, digest, now)` before (or instead of)
`a.Store.AdoptSessionName(...)`.

Run: `go test ./internal/rebuild/ -count=1`

Expected: FAIL — Task 3's lock-time reclassification test, the one asserting
that a case-1 candidate whose row appeared in the gap adopts rather than
registers and leaves every pre-existing field byte-identical.

If no test fails, that assertion does not exist yet. Write it before continuing:
a store pre-seeded with a record whose `slug`, `worktree`, `is_primary`, and
`desired_digest` differ from what the resolver would produce, and an assertion
that all four are unchanged after `Apply`. Then re-run this mutation. Revert the
edit before continuing.

- [ ] **Step 6: Final verification gate**

All four must pass before any completion claim. Run each and read its output;
none of them may be reported as passing on the strength of a previous run.

```sh
gofmt -l .
go vet ./...
CGO_ENABLED=0 go build ./cmd/projectmux
go test ./... -count=1 -race
```

Expected: `gofmt -l .` prints nothing at all (any path it prints is a file to
format); `go vet` is silent; the build succeeds; the full test run passes with
the race detector on.

- [ ] **Step 7: Commit**

`git commit -m "Add the end-to-end rebuild lifecycle test"`
