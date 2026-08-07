# Command reference

Every command below was run against a real installation to produce the output
shown. Absolute paths are the only thing edited, replaced with a short
illustrative path so the examples stay readable.

- [Conventions](#conventions)
- [Exit codes](#exit-codes)
- Observation — [`config`](#projectmux-config), [`list`](#projectmux-list),
  [`status`](#projectmux-status), [`doctor`](#projectmux-doctor)
- Lifecycle — [`open`](#projectmux-open), [`attach`](#projectmux-attach),
  [`stop`](#projectmux-stop)
- Operations — [`autostart`](#projectmux-autostart),
  [systemd](#running-autostart-from-systemd), [`rebuild`](#projectmux-rebuild)
- [`version`](#projectmux-version)

## Conventions

**Naming a workspace.** Commands that accept `<workspace>` resolve it two
ways. With no argument, the workspace is the one containing the current
directory. With an argument, it is looked up by name under the
`repository_roots` configured in `defaults.yaml`, including linked worktrees in
the conventional `.worktrees/` and `.claude/worktrees/` directories.

`projectmux <workspace>` with no command is shorthand for
`projectmux open <workspace>`. A mistyped command therefore resolves as a
workspace name and exits 4, not 2 — a documented trade for the shorthand.

**`--json` and `--compact`.** Every command that produces a report accepts
`--json`, which emits a versioned envelope carrying `schema_version`, and
`--compact`, which puts that envelope on one line and implies `--json`.

**What is a contract.** The JSON envelopes and the exit codes are
compatibility contracts. Human-readable output is not — its layout may change
in any release. Parse `--json`.

**Environment.** Three variables move the resources every command touches:

| Variable | Overrides |
| --- | --- |
| `PROJECTMUX_CONFIG_ROOT` | the configuration directory |
| `PROJECTMUX_STATE_ROOT` | the state directory holding `state.db` |
| `PROJECTMUX_TMUX_SOCKET` | the tmux server, passed to tmux as `-L <name>` |

Set together they give a fully separate installation, which is how a new
ProjectMux is validated beside a working one: sessions on another tmux server
are invisible to it, and unreachable by it, even when they carry the same
identity keys.

## Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | unexpected or I/O failure |
| 2 | usage error |
| 3 | the workspace name matched more than one worktree |
| 4 | the workspace name matched no worktree |
| 5 | invalid configuration |
| 6 | the plan refused: a conflict or uncertainty; do not blindly retry |

Exit 6 deserves a note. It means ProjectMux declined to act because it could
not establish what state the world was in — an unreachable tmux server, an
ambiguous session, a container whose identity could not be confirmed. It is
not a transient error, and retrying without changing something will produce
the same refusal.

A failing command writes nothing to stdout, so stdout can be piped without
filtering diagnostics out of it. The deliberate exception is a command whose
report *is* its output — a partially succeeding `stop`, an `autostart` batch,
`config --validate` finding problems, or a `rebuild` reporting conflicts.
Those write the report to stdout and a one-line summary to stderr.

## projectmux config

```text
projectmux config [--validate] [--json] [--compact] [<workspace>]
```

Prints the normalized, merged configuration for a workspace, or with
`--validate` checks configuration files and reports what is wrong and where.

```text
$ projectmux config
workspace     slabledger
worktree      /home/you/src/slabledger
id            d7142c2621eba1b47024261c980871d9e70d982e0e9fab5e0924100dcc300493
session       slabledger
primary       true
digest        sha256:40dd44f74953a6333ea57bbb1fae15be218847229858600cfdc6a763348f7318
autostart     false
devcontainer  enabled=auto start_timeout=5m0s

WINDOW  RUNS                       LOCATION  CWD  FOCUS
editor  command nvim               -         -    yes
shell   shell                      -         -    -
logs    command tail -f /dev/null  -         .    -
```

Alongside the configuration it reports the derived identity: a stable ID for
the worktree path, the repository slug, the proposed session name, whether the
tree is the repository's primary one, and a `sha256:` digest of the normalized
configuration. The digest ignores cosmetic YAML edits and map ordering, which
is how `status` distinguishes real drift from reformatting.

`--json` adds `schema_version`, `repository_roots`, and the full `config`
document. `repository_roots` sits outside the digest deliberately, so adding a
root does not read as workspace configuration drift.

### `--validate`

Checks configuration files **without resolving a worktree**. This is the point
of the mode: ordinary `config <workspace>` resolves through git, so a workspace
whose worktree has moved reports as unknown and never receives a configuration
verdict. Here the argument names a workspace *file* directly.

With no argument every configured workspace is checked:

```text
$ projectmux config --validate
defaults.yaml  ok
broken         3 problems (invalid)
slabledger     ok

broken:
  workspaces/broken.yaml:4: window "dev" sets location: container but devcontainer.enabled is false (also defaults.yaml:5)
  workspaces/broken.yaml:5: window "logs" must set exactly one of agent, command, or shell: true (it sets none)
  workspaces/broken.yaml:6: window "logs" cwd must not escape the worktree, got "../escape"

3 problems in 1 of 3 subjects
```

Every problem carries the file and line that caused it. A problem owned by two
fields names both positions — the first line above is only wrong *because*
another file disabled containers, so pointing at one of them would send you
somewhere with nothing to fix.

A problem about a key no layer ever set has no position, and none is printed.
ProjectMux does not invent a line number.

`defaults.yaml` read alone is a **warning**, not a failure: it is the bottom
layer and a workspace layer may legitimately supply what it omits. A
warnings-only run exits 0.

The argument must name a configuration file that exists. Anything else — a
path, a glob, a name with no file — is a usage error listing what was found:

```text
$ projectmux config --validate nosuch
projectmux: config --validate: no workspace named "nosuch"; known workspaces: broken, slabledger
```

Exit codes: 0 clean or warnings-only, 5 invalid, 2 unknown name, 1 when a
subject could not be examined at all — an unreadable `workspaces/` directory is
reported as *unknown*, never as "no workspaces".

## projectmux list

```text
projectmux list [--json] [--compact]
```

Lists recorded workspaces and live identity-carrying tmux sessions.

```text
$ projectmux list
WORKSPACE   SESSION     TMUX  CONTAINER  NOTES
slabledger  slabledger  live  -          -
```

On a fresh installation it says so plainly rather than printing an empty
table:

```text
$ projectmux list
no workspaces recorded and no identity-carrying tmux sessions found
```

`TMUX` is `live`, `missing`, or `unknown`. `unknown` means the tmux server
could not be observed — it is not a synonym for absent, and the distinction is
deliberate throughout ProjectMux.

## projectmux status

```text
projectmux status [--json] [--compact] [<workspace>]
```

Observes one workspace and explains drift and dependency failures. It changes
nothing.

```text
$ projectmux status
workspace         slabledger
worktree          /home/you/src/slabledger
id                d7142c2621eba1b47024261c980871d9e70d982e0e9fab5e0924100dcc300493
primary           true
recorded session  slabledger
registered        2026-08-06T05:53:54.037942782Z
updated           2026-08-06T05:53:54.181608075Z
tmux session      live (slabledger, identity match)
container         none
config            in sync (desired sha256:40dd44f7…, applied sha256:40dd44f7…)
last operation    open ok at 2026-08-06T05:53:54.181608075Z
plan              session=none container=none reapply=false record-name=false
```

`plan` is what `open` *would* do right now. `session=none` means the session
already matches the desired state.

Before a workspace has ever been opened, the same command reports what is not
yet true, and why it would refuse:

```text
$ projectmux status
recorded session  not registered
tmux session      unknown
container         none
config            drifted (desired sha256:40dd44f7…, applied never applied)
plan              session=refuse container=none reapply=false record-name=false
refusal           tmux could not be observed; refusing to act on an unknown session state
```

Status exits 0 whenever the observation succeeded. Findings are report content,
not command failure.

## projectmux doctor

```text
projectmux doctor [--json] [--compact]
```

Diagnoses dependencies, configuration, the state database, and drift between
what is recorded and what tmux and Docker actually hold. It is strictly
read-only — it never creates, migrates, kills, or starts anything, and it does
not even open the state database read-write.

```text
$ projectmux doctor
ok       dependencies
  ok       tmux           tmux 3.4
  ok       git            git version 2.54.0
  ok       docker         Docker version 29.7.1, build e9452d6
  ok       docker daemon  29.7.1
  ok       devcontainer   0.86.1
ok       configuration
  ok       defaults
  ok       slabledger
ok       database           schema version 1 at /home/you/.local/state/projectmux/state.db
unknown  orphaned-sessions  tmux list-sessions exited 1: error connecting to /tmp/tmux-1000/default
ok       stale-bindings     no container bindings are recorded
```

Each check reports `ok`, `warn`, `unknown`, or `fail`, and a check's status is
the worst of its items. **`unknown` outranks `warn`**: unexamined ground is
more alarming than a known, bounded finding.

A failing configuration reports the problem count, the first problem with its
position, and where to get the rest:

```text
  fail     dev            3 problems, first at workspaces/dev.yaml:4; run: projectmux config --validate dev
```

Doctor exits 0 whenever the diagnosis completed, regardless of what it found.
Findings are report content, exactly as drift is for `list` and `status`.

## projectmux open

```text
projectmux open [--no-attach] [--json] [--compact] [<workspace>]
```

Observes, ensures, records, and attaches the workspace session. This is the
only command that creates one.

```text
$ projectmux open --no-attach
session slabledger (created)
```

It is idempotent. Reopening an already-correct workspace changes nothing and
says so:

```text
$ projectmux open --no-attach
session slabledger (already-running)
```

`--no-attach` does everything except hand over the terminal, which is what
scripts and the systemd unit want. Without it, `open` attaches on success.

Open takes a per-workspace lock, so two concurrent opens cannot both create a
session. If the world cannot be observed clearly it refuses with exit 6 rather
than guessing:

```text
$ projectmux open --no-attach
projectmux: tmux could not be observed; refusing to act on an unknown session state
```

## projectmux attach

```text
projectmux attach [--json] [--compact] [<workspace>]
```

Attaches to the live workspace session and **never creates one**. If no
session is live it fails rather than silently doing what `open` would do.

Attach hands the terminal to tmux, so it has no captured transcript here: on
success you are simply inside tmux, and from there you attach, detach, and
navigate exactly as you normally would. Use `open` when you want the session
created if it is missing, and `attach` when you want to be told instead.

Both `open` and `attach` refuse with exit 6 when your terminal is already
attached to a tmux server other than the one `PROJECTMUX_TMUX_SOCKET` selects.
tmux cannot move a client between servers — `switch-client` works within one
server and `attach-session` refuses to nest — so this is not a policy but the
honest report of an impossible operation. Detach first, or use
`open --no-attach`.

## projectmux stop

```text
projectmux stop [--container] [--json] [--compact] [<workspace>]
```

Ends the workspace session, and with `--container` its container too.

```text
$ projectmux stop
stopped session slabledger
```

Stop is idempotent: stopping an already-stopped workspace succeeds. It kills
the session by its observed session ID rather than by name, so a session
renamed or replaced between observation and action is not killed by mistake.

A partial failure — the session ended but the container did not — reports what
succeeded and what did not on stdout, with a one-line summary on stderr and a
non-zero exit.

## projectmux autostart

```text
projectmux autostart [--json] [--compact]
```

Starts containers for registered primary worktrees with `autostart: true`. It
is a batch command intended for boot, and it reports one line per workspace:

```text
$ projectmux autostart
slabledger	skipped	(autostart is not enabled)
```

Only *primary* worktrees are considered — linked worktrees share their
parent's container and would otherwise start it several times. Autostart
starts containers; it does not create tmux sessions.

The batch report is the output, so it goes to stdout even when some workspaces
fail, with the summary on stderr.

### Running autostart from systemd

A user unit template ships at
[`contrib/systemd/projectmux-autostart.service`](../contrib/systemd/projectmux-autostart.service).
Install and enable it as a user service:

```sh
mkdir -p ~/.config/systemd/user
cp contrib/systemd/projectmux-autostart.service ~/.config/systemd/user/
systemctl --user daemon-reload
systemctl --user enable --now projectmux-autostart.service
```

Check what it did with `systemctl --user status projectmux-autostart.service`
and `journalctl --user -u projectmux-autostart.service`.

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

## projectmux version

```text
projectmux version
```

What it prints depends on how the binary was produced, and all three forms are
normal:

| How it was built | Reports |
| --- | --- |
| Release binary from a tagged build | the tag, e.g. `v0.4.0` |
| `go install github.com/gambtho/projectmux/cmd/projectmux@v0.4.0` | `v0.4.0` |
| `go build` from a checkout | a pseudo-version, e.g. `v0.0.0-20260806051648-1ccf6afb9c41` |

The pseudo-version encodes the commit timestamp and hash, so it identifies the
source exactly even though it is not a release. Only a build with no version
information available at all — no linker stamp and no VCS metadata, such as a
build from an extracted tarball — reports `dev`.
