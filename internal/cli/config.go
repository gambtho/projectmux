package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
)

// OutputSchemaVersion versions the JSON envelope. Below 1.0 nothing
// ProjectMux emits is a compatibility contract, this included; the field
// exists so that a break is expressible when there is something to break.
// Bump it only for a breaking change to the structure below.
//
// Version 2 is the first such break: it renamed the workspace's path field
// from "worktree" to "repo_root", dropped "is_primary", and added "session".
// The rename is deliberate: a consumer that breaks loudly on a missing field
// is better than one that silently reads a repository root as a worktree
// path.
const OutputSchemaVersion = 2

const configHelp = `usage: projectmux config [--validate] [--json] [--compact] [<workspace>]

Print the normalized, merged configuration for a workspace, resolved either
from <workspace> or from the current directory.

  --validate check configuration files instead of printing one, and report
             what is wrong and where. The argument names a workspace file
             directly rather than a worktree to resolve, so a workspace whose
             worktree has moved can still be checked; with no argument every
             configured workspace is checked.
  --json     emit the versioned JSON envelope instead of human-readable text
  --compact  emit the JSON on a single line (implies --json)
`

// envelope is the versioned JSON structure for projectmux config.
//
// Config is exactly the digested document. Workspace, RepositoryRoots, and
// Digest are envelope metadata and sit outside the digest, so that adding a
// repository root does not read as workspace configuration drift.
type envelope struct {
	SchemaVersion   int           `json:"schema_version"`
	Workspace       workspaceInfo `json:"workspace"`
	RepositoryRoots []string      `json:"repository_roots"`
	Digest          string        `json:"digest"`
	Config          config.Config `json:"config"`
}

// workspaceInfo is the resolved identity shared by config, open, attach,
// stop, and status. Session is the empty string for a repository's default
// session, and is always emitted: a consumer cannot tell an absent field
// from a default one.
type workspaceInfo struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	RepoRoot    string `json:"repo_root"`
	Session     string `json:"session"`
	SessionName string `json:"session_name"`
}

func runConfig(args []string, stdout io.Writer) error {
	fs := newFlagSet("config")
	validate := fs.Bool("validate", false, "check configuration files instead of printing one")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, configHelp)
			return nil
		}
		return usagef("config: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("config: expected at most one workspace, got %d", fs.NArg())
	}
	// --compact only affects JSON, so asking for it is asking for JSON.
	if *compact {
		*asJSON = true
	}

	if *validate {
		root, err := config.Root()
		if err != nil {
			return err
		}
		return runValidate(root, fs.Arg(0), *asJSON, *compact, stdout)
	}

	env, err := buildEnvelope(fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	return writeHuman(stdout, env)
}

// buildEnvelope runs the whole read-only pipeline: defaults, resolution,
// workspace layers, merge, normalization, validation, and digest.
//
// defaults.yaml is read before resolution because it carries repository_roots,
// which resolution needs. An invalid defaults.yaml therefore fails first.
func buildEnvelope(name string) (envelope, error) {
	root, err := config.Root()
	if err != nil {
		return envelope{}, err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return envelope{}, err
	}

	cwd, err := os.Getwd()
	if err != nil {
		return envelope{}, fmt.Errorf("determining the current directory: %w", err)
	}
	ws, err := resolve.Resolve(name, defaults.Layer.RepositoryRoots, cwd)
	if err != nil {
		return envelope{}, err
	}

	effective, err := config.Load(root, defaults, ws.Slug)
	if err != nil {
		return envelope{}, err
	}

	roots := defaults.Layer.RepositoryRoots
	if roots == nil {
		roots = []string{}
	}
	return envelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			RepoRoot:    ws.RepoRoot,
			Session:     ws.Session,
			SessionName: ws.SessionName,
		},
		RepositoryRoots: roots,
		Digest:          effective.Digest,
		Config:          effective.Config,
	}, nil
}

func writeJSON(w io.Writer, v any, compact bool) error {
	enc := json.NewEncoder(w)
	if !compact {
		enc.SetIndent("", "  ")
	}
	if err := enc.Encode(v); err != nil {
		return fmt.Errorf("writing JSON: %w", err)
	}
	return nil
}

// writeHuman renders a readable summary. This layout
// may change in any release; automation should use --json.
func writeHuman(w io.Writer, env envelope) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintf(tw, "workspace\t%s\n", env.Workspace.Slug)
	fmt.Fprintf(tw, "repository\t%s\n", env.Workspace.RepoRoot)
	fmt.Fprintf(tw, "id\t%s\n", env.Workspace.ID)
	fmt.Fprintf(tw, "session\t%s\n", env.Workspace.SessionName)
	fmt.Fprintf(tw, "digest\t%s\n", env.Digest)
	fmt.Fprintf(tw, "autostart\t%t\n", env.Config.Autostart)

	dc := env.Config.DevContainer
	devcontainer := fmt.Sprintf("enabled=%s start_timeout=%s", dc.Enabled, dc.StartTimeout)
	if dc.Config != nil {
		devcontainer += " config=" + *dc.Config
	}
	fmt.Fprintf(tw, "devcontainer\t%s\n", devcontainer)

	if len(env.Config.Environment) > 0 {
		keys := slices.Sorted(maps.Keys(env.Config.Environment))
		fmt.Fprint(tw, "environment\t")
		for i, k := range keys {
			if i > 0 {
				fmt.Fprint(tw, "\t")
			}
			fmt.Fprintf(tw, "%s=%s\n", k, env.Config.Environment[k])
		}
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}

	if len(env.Config.Windows) == 0 {
		_, err := fmt.Fprintln(w, "\nno windows configured")
		return err
	}

	fmt.Fprintln(w)
	wt := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(wt, "WINDOW\tRUNS\tLOCATION\tCWD\tFOCUS")
	for _, win := range env.Config.Windows {
		fmt.Fprintf(wt, "%s\t%s\t%s\t%s\t%s\n",
			win.Name, runsAs(win), orDash(win.Location), orDash(win.Cwd), focusMark(win.Focus))
	}
	if err := wt.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func runsAs(w config.Window) string {
	switch {
	case w.Agent != nil:
		return "agent " + *w.Agent
	case w.Command != nil:
		return "command " + *w.Command
	case w.Shell:
		return "shell"
	default:
		// Unreachable for a validated configuration; kept so a future mode
		// cannot render as a silently blank column.
		return "unknown"
	}
}

// orDash renders an unresolved optional value. A dash rather than a blank so
// that "not set" is visibly distinct from a value that failed to render, and
// "unresolved" for location specifically because it is decided later against
// the workspace's container binding, not defaulted here.
func orDash(v *string) string {
	if v == nil {
		return "-"
	}
	if strings.TrimSpace(*v) == "" {
		return "-"
	}
	return *v
}

func focusMark(focused bool) string {
	if focused {
		return "yes"
	}
	return "-"
}
