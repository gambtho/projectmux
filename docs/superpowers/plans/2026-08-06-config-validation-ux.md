# Config-Validation UX Slice — Implementation Plan

**Status: the code is authoritative.** This plan was written after the fact
from the verified implementation, in the workflow established in the container
slice (#8). Where the design spec
(`../specs/2026-08-06-config-validation-ux-design.md`) and the code disagree,
the code is right and the difference is recorded in §7.

Branch `feat/config-validation-ux`, eleven implementation commits on
`1913256`. Every commit left `go build`, `go vet`, and the full test suite
green; the final state passes every CI gate including `go test -race`.

## 1. What shipped

A user with a broken configuration is now told what is wrong **and where**:

```
defaults.yaml  ok
api            ok
dev            3 problems (invalid)

dev:
  workspaces/dev.yaml:4: window "dev" sets location: container but
                         devcontainer.enabled is false (also defaults.yaml:3)
  workspaces/dev.yaml:5: window "logs" must set exactly one of agent,
                         command, or shell: true (it sets none)
  workspaces/dev.yaml:6: window "logs" cwd must not escape the worktree

3 problems in 1 of 3 subjects
```

The same attribution reaches every command that loads configuration, because
it lives in the error type rather than in the new command.

## 2. Build order

Bottom-up, so the tree compiled and the suite passed at every commit.

| # | Commit | What it established |
|---|---|---|
| 1 | `8b4717f` | `lineOf` — resolve a dotted field path to a line |
| 2 | `00f4fb2` | `Source` — carry each layer's document node |
| 3 | `24d43ba` | `Merged` — record which layer set each field |
| 4 | `978a995` | `Problem`/`Origin` — attributed validation |
| 5 | `6f568ef` | Per-layer duplicate detection |
| 6 | `d7acb4f` | `config.Slugs` moved out of doctor |
| 7 | `3b07eec` | `reportedError` unwrapping for exit codes |
| 8 | `7d1b407` | `config --validate` |
| 9 | `91cbd86` | Doctor's summarized detail |
| 10 | `69d5e85` | Polish: removed duplicated rendering |
| 11 | `43063fa` | Polish: clone attributions on merge |

Steps 7 and 8 are inverted relative to the spec's ordering: `--validate`
cannot return exit 5 until `reportedError` unwraps, so the plumbing landed
first.

## 3. Where each piece lives

- `internal/config/origin.go` — `Origin`, `Merged`, the `problem` builder with
  the fallback ladder and ordering rule, `lineOf` and its node walk.
- `internal/config/duplicate.go` — the per-layer duplicate check.
- `internal/config/slugs.go` — workspace discovery, moved from doctor.
- `internal/config/validate.go` — validation, now emitting `Problem`.
- `internal/cli/validate.go` — the `--validate` mode, its human and JSON
  renderers, and the exit-code decision.
- `internal/doctor/configuration.go` — `summarizeProblems`, replacing the
  `oneLine` flattening.

## 4. Verification actually performed

Every CI gate, run on the final tree:

- `gofmt -l .` — clean.
- `go vet ./...` — clean.
- `go test -race ./...` — all eleven packages pass.
- `go mod tidy` — no diff in `go.mod`/`go.sum`.
- `go build ./cmd/projectmux` — ok.

New tests, all written before the code that satisfied them:

- **`lineOf`** — path resolution, absent paths, whole-name window matching,
  empty and nil documents, and a dotted window name.
- **Merge origins** — which of three layers is credited per field, window
  fields credited independently, mode credited as a unit, environment
  credited per variable, unset fields left unattributed, and the base left
  unmutated.
- **Problems** — field and position, both sides of a cross-field conflict,
  every focused window in layer order, absent-key attribution to the
  enclosing window, no origin for a key no layer set, and the whole rendered
  report.
- **Duplicates** — the defaults-only regression, `LoadDefaults` still
  succeeding, rejection in either layer, cross-layer names still legal, and
  decode-before-duplicate ordering.
- **`Slugs`** — listing, absent directory, unreadable directory, sorting.
- **`--validate`** — clean install, problems on stdout with exit 5, unknown
  slug, five rejected non-slug arguments, bulk mode, warnings-only, unreadable
  directory, invalid outranking unknown, and the JSON shape.
- **Doctor** — position and pointer in the detail, singular phrasing, and the
  duplicate-in-defaults warning path.

## 5. The regression proof

The duplicate-detection hole was verified against `main` rather than asserted:

```
git show main:internal/config/validate.go   # ValidateDefaults calls validate directly
git show main:internal/config/merge.go      # rejectDuplicates only inside mergeWindows
```

`ValidateDefaults` on `main` contains no reference to the merge, so a
`defaults.yaml` naming one window twice validated clean and doctor called the
installation healthy.

## 6. What contact with the code changed

Five things the two review rounds did not predict.

**The decode happens twice, and must.** The plan was one pass: decode into a
`yaml.Node`, then `Node.Decode` into `Layer`. `Node.Decode` does not honor
`KnownFields`, so that would have traded unknown-field rejection for line
numbers. Positions now come from a separate best-effort pass; if it ever
fails, line numbers are lost and validation strictness is not.

**The mode unit needed a shared position, not three lookups.** Crediting
`agent`/`command`/`shell` to the same file still left two of them at line 0,
because those keys appear nowhere in the file. They now share the position of
whichever key the layer physically wrote.

**Secondary origins had to reach the human text.** The first implementation
rendered `Origins[0]` only. For a cross-field conflict that points at a line
which is correct in isolation — `location: container` is only wrong because
another file disabled containers. Hence the `(also defaults.yaml:3)` suffix.

**The signature ripple was smaller than estimated.** The spec predicted seven
call sites; five lines changed, all `defaults.RepositoryRoots` →
`defaults.Layer.RepositoryRoots`. Most callers hand the value straight to
`Load` and never look inside.

**A defaults duplicate fails every workspace, and that is correct.** The first
doctor test asserted an unrelated workspace stayed `ok`. It does not, because
`defaults.yaml` is part of every workspace's configuration and opening any of
them would refuse. The test was corrected to assert what matters instead: each
workspace is still examined, and the position named is the defaults file, so
one edit fixes all of them.

## 7. Deviations from the spec

- **JSON discriminator dropped.** The spec's first draft added
  `"report": "config-validation"`. Doctor's envelope sets the convention —
  each command owns its top-level shape under a shared `schema_version` — so
  the discriminator was removed before implementation.
- **`ValidateDefaults` takes a `Source`,** not a `Layer`, so it can attribute
  what it finds.
- **`config.ProblemsOf` is exported.** Both the CLI and doctor need to turn an
  arbitrary error into problems; leaving it unexported would have meant each
  caller choosing between structured detail and error text.
- **`summarizeProblems`, not `summarize`,** in doctor — `dependencies.go`
  already had a `summarize`.

## 8. Deferred, deliberately

- **`belongsTo` unification** — still owed to the next slice touching
  `internal/controller`. This slice does not touch it.
- **Symlinked layer files.** `--validate`'s containment guarantee is lexical:
  it stops a hostile argument, not a `workspaces/evil.yaml` symlinked outside
  the root. Exploiting that needs write access to the configuration
  directory, where a real file would serve equally well. Recorded in the spec
  §4 rather than silently assumed.
- **Layer attribution in ordinary `config` output** — showing which file
  supplied each effective value. Adjacent, and a different feature.
- **Repair of any kind.** Validation diagnoses, exactly as doctor does.

## 9. Where reviewers should look

1. `internal/config/origin.go` — the `problem` builder and the fallback
   ladder carry the design's core rule. The ordering guarantee
   (`Origins[0]` is the field's own position, the rest by layer then line) is
   what makes reports stable and diffable.
2. `internal/config/merge.go` — every merge branch must credit what it took.
   A merge rule that drifts from its attribution produces reports naming the
   wrong file, which is worse than no report.
3. `internal/cli/validate.go` — the exit-code decision in
   `validationOutcome`, and that `invalid` outranks `unknown`.
4. `internal/config/load.go` — layers are all read before anything is checked,
   which is what preserves decode-before-semantic error ordering.
