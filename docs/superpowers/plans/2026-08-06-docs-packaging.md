# Docs/Packaging Slice — Implementation Plan

**Status: the code is authoritative.** Written after the fact from the verified
implementation, per the workflow established in the container slice (#8). Where
the design spec (`../specs/2026-08-06-docs-packaging-design.md`) and the tree
disagree, the tree is right and the difference is recorded in §5.

Branch `worktree-feat+docs-packaging`, nine commits on `1ccf6af`.

## 1. What shipped

Two problems, both of which made the project misrepresent itself.

**The README was actively false.** It told readers the build was read-only and
that `config` was the only command, six slices after that stopped being true.
`cmd/projectmux/main.go` said the same thing *in its package comment*, which
reaches `go doc` and pkg.go.dev. Four more source comments described shipped
capability as future work.

**There was no way to ship a binary.** No release workflow, no tags, nothing to
pin — which blocked §13 step 5's dotfiles installer and the cutover behind it.

## 2. Build order

| # | Commit | What |
|---|---|---|
| 1 | `bd3e6de` | Design spec |
| 2 | `ac4c299` | Spec review: publishing boundary, atomicity, reproducibility decision, stale source docs |
| 3 | `909b761` | Corrected every stale claim in docs and source |
| 4 | `a9c0093` | `docs/commands.md` from real captured output |
| 5 | `e6bd17b` | README summary table and release install path |
| 6 | `e154341` | `docs/design.md` §12 gate-phasing decision |
| 7 | `9a9bc4a` | Release workflow |
| 8 | `16263c7` | Polish: removed a silently-swallowed flag error |
| 9 | `934056e` | Polish: per-tag concurrency group |

The design amendment (6) deliberately landed before the workflow (7): the
workflow enforces `govulncheck` and not reproducibility, and that gap needed to
be a recorded decision before the code embodying it existed.

## 3. The publishing boundary

The load-bearing part of this slice, and the thing a reviewer should read
first.

A `v*` tag can point at **any** commit, and GitHub runs the copy of
`release.yml` at that commit. Tag-push alone therefore grants release authority
to code that never passed review. Checksums do not help: they would faithfully
attest to the wrong artifact.

Three controls, ordered by how much they actually protect:

1. **The `release` environment** on the publish job, with required reviewers.
   The only real boundary, because it is enforced server-side from repository
   settings and cannot be edited by the tagged commit.
2. **Least privilege.** `verify` builds at `contents: read`; only `publish`
   receives `contents: write`, and it does nothing but upload what `verify`
   produced.
3. **An ancestry check** — the tagged commit must be reachable from
   `origin/main`. Advisory only, and labelled as such in the file: it lives in
   the very file an attacker would control.

**This is not finished in the repository.** If a workflow names an environment
that does not exist, GitHub creates it implicitly **with no protection rules** —
the run succeeds and looks exactly as though the boundary were in place.
Control 1 is inert until the `release` environment is created in repository
settings with required reviewers. That step cannot be done from a commit, and
is the one outstanding item in this slice.

Publication is atomic: create a draft, upload binaries and `SHA256SUMS`,
re-download and verify the checksums against what was actually uploaded, then
publish. Failure at any point deletes the draft, so a retry starts clean. A
half-published release is worse than none — it is the state where a user
downloads a binary that fails verification and reasonably blames the checksum
file.

Third-party actions were deliberately kept off the publish path: it uses the
preinstalled `gh` CLI rather than a release action. In a slice about not
granting publishing authority to unreviewed code, adding a third-party
dependency to that exact path works against the point. The workflow depends
only on `actions/checkout`, `setup-go`, and the artifact pair, all SHA-pinned
with version comments.

## 4. Verification actually performed

Everything the workflow will do was run locally first, because a workflow that
has only ever been read is a guess:

- Both architectures cross-compile with the real flags; `file` confirms
  **statically linked** for each.
- The version stamp reads back: `./projectmux-linux-amd64 version` → `v0.1.0`.
- `sha256sum --check` passes, and `--ignore-missing` behaves correctly when
  only one architecture is present — the actual path the README documents.
- `govulncheck v1.1.4` reports no vulnerabilities, so the new gate will not
  block the first release.
- `actionlint` **with shellcheck installed** passes on all three workflows, so
  every `run:` block received real shell analysis.
- `gofmt`, `go vet`, and the full suite are green.

Documentation was verified by execution, not by reading. Every example in
`docs/commands.md` came from a real run against real tmux on an isolated
socket, with its own config root, state root, and throwaway repository: the
full lifecycle of `open` creating a session, idempotent reopen reporting
`already-running`, `status` before and after, `stop`, `autostart`, `doctor`,
and `config --validate` against a deliberately broken workspace. Only absolute
paths were edited, for readability. `attach` has no transcript because it hands
the terminal to tmux; it is described instead, exactly as the spec committed.

The exit-code table was checked against `internal/cli/cli.go:25-31` rather than
transcribed — it had already drifted once, missing exit 6.

## 5. What contact with the work changed

**The version ladder was wrong in the spec.** It claimed a local `go build`
reports `dev`, assuming build info yields `(devel)`. It does not: Go embeds VCS
metadata, so a local build reports a pseudo-version such as
`v0.0.0-20260806051648-1ccf6afb9c41`. Measured on both clean and dirty working
trees. `dev` appears only when no version information exists at all. The
documentation now presents all three forms as normal.

**A failed capture run was useful.** The first attempt used a scratch socket
path exceeding tmux's limit, so `open` refused with exit 6 and `doctor`
reported `orphaned-sessions: unknown`. Accidental, but it exercised the refusal
path and the unknown-is-not-absent rule under a real failure, and both are
documented from that observation rather than from theory.

**The polish finding was right for the wrong reason.** It claimed two tags
pushed seconds apart could have one run's cleanup delete the other's release.
They cannot: the trap deletes `$tag`, its own tag. The real hazard is a re-run
of the *same* tag, where two runs genuinely share one release. The per-tag
concurrency group addresses exactly that, and `cancel-in-progress: false`
matters because cancelling a publish would fire its cleanup trap mid-flight.

**The stale-claim audit had to cover source, not Markdown.** The spec's own
first draft missed `cmd/projectmux/main.go` because the audit behind it only
looked at documentation files.

## 6. Deviations from the spec

- **`gh` CLI instead of a release action** — reasoning in §3.
- **`docs/design.md` §12 was amended**, which §7 of the spec otherwise
  excludes. The exception is narrow: a design document that states a gate the
  project then skips is worse than one that records the decision.
- **A concurrency group** was added during polish; the spec did not mention
  one.

## 7. Outstanding

- **Create the `release` environment** with required reviewers (§3). Until
  then the only non-self-attesting control is inert.
- **Cut `v0.1.0` from `main` after merge** to exercise the workflow end to end.
  Tagging after merge also satisfies the workflow's own ancestry check.
- **`ci.yml` still floats its action versions** while the other two workflows
  pin. Tracked separately; `checkout@v7.0.1` and `setup-go@v7.0.0` are the
  versions that also move off the deprecated Node 20, and their SHAs are in
  `release.yml` already.
- **The README hardcodes `VERSION=v0.1.0`.** The usual
  `/releases/latest/download/` fix is wrong here: GitHub's "latest" excludes
  prereleases, and every `v0.x` release is one.
- **The dotfiles installer** — the other half of §13 step 5, in the other
  repository, and genuinely downstream of the first release.

## 8. Where reviewers should look

1. `.github/workflows/release.yml` header and the `publish` job — the trust
   model, and the fact that control 1 lives outside the repository.
2. The atomic publish block: draft, upload, re-download, verify, publish, with
   cleanup on failure.
3. `docs/commands.md` exit-code table against `internal/cli/cli.go`.
4. `docs/design.md` §12 — whether the gate-phasing decision is the one you
   want recorded.
