# Retract the Advertised Compatibility Contracts — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Withdraw the claim that the JSON envelopes and exit codes are compatibility contracts, from every place the project asserts it, so that nothing is promised below 1.0.

**Architecture:** Four surfaces assert or rely on the retracted claim — `README.md`, `docs/commands.md`, ten Go doc comments, and `docs/design.md` §12. Documentation lands first, then the comments that point at it, then the design amendment that records the decision, then a repository-wide grep that proves nothing was missed. No executable line changes anywhere in this plan.

**Tech Stack:** Go 1.x (comment-only edits), Markdown. Verification is `go build ./...`, `go test ./...`, and `grep`.

**Spec:** `docs/superpowers/specs/2026-08-07-v1-stability-contracts-design.md`

## Global Constraints

- **No executable line changes.** Every Go edit is inside a `//` comment. No statement, expression, signature, constant value, or test may change. `go test ./...` must pass identically before and after.
- **Keep the relative claim; drop only the promise.** Human-readable output really is less stable than `--json`, and that was true before either was promised. Six comments and both docs make this point — rewrite them, do not delete them.
- **`OutputSchemaVersion` stays `1`.** The constant is not removed, renamed, or changed. Only its doc comment changes.
- **The exit-code tables stay.** Documenting current behavior is not promising it. Both tables (`README.md` and `docs/commands.md`) keep every row.
- **This is a documentation change, so it has no unit tests.** Do not invent Go tests for prose. Each task's verification is an exact-string grep plus, for Task 3, the unchanged build and test suite. Adding a test that asserts on comment text would be a defect, not thoroughness.
- **Three `contract` uses in the Go sources are deliberately out of scope** — see Task 5. Do not edit them.

---

### Task 1: Retract the claim in `README.md`

**Files:**
- Modify: `README.md:87-91` (alpha admonition), `README.md:95-110` (Project status), `README.md:299-309` (exit-code table caveat)

**Interfaces:**
- Consumes: nothing.
- Produces: the wording Task 2 mirrors and Task 4 cites. Task 2's `docs/commands.md` paragraph must not contradict this text.

- [ ] **Step 1: Rewrite the alpha admonition**

Replace lines 87-91 exactly:

```markdown
> [!IMPORTANT]
> ProjectMux is alpha. Every documented command works, but the configuration
> schema and the command surface may change before 1.0, and there is no
> migration support for `v0.x` configuration. The versioned JSON envelope and
> the documented exit codes are the compatibility contracts.
```

with:

```markdown
> [!IMPORTANT]
> ProjectMux is alpha. Every documented command works, but **nothing it emits
> is a compatibility contract below 1.0** — the configuration schema, the
> command surface, the JSON envelopes, and the exit codes may all change, and
> there is no migration support for `v0.x` configuration.
```

- [ ] **Step 2: Rewrite the Project status section**

Replace lines 95-110 exactly:

```markdown
**The command surface is complete; the contracts are not yet stable.**
ProjectMux opens, attaches to, stops, and reports on workspaces, starts
containers at boot, and diagnoses its own installation. Every command listed
below works.

Expect breaking changes to the configuration schema and the command surface
while the version remains below 1.0. There is no migration support, and `v0.x`
builds are published as prereleases.

Two things are compatibility contracts even during the alpha:

- The **JSON output** of the reporting commands, which carries a
  `schema_version` field.
- The **exit codes** listed below.

Human-readable output is deliberately *not* a contract. Parse `--json` instead.
```

with:

```markdown
**The command surface is complete; nothing about it is frozen.**
ProjectMux opens, attaches to, stops, and reports on workspaces, starts
containers at boot, and diagnoses its own installation. Every command listed
below works.

**Nothing is a compatibility contract below 1.0.** Expect breaking changes to
the configuration schema, the command surface, the JSON envelopes, and the
exit codes while the version remains below 1.0. There is no migration support,
and `v0.x` builds are published as prereleases.

Releases up to `v0.4.0` named the JSON output and the exit codes as contracts
that held even during the alpha. That claim is withdrawn. It was made before
anything pinned those shapes, and only `config --json` is checked against a
fixed set of keys today; the other nine commands could be renamed by accident
without a test noticing.

Human-readable output remains the least stable of the three, so parse `--json`
if you automate against ProjectMux — but pin the version you tested against.
```

