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
- **A command summary table**, one line per command, linking to the reference.
- **Install** gains a release-download path with checksum verification
  alongside the existing build-from-source instructions.

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

`.github/workflows/release.yml`, triggered on `v*` tags, with
`permissions: contents: write` declared explicitly — both existing workflows
declare permissions, and the default token may be read-only.

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
- **Reproducible-build verification.** `-trimpath` is included; the full
  build-twice-and-compare gate is fiddly and separate.
- **macOS and Windows binaries.** §11 says "required architectures", and both
  the README and the design commit to Linux and WSL2 only for v1.
- **Homebrew, apt, or any package manager.** Nobody has asked, and each is an
  ongoing maintenance obligation.
- **Rewriting `docs/design.md`.** It is the design record, not user
  documentation, and it is accurate.
- **Bumping `ci.yml` off Node 20.** Tracked separately; unrelated to this
  slice beyond sharing the pinning convention.
