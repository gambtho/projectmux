# Config-Validation UX Slice Design

The validation *rules* already exist in `internal/config` and have grown
through the container slice (numeric window names, `enabled: false` plus
`location: container`). What is missing is the **UX**: a user with a broken
`defaults.yaml` or workspace file is told what is wrong but never where.

This slice adds origin attribution to every validation problem, and a
`projectmux config --validate` mode that can check a workspace file without
a resolvable worktree. It diagnoses only. Nothing here repairs, rewrites, or
migrates configuration.

## 1. The problem being fixed

Three concrete gaps, each confirmed against the current tree:

1. **Semantic problems carry no provenance.** `validate` runs against the
   merged layer (`internal/config/validate.go`), so
   `more than one window sets focus: dev, test` cannot say whether
   `defaults.yaml`, `<slug>.yaml`, or `<slug>.local.yaml` is at fault. YAML
   *decode* errors are better off, but not uniformly: errors raised by yaml.v3
   itself carry both a path and a `line N`, while the multi-document rejection
   `load.go` constructs by hand carries the path only. So the gap is semantic
   validation plus that one hand-built decode message.
2. **`config` cannot reach a file that `doctor` can.** `buildEnvelope`
   resolves the worktree through git before loading configuration, so a
   workspace whose worktree has moved fails with *unknown workspace*
   (exit 4) and never receives a configuration verdict. Doctor's
   `configuration` check loads by slug with no resolution and reaches it.
3. **Doctor discards the detail it was given.** `oneLine(err.Error())`
   flattens `InvalidConfigError`'s deliberate multi-problem list into a
   single line, undoing the "report every problem, not only the first"
   decision recorded in `config.go`.

A fourth gap surfaced during review and is fixed here too: duplicate window
names are undetectable in a defaults-only installation (§3.4). It is a
pre-existing defect rather than a UX one, but it lands squarely in the path
this slice adds.

## 2. Data model

`InvalidConfigError.Problems` becomes `[]Problem`:

```go
// Problem is one validation failure together with every layer position
// that contributed to it.
type Problem struct {
	// Field is the dotted path the problem is primarily about:
	// "devcontainer.start_timeout", "windows[dev].location". It is empty
	// for a problem no single field owns, such as a duplicated focus.
	Field   string
	Message string
	// Origins are the positions the user may have to edit, primary first.
	// It is empty when no layer set the field: an absent required key has
	// no file to point at, and inventing one would be a lie.
	Origins []Origin
}

// Origin is a location in a configuration layer. File is relative to the
// configuration root so that reports are readable and stable across
// machines; Line is 0 when the position is not known.
type Origin struct {
	File string
	Line int
}
```

Four properties are load-bearing.

**Origins is a list, because real failures are cross-field.** Two of the
existing checks are jointly owned: `window "dev" sets location: container but
devcontainer.enabled is false` implicates a window's `location` *and*
`devcontainer.enabled`, potentially in different files; `more than one window
sets focus` implicates every focused window. A single origin would force the
report to name one file and stay silent about the other the user must also
look at.

**Attribution order is deterministic**, so output is stable and diffable:
`Origins[0]` is the position of `Field` — the primary, the one the human
renderer puts in the left column. The remainder follow in layer order
(`defaults.yaml`, then `<slug>.yaml`, then `<slug>.local.yaml`), and within a
file by ascending line. For a problem with no primary field, `Field` is empty
and the whole list is in that same layer-then-line order.

**Origins is optional by construction, but rarely empty.** The rule is that a
position is reported when *something in a file* corresponds to the problem,
even if the offending key itself is absent. `window "dev" must set exactly one
of agent, command, or shell (it sets none)` names no key, but the window it is
about is written down somewhere, so it attributes to that window's node per the
fallback ladder in §3.3.

