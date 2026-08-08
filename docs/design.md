# ProjectMux Application Extraction — Design

**Status:** Approved design

**Date:** 2026-08-04

**Scope:** Replace the merged Bash workspace platform with ProjectMux, an
independently released Go application. Keep personal configuration in this
dotfiles repository.

## 1. Outcome

Extract the `dev` workspace platform from `bin/dev` and `tools/dev/` into
ProjectMux. The independent repository and public binary are both named
`projectmux`; dotfiles retain `dev` as a personal alias. The first release
prioritizes maintainability. It is a command-line application, not a daemon or
graphical interface, but its internal boundaries must allow a daemon to be
added without rewriting the workspace controller.

The application continues to orchestrate host-side tmux sessions whose panes
may run on the host or exec into Dev Containers. It replaces the Bash
implementation's event-sourced JSON files with a small current-state SQLite
database. Desired state remains declarative YAML owned by dotfiles.

Breaking compatibility is allowed. The application does not migrate the
existing `~/.local/state/dev` records or event log.

## 2. Evidence and constraints

PR 54's defect history is the strongest evidence for extraction. A detached
tmux server inherited the operation-lock file descriptor and held the lock
forever. `dev attach` and `dev stop` reached production paths where helpers were
undefined because the dispatcher sourced one command file while the test helper
sourced several. A corrupt event line produced empty fold output that could
overwrite a valid record while the command reported success. Go does not make
orchestration infallible, but close-on-exec subprocess handling, compiled symbol
resolution, typed decoding, and ordinary error returns structurally remove or
narrow these failure classes.

Size is the secondary signal. The merged implementation has 2,537 lines under
`tools/dev/`, excluding its dispatcher and tests. Its own ADR-3 named either
1,500 total lines or a library near 300 lines as the replacement threshold; the
implementation crossed the total threshold and `commands/open.sh` reached the
per-file threshold.

The difficult parts are application concerns rather than shell conveniences:

- concurrent commands that mutate external resources;
- reconciliation after partial failure;
- one collision-safe identity per repository, shared by its linked worktrees;
- persistent operational metadata;
- structured subprocess execution and error retention; and
- a future long-lived process observing Docker and tmux.

The useful boundaries already proven by PR 54 remain:

- tmux is the authority for whether a session is live;
- Docker is the authority for whether a container is reachable;
- desired configuration is separate from observed operational state;
- observation-only commands must not repair or start resources; and
- no database transaction or state lock spans a container pull, network call,
  or another unbounded subprocess.

Version 1 targets Linux and WSL only. macOS support is additive, not a release
requirement.

## 3. Goals and non-goals

### Goals

1. Replace the Bash control plane with the independently versioned
   `projectmux` Go binary.
2. Preserve the useful command model while allowing schemas and output to
   change; `dev` remains a dotfiles-only alias.
3. Make configuration, identity, planning, state, tmux, and container behavior
   independently testable.
4. Store only operational current state, with transactions and explicit schema
   migrations.
5. Recover safely when the process crashes between changing an external
   resource and recording the result.
6. Leave a clean path to a daemon and, later, a UI without implementing either.

### Non-goals

- Migrating Phase 1 JSON records or JSONL events.
- Preserving lifecycle history or providing audit queries.
- Real-time container or session notifications.
- A graphical interface, HTTP API, or remote multi-user service.
- Managing project source, creating worktrees, or replacing the Dev Containers
  CLI.
- Cross-platform support beyond Linux and WSL in the first release.
- Detecting that `projectmux` is itself running inside a container and
  refusing on that basis. See the step 7 amendment in §13 for why the Bash
  guard was not carried over.

## 4. Repository and ownership boundary

The Go source, release workflow, systemd unit template, database migrations,
schemas, and application documentation live in the independent `projectmux`
repository. ProjectMux must not know the location or structure of this dotfiles
checkout.

This repository owns only personal installation policy and configuration:

- pin and install a reviewed application release;
- expose configuration under `$XDG_CONFIG_HOME/projectmux/`; and
- retain machine-local overrides in ignored files.