- [ ] **Step 3: Add the code-2 caveat under the exit-code table**

This is spec finding 3.5, in scope as documentation only. Immediately after the exit-code table (the `| 6 | ... |` row at `README.md:309`, before the `## Relationship to tmux and Dev Containers` heading), insert a blank line and:

```markdown
A mistyped *bare* command resolves as a workspace name rather than a usage
error — `projectmux opne` exits 4, not 2. Flag-shaped tokens and bad arguments
to real commands still exit 2.
```

- [ ] **Step 4: Verify no surviving contract *claim* in the README**

Run: `grep -n "contract" README.md`

Expected: exactly two matches, both in the new text, both in the negative —
"nothing it emits is a compatibility contract below 1.0" in the admonition and
"Nothing is a compatibility contract below 1.0" in Project status. The word
still appears; what must be gone is any sentence saying something *is* one.
Read both matches. Three or more matches means an assertion survived.

- [ ] **Step 5: Verify the caveat and the table both landed**

Run: `grep -c "^| [0-6] |" README.md` — expected: `7` (the table is intact).

Run: `grep -n "exits 4, not 2" README.md` — expected: one match.

- [ ] **Step 6: Commit**

```bash
git add README.md
git commit -m "docs: withdraw the compatibility-contract claim from the README

Nothing is frozen below 1.0. The JSON envelopes and exit codes were
promised before anything pinned their shapes; only config --json is
checked against a fixed key set today. The retraction is stated as a
retraction rather than a silent rewrite, because v0.1.0 through v0.4.0
shipped with the promise.

Also documents that a mistyped bare command exits 4, not 2 (design
shorthand trade) — an accuracy fix the table needed independently."
```

---

### Task 2: Retract the claim in `docs/commands.md`

**Files:**
- Modify: `docs/commands.md:33-35`

**Interfaces:**
- Consumes: Task 1's wording — this paragraph must agree with it.
- Produces: nothing later tasks depend on.

This file was missed when the change was first scoped and is the more consequential of the two documents: it is the reference an automation author actually opens.

- [ ] **Step 1: Rewrite the "What is a contract" paragraph**

Replace lines 33-35 exactly:

```markdown
**What is a contract.** The JSON envelopes and the exit codes are
compatibility contracts. Human-readable output is not — its layout may change
in any release. Parse `--json`.
```

with:

```markdown
**What is not frozen.** Nothing here is a compatibility contract below 1.0 —
the JSON envelopes, the exit codes, and the command surface may all change.
Human-readable output is the least stable of them: its layout may change in
any release. Parse `--json`, and pin the version you tested against.
```

- [ ] **Step 2: Verify**

Run: `grep -n "contract" docs/commands.md`

Expected: exactly one match, the phrase "not a compatibility contract below 1.0" on line 33. No line may assert that anything *is* a contract.

- [ ] **Step 3: Confirm the exit-code table and the shorthand note are untouched**

Run: `grep -n "exits 4, not 2" docs/commands.md`

Expected: one match at line 27 — this file already documented the trade before this change. Nothing to add here; Task 1 Step 3 brought the README up to the same standard.

- [ ] **Step 4: Commit**

```bash
git add docs/commands.md
git commit -m "docs: withdraw the contract claim from the command reference

Same retraction as the README. This is the page automation authors
actually read, so leaving it would have kept the promise standing where
it does the most damage."
```

---

### Task 3: Correct the ten Go doc comments

**Files:**
- Modify: `internal/cli/config.go:19-21` and `:157-158`, `internal/cli/cli.go:22-23`, `internal/cli/validate.go:13` and `:228-229`, `internal/cli/list.go:168-169`, `internal/cli/doctor.go:133-135`, `internal/cli/rebuild.go:141-143`, `internal/cli/status.go:251-252`, `internal/config/config.go:23-25`
- Test: none created. `go build ./...` and `go test ./...` are the verification.

**Interfaces:**
- Consumes: Task 1 and Task 2's wording — these comments must not point at a promise the docs no longer make.
- Produces: nothing. No exported name, signature, or value changes.

Line numbers shift as edits land within a file. Match on the exact comment text shown, not on the line number.