`Origins` is empty only when nothing exists to point at anywhere — the
motivating case is `version is required and must be 1` on a layer set that
never mentions `version`. Then rendering omits the prefix entirely. Printing
`:0`, or attributing to an arbitrary file, would assert a position that does
not exist — the same error as converting uncertainty into a finding.

**Nothing here reaches `Config`.** The digest covers the normalized `Config`
and is compared against recorded workspace state as drift. Had provenance
entered `Config`, every registered workspace would have re-digested and read
as drifted on first run after upgrade. `Problem` lives beside the config, not
in it.

## 3. Computing origin

### 3.1 Sources

`loadLayer` keeps the document node *beside* the decoded struct, never
inside it:

```go
// Source is one layer file: its decoded form, its path, and its document
// node. The node is kept separately because Layer's fields must stay
// concretely typed; a yaml.Node field would reopen the unknown-field hole
// that keeps a misspelled key from becoming default behavior.
type Source struct {
	Layer Layer
	File  string
	root  *yaml.Node
}
```

`LoadDefaults` returns `Source`; `Load` accepts it. That signature change
ripples to seven call sites (`open`, `attach`, `stop`, `status`,
`autostart`, `config`, `doctor`), most of which pass the value straight
through.

The alternative — having `Load` re-read `DefaultsPath(root)` itself to avoid
the ripple — is rejected. It reads the file twice, and the second read can
differ from the first, so the line reported would not necessarily describe
the content validated.

### 3.2 Merge records origin

```go
// Merged is the accumulated layer plus the file and line that last set
// each field path.
type Merged struct {
	Layer   Layer
	origins map[string]Origin
}
```

`mergeLayers(base Merged, over Source) (Merged, error)` already walks every
field explicitly. Each existing `if over.X != nil` branch also records
`origins["x"] = over.originOf("x")`. Later layers overwrite earlier entries,
so the map always names the layer that actually won — which is the file the
user must edit.

`originOf` resolves a line through a pure `lineOf(root, path)` walk of the
node tree. `windows[dev]` is resolved by scanning the sequence for the
element whose `name` value is `dev`, matching the whole name exactly, for the
same reason `mergeWindows` does: substring matching would swallow `age` into
`agent-1`.

### 3.3 Two subtleties

**Mode is a merge unit.** `setsMode()` replaces `agent`, `command`, and
`shell` together, so a layer that sets any one of them owns all three. All
three paths therefore record the origin of whichever key was physically
present. A `must set exactly one` problem attributes to the enclosing
`windows[dev]` node rather than to a single key, in **both** of its forms: when
the window sets two modes, neither key alone is the answer; when it sets none,
there is no key at all, and the window is the only thing written down. This is
the rule §2 defers to — a problem about an absent key still has a position when
the thing it is about was written down.

**Fallback ladder** when a field has no node of its own: field node →
enclosing window node → file with no line → unattributed. Each rung is a
weaker but still true statement; no rung invents a position.

Duplicate window names are the one problem this machinery does not serve; §3.4
covers why and what replaces it.

### 3.4 A duplicate-detection hole this slice closes

Duplicate window names are caught only inside `mergeWindows`, which
`ValidateDefaults` never reaches — it calls `validate` directly. On an
installation with no workspace layers, nothing merges, so a `defaults.yaml`
declaring `dev` twice validates clean and `projectmux doctor` reports it
healthy. The defect only surfaces later, when some workspace layer forces a
merge, and it surfaces attributed to that workspace rather than to defaults.

This is a pre-existing bug on `main`, not one this design introduces, but the
`--validate` defaults-alone path would give it a second front door and make
the wrong answer easier to reach. So duplicate detection moves out of
`mergeWindows` into a per-layer check. `mergeWindows` keeps merging and stops
policing.

**Where the check is invoked is load-bearing, not an implementation detail.**
It is called by `Load` and by `ValidateDefaults`. It is **not** called by
`loadLayer` or `LoadDefaults`, and the distinction decides whether doctor keeps
working:

- `LoadDefaults` returning an error routes doctor into its `DefaultsErr` branch,
  which reports `configuration` as an outright **fail** and abandons the rest of
  the check — "without a defaults layer no workspace can be merged, so there is
  nothing further to diagnose."
- `ValidateDefaults` returning problems routes doctor into `defaultsItem`, which
  reports a **warn** and continues to every workspace.

A duplicated window name in `defaults.yaml` belongs in the second branch. It is
a real defect, but the file parsed fine and every other workspace remains
diagnosable; failing the whole check would hide them. Putting the check in
`loadLayer` would silently convert that warning into a fatal error and is the
mistake to avoid.

**Existing error ordering is preserved.** Today `Load` decodes an overlay before
`mergeWindows` inspects the base for duplicates, so a defaults file with a
duplicate *and* an overlay that fails to decode reports the decode failure. The
new check runs at the same point in `Load` — after the layers are read, not
during — so that ordering does not change and no existing test needs rewriting
to accommodate it.

The distinction it encodes is unchanged and still correct: repetition *within*
one file is always a mistake; repetition *across* files is the merge working
as intended.

**A duplicate reports both definition lines**, which the ordinary name-based
lookup cannot supply: `lineOf` resolves `windows[dev]` to the *first* sequence
element whose name matches, and for this problem every colliding entry is
`windows[dev]`. Reporting one of the two would send the user to a line that
looks perfectly correct in isolation — the duplicate is only visible as a pair.

So the duplicate check does not go through `lineOf`. It scans the layer's window
sequence directly, collecting every element that carries the repeated name, and
emits them as `Origins` in ascending line order within the one file. `Field` is
`windows[dev]` and `Origins[0]` is the first definition, keeping the general
ordering rule from §2 intact.

## 4. Command surface

```
projectmux config --validate [--json] [--compact] [<slug>]
```

**The argument is a slug, not a name to resolve.** `config <workspace>`
resolves through git; `--validate` deliberately does not, because the moved-
or missing-worktree case is precisely what this mode exists to serve. Given
no matching file it says so and lists the slugs it did find, rather than
reporting an unknown workspace.

With a slug: validate `defaults.yaml` merged with `workspaces/<slug>.yaml`
and `workspaces/<slug>.local.yaml`.

**The slug must be contained before it becomes a path.** `WorkspacePath` and
`LocalPath` join their argument straight into the configuration root. Every
existing caller obtains the slug from `resolve.slugFor`, so it is git-derived
and inherently a single component; `--validate` is the first path that takes
it from the command line. `resolve.byName` already guards the equivalent case,
rejecting anything that is not exactly `filepath.Base(name)` or that carries a
glob metacharacter, and states why: without it a separator escapes the
configured roots.

`--validate` applies the same rule and then one stronger one: the argument
must be an exact member of `config.Slugs(root)`. Membership is the real
contract — the mode validates workspace files that exist — so
`--validate ../../etc/x` reports an unknown slug rather than reading outside
the configuration root.

**The guarantee is lexical, and the spec claims no more than that.** Membership
constrains the *name*, not the bytes behind it: discovery accepts any
non-directory `*.yaml` entry, and a symlink is a non-directory entry, so
`workspaces/evil.yaml` pointing at `/etc/passwd` is a legitimate member of
`Slugs` and `os.ReadFile` will follow it. Resolving and containing every layer
path before reading is deliberately **not** in this slice: it requires write
access to the configuration directory to exploit, and anyone with that access
can simply write a real file there. What this rule buys is that a hostile
*argument* cannot escape — which is the part `--validate` newly exposes, since
every existing caller gets its slug from git via `resolve.slugFor`. Tests assert
the argument guard, not a symlink guarantee.

Without: validate `defaults.yaml` alone through `ValidateDefaults`, then
every discovered slug. Defaults read alone stay a *warning* rather than a
failure, unchanged from doctor's existing treatment: defaults are the bottom
layer and a workspace layer may legitimately supply what they omit.

