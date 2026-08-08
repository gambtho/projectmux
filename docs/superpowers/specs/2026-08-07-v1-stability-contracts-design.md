# v1.0 Stability Contracts Design

`docs/design.md` §12 named two pre-1.0 gates — linting and reproducible builds
— and both are closed as of `v0.4.0`. §12 never said what 1.0 *means*, only
what had to pass before it. The README says it implicitly instead: the project
is alpha, "the contracts are not yet stable," there is no migration support
below 1.0, and two things are compatibility contracts even during the alpha.

This spec settles the question §12 left open. The answer is not the expected
one.

**ProjectMux stays below 1.0, and nothing is a compatibility contract until it
gets there.** The README's two advertised contracts — the `--json` envelope
and the exit codes — are retracted rather than extended.

## 1. Why this is not "freeze the list README already has"

The obvious move was to promote README's existing list (`--json` output, exit
codes, command surface) into 1.0 promises and add the config schema. Three
findings ruled it out.

**There are no external users** — an assumption, recorded with its basis in §2.
That is not an argument for freezing cheaply.
It is the opposite: with no users, the project has the *least* evidence about
which contracts are the right shape. Freezing is a commitment made at the
moment of minimum information, and every wrong guess becomes a 2.0.

**The advertised contract is enforced for one of the ten `--json` shapes.**
There is no `testdata/` under `internal/` and no golden file anywhere in the
repository. `TestConfigJSONEnvelopeIsVersionedAndComplete`
(`internal/cli/cli_test.go:192-210`) is the exception: it asserts `config
--json`'s exact top-level key set *and its count*, so a field added, removed,
or renamed there fails a test. Nothing equivalent exists for the other nine
shapes.

The gap is narrower than "no tests" but real. The remaining commands are
covered by tests that read the fields they care about, which catches a field
disappearing *if some test happens to read it* and misses a shape no test was
written to look for. Decoding into the production envelope types does not close
this, either: retagging a field moves the assertion along with the wire format,
so a rename passes silently. Only a pinned key set — or a golden file — catches
that, and only `config` has one. Nine-tenths of `--json` was a promise backed
by prose.

**§12 already reasoned its way to the edge of this.** Its closing amendment
states that closing the two CI gates "was release engineering, not a
compatibility commitment," and refuses a 1.0 that would mean "our CI got
better," on the ground that such a number "teaches readers to distrust the
version number." A 1.0 justified by *nothing being at stake* is the same
hollow number. This spec completes that argument rather than contradicting it.

## 2. What is being retracted, and from where

`README.md` currently states, in the alpha admonition and again under Project
status:

> Two things are compatibility contracts even during the alpha:
>
> - The **JSON output** of the reporting commands, which carries a
>   `schema_version` field.
> - The **exit codes** listed below.

That claim was introduced deliberately, by
`docs/superpowers/specs/2026-08-06-docs-packaging-design.md` §2, which
preserved it "verbatim in substance" while rewriting the status section around
it. This spec supersedes that decision.

**It appears in a second place.** `docs/commands.md:33-34` makes the same
promise in its own words — "The JSON envelopes and the exit codes are
compatibility contracts. Human-readable output is not" — and is the document a
reader consulting the command reference actually lands on. Retracting only in
the README would leave the promise standing where automation authors are most
likely to read it.

**This is a retraction of a published promise.** `v0.1.0` through `v0.4.0`
shipped asserting both contracts. The spec records it as a retraction rather
than pretending the promise was never made. Anyone reading the tags later
should find the reversal documented, not inferred.

**"No external users" is an assumption, not a verified fact.** Its basis is the
maintainer's statement in the design conversation. It is not checkable from the
repository: release artifacts are public and download counts were not consulted,
so someone may be running `v0.4.0` unobserved. The assumption is what makes the
retraction cheap, so it is recorded here as load-bearing. If it turns out to be
wrong, the retraction is still the right call — a promise the tests do not back
is worse for a real user than an honest withdrawal — but the cost is no longer
zero, and §10 carries that risk rather than dismissing it.

## 3. Findings behind the retraction

These are recorded so they are not rediscovered. Each is a reason the
contracts were not ready to be frozen, and most become work items when 1.0 is
actually approached.