- [ ] **Step 1: Rewrite the three comments that assert a contract directly**

`internal/cli/config.go:19-21` — replace:

```go
// OutputSchemaVersion versions the JSON envelope. Human output is not a
// compatibility contract; this is. Bump it only for a breaking change to the
// structure below.
```

with:

```go
// OutputSchemaVersion versions the JSON envelope. Nothing is a compatibility
// contract below 1.0, this included; the field exists so that a break is
// expressible when there is something to break. Bump it only for a breaking
// change to the structure below.
```

`internal/cli/cli.go:22-23` — replace:

```go
// Exit codes. They are part of the command contract: automation branches on
// them, so they must stay stable as commands are added.
```

with:

```go
// Exit codes. Automation branches on them, so they should not churn without a
// reason — but they are not frozen below 1.0. See docs/design.md §12.
```

`internal/config/config.go:23-25` — replace:

```go
// Config is the normalized, validated configuration for one workspace. Its
// JSON encoding is the digested document and the `config` member of the
// versioned CLI envelope, so field order and names are a public contract.
```

with:

```go
// Config is the normalized, validated configuration for one workspace. Its
// JSON encoding is the digested document and the `config` member of the
// versioned CLI envelope, so field order and names are load-bearing in two
// places at once. Neither is frozen below 1.0, but changing one changes both.
```

- [ ] **Step 2: Rewrite the six comments that redirect automation to the contract**

Each keeps its relative claim and loses the word "contract". Every replacement
puts "may change in any release" on a single line, which is what Step 6 counts.
Replace each exactly:

`internal/cli/config.go:157-158`:

```go
// writeHuman renders a readable summary. This layout is explicitly not a
// compatibility contract; automation should use --json.
```
→
```go
// writeHuman renders a readable summary. This layout
// may change in any release; automation should use --json.
```

`internal/cli/list.go:168-169`:

```go
// writeListHuman renders the summary table. This layout is explicitly
// not a compatibility contract; automation should use --json.
```
→
```go
// writeListHuman renders the summary table. This layout
// may change in any release; automation should use --json.
```

`internal/cli/status.go:251-252`:

```go
// writeStatusHuman renders a readable report. This layout is explicitly
// not a compatibility contract; automation should use --json.
```
→
```go
// writeStatusHuman renders a readable report. This layout
// may change in any release; automation should use --json.
```

`internal/cli/doctor.go:133-135`:

```go
// writeDoctorHuman renders one line per check with indented item lines.
// This layout is not a compatibility contract; automation should use
// --json.
```
→
```go
// writeDoctorHuman renders one line per check with indented item lines.
// This layout may change in any release; automation should use --json.
```

`internal/cli/rebuild.go:141-143`:

```go
// writeRebuildHuman renders one line per registration and one per
// conflict. This layout is not a compatibility contract; automation
// should use --json.
```
→
```go
// writeRebuildHuman renders one line per registration and one per
// conflict. This layout may change in any release; automation should
// use --json.
```

`internal/cli/validate.go:228-229`:

```go
// writeValidationText renders the human report. As everywhere else, this
// layout is not a compatibility contract; automation uses --json.
```
→
```go
// writeValidationText renders the human report. As everywhere else, this
// layout may change in any release; automation uses --json.
```

- [ ] **Step 3: Rewrite the one comment naming a narrower contract**

`internal/cli/validate.go:13` — replace:

```go
// Validation statuses. They are the JSON contract, so they are spelled once
// here rather than inline.
```

with:

```go
// Validation statuses. They appear verbatim in the JSON output, so they are
// spelled once here rather than inline.
```

- [ ] **Step 4: Verify nothing executable changed**

Run: `git diff -U0 -- '*.go' | grep -E '^[+-]' | grep -vE '^[+-]{3}' | grep -v '^[+-]\s*//'`

Expected: **no output.** Every added and removed line must be a comment. Any line printed here is an executable change and must be reverted before continuing.

- [ ] **Step 5: Verify the build and the full suite are unaffected**

Run: `go build ./... && go test ./...`

Expected: build succeeds, all 12 packages pass. This is the same result as the pre-change baseline; a failure means Step 4's check missed something.