Enumeration reuses the tri-state discipline: an unreadable `workspaces/`
directory reports **unknown**, never "no workspaces". Only an absent
directory means none.

Human output:

```
defaults.yaml          ok
workspaces/api.yaml    ok
workspaces/dev.yaml    2 problems
  dev.yaml:12          window "dev" sets location: container but
                       devcontainer.enabled is false (defaults.yaml:4)
  dev.local.yaml:3     more than one window sets focus: dev
                       (dev.yaml:9), test

2 problems in 1 of 2 workspaces
```

As with every other command, human layout is not a compatibility contract;
automation uses `--json`.

## 5. Output contract and exit codes

Problems found is a *failure* whose report **is** the output — the same shape
as autostart's batch and a partially succeeding stop, and covered by the
existing documented exception in `cli.go`. The full report goes to stdout, a
one-line summary to stderr, and the exit code stays `5`
(`ExitInvalidConfig`).

Reaching exit 5 through that path requires one change. `exitCode`
classifies with `errors.As`, and `reportedError` is currently a bare struct
with no `Unwrap`, so it falls through to `ExitError`. It gains one:

```go
// reportedError marks a failure whose full detail already went to stdout
// as the command's structured report. It wraps the underlying error so
// that exit-code classification still sees the real cause: the exit code
// is a property of what went wrong, not of which command reported it.
type reportedError struct {
	msg string
	err error
}

func (e *reportedError) Error() string { return e.msg }
func (e *reportedError) Unwrap() error { return e.err }
```

Existing construction sites pass `err: nil` and keep today's `ExitError`
behavior exactly. Adding a command-keyed case to `exitCode` instead was
rejected: it would make the exit-code table depend on which command ran
rather than on what failed.

Exit `0` when nothing is wrong. Exit `2` for usage errors.

**A requested slug with no configuration file exits `2`.** It is a caller
mistake — you named a workspace that does not exist — and `usageError` is the
established carrier for that. It deliberately does **not** exit `4`:
`ExitUnknownWorkspace` is the resolver's answer about worktrees, and
`--validate` never consults the resolver. Reusing `4` would imply a git lookup
happened. The message lists the slugs that were found, so the correction is
immediate.

**A warnings-only run exits `0`.** Warnings arise from `defaults.yaml` read
alone, where an incomplete bottom layer is legitimate and a workspace layer
may supply the rest. Treating that as failure would make
`config --validate && deploy` fail on a correct installation. This matches
doctor, where a `warn` is report content and the command still exits `0`.

A subject reported **unknown** — one that could not be examined at all, such
as an unreadable `workspaces/` directory — exits `1` (`ExitError`), not `5`
and not `0`. It is an I/O failure, not invalid configuration, and it must not
read as success: uncertainty never converts to absence. When a run produces
both invalid subjects and unknown ones, `5` wins, because a confirmed defect
is the more actionable answer and the report carries both.

JSON envelope, additive to `schema_version: 1`, following the same
convention as the doctor envelope — each command owns its own top-level
shape under the shared version:

```json
{
  "schema_version": 1,
  "config_root": "/home/u/.config/projectmux",
  "results": [
    {"subject": "defaults.yaml", "status": "ok", "problems": []},
    {
      "subject": "dev",
      "status": "invalid",
      "problems": [
        {
          "field": "windows[dev].location",
          "message": "window \"dev\" sets location: container but devcontainer.enabled is false",
          "origins": [
            {"file": "workspaces/dev.yaml", "line": 12},
            {"file": "defaults.yaml", "line": 4}
          ]
        }
      ]
    }
  ],
  "summary": {"subjects": 2, "with_problems": 1, "problems": 1}
}
```

`status` is `ok`, `warn` (defaults read alone), `invalid`, or `unknown` (the
subject could not be examined). `origins` is always present and is `[]` for an
unattributable problem; `line` is omitted within an origin whose position is
not known.