**3.1 One `schema_version` spans ten independent shapes.**
`cli.OutputSchemaVersion = 1` (`internal/cli/config.go:23`) is embedded in ten
unrelated envelopes: `open`, `attach`, `stop`, `autostart`, `config`,
`config --validate`, `list`, `status`, `doctor`, `rebuild`.
`internal/cli/validate.go:24-25` states the convention — "each owns its own
top-level shape under the shared version." The consequence was never worked
out. If `status`'s payload breaks, bumping the constant signals a break to
`list` consumers who saw none; not bumping it means the number tracks nothing.
The constant has never moved, so the ambiguity is untested. This is the part
of the promise most likely to fail first.

**3.2 Nine of the ten shapes are unpinned.** `config --json`'s key set and
count are pinned (§1); no other command's are. The versioning rule in 3.1
cannot be enforced by a rule that nine of its ten subjects can violate
undetected.

**3.3 `config --json` embeds `config.Config` whole**
(`internal/cli/config.go:49`). The type's own doc comment said, before this
change, that its JSON encoding "is the digested document and the `config`
member of the versioned CLI envelope, so field order and names are a public
contract" (`internal/config/config.go:23-25`). The coupling this creates runs
from `config.Config` to *two* consumers at once — the CLI envelope and the
digest — so neither can be changed without deciding for the other.

It does **not** weld the YAML input schema to the JSON output. Those are
separate structs with separate tag sets: `Layer` carries the `yaml` tags
(`internal/config/layer.go:19-28`), `Config` carries the `json` tags
(`internal/config/config.go:23-32`), and `normalize` maps between them. A YAML
key can be renamed without touching the JSON wire name, and vice versa. The
constraint is real but narrower than "one schema serving two masters."

**3.4 `omitempty` is applied inconsistently to meaningful fields.**
`container_windows_stale` disappears when false; `container`, `stored`,
`last_operation`, and `error` disappear when absent. Meanwhile
`internal/cli/validate.go:46-48` deliberately makes `Origins` always present
and empty "so a consumer can iterate it without a nil check." That is a real
contract-quality rule, applied in one file and nowhere else. Absent-means-false
is a defensible convention; it is currently unstated.

**3.5 The exit-code table describes code 2 inaccurately.** `internal/cli/cli.go:157-164`
routes an unrecognized *bare* token to `open` as a workspace name, so
`projectmux opne` exits **4** (no worktree matched), not 2. The code calls this
"the documented trade." README's table presents 2 as the usage-error code
without that caveat, so a consumer branching on 2 for "the user typed a command
wrong" will not see it in that case.

Code 2 is otherwise ordinary and well covered. A flag-shaped token reaches
`usagef` and exits 2 — `TestUnknownCommandIsAUsageError`
(`internal/cli/cli_test.go:96-105`) exercises exactly that, and its own comment
names the split. Bad flags and bad arguments to real commands take the same
path. **This is a documentation-accuracy finding, not a reachability defect**,
and it is the correction of an earlier draft that called the code "close to
unreachable."

**3.6 The exit-code set may be incomplete.** A missing dependency — `tmux` not
on `PATH` — falls through `exitCode`'s default to `ExitError` (1),
indistinguishable from an I/O failure. Automation would reasonably treat
"install tmux" and "something broke" differently.

## 4. The exit codes are a deliberate, asymmetric cost

The exit codes are not in the same position as `--json`, and the spec should
not imply they failed for the same reason. They are genuinely enforced: **100
lines across `internal/cli`'s tests reference the seven exit-code constants
(101 occurrences; `wiring_test.go:222` carries two), and all
seven are exercised** (`ExitOK` through `ExitRefused`, the least-referenced
being `ExitAmbiguous`). `internal/cli/exit_test.go` covers
the subtle case directly — a reporting wrapper classifies on its wrapped
cause, not on which command produced it. `internal/cli/cli.go:22-23` said,
before this change, that they were "part of the command contract."

They are withdrawn anyway, **for consistency of message alone**: a README that
says "nothing is frozen below 1.0, except this one thing" invites exactly the
selective reading the retraction exists to prevent. A reader who has to hold
two rules cannot be relied on to apply the right one.

