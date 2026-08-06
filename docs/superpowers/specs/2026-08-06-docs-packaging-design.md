# Docs/Packaging Slice Design

`docs/design.md` §13 step 5: "Add a dotfiles installer and XDG configuration
links pinned to a reviewed release."

That step spans two repositories. §4 draws the line: the Go source, **release
workflow**, systemd unit template, and application documentation live here;
dotfiles owns "only personal installation policy" — pinning and installing a
reviewed release. This slice covers **this repository's half**: documentation
and the release workflow. The installer is a follow-up in the dotfiles
checkout, and it is genuinely downstream — it pins *a reviewed release*, and
no release exists yet.

## 1. The problem being fixed

**The README is not incomplete, it is actively false.** It tells a reader:

> This is an early alpha and the current build is read-only. The only
> implemented command is `projectmux config` [...] Nothing yet creates,
> attaches to, or modifies a tmux session or a container.

Six slices have shipped since that was written. All nine commands exist.
Measured against the current tree, `open`, `stop`, `autostart`, `doctor`, and
`systemd` appear in the README **zero times**. The exit-code table is missing
`6` (`ExitRefused`), added in the open/attach slice.
`contrib/systemd/projectmux-autostart.service` ships and is undocumented.

This is the most user-visible defect in the repository, which is why
documentation leads this slice rather than trailing the packaging work.

Second problem: **there is no release workflow and no tags.** §11 requires
versioned Linux binaries with checksums, and states that a static
`CGO_ENABLED=0` binary is "a release gate rather than an aspiration". Nothing
enforces that today, and cutover (§13 steps 6-8) cannot begin without
something to pin.

## 2. Documentation structure

`README.md` remains the front door and keeps its spine: what ProjectMux is,
platform support, install, configuration with the layered YAML example, the
tmux/Dev Containers relationship, the design link, the license. Three changes:

