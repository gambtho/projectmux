package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/controller"
)

const listHelp = `usage: projectmux list [--json] [--compact]

List recorded workspaces and live identity-carrying tmux sessions.

  --json     emit the versioned JSON envelope instead of human-readable text
  --compact  emit the JSON on a single line (implies --json)
`

// listEnvelope is the versioned JSON structure for projectmux list.
// tmux_observed is false when the session observation failed; every
// session_state is then "unknown" (a tmux outage is not absence).
type listEnvelope struct {
	SchemaVersion int       `json:"schema_version"`
	TmuxObserved  bool      `json:"tmux_observed"`
	Workspaces    []listRow `json:"workspaces"`
}

// listRow is one workspace or unrecorded live session. live_session is
// present only when exactly one live session corresponds to the row.
type listRow struct {
	ID               string               `json:"id"`
	Slug             string               `json:"slug"`
	Worktree         string               `json:"worktree"`
	IsPrimary        bool                 `json:"is_primary"`
	ProposedSession  string               `json:"proposed_session,omitempty"`
	ActualSession    *string              `json:"actual_session,omitempty"`
	SessionState     string               `json:"session_state"`
	LiveSession      *string              `json:"live_session,omitempty"`
	Container        *storedContainerInfo `json:"container,omitempty"`
	Recorded         bool                 `json:"recorded"`
	IdentityConflict bool                 `json:"identity_conflict"`
}

func runList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("list")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, listHelp)
			return nil
		}
		return usagef("list: %s", err)
	}
	if fs.NArg() > 0 {
		return usagef("list: unexpected argument %q", fs.Arg(0))
	}
	if *compact {
		*asJSON = true
	}

	env, err := buildList(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	return writeListHuman(stdout, env)
}

// buildList unions stored workspaces with live identity-carrying
// sessions. It loads no workspace configuration and resolves nothing: a
// broken workspace YAML cannot break the summary, and any number of
// workspaces costs one tmux subprocess. A failed tmux observation
// renders as uncertainty; only a store failure aborts.
func buildList(ctx context.Context) (listEnvelope, error) {
	st, err := openStore()
	if err != nil {
		return listEnvelope{}, err
	}
	defer st.Close()

	records, err := st.Workspaces()
	if err != nil {
		return listEnvelope{}, fmt.Errorf("reading stored workspaces: %w", err)
	}

	live, obsErr := liveSessions(ctx)
	env := listEnvelope{
		SchemaVersion: OutputSchemaVersion,
		TmuxObserved:  obsErr == nil,
		Workspaces:    []listRow{},
	}

	// Group live sessions by their workspace-ID key. Keyless sessions
	// are not ours and never appear in list output.
	byID := map[string][]controller.LiveSession{}
	for _, s := range live {
		if s.WorkspaceID != "" {
			byID[s.WorkspaceID] = append(byID[s.WorkspaceID], s)
		}
	}

	consumed := map[string]bool{}
	for i := range records {
		rec := records[i]
		row := listRow{
			ID:              rec.ID,
			Slug:            rec.Slug,
			Worktree:        rec.Worktree,
			IsPrimary:       rec.IsPrimary,
			ProposedSession: rec.ProposedSession,
			ActualSession:   rec.ActualSession,
			Container:       storedContainer(rec.Container),
			Recorded:        true,
		}
		switch claimants := byID[rec.ID]; {
		case obsErr != nil:
			row.SessionState = "unknown"
		case len(claimants) == 0:
			row.SessionState = "absent"
		case len(claimants) == 1:
			s := claimants[0]
			name := s.Name
			row.SessionState = "live"
			row.LiveSession = &name
			row.IdentityConflict = s.Slug != rec.Slug || s.Worktree != rec.Worktree
			consumed[rec.ID] = true
		default:
			// Multiple sessions claim this workspace: uncertainty,
			// consistent with ObserveSession — no claimant is picked.
			// The claimants also render below as unrecorded rows.
			row.SessionState = "unknown"
			row.IdentityConflict = true
		}
		env.Workspaces = append(env.Workspaces, row)
	}

	var extras []controller.LiveSession
	for id, sessions := range byID {
		if !consumed[id] {
			extras = append(extras, sessions...)
		}
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
	for _, s := range extras {
		name := s.Name
		env.Workspaces = append(env.Workspaces, listRow{
			ID:               s.WorkspaceID,
			Slug:             s.Slug,
			Worktree:         s.Worktree,
			SessionState:     "live",
			LiveSession:      &name,
			Recorded:         false,
			IdentityConflict: len(byID[s.WorkspaceID]) > 1,
		})
	}
	return env, nil
}

// writeListHuman renders the summary table. This layout is explicitly
// not a compatibility contract; automation should use --json.
func writeListHuman(w io.Writer, env listEnvelope) error {
	if len(env.Workspaces) == 0 {
		_, err := fmt.Fprintln(w,
			"no workspaces recorded and no identity-carrying tmux sessions found")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKSPACE\tSESSION\tTMUX\tCONTAINER\tNOTES")
	for _, row := range env.Workspaces {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			dashIfEmpty(row.Slug), listSessionCell(row), row.SessionState,
			listContainerCell(row.Container), listNotesCell(row))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func listSessionCell(row listRow) string {
	if row.Recorded {
		if row.ActualSession != nil {
			return *row.ActualSession
		}
		return row.ProposedSession + " (unassigned)"
	}
	if row.LiveSession != nil {
		return *row.LiveSession
	}
	return "-"
}

// listContainerCell renders last-observed binding state. A retained
// binding with health missing or unknown must never read as a live
// container (design §8), so health always leads and carries its age.
func listContainerCell(c *storedContainerInfo) string {
	if c == nil {
		return "-"
	}
	return fmt.Sprintf("%s (as of %s)", c.Health, c.ObservedAt)
}

func listNotesCell(row listRow) string {
	var notes []string
	if !row.Recorded {
		notes = append(notes, "unrecorded")
	}
	if row.IdentityConflict {
		notes = append(notes, "conflict")
	}
	if len(notes) == 0 {
		return "-"
	}
	return strings.Join(notes, ", ")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