- [ ] **Step 6: Verify the redirects kept their relative claim**

Run: `grep -rn "may change in any release" internal/cli/ | wc -l`

Expected: `6` — one per redirect. If the count is short, a redirect was deleted
rather than rewritten, or a replacement wrapped the phrase across two lines
(grep is line-based and will not match it). Restore the guidance and the
wrapping shown in Step 2.

- [ ] **Step 7: Commit**

```bash
git add internal/
git commit -m "refactor: stop asserting a compatibility contract in doc comments

Ten comments either claimed the JSON envelope and exit codes were
contracts or pointed automation at that guarantee. Both became false when
the README retracted it. The six redirects keep their real point —
human-readable layout is less stable than --json, which was true before
anything was promised — and lose only the word 'contract'.

Comment-only: go build and go test are unchanged."
```

---

### Task 4: Record the decision in `docs/design.md` §12

**Files:**
- Modify: `docs/design.md` — append a new subsection at the end of §12, immediately before `## 13. Extraction sequence` (currently line 488)

**Interfaces:**
- Consumes: Tasks 1-3, which must already be committed — this amendment describes what landed, and writing it first would make it a prediction.
- Produces: the durable record. Nothing depends on it.

§12 closed its two named gates but never defined what 1.0 means. This closes that question in the negative.

- [ ] **Step 1: Append the amendment**

Insert immediately before the `## 13. Extraction sequence` heading, preserving the blank line that separates sections:

```markdown
### What 1.0 means, and why nothing is promised before it

The section above closed the two named pre-1.0 gates without saying what 1.0
would commit to. The answer is now recorded: **nothing is a compatibility
contract below 1.0**, and the two that were advertised — the versioned JSON
envelope and the exit codes — are withdrawn rather than extended. The full
reasoning is in
`docs/superpowers/specs/2026-08-07-v1-stability-contracts-design.md`; the
durable parts are these.

**The promise was not backed.** One of the ten `--json` shapes is pinned
against a fixed key set (`config`, by
`TestConfigJSONEnvelopeIsVersionedAndComplete`). The other nine are covered
only by tests that read the fields they happen to care about, which cannot
catch a rename — retagging a field moves the assertion along with the wire
format. A promise nine-tenths of which no test can enforce is a promise the
project could break without noticing.

**One `schema_version` spans ten independent shapes.** The constant is shared,
so bumping it for a break in `status` signals a break to `list` consumers who
saw none, and not bumping it makes the number track nothing. It has never
moved, so the ambiguity is untested. This is the part of the envelope promise
most likely to fail first, and it has no decided rule.

**The exit codes were different, and were withdrawn anyway.** They are
genuinely enforced — 101 lines across `internal/cli`'s tests reference the
seven constants and all seven are exercised. They go for consistency of
message alone: a README saying "nothing is frozen below 1.0, except this one
thing" invites exactly the selective reading the retraction exists to prevent.
This is the one place the decision goes further than its evidence demands, and
it is recorded as a cost. The tests do not change; they still pin the
behavior. The project simply stops promising it to third parties.

**The prerelease flag still needs no change**, for the third time and now for
this reason too: it keys on the tag pattern, so `v0.*` publishes as a
prerelease and a future `v1.0.0` as a full release, with no workflow edit
either way.

**What 1.0 would require**, so it is not re-litigated from scratch: users whose
breakage would carry real cost, or a survived config-schema change; golden
tests pinning the nine unpinned shapes; a decided `schema_version` rule; a
config upgrade path (`internal/config` has the version *gate* but no
migrations, where `internal/state` has both); a resolved exit-code set; and a
deprecation policy, which only becomes meaningful once something is promised.
None of it is scheduled. The trigger is evidence, not a date.
```

- [ ] **Step 2: Verify placement**

Run: `grep -n "^## 13\|^### What 1.0 means" docs/design.md`

Expected: the `### What 1.0 means` line number is smaller than the `## 13` line number — the amendment is inside §12, not after it.

- [ ] **Step 3: Verify the design doc asserts no surviving contract**

Run: `grep -n "compatibility contract" docs/design.md`

Expected: one match, inside the amendment, in the sentence stating nothing is one below 1.0.

- [ ] **Step 4: Commit**