An earlier draft also leaned on 3.5 and 3.6 — that the set was not obviously
final. 3.6 still stands and is a genuine loose end. 3.5 does not: review
established that code 2 is reachable and tested, so it argues for fixing a
sentence in the README's table, not against the set. That evidence is
withdrawn from this argument, and the decision is unchanged without it —
consistency was always the reason that carried it.

**This is the one place the decision goes further than the evidence demands**,
and with 3.5 removed it goes further still. It is recorded as a cost, not as a
finding.

The tests do not change. They continue to pin the behavior; the README simply
stops promising it to third parties.

## 5. Changes

**5.1 `README.md`.** The alpha admonition and the Project status section stop
naming contracts. They state plainly that nothing is a compatibility contract
below 1.0, that `--json` output and exit codes may both change, and that
human-readable output remains the least stable of the three. The existing
exit-code table stays — it is accurate documentation of current behavior, and
documenting is not promising. Finding 3.5's caveat is worth adding to the
table on accuracy grounds alone, independent of any contract.

**5.2 `docs/commands.md`.** The "What is a contract" paragraph
(`docs/commands.md:33-34`) asserts both contracts in its own words and must be
retracted with the README, not after it. This is the reference automation
authors actually read; a retraction that misses it leaves the promise standing
in the more consequential of the two places. The relative claim — human output
is less stable, parse `--json` — is kept; only the contract language goes.

**5.3 `docs/design.md` §12.** A new amendment records the retraction, its
reasoning, and the findings in §3, superseding the "two things are contracts"
framing and closing the question the section left open.

**5.4 No functional code changes — but twelve doc comments assert or rely on the
retracted contract, and become false the moment the README changes.** This was
missed when the change was first scoped as "two documentation files," and it is
the reason the diff is larger than it looks:

- Three comments *assert* a contract directly. `internal/cli/config.go:19-21`
  says of `OutputSchemaVersion`: "Human output is not a compatibility contract;
  this is." `internal/cli/cli.go:22-23` says the exit codes "are part of the
  command contract." `internal/config/config.go:23-25` says `Config`'s field
  order and names "are a public contract" — cited in 3.3 as evidence and
  omitted from an earlier version of this list, though it is the same claim.
- Six comments *redirect automation to* the contract, on the pattern "this
  layout is not a compatibility contract; automation should use `--json`" —
  `config.go:158`, `doctor.go:134`, `rebuild.go:142`, `list.go:169`,
  `validate.go:229`, `status.go:252`. These are the more damaging ones: they
  point automation at a guarantee that no longer exists.
- Two name a narrower contract. `internal/cli/validate.go:13` calls the
  validation status strings "the JSON contract"; `internal/rebuild/classify.go:45`
  says a conflict reason is not "any output contract."
- One is the package doc. `cmd/projectmux/main.go:9-11` carries the alpha
  notice, which says human-readable output "is deliberately not a
  compatibility contract" — the same claim in the binary's own documentation.

These are corrected as comment-only edits. No statement, expression, or test
changes; `go build` and `go test` are expected to be unaffected, which §9
verifies rather than assumes.
The relative claim in the six redirects survives the retraction and should be
preserved, not deleted — human-readable layout really is less stable than
`--json`, and that was true before either was promised to anyone. Only the
word "contract" has to go.

**5.5 No release-workflow change.** Verified rather than assumed:
`.github/workflows/release.yml:265-273` selects `--prerelease` from a `case`
on the tag, matching `v0.*`. Staying on `v0.x` therefore requires no edit, and
a future `v1.0.0` would publish as a full release with no edit either. §12's
amendment already reached this conclusion for a different reason; it holds for
this one too.

## 6. What 1.0 would require

Recorded so the decision is not re-litigated from scratch. None of this is
scheduled.

- **Users, or a survived schema change.** Either external users whose
  breakage would carry real cost, or evidence from a real config-schema change
  that the schema's shape holds up.
- **Golden tests pinning every `--json` shape** (3.2). Nine of the ten need
  what `config` already has. The prerequisite for promising any of them.
- **A decided `schema_version` rule** (3.1) — per-command versions, one number
  bumping globally, or a number that versions only the envelope convention and
  shared sub-objects.