The default configuration location is `$XDG_CONFIG_HOME/projectmux`, falling
back to `~/.config/projectmux`. `PROJECTMUX_CONFIG_ROOT` may override it for
tests and unusual installations. Runtime state follows XDG state conventions
and may be overridden independently for tests.

## 5. Application architecture

The initial binary calls the controller in-process:

```text
CLI commands
    |
    v
Workspace controller ---- Config loader
    |        |             Workspace resolver
    |        |             State store (SQLite)
    |        +-----------> tmux adapter ------> tmux subprocess
    +--------------------> container adapter -> docker/devcontainer subprocesses
```

The packages have narrow responsibilities:

- **CLI:** parse commands, choose human or JSON presentation, map typed errors
  to exit codes, and handle signals. It contains no orchestration logic.
- **Config:** load defaults plus workspace layers, merge windows by name,
  validate, normalize, and compute a stable digest.
- **Resolver:** resolve a name or current directory to a canonical repository
  root, slug, stable workspace ID, repository ID, and proposed session name. A
  linked worktree resolves to the repository containing it, so every tree of a
  project answers with the same identity.
- **State store:** apply SQLite migrations and transact operational metadata.
  No other package issues SQL.
- **Controller:** observe, plan, ensure, stop, and report a workspace. It depends
  on interfaces rather than subprocess details.
- **tmux adapter:** own every tmux command and translate tmux output into domain
  types. No higher layer parses tmux formats.
- **Container adapter:** detect Dev Container kind, run `devcontainer up`, query
  Docker liveness, and build container execution requests.
- **Runner:** a small internal adapter utility that executes subprocesses with
  contexts, timeouts, structured argv, bounded output capture, and retained
  exit status. It is not a public package or domain boundary.

Domain types belong to the controller-facing packages rather than to adapters.
Adapters return typed observations; they do not write state or decide policy.

## 6. Configuration

Configuration remains layered and file-based:

```text
$XDG_CONFIG_HOME/projectmux/
  defaults.yaml
  workspaces/
    <slug>.yaml
    <slug>.local.yaml
```

The local layer is optional and should be ignored by dotfiles version control.
Later layers override earlier ones. Window lists merge by `name`, not by array
position, so a workspace can adjust one window without copying the default
layout.

`defaults.yaml` also names one or more repository roots. They are used for name
resolution and for rebuilding registration metadata; a central manifest of all
repositories is intentionally unnecessary.

Version 1 uses this schema:

- `version`: required integer, initially `1`;
- `repository_roots`: a defaults-only list of paths searched by the resolver;
- `autostart`: optional boolean, default `false`;
- `devcontainer.enabled`: `auto`, `true`, or `false`;
- `devcontainer.config`: optional relative path to `devcontainer.json`;
- `devcontainer.start_timeout`: positive duration, default `5m`;
- `environment`: optional map of string keys to string values; and
- `windows`: named entries containing exactly one of `agent: <command>`,
  `command: <command>`, or `shell: true`, plus optional relative `cwd`,
  `location` (`host` or `container`), and `focus`.
  - `windows[].panes`: optional list of *additional* panes beyond the primary
    pane the window's own fields describe. Each entry mirrors the window
    fields minus `location` (panes inherit the window's resolved location):
    `name`, exactly one of `agent`/`command`/`shell: true`, optional `cwd`,
    optional `focus` (at most one per window; the primary pane is active by
    default). Omitted, it defaults to a single shell pane in the window's
    directory — every window is two panes by default. The opt-out is an
    explicit empty list, `panes: []`; a bare `panes:` key is the same as
    omitting it. Across config layers, `panes` merges as a unit: a layer
    that states it replaces the whole list.

Exactly one merged window may be focused. Window names are unique and limited
to a documented portable character set. Relative paths cannot escape the
repository root. Unknown fields are rejected so misspelled policy does not silently
turn into default behavior.

Secrets and machine-specific values belong in `<slug>.local.yaml`. A corrupt or
invalid layer fails before any workspace mutation.

## 7. Identity and state

The workspace ID is derived from the canonical repository root and is stable
for that repository, so working in a linked worktree yields the identity of the
repository it belongs to rather than one of its own. The human-facing session
name is derived from the slug. The database enforces a `UNIQUE` constraint on every
non-null actual session name. Collision resolution and assignment happen in one
transaction; application-level check-then-insert is forbidden because it would
retain the cross-workspace TOCTOU race SQLite is meant to remove.

tmux sessions retain identity using the Phase 1 session-scoped keys
`@dev_workspace_id`, `@dev_slug`, and `@dev_worktree`. The Go application reuses
exactly these key names; `@dev_worktree` now carries the repository root, since
that is the tree a workspace is keyed on. They prevent cross-workspace attachment, allow database
rebuilding, and let the Go application adopt sessions created by the Bash
implementation during atomic cutover.

SQLite stores current operational metadata only:

- repository ID, slug, and canonical repository root;
- workspace ID and the session it names;
- proposed and actual session names;
- desired and applied configuration digests;
- current container kind, ID, user, working directory, and observed health
  (`present`, `missing`, or `unknown`);
- last operation outcome, timestamp, exit status, and bounded error summary;
  and
- timestamps needed for diagnostics and cleanup.

It does not store an append-only event stream, fold cursor, rotated segments,
or historical projections. Runtime liveness in SQLite is always timestamped
last-observed state; tmux and Docker remain authoritative at command time.

SQLite is chosen over per-workspace JSON because actual session-name uniqueness
is a global invariant and commands for different workspaces may run
concurrently. A JSON implementation would need a global lock hierarchy and
cross-record commit protocol to provide the same guarantee. The future daemon
benefits from the choice, but it is not the primary justification.

The database schema is private and versioned by migrations. Automation consumes
versioned command JSON, never the database directly. The connection uses WAL
mode and an explicit five-second busy timeout so concurrent observation and
mutation wait for short transactions instead of surfacing default
`SQLITE_BUSY` errors.

A confirmed missing container does not erase its binding. The store retains the
last ID and connection metadata with `health=missing` until replacement
succeeds. A failed Docker probe records `health=unknown`; it does not convert
uncertainty into loss. This preserves the old binding needed for repair without
an event-log lookup or historical column. No container row means no binding has
ever been recorded; the health enum is non-null whenever a binding exists.

The database is rebuildable. Repository roots rediscover source repositories,
tmux user options rediscover live sessions, and a later open can reacquire a
container binding. Rebuilding may lose diagnostic timestamps and last errors,
which is acceptable because they are operational metadata rather than desired
state.

## 8. Commands and observable behavior

The first release retains the useful command vocabulary under the public
`projectmux` binary:

- `projectmux <workspace>` and `projectmux open [<workspace>]` observe, ensure,
  record, and attach. A non-attaching option supports automation and autostart.
- `projectmux attach [<workspace>]` observes and attaches only; it never
  creates.
- `projectmux list` observes registered workspaces and prints a summary.
- `projectmux status [<workspace>]` observes one workspace and explains
  configuration drift and dependency failures.
- `projectmux stop [<workspace>]` ends the session and optionally the container.
  It is the only destructive command.
- `projectmux config [<workspace>]` prints normalized merged configuration.
- `projectmux autostart` starts containers for eligible registered
  repositories. It does not create tmux sessions merely to satisfy boot
  behavior.
- `projectmux doctor` diagnoses dependencies, configuration, database
  integrity, orphaned sessions, and stale bindings. State rebuilding is
  explicit rather than an automatic response to corruption.

Human output is not a compatibility contract. Commands offering JSON emit a
top-level schema version and use a documented, versioned structure.

`projectmux list` and `projectmux status` render container health as the primary
fact. A retained ID with `health=missing` or `health=unknown` must never read as
a live container merely because the binding is non-null.

Observation-only commands may update last-observed metadata in SQLite, but they
must not start a container, create or respawn a pane, or alter a tmux layout.

## 9. Reconciliation and failure behavior

Every command begins by loading desired configuration and observing external
reality. The controller compares desired configuration, stored operational
metadata, and adapter observations to produce a plan.

Workspace-mutating commands take a filesystem lock before the final
observation and hold it through external mutations and the resulting state
commit. The workspace lock is always taken; a command with a container phase
takes the repository lock ahead of it, in that fixed order, because a
repository's container is shared by every tree of the project, so concurrent
commands in two worktrees must serialize on it. A command with no container
phase — `stop` without `--container`, say — takes the workspace lock alone.
They do not hold a SQLite transaction while a subprocess runs.
Observation-only commands do not take the operation lock.

External resource changes and SQLite cannot be one transaction. The design
therefore provides convergence rather than pretending to provide atomicity:

1. observe current resources;
2. compute the next required action;
3. perform one idempotent or discoverable external mutation;
4. observe its result; and
5. transact the new operational metadata.

If the process crashes between steps 3 and 5, the next command observes the
resource and adopts or repairs it. tmux identity options are the recovery marker
for sessions.

The same convergence applies to the schema itself. A migration is pure SQL and
must succeed with the filesystem entirely absent, so the migration that made
the repository the unit of a workspace moved every stored row verbatim: each
recorded path became a repository root, over-counting linked worktrees rather
than guessing. `projectmux rebuild` is the correction pass, because it may ask
git. It collapses a row recorded at a linked worktree into its parent
repository — registering the parent before dropping the stale row, so a crash
between the two leaves an extra collapsible row rather than a lost registration
— drops rows whose path no longer exists, and refuses, as a reported conflict,
to drop a row whose path is present but unresolvable. Live sessions are matched
by the tree recorded in `@dev_worktree` and retagged onto their repository, so
a session running from before the change is adopted rather than duplicated.
`projectmux status` reports a repository whose recorded root is not a main
worktree as needing this run.

Container health is tri-state. A successful liveness probe yields `present`; a
successful probe that confirms absence yields `missing`; and a failed probe,
including Docker daemon unavailability, yields `unknown`. Neither `missing` nor
`unknown` clears the stored binding. Dev Container startup has an explicit
timeout and retains its real exit status and a bounded stderr summary. Context
cancellation propagates to subprocesses.

Database corruption is reported rather than silently overwritten.
`projectmux doctor` can back up the damaged file and explicitly rebuild
disposable state after user confirmation.

## 10. Autostart and future daemon

ProjectMux ships a systemd user unit template whose only job is to invoke
`projectmux autostart`. Dotfiles may enable that unit according to personal
installation policy. Autostart remains opt-in per workspace, and acts once per
repository rather than once per tree.

Version 1 has no hook-driven event emitter and no resident process. Session or
container loss is discovered on the next command. Removing those hooks is a
deliberate simplification: there is no lifecycle-history consumer in version 1.

The controller and adapters must not depend on CLI presentation. A future
daemon can instantiate the same controller, subscribe to `docker events` and
tmux notifications, and expose operations over a Unix socket. At that point the
CLI becomes a transport client when the socket is available. The daemon updates
the same current-state model; persistent event history is added only if an
actual consumer requires it.

A later UI talks to the daemon API. It never embeds orchestration or reads
SQLite directly.

## 11. Packaging and installation

The `projectmux` repository produces versioned Linux binaries for the required
architectures with checksums. It uses `modernc.org/sqlite` and builds with
`CGO_ENABLED=0`; a static binary is a release gate rather than an aspiration.
Database initialization enables WAL and a five-second busy timeout explicitly;
the data-source configuration applies both pragmas to every pooled connection,
not once on an arbitrary connection. Driver defaults are not accepted as
concurrency policy.

Dotfiles pins a reviewed version and digest, installs the binary atomically, and
links or renders personal configuration under the XDG config root. Application
upgrades run SQLite migrations before normal command execution and retain a
backup when a migration is destructive.

For active development, the dotfiles installer accepts
`PROJECTMUX_LOCAL_BINARY=/absolute/path/to/projectmux`. In that mode it links
the local build instead of downloading a release, without modifying the
reviewed version or digest. Running the installer without the override restores
the pinned release.

ProjectMux reports dependency versions in `projectmux doctor`: tmux, Docker,
Dev Containers CLI, and any tool used to resolve its invocation. It invokes
dependencies by structured argv, never by interpolating configuration into a
shell command.

## 12. Testing and release gates

The independent repository should provide:

- unit tests for configuration merge, validation, identity, planning, and
  error classification;
- migration tests from every supported SQLite schema version;
- a concurrent allocation test proving the database `UNIQUE` constraint, not a
  check-then-insert convention, prevents duplicate actual session names;
- controller tests with fake state, tmux, container, clock, and runner
  implementations;
- subprocess adapter contract tests against realistic fixture output;
- real-tmux integration tests on isolated sockets;
- fake Docker and Dev Containers integration tests for failure and timeout
  behavior;
- lifecycle tests covering open, idempotent reopen, attach, container loss,
  repair, stop, collision handling, and crash reconciliation;
- adoption tests for live Phase 1 sessions carrying the three existing tmux
  identity keys;
- tri-state container tests proving failed probes yield `unknown`, confirmed
  absence yields `missing`, replacement retains the old binding until commit,
  and list/status never present a stale ID as live;
- concurrent-command tests and `go test -race`; and
- formatting, linting, vulnerability scanning, and reproducible release checks.

Tests must assert that observation-only commands do not mutate workspaces and
that Docker unavailability never masquerades as container loss.

### Gate phasing below 1.0

Two of the gates above were not enforced by the `v0.x` release workflow, and
this records that as a decision rather than leaving the list aspirational:

- **Linting.** `gofmt`, `go vet`, and `go test -race` run on every pull
  request, every push to the default branch, and every release. A dedicated
  linter is not yet wired in; introducing one to an existing codebase surfaces
  a backlog that deserves its own focused pass.
- **Reproducible release checks.** Release builds use `-trimpath` and
  `CGO_ENABLED=0`, which removes local paths and the cgo toolchain as sources
  of variation, but nothing yet builds twice and compares digests.

Both become gates before `v1.0`. Until then `v0.x` artifacts are published as
prereleases, which is the honest signal: they are reproducible in intent and
not yet verified to be so. Vulnerability scanning (`govulncheck`) *is*
enforced on release, and a release failing any enforced gate publishes
nothing.

#### Amendment: both gates closed after v0.3.0

**Linting.** `golangci-lint` runs on every pull request and every release,
invoked through `go run` at a pinned version — the same shape as
`govulncheck`, so no third-party action needs pinning and the version that
runs is the version written in the workflow. The linter set is golangci-lint's
defaults: `errcheck`, `govet`, `ineffassign`, `staticcheck`, `unused`. Four of
those five were already clean when the gate was introduced, so the defaults are
what the codebase holds itself to rather than a compromise.

The backlog the phasing note anticipated was 116 `errcheck` findings. Of those,
68 were `fmt.Fprint*` calls on the CLI's output-writer seam; `.golangci.yml`
exempts those functions, on the ground that a failed write to stdout is the one
error that cannot be reported — errcheck exempts bare `fmt.Print*` for the same
reason, and these are caught only because output is routed through an
`io.Writer` so tests can capture it. The exemption names those three functions
and nothing else: `Close`, `Rollback`, and `Release` stay checked everywhere,
including tests. The remaining 48 were fixed.

The configuration also sets `max-same-issues: 0` and `max-issues-per-linter: 0`.
golangci-lint truncates by default, and the run that first measured this
backlog reported 48 of 116 without saying so. A gate that stops counting past a
threshold reports clean for the wrong reason.

**Reproducible release checks.** The release workflow builds both
architectures a second time into a separate directory and requires `cmp` to
find the binaries bit-identical, before any checksum is computed or artifact
uploaded. This proves reproducibility *within one runner*, which catches the
realistic regression — an embedded timestamp, a nondeterministic map order, a
path leaking into the binary. It does not prove an independent machine
reproduces the same digests; that needs a rebuild elsewhere, and claiming it
from this step would restate the dishonesty the phasing note was written to
avoid.

The step failed on its first real run, and the failure was in the check
rather than the binaries. Go embeds `vcs.modified` — computed from
`git status --porcelain`, which counts untracked files — in every binary.
`dist/` is gitignored, so release builds leave the tree clean; the original
step rebuilt into `rebuild/`, which is not ignored, so writing the first
binary there dirtied the tree and stamped every later binary
`vcs.modified=true`. `amd64` matched and `arm64` differed, separated only by
which one the loop builds first. The rebuild now goes to `$RUNNER_TEMP`,
outside the work tree, and the step asserts the tree is clean before
comparing — a dirty tree would mean the *release* binaries carry
`vcs.modified=true`, making "reproducible" a claim about a state nobody can
reproduce from the tag.

Worth recording alongside it: the local pre-merge check that reported both
architectures identical was structurally unable to catch this. It built
`dist/` and `rebuild/` interleaved per architecture, so both `arm64` builds
saw an equally dirty tree and the discrepancy cancelled. A verification that
perturbs its own subject can report success for the same reason it should
have reported failure.

With both gates enforced, the prerelease flag needs no change: it already keys
on the tag pattern, so `v0.*` continues to publish as a prerelease and `v1.0.0`
publishes as a full release.

**What the prerelease flag now signals.** The phasing note above gave a reason
for it that has since expired — that `v0.x` artifacts were "reproducible in
intent and not yet verified to be so." From `v0.4.0` on they are verified. The
flag stays because the *other* reason is untouched: the configuration schema
and the command surface are still unfrozen, and there is no migration support
for `v0.x` configuration. Closing these two gates was release engineering, not
a compatibility commitment. `v1.0.0` is deliberately not being cut on the
strength of it, because the number would promise a stability the project has
not yet decided to offer — and a 1.0 that quietly means "our CI got better"
teaches readers to distrust the version number.

## 13. Extraction sequence

The merged Bash platform is a maintenance-only behavioral reference during
extraction. Fix only defects that block characterization or safe cutover. New
features, event-history work, and discretionary final-review effort belong in
the Go application.

1. Create the independent `projectmux` repository with domain types,
   configuration, the internal runner utility, adapters, and state interfaces.
2. Implement the SQLite schema and current-state controller without historical
   event machinery.
3. Port commands one vertical slice at a time, starting with `config` and
   observation, then `open`/`attach`, and finally `stop`/autostart.
4. Prove behavior through the new repository's lifecycle suite against real
   tmux and fake container tooling. Port behavioral guarantees from the Bash
   tests, not fold cursors, event rotation, or other machinery being retired.
5. Add a dotfiles installer and XDG configuration links pinned to a reviewed
   release.
6. Run the Bash and Go implementations side by side only during validation,
   with separate state roots and tmux sockets.
7. Before switching, prove ProjectMux adopts a live Bash-created
   session through `@dev_workspace_id`, `@dev_slug`, and `@dev_worktree` without
   creating, renaming, or attaching to the wrong session. Then install the
   `projectmux` binary and switch the dotfiles `dev` alias atomically. Do not
   import the old JSON state.
8. Treat removal as migration of installed, working assets rather than deletion
   of dead source. After the Go path passes its release gates:
   - disable and remove the Phase 1 `dev-autostart` unit before enabling the new
     application-owned unit;
   - inspect the live tmux hook commands and unset `session-closed`,
     `client-attached`, `client-detached`, and window-scoped `pane-died` only
     when they still match the managed `dev-event` commands; otherwise warn and
     preserve the user's replacement;
   - remove the managed `dev-workspace-config` marker block from
     `tools/tmux/tmux.conf.symlink` and reload tmux configuration;
   - remove `bin/dev`, the Bash sources under `tools/dev/` including
     `dev-event`, `dev.tmux.conf`, the old unit and installer, and tests whose
     subject is state/event machinery rather than retained behavior; and
   - preserve the old state directory as a dated backup through the validation
     window, then remove it explicitly because no data migration is promised.

### Step 6 amendment: how the two installations are separated

Step 6's separate state root already existed as `PROJECTMUX_STATE_ROOT`. The
separate socket did not: `tmux.Client` carried a `Socket` field, but only
integration tests ever set it, so every shipped command drove the default tmux
server. `PROJECTMUX_TMUX_SOCKET` closes that gap, following the same
per-package override convention as `PROJECTMUX_STATE_ROOT` and
`PROJECTMUX_CONFIG_ROOT`.

The gap mattered because the identity keys are not a distinguishing feature
here. Bash-created sessions carry the same `@dev_workspace_id`, `@dev_slug`,
and `@dev_worktree` that ProjectMux keys on, so on a shared server ProjectMux
would observe the running installation's sessions as its own and could rename
or kill them — during the one phase whose purpose is to leave them alone. The
socket, not the identity keys, is what makes the two installations disjoint.

Attaching is the one operation that cannot be made socket-agnostic: tmux
`switch-client` is intra-server and `attach-session` refuses to nest, so a
terminal attached to one server cannot be moved to another. Rather than
attempt it and produce a confusing failure — or worse, act on the wrong
server — `open` and `attach` refuse with exit 6 when the terminal's server
does not match the configured one. The comparison is by socket *path*, not by
socket name: `$TMUX` carries the path, and a client on `tmux -S /elsewhere/pmx`
shares a base name with `-L pmx` while addressing a different server, so
comparing names would call that a match. Step 7 deliberately runs on the
*default* socket, since adopting a live Bash-created session is the thing being
proved; this is why the setting is an environment variable rather than
configuration.

Step 6 was then performed against a live installation on 2026-08-07. With
`PROJECTMUX_CONFIG_ROOT`, `PROJECTMUX_STATE_ROOT`, and `PROJECTMUX_TMUX_SOCKET`
all pointed at throwaway locations, a full `open --no-attach` / `list` /
`status` / `stop` cycle ran while a Bash-created session was live on the
default server. That session carried all three identity keys, read back
directly from tmux, and `projectmux list` did not report it; the default
server's session name, window count, and key values were identical before and
after. Attaching from a terminal on the default server exited 6 and ran no
tmux command.

### Step 7 amendment: the in-container refusal is deliberately not carried over

The Bash `dev` dispatcher opened with a guard that ran before argument
parsing: if `/.dockerenv` or `/run/.containerenv` existed, every verb —
including read-only ones — printed two lines and exited 10. Step 7 replaced
`bin/dev` with `exec projectmux "$@"`, so that behavior is gone. ProjectMux
will not reimplement it. This is a decision, not an oversight.

Note the scope. Managing a workspace *whose project runs in a devcontainer*
is a supported first-class case and is unaffected: `projectmux` runs on the
host, starts the container through the Dev Containers CLI, and execs the
container-bound windows into it (§5, §9). The guard concerned only the
inverse — invoking `projectmux` from a shell that is already inside a
container.

Half of the Bash reasoning has expired and half has not. Measured against a
live devcontainer on 2026-08-07:

- The container's `~/.dotfiles` is no longer a stale seed-copy; it is a bind
  mount of the host tree. That reason is dead.
- The host tmux server is still unreachable. The container has its own `/tmp`
  namespace, `/tmp/tmux-1000/` does not exist inside it, and the image did not
  ship a `tmux` binary at all. That reason still holds.

What makes it a non-goal is reachability, not harmlessness. The Bash guard was
worth its cost because the dotfiles tree was seeded into every container, so
`dev` was genuinely on PATH there. ProjectMux is not. The dotfiles `bin/install`
does install it — as an optional phase, with no container guard — but that full
install is not what runs inside these containers: the measured container had no
`~/.dotfiles-profile`, no `projectmux` on PATH, and neither
`~/.config/projectmux` nor `~/.local/state/projectmux`. Its bootstrap is a
lighter path that drops a few tools into `~/.local/bin`. A guard added today
would be unreachable code with a test suite attached.

The cost of being wrong is recorded honestly, because it is the argument for
revisiting this. Absent a guard, a `projectmux` binary that *did* reach a
container image would not fail loudly — it would start a second tmux server
inside the container, create a session there, and fork a state database into
the container's `~/.local`, all invisible from the host and indistinguishable
from success. That is the silent-wrong-thing category the cross-server refusal
above exists to prevent. Reopen this decision if the binary ships inside an
image, if a container mounts the host tmux socket, or if the dotfiles installer
starts running in full inside containers. The refusal would then belong in the
commands that need tmux or the container adapter — not in `config`, `version`,
or `doctor`, which are useful in a container and need no server — and it would
use `RefusalError` and exit 6 like every other refusal, not Bash's exit 10.

Until then the in-container failure is an ordinary "command not found", which
names the problem accurately.

### Step 8 amendment: what removal actually removed, and what it kept

Step 8 was performed against the live installation on 2026-08-07. Its runtime
half is a script, `tools/projectmux/migrate-from-dev.sh` in the dotfiles
repository, rather than a list of one-time commands: the "otherwise warn and
preserve the user's replacement" branch is real behavior, and behavior that
only ever exists in a runbook is behavior nobody tested.

Three findings from performing it, each of which changed the implementation.

**`pane-died` is global-window scoped.** It is registered with `-gw`, which
makes it invisible to both `show-hooks -g` and `show-hooks -w -t <window>`. An
inspection using either flag reports it absent and concludes three hooks are
live. Four are. Only `show-hooks -gw` reports it, and only `set-hook -gwu`
clears it.

**Matching must be a substring test, not equality.** The step says to unset the
hooks "only when they still match the managed `dev-event` commands", which
invites comparing against the current `dev.tmux.conf`. On the live machine that
comparison fails on all four: the hooks were set by an older revision, so
`pane-died` lacked the later `'pane=#{@dev_pane}'` argument, and tmux had
expanded `~` to an absolute path at registration time. Exact matching would
therefore take the preserve branch on every hook, report success, and leave all
four set permanently — precisely the outcome the preserve branch exists to
prevent. The `dev-event` command string is the only evidence that survives in a
server started weeks earlier, so it is what the script matches. Unsetting is
per array index, so a handler the user appended beside the managed one
survives.

**The marker block is an ordering constraint, not a cleanup step.** While
`dev-workspace-config` remains in `tools/tmux/tmux.conf.symlink`, every new
tmux server sources `dev.tmux.conf` and re-registers the hooks, so unsetting
them on the running server is undone by the next server start. The script is
only correct when run from a checkout that no longer has the block. Reloading
the configuration afterwards and observing that no hook returns is what proves
the removal took.

Where systemd is unreachable, an installed `dev-autostart.service` is
deliberately left in place rather than deleted. Removing the unit file without
being able to `systemctl --user disable` it strands the enablement symlink in
`default.target.wants/` pointing at a unit that no longer exists, which fails
on every login and is harder to clear than what it replaced. The run warns that
it was incomplete instead of reporting a clean skip.

**`bin/dev` is retained as a thin wrapper, contrary to the step's text.** The
step lists it for removal alongside the Bash sources. It remains what step 7
made it: two lines that `exec projectmux "$@"`. The step's purpose is to retire
the Bash implementation, and that implementation is gone; removing the wrapper
would additionally retire the `dev` entry point people type, which is a separate
decision the extraction never argued for. Recorded here as a deliberate
deviation rather than left as a silent gap.

One behavior did not survive the port. The Bash default workspace put
`location: host` on a *pane*; `config.PaneLayer` has no `Location` field, since
panes inherit their window's resolved location, and the validator reports the
key as an unknown field — which reads like a typo rather than a missing
feature. Nothing in the shipped configuration template uses it, so this is
recorded rather than resolved.

## 14. Decisions recorded

- Maintainability is the primary reason for extraction.
- Daemon readiness is secondary; the daemon itself is deferred.
- A UI is explicitly deferred.
- The independent repository and public binary are named `projectmux`; `dev` is
  a personal dotfiles alias only.
- Personal configuration remains in dotfiles as defaults plus per-workspace
  files.
- Breaking changes are acceptable.
- Existing operational data is not migrated.
- Version 1 keeps current operational state but no lifecycle history.
- SQLite is private implementation state; global session-name uniqueness is a
  database constraint, WAL plus a busy timeout is explicit concurrency policy,
  and versioned command JSON is public.
- Container health is `present`, `missing`, or `unknown`; bindings survive loss
  and probe failure until replacement succeeds.
- The Go application reuses the three Phase 1 tmux identity keys for adoption.
- Linux and WSL are the only initial platforms.
- The Bash in-container refusal is not carried over: managing containerized
  workspaces from the host is unaffected, and the guard would be unreachable
  because the binary is not installed into container images.
