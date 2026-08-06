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
   *decode* errors do carry a path and a `line N` (`load.go`, via yaml.v3);
   the gap is semantic validation specifically.
2. **`config` cannot reach a file that `doctor` can.** `buildEnvelope`
   resolves the worktree through git before loading configuration, so a
   workspace whose worktree has moved fails with *unknown workspace*
   (exit 4) and never receives a configuration verdict. Doctor's
   `configuration` check loads by slug with no resolution and reaches it.
3. **Doctor discards the detail it was given.** `oneLine(err.Error())`
   flattens `InvalidConfigError`'s deliberate multi-problem list into a
   single line, undoing the "report every problem, not only the first"
   decision recorded in `config.go`.

## 2. Data model

`InvalidConfigError.Problems` becomes `[]Problem`:

```go
// Problem is one validation failure together with where it came from.
type Problem struct {
	// Field is the dotted path the problem is about:
	// "devcontainer.start_timeout", "windows[dev].location". It is empty
	// for a problem no single field owns, such as a duplicated focus.
	Field   string
	Message string
	// Origin is zero when no layer set the field. An absent required key
	// has no file to point at, and inventing one would be a lie.
	Origin Origin
}

// Origin is a location in a configuration layer. File is relative to the
// configuration root so that reports are readable and stable across
// machines; Line is 0 when the position is not known.
type Origin struct {
	File string
	Line int
}
```

Two properties are load-bearing.

**Origin is optional by construction.** `version is required` and
`window "dev" must set exactly one of agent, command, or shell (it sets
none)` describe *absent* keys. There is no node to point at, so `Origin` is
zero and rendering omits the prefix entirely. Printing `:0` would assert a
position that does not exist — the same error as converting uncertainty into
a finding.

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
`windows[dev]` node rather than to a single key, because when two keys are at
fault neither one alone is the answer.

**Fallback ladder** when a field has no node of its own: field node →
enclosing window node → file with no line → unattributed. Each rung is a
weaker but still true statement; no rung invents a position.

`rejectDuplicates` already runs per-layer, so
`window "dev" is defined more than once` gains its file for free.

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
          "file": "workspaces/dev.yaml",
          "line": 12
        }
      ]
    }
  ],
  "summary": {"subjects": 2, "with_problems": 1, "problems": 1}
}
```

`status` is `ok`, `warn` (defaults read alone), `invalid`, or `unknown` (the
subject could not be examined). `file` is `null` and `line` is omitted for an
unattributable problem.

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
- **Brittleness guard.** Assertions are structural — file plus field — with
  line numbers asserted only in a small number of dedicated cases whose
  fixtures carry a comment marking the expected line. Otherwise every
  fixture edit breaks unrelated tests for no reason.
- **`internal/cli`** — `--validate` with a slug, with no argument, with an
  unknown slug, with an unreadable `workspaces/` directory (must report
  unknown, not none), the `--json` shape, and the exit-5 path through
  `reportedError`.
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