- **A config upgrade path.** `internal/config` has the version *gate* —
  `config.SchemaVersion = 1`, and `validate` rejects both an absent and an
  unsupported version (`internal/config/validate.go:50-58`) — but no upgrade
  path. `internal/state` has both: embedded forward migrations plus
  `FutureSchemaError`'s refuse-never-repair rule. Config at version 2 is
  currently a hard stop with nowhere to go.
- **A resolved exit-code set** (3.6, plus the README table correction in 3.5).
- **A deprecation policy**, which only becomes meaningful once something is
  promised.

## 7. Non-goals

Each of these is 1.0 work. Doing any of it now is building for users who do
not exist.

- No config migration machinery.
- No golden tests.
- No exit-code changes — neither the missing dependency code (3.6) nor the
  code-2 dispatch trade (3.5). 3.5's README-table correction is documentation
  and is in scope (§5.1); changing the dispatch itself is not.
- No `schema_version` redesign.
- No `omitempty` normalization (3.4).
- No change to `contrib/`, the systemd unit, or the install instructions.

## 8. A note on the missing-`version` question

The brief flagged that `projectmux config --validate` does not flag a missing
`version` key, as potentially relevant if versioning became the freezing
mechanism. Inspection shows this is narrower than it appears and is not a
defect.

`ValidateDefaults` injects the supported version when `defaults.yaml` omits it
(`internal/config/validate.go:24-42`), for a documented reason: defaults are
legitimately incomplete, since a workspace layer supplies the rest. Every other
path runs `config.Load` → `validate`, which *does* report a missing version
(`internal/config/validate.go:50-58`).

The requirement lands on the **merged** configuration, not on each file.
`version` inherits like any other key — `mergeLayers` takes it from a later
layer only when that layer states it, and otherwise leaves the base value in
place (`internal/config/merge.go:9-10, 30-33`). So a workspace file need not
declare `version: 1`; a `defaults.yaml` that declares it satisfies the check
for every workspace beneath it, which is what
`TestLoadAcceptsADefaultsSource` (`internal/config/source_test.go:62-66`)
exercises — `version: 1` in defaults, none in either workspace layer, load
succeeds. An earlier draft of this section stated the opposite, that each
workspace file is required to declare it.

What remains true is the shape of the gap: an installation whose defaults omit
`version` and whose workspace files omit it too is rejected at load, while
`config --validate` reading defaults alone accepts it. The leniency is confined
to the single-layer defaults check and is intentional there.

Nothing here needs to change. It is recorded because the asymmetry is easy to
misread as a hole in config validation, and because the real config gap is the
missing upgrade path (§6), not the version check.

## 9. Verification

The change alters documentation and code comments only — no executable line
moves — so verification is correspondingly narrow:

- `go build ./...` and `go test ./...` still pass, establishing that the
  comment edits in §5.4 touched nothing that runs. A clean baseline was
  recorded before any edit (all 12 packages passing).
- Every file, line, and quotation cited in this spec and in the resulting
  README and design amendments is re-checked against the tree at write time.
  The findings in §3 are the spec's substance; a stale line number in a
  document *about* stating things accurately would be its own defect.
- `grep -rn "contract"` over `README.md`, `docs/commands.md`, and the Go
  sources confirms no remaining assertion of a compatibility contract. The
  §5.4 list is that search's current result — `README.md:89`, `:100`, `:105`
  and `docs/commands.md:35` on the documentation side — so re-running
  the same search at the end is the completeness check. `docs/commands.md` was
  missed by the original scoping and surfaced by independent review; the grep
  is what makes a third such miss detectable instead of hoped-against.

## 10. Risks

**Retracting a published promise** (§2). The cost is near zero *if* the
no-external-users assumption holds, and that assumption is the maintainer's
statement rather than a measured fact — public release artifacts were not
checked for downloads. If someone is running `v0.4.0` against the promise, they
lose it without warning. The retraction is still correct in that case, since
the promise was not enforced for nine of the ten shapes it covered; but the
mitigation is documentation, not silence, which is why §2 records the reversal
explicitly instead of quietly rewriting the README.

**A README that promises nothing may read as lower quality than one that
promises little.** Accepted. The alternative is a promise the test suite does
not back, which is worse in the only way that matters later.

**Deferring 1.0 indefinitely.** §6 names the triggers precisely so that "not
yet" does not decay into "never by inertia." The triggering condition is
evidence, not a date.
