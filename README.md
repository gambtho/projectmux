# ProjectMux

ProjectMux turns a declarative YAML description of a project workspace into a
running tmux session, where individual windows may run on the host or inside a
Dev Container.

You describe a workspace once — its windows, what each one runs, where it runs,
and which one gets focus — and ProjectMux is responsible for making the machine
match that description.

## Status: alpha

**This is an early alpha and the current build is read-only.** The only
implemented command is `projectmux config`, which loads, merges, validates, and
prints the effective configuration for a workspace. Nothing yet creates,
attaches to, or modifies a tmux session or a container.

Breaking changes to the configuration schema, the command surface, and the exit
codes should be expected while the version remains below 1.0. There is no
migration support.

Two things are compatibility contracts even during the alpha:

- The **JSON output** of `projectmux config --json`, which carries a
  `schema_version` field.
- The **exit codes** listed below.

Human-readable output is deliberately *not* a contract. Parse `--json` instead.

## Platform support

Linux and WSL2 are the supported platforms for v1. macOS and native Windows are
not supported and are not currently being tested.

ProjectMux expects `git` on `PATH`. Later slices will additionally require
`tmux` and, for container-backed windows, a Docker-compatible runtime and the
Dev Containers CLI. The current read-only slice needs none of those.

## Install and build

```sh
git clone https://github.com/gambtho/projectmux
cd projectmux
CGO_ENABLED=0 go build -o projectmux ./cmd/projectmux
```

Go 1.25 or newer is required. The binary is statically linked and has no runtime
dependency on cgo.

Run the tests, the vet pass, and a formatting check the same way CI does:

```sh
test -z "$(gofmt -l .)"
go vet ./...
go test -race ./...
```

The configuration and resolver tests are hermetic: they create throwaway git
repositories in temporary directories and require no live tmux session, Docker
daemon, or Dev Containers CLI.

## Configuration

Configuration lives under `$XDG_CONFIG_HOME/projectmux` (falling back to
`~/.config/projectmux` when `XDG_CONFIG_HOME` is unset). Set
`PROJECTMUX_CONFIG_ROOT` to point at a different directory; this is intended for
tests and for trying out a configuration without disturbing your own.

Three layers are read, in order, and later layers override earlier ones:

| Layer | Path | Purpose |
| --- | --- | --- |
| Defaults | `defaults.yaml` | settings shared by every workspace |
| Workspace | `workspaces/<slug>.yaml` | settings for one repository |
| Local | `workspaces/<slug>.local.yaml` | machine-specific overrides, not usually committed |

Every layer is optional. A missing file, an empty file, and a comment-only file
are all treated as an empty document.

`<slug>` is the repository name. A linked git worktree inherits its parent
repository's slug, so every worktree of a project shares one configuration.

Windows merge by `name`, not by position. An overriding layer that names an
existing window edits that window in place and leaves the base ordering alone;
a window whose name is new is appended. `repository_roots` may only be set in
`defaults.yaml`, because it is what makes lookup by workspace name possible in
the first place.

Configuration is validated strictly and fails *before* anything is mutated.
Unknown fields, invalid enum values, durations without units, unsupported schema
versions, duplicate window names, more than one focused window, and a window
that does not name exactly one of `agent`, `command`, or `shell` are all errors.

### Example

```yaml
version: 1

# Only meaningful in defaults.yaml. Directories searched when you name a
# workspace instead of running inside it.
repository_roots:
  - /home/you/workspace

devcontainer:
  enabled: auto        # auto | true | false
  start_timeout: 5m

environment:
  CGO_ENABLED: "1"

windows:
  - name: agent-1
    agent: claude
    focus: true
  - name: shell
    shell: true
  - name: build
    command: make test
    cwd: sub/dir       # relative to the worktree; may not escape it
    location: container
```

`location` is left unresolved when you omit it. It is decided later against the
workspace's actual container binding rather than being defaulted to `container`
or `host` at load time.

## Usage

```text
projectmux config [--json] [--compact] [<workspace>]
```

With no argument, the workspace is resolved from the current directory. With an
argument, it is looked up by name under the configured `repository_roots`,
including linked worktrees kept in the conventional `.worktrees/` and
`.claude/worktrees/` directories.

```sh
projectmux config                # human-readable summary of the current tree
projectmux config --json         # the versioned JSON envelope
projectmux config --compact      # the same envelope on one line, implies --json
projectmux config slabledger     # look a workspace up by name
```

Alongside the configuration itself, the output reports the derived workspace
identity: a stable ID for the worktree path, the repository slug, the proposed
tmux session name, whether the tree is the repository's primary one, and a
`sha256:`-prefixed digest of the normalized configuration. The digest is stable
across cosmetic YAML edits and map ordering, so later slices can use it to tell
real configuration drift from reformatting.

### Exit codes

| Code | Meaning |
| --- | --- |
| 0 | success |
| 1 | unexpected or I/O failure |
| 2 | usage error |
| 3 | the workspace name matched more than one worktree |
| 4 | the workspace name matched no worktree |
| 5 | invalid configuration |

## Relationship to tmux and Dev Containers

ProjectMux is a layer above both, and it owns neither.

**tmux** is the terminal multiplexer ProjectMux drives. ProjectMux does not
replace it, wrap its key bindings, or manage your `tmux.conf`; it creates and
reconciles sessions and windows and otherwise leaves tmux to you. You attach,
detach, and navigate with tmux exactly as you normally would.

**Dev Containers** provide the optional container a window can run in.
ProjectMux reads the standard `devcontainer.json` your editor already uses and
delegates to the Dev Containers CLI rather than defining a competing container
format. `devcontainer.enabled: auto` means "use a container when this repository
has a Dev Container configuration"; `true` requires one and fails if it is
missing; `false` keeps every window on the host.

The design goal is that a workspace remains usable without ProjectMux. Removing
it should leave a working repository, a working tmux, and a working Dev
Container.

## Design

The full design document lives at [`docs/design.md`](docs/design.md).

## License

MIT. See [LICENSE](LICENSE).