## 6. Relationship to doctor

`workspaceSlugs` **moves** from `internal/doctor` to `internal/config` as an
exported `config.Slugs(root)`; doctor calls it. It encodes the rule that only
an absent directory means none, and a second copy of that rule is exactly the
`belongsTo` duplication this project already owes a fix for. Move it, do not
copy it.

Doctor keeps the `configuration` check and its whole-installation altitude.
`oneLine` flattening is removed by design rather than by truncation: each
slug's `Item.Detail` becomes the problem count plus the first problem *with
its provenance*, and points at the focused command.

```
configuration        fail
  defaults           ok
  api                ok
  dev                fail   2 problems, first at workspaces/dev.yaml:12;
                            run: projectmux config --validate dev
```

The division is: doctor answers *is anything wrong across this
installation*; `--validate` answers *what exactly, and where*. Both call the
same `config.Load`, so they cannot disagree.

## 7. Testing

- **`internal/config`** — table tests for `lineOf` path resolution, covering
  `windows[name]` lookup, the mode-unit rule, and every rung of the fallback
  ladder. Merge-origin tests assert which of three layers is credited for
  each field, including a field set by all three.
- **Multi-origin ordering** — the two cross-field checks (container/enabled,
  duplicated focus) assert the full `Origins` list, that `Origins[0]` is the
  position of `Field`, and that the remainder are in layer-then-line order.
  Ordering is asserted explicitly because unstable output would churn diffs.
- **The duplicate-in-defaults hole** gets a regression test that fails on
  today's `main`: a `defaults.yaml` naming one window twice, with no workspace
  layer present, must be rejected by `ValidateDefaults`. Two further cases pin
  the lifecycle from §3.4: doctor reports it as a **warn** and still diagnoses
  every workspace (never the fatal `DefaultsErr` branch), and a duplicate in
  defaults alongside an undecodable overlay still reports the decode failure
  first, proving error ordering is unchanged. Both `Origins` entries are
  asserted, in ascending line order.
- **Slug containment** — `--validate` with a separator, a `..` component, and
  a glob metacharacter each report an unknown slug. The assertion is that the
  argument is rejected, not that no file outside the root is reachable: per §4
  the guarantee is lexical, and a symlinked layer file is out of scope.
- **Brittleness guard.** Assertions are structural — file plus field — with
  line numbers asserted only in a small number of dedicated cases whose
  fixtures carry a comment marking the expected line. Otherwise every
  fixture edit breaks unrelated tests for no reason.
- **`internal/cli`** — `--validate` with a slug, with no argument, with an
  unknown slug, with an unreadable `workspaces/` directory (must report
  unknown, not none), the `--json` shape, and the exit-5 path through
  `reportedError`. Each documented exit code gets a case: `0` clean, `0`
  warnings-only, `2` unknown slug, `1` unknown subject, `5` invalid, and `5`
  winning over `1` when a run produces both.
- **`internal/doctor`** — existing tests updated for the new `Detail` shape
  and for `config.Slugs`.

**Secrets rule held, not widened.** `<slug>.local.yaml` is the documented
home for secrets (design.md §6). Messages keep quoting values only for the
path-shaped fields that already do so (`checkContained`), and never for
`environment` values. A `--validate` report is far likelier to land in a CI
log than a one-off error was, so this constraint tightens in practice even
though the rule is unchanged.

## 8. Exclusions

- **Layer attribution in normal `config` output** — showing which file
  supplied each *effective value*. Adjacent and tempting; a different
  feature.
- **Any repair or auto-fix.** Validation diagnoses, exactly as doctor does.
  Configuration rewriting is not deferred-and-planned, it is out of scope.
- **`belongsTo` unification** — still owed, but to the next slice that
  touches `internal/controller`. This slice does not, so it stays deferred
  rather than being smuggled in alongside unrelated work.
- **Schema documentation generation** — belongs to the docs/packaging slice.