- **Status** is rewritten. The command surface is complete; what remains
  unstable is the schema and the exit codes below 1.0. The genuinely important
  part is preserved verbatim in substance: the two compatibility contracts
  (the JSON envelope's `schema_version`, and the exit codes) and the explicit
  statement that human-readable output is *not* one.
- **Platform support** is corrected. It currently says tmux and container
  tooling are needed by "later slices" and that "the current read-only slice
  needs none of those". Both are false: `tmux` is required now, and a
  Docker-compatible runtime plus the Dev Containers CLI are required for
  container-backed windows.
- **A command summary table**, one line per command, linking to the reference.
- **Install** gains a release-download path with checksum verification
  alongside the existing build-from-source instructions.

**The same false claim exists in Go source and is fixed with it.**
`cmd/projectmux/main.go` opens with "This build is an alpha and implements the
read-only configuration slice only." That is package documentation — it
reaches `go doc` and pkg.go.dev, not just readers of the file — so the audit
covers source comments, not only Markdown. Every remaining "read-only slice"
claim in the tree is corrected together.

`docs/commands.md` is the new reference: every command with its flags,
arguments, exit codes, and JSON envelope where it has one. Grouped as
observation (`list`, `status`, `config`, `doctor`), lifecycle (`open`,
`attach`, `stop`), and operations (`autostart` and the systemd unit). It sits
beside `docs/design.md`, matching the existing convention.

**Why split rather than one file:** documenting nine commands inline would
roughly triple a 189-line README and bury the orientation material that helps
someone decide whether to use the tool at all. **Why not generate from
`--help`:** the help strings are terse by design, and a generator is real
machinery to maintain for nine commands.

## 3. Release workflow

`.github/workflows/release.yml`, triggered on `v*` tags.

### 3.1 A tag is not a review

The obvious design — one tag-triggered job holding `contents: write` — quietly
assumes that a `v*` tag implies reviewed code. It does not. A tag can point at
**any** commit: an unmerged branch, an unreviewed SHA, anything the tagger
chooses. GitHub then runs the copy of `release.yml` **at that commit**, with
whatever permissions it declares. Tag-push alone would therefore hand release
authority to code that never passed review, in a slice whose entire purpose is
publishing binaries people install. Checksums do not help here: they would
faithfully attest to the wrong artifact.

Three controls, in decreasing order of how much they actually protect:

1. **A protected GitHub environment** (`release`) on the publishing job, with
   required reviewers. This is the real boundary, because it is enforced
   **server-side from repository settings** and cannot be edited by the tagged
   commit. Everything else in this list can be removed by whoever controls
   that commit.
2. **Split jobs, least privilege.** `verify` runs the gates and builds with
   `permissions: contents: read` — matching `ci.yml`. Only `publish` receives
   `contents: write`, and it does nothing but upload what `verify` produced.
3. **An ancestry check** — the tagged commit must be reachable from
   `origin/main` (`git merge-base --is-ancestor`). This catches the honest
   mistake of tagging a feature branch. It is **advisory only**, and the spec
   says so plainly: it lives in the file an attacker would already control.

The ordering matters more than the list. A reader who implements 2 and 3 and
skips 1 has bought almost nothing, because both are self-attestations by the
code under suspicion.

### 3.2 Build and publish

```text
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X github.com/gambtho/projectmux/internal/cli.version=${TAG}" \
  -o projectmux-linux-${ARCH} ./cmd/projectmux
```

for `linux/amd64` and `linux/arm64` — the platforms §11 and the README both
commit to. The `-X` path is verified against `internal/cli/cli.go`, which
already documents this exact hook.

`-trimpath` strips local build paths from the binary. It is not the full
reproducibility gate §12 asks for, but it is nearly free and moves toward it.

Artifacts plus a `SHA256SUMS` file attach to a GitHub Release, marked
**prerelease while the tag is `v0.x`** — the README warns that breaking
changes are expected below 1.0, and a plain "Latest release" badge on an alpha
would contradict it.

**Publication is atomic: draft first, publish last.** The release is created
as a draft, both binaries and `SHA256SUMS` are uploaded, the checksums are
verified against the uploaded files, and only then is the draft published. A
failure at any point leaves a draft — invisible to anyone fetching releases —
rather than a public release missing an artifact or carrying a checksum file
that does not describe what shipped. A half-published release is worse than no
release: it is the state in which a user downloads a binary whose checksum
cannot be verified and reasonably concludes the checksum file is the broken
part.

The draft is deleted on failure so a retry starts clean rather than appending
to a partial one.

**Gates before publishing:** the existing CI set (`gofmt`, `go vet`,
`go test -race`, `go mod tidy`) plus **`govulncheck`**, which §12 names
explicitly and which is cheap. A release failing any gate publishes nothing.

**Actions are SHA-pinned with a version comment**, following
`dependabot-auto-merge.yml`. The repository currently has two conventions —
`ci.yml` floats (`@v4`), the dependabot workflow pins — and the pinned form is
the newer reviewed one. Bringing `ci.yml` into line is a separate chore, so
this slice adds no third convention.

## 4. What `projectmux version` reports

Stated because it is already half-solved, and the documentation must not
overpromise. `versionString()` has three rungs:

1. **Release binary** — `-ldflags -X` stamps the tag; reports `v0.2.0`.
2. **`go install .../cmd/projectmux@v0.2.0`** — no ldflags, but
   `debug.ReadBuildInfo()` supplies `Main.Version`; reports `v0.2.0` anyway.
   This works today.
3. **Local `go build` from a checkout** — build info reports `(devel)`, which
   the ladder rejects; reports `dev`.

The release workflow therefore does not create version reporting; it upgrades
rung 1 from `dev` to a real tag. `docs/commands.md` documents all three,
because "why does my binary say `dev`" is otherwise a support question.

## 5. Verification

Documentation and workflow YAML do not get unit tests, so verification is
concrete rather than ceremonial:

- **Every command invocation in the docs is executed** against a built binary,
  and its real output is what gets documented. No invented sample output. The
  config-validation slice established this the hard way: passing structural
  assertions coexisted twice with output that read badly, and printing the
  real thing is what exposed it.

  Two qualifications, so this is a promise that can actually be kept. `attach`
  hands the terminal to tmux and cannot be captured non-interactively; its
  documentation describes behavior and exit codes rather than showing a
  transcript. Everything else is runnable — real tmux is already available
  (`internal/tmux` integration tests exercise it), so `open --no-attach`,
  `stop`, `list`, `status`, `config`, `doctor`, `autostart`, and `version` are
  run against a scratch repository on an isolated socket and their genuine
  output captured. Where a real run is impractical, the documentation says
  what the command does and omits the transcript; it never shows invented
  output.
- **The exit-code table is checked against the constants in
  `internal/cli/cli.go`**, not transcribed from memory. It is already wrong
  once.
- **The release workflow is validated by cutting a real tag** — `v0.1.0` —
  confirming the artifacts build, the checksums verify, and
  `./projectmux-linux-amd64 version` prints `v0.1.0`. A release workflow that
  has never run is a guess.
- **The trust boundary is verified as configuration, not as YAML.** The
  `release` environment and its required reviewers live in repository
  settings; the spec's §3.1 controls are worth nothing if that environment
  does not exist. Confirming it is present, and that the publishing job is
  bound to it, is part of finishing this slice — and is the one step that
  cannot be done from the repository alone.
- **A no-source-claims-stale check**: grep the tree for "read-only" and
  "later slices" and confirm every remaining hit is deliberate. The audit that
  produced this spec missed `cmd/projectmux/main.go` by looking only at
  Markdown.
- `actionlint` if available, otherwise a careful read. YAML that only fails on
  tag-push is expensive to debug.

That `v0.1.0` tag is also what unblocks the dotfiles follow-up: it becomes the
first reviewed release there is anything to pin.

## 6. Risks

**Documentation drifts from behavior** — exactly what just happened. This
slice fixes the current drift; it does not prevent the next one. Generated
docs are deliberately *not* the answer, because that machinery costs more than
it saves at this size. The mitigation is structural: flags, arguments, and
exit codes live in exactly one place, and the README carries only a summary
table that stays true as long as command *names* do not change.

**Tagging `v0.1.0` implies more stability than exists.** Mitigated by marking
`v0.x` as prereleases and keeping the breaking-changes warning prominent.

## 7. Exclusions

- **The dotfiles installer.** §4 puts it in the other repository, and it needs
  a release to pin. Its own follow-up.
- **`golangci-lint`.** §12 asks for it, but introducing a linter mid-stream
  surfaces a backlog that would swamp this slice. Its own pass, where the
  findings get real attention.
- **Reproducible-build verification** — but *not* silently. §12 lists
  "reproducible release checks" among the release gates, and this slice cuts
  the first release, so quietly deferring it would ship the artifact while
  skipping a gate the design states governs it. That is resolved by amending
  the governing document rather than by omission: `docs/design.md` §12 gains
  an explicit statement that `v0.x` prereleases ship without the
  reproducibility gate, and that it becomes a gate before `v1.0`. `-trimpath`
  is included now as a step toward it and is not claimed to be it.

  This deliberately widens the slice: §7 otherwise excludes editing
  `docs/design.md`. The exception is narrow and specific — a design document
  that states a gate the project then skips is worse than one that records the
  decision, and the alternative (building the full build-twice-and-compare
  check now) would dominate a slice that is otherwise documentation.
- **macOS and Windows binaries.** §11 says "required architectures", and both
  the README and the design commit to Linux and WSL2 only for v1.
- **Homebrew, apt, or any package manager.** Nobody has asked, and each is an
  ongoing maintenance obligation.
- **Rewriting `docs/design.md`.** It is the design record, not user
  documentation, and it is accurate. The single exception is the §12
  reproducibility amendment described above — one recorded decision, not a
  rewrite.
- **Bumping `ci.yml` off Node 20.** Tracked separately; unrelated to this
  slice beyond sharing the pinning convention.