```bash
git add docs/design.md
git commit -m "docs: record in design §12 what 1.0 means

§12 closed its two gates without defining 1.0. The answer is negative:
nothing is a contract below it, and the two advertised contracts are
withdrawn. Records why the envelope promise was unbacked, why the exit
codes went anyway despite being enforced, and what 1.0 would require."
```

---

### Task 5: Prove nothing was missed

**Files:** none modified unless the grep finds a survivor.

**Interfaces:**
- Consumes: Tasks 1-4, all committed.
- Produces: the completeness result.

`docs/commands.md` was missed by the original scoping and found by review. This task exists so a third miss is detectable rather than hoped against. It is a separate gate because it is the only step that can invalidate the four before it.

- [ ] **Step 1: Sweep the whole repository**

Run: `grep -rn "compatibility contract" --include='*.md' --include='*.go' . | grep -v docs/superpowers/`

Expected: exactly five matches — two in `README.md` (Task 1), one in
`docs/commands.md` (Task 2), one in `docs/design.md` (Task 4), and one in
`internal/cli/config.go` (Task 3 Step 1). Read every one. **Each must be a sentence saying nothing *is* a contract below
1.0.** Any line asserting that something *is* a compatibility contract is a
survivor: fix it, then re-run this step.

`docs/superpowers/` is excluded because the spec and this plan discuss the
retracted claim by necessity; they are the record of it, not an assertion of
it.

- [ ] **Step 2: Check the three deliberate exclusions are still intact**

Three Go comments use the word "contract" for internal design decisions, not for a public compatibility promise. They are **out of scope and must not have been edited**:

```
internal/cli/validate.go:84  "Membership is the contract: the mode validates workspace files that..."
internal/cli/stop.go:31      "...(the deliberate contract amendment, spec §5)..."
internal/cli/cli.go:75       "...the deliberate exception to the no-stdout-on-failure contract..."
```

Run: `git diff main --stat -- internal/cli/stop.go`

Expected: **no output** — `stop.go` is not touched by this plan at all. For the other two, confirm the quoted lines above still appear verbatim:

Run: `grep -n "Membership is the contract" internal/cli/validate.go` — expected: one match.

Run: `grep -n "no-stdout-on-failure contract" internal/cli/cli.go` — expected: one match.

If any is missing, it was rewritten in error. Restore it: these describe internal invariants between components, and weakening them would lose real design intent.

- [ ] **Step 3: Final build and test**

Run: `go build ./... && go test ./...`

Expected: build succeeds, all 12 packages pass.

- [ ] **Step 4: Review the whole diff**

Run: `git diff main --stat`

Expected files: `README.md`, `docs/commands.md`, `docs/design.md`, `internal/cli/cli.go`, `internal/cli/config.go`, `internal/cli/doctor.go`, `internal/cli/list.go`, `internal/cli/rebuild.go`, `internal/cli/status.go`, `internal/cli/validate.go`, `internal/config/config.go`, and the spec and plan under `docs/superpowers/`. Anything else is scope creep — investigate before proceeding.

Run: `git diff main -- '*.go' | grep -E '^[+-]' | grep -vE '^[+-]{3}' | grep -v '^[+-]\s*//'`

Expected: **no output**, repeating Task 3 Step 4 against the full branch.

- [ ] **Step 5: No commit unless the sweep found something**

If Steps 1-4 all passed, there is nothing to commit — the branch is complete. If a survivor was fixed, commit it:

```bash
git add -A
git commit -m "docs: retract the contract claim from the site the sweep found"
```

---

## Notes for the reviewer

**Where to focus.** Task 1 Step 2's wording is the substance of the change — everything else follows it. The question worth asking is whether "nothing is a compatibility contract below 1.0" reads as honest or as evasive; the spec argues the former, and §10 records the risk that a reader sees the latter.

**What deliberately did not change.** No config migration machinery, no golden tests, no exit-code changes, no `schema_version` redesign, no `omitempty` normalization, and no release-workflow edit. Each is 1.0 work; doing it now builds for users who do not exist. Spec §7 lists them.

**The one assumption.** "No external users" is the maintainer's statement, not a measured fact — public release artifacts were not checked for downloads. Spec §2 and §10 carry it.
