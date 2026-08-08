package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
)

const statusHelp = `usage: projectmux status [--json] [--compact] [<workspace>]

Observe one workspace and explain configuration drift and dependency
failures, resolved either from <workspace> or from the current directory.

  --json     emit the versioned JSON envelope instead of human-readable text
  --compact  emit the JSON on a single line (implies --json)
`

// statusEnvelope is the versioned JSON structure for projectmux status.
type statusEnvelope struct {
	SchemaVersion int            `json:"schema_version"`
	Workspace     workspaceInfo  `json:"workspace"`
	Registered    bool           `json:"registered"`
	Stored        *storedInfo    `json:"stored,omitempty"`
	Session       sessionInfo    `json:"session"`
	Container     *containerInfo `json:"container,omitempty"`
	Config        configInfo     `json:"config"`
	LastOperation *operationInfo `json:"last_operation,omitempty"`
	Plan          planInfo       `json:"plan"`
}

type storedInfo struct {
	ProposedSession string  `json:"proposed_session"`
	ActualSession   *string `json:"actual_session,omitempty"`
	RegisteredAt    string  `json:"registered_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// sessionInfo reports tri-state session knowledge. Name and Identity
// ("match" or "conflict") are present only when State is "live": an
// unobserved session never carries a verdict (spec §3).
type sessionInfo struct {
	State    string  `json:"state"`
	Name     *string `json:"name,omitempty"`
	Identity *string `json:"identity,omitempty"`
}

// containerInfo separates the stored binding (last-observed state) from
// the current observation's outcome, so a stored "present" can never
// hide that the live observation failed or was unsupported (spec §3).
type containerInfo struct {
	Stored      *storedContainerInfo     `json:"stored,omitempty"`
	Observation containerObservationInfo `json:"observation"`
}

// containerObservationInfo is the live observation's outcome: Attempted
// with Health/Error after a real probe or discovery, Reason for
// not-applicable cases.
type containerObservationInfo struct {
	Attempted bool   `json:"attempted"`
	Health    string `json:"health,omitempty"`
	Error     string `json:"error,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type configInfo struct {
	DesiredDigest string  `json:"desired_digest"`
	AppliedDigest *string `json:"applied_digest,omitempty"`
	Drifted       bool    `json:"drifted"`
}

type operationInfo struct {
	Operation    string `json:"operation"`
	Outcome      string `json:"outcome"`
	ExitStatus   *int   `json:"exit_status,omitempty"`
	ErrorSummary string `json:"error_summary,omitempty"`
	FinishedAt   string `json:"finished_at"`
}

type planInfo struct {
	Session    string `json:"session"`
	Container  string `json:"container"`
	Reapply    bool   `json:"reapply"`
	RecordName bool   `json:"record_name"`
	Refusal    string `json:"refusal,omitempty"`
}

func runStatus(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("status")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, statusHelp)
			return nil
		}
		return usagef("status: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("status: expected at most one workspace, got %d", fs.NArg())
	}
	if *compact {
		*asJSON = true
	}

	env, err := buildStatus(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	return writeStatusHuman(stdout, env)
}

// buildStatus runs the full observation pipeline: the config command's
// read-only resolution, then Observe and BuildPlan. Rendering never
// re-implements planning logic (spec §2).
func buildStatus(ctx context.Context, name string) (statusEnvelope, error) {
	root, err := config.Root()
	if err != nil {
		return statusEnvelope{}, err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return statusEnvelope{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return statusEnvelope{}, fmt.Errorf("determining the current directory: %w", err)
	}
	ws, err := resolve.Resolve(name, defaults.Layer.RepositoryRoots, cwd)
	if err != nil {
		return statusEnvelope{}, err
	}
	effective, err := config.Load(root, defaults, ws.Slug)
	if err != nil {
		return statusEnvelope{}, err
	}

	st, err := openStore()
	if err != nil {
		return statusEnvelope{}, err
	}
	defer func() { _ = st.Close() }()

	ctrl := controller.Controller{
		Store:      st,
		Sessions:   newSessionObserver(),
		Containers: newContainerObserver(),
		Clock:      systemClock{},
	}
	snap, err := ctrl.Observe(ctx, controller.Desired{
		Workspace: ws,
		Config:    effective.Config,
		Digest:    effective.Digest,
	})
	if err != nil {
		return statusEnvelope{}, err
	}
	return statusEnvelopeFrom(ws, effective, snap, controller.BuildPlan(snap)), nil
}

func statusEnvelopeFrom(ws resolve.Workspace, effective config.Effective, snap controller.Snapshot, plan controller.Plan) statusEnvelope {
	env := statusEnvelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			Worktree:    ws.Worktree,
			SessionName: ws.SessionName,
			IsPrimary:   ws.IsPrimary,
		},
		Session: sessionInfo{State: string(snap.Session.State)},
		Config:  configInfo{DesiredDigest: effective.Digest},
		Plan: planInfo{
			Session:    string(plan.Session),
			Container:  string(plan.Container),
			Reapply:    plan.Reapply,
			RecordName: plan.RecordName,
			Refusal:    plan.Refusal,
		},
	}

	if live := snap.Session.ByIdentity; live != nil && snap.Session.State == controller.SessionLive {
		name := live.Name
		env.Session.Name = &name
		verdict := "conflict"
		if controller.SessionBelongsTo(*live, ws) {
			verdict = "match"
		}
		env.Session.Identity = &verdict
	}

	var storedBinding *storedContainerInfo
	if rec := snap.Stored; rec != nil {
		env.Registered = true
		env.Stored = &storedInfo{
			ProposedSession: rec.ProposedSession,
			ActualSession:   rec.ActualSession,
			RegisteredAt:    stamp(rec.RegisteredAt),
			UpdatedAt:       stamp(rec.UpdatedAt),
		}
		if rec.AppliedDigest != nil {
			applied := *rec.AppliedDigest
			env.Config.AppliedDigest = &applied
		}
		if op := rec.LastOperation; op != nil {
			env.LastOperation = &operationInfo{
				Operation:    op.Name,
				Outcome:      string(op.Outcome),
				ExitStatus:   op.ExitStatus,
				ErrorSummary: op.ErrorSummary,
				FinishedAt:   stamp(op.FinishedAt),
			}
		}
		storedBinding = storedContainer(rec.Container)
	}

	// Drift is string inequality on digests (design §7). plan.Reapply
	// cannot serve here: a refusing plan clears every mutating flag.
	env.Config.Drifted = env.Config.AppliedDigest == nil ||
		*env.Config.AppliedDigest != effective.Digest

	if storedBinding != nil || snap.Container.Observed != nil || snap.Container.Err != nil {
		obs := containerObservationInfo{}
		if snap.Container.Observed != nil || snap.Container.Err != nil {
			obs.Attempted = true
			if snap.Container.Observed != nil {
				obs.Health = string(snap.Container.Observed.Health)
			}
			if snap.Container.Err != nil {
				obs.Error = snap.Container.Err.Error()
			}
		} else {
			obs.Reason = "no container applies to this workspace"
		}
		env.Container = &containerInfo{Stored: storedBinding, Observation: obs}
	}
	return env
}

// writeStatusHuman renders a readable report. This layout
// may change in any release; automation should use --json.
func writeStatusHuman(w io.Writer, env statusEnvelope) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintf(tw, "workspace\t%s\n", env.Workspace.Slug)
	fmt.Fprintf(tw, "worktree\t%s\n", env.Workspace.Worktree)
	fmt.Fprintf(tw, "id\t%s\n", env.Workspace.ID)
	fmt.Fprintf(tw, "primary\t%t\n", env.Workspace.IsPrimary)

	if env.Registered {
		recorded := env.Stored.ProposedSession + " (proposed, unassigned)"
		if env.Stored.ActualSession != nil {
			recorded = *env.Stored.ActualSession
		}
		fmt.Fprintf(tw, "recorded session\t%s\n", recorded)
		fmt.Fprintf(tw, "registered\t%s\n", env.Stored.RegisteredAt)
		fmt.Fprintf(tw, "updated\t%s\n", env.Stored.UpdatedAt)
	} else {
		fmt.Fprint(tw, "recorded session\tnot registered\n")
	}

	sessionLine := env.Session.State
	if env.Session.Name != nil {
		sessionLine = fmt.Sprintf("%s (%s, identity %s)",
			env.Session.State, *env.Session.Name, *env.Session.Identity)
	}
	fmt.Fprintf(tw, "tmux session\t%s\n", sessionLine)

	if env.Container == nil {
		fmt.Fprint(tw, "container\tnone\n")
	} else {
		stored := "no binding recorded"
		if c := env.Container.Stored; c != nil {
			stored = fmt.Sprintf("%s %s (as of %s)", c.Health, c.ContainerID, c.ObservedAt)
		}
		obs := env.Container.Observation
		var live string
		switch {
		case obs.Attempted && obs.Error != "":
			live = "observation failed: " + obs.Error
		case obs.Attempted:
			live = "observed " + obs.Health
		default:
			live = obs.Reason
		}
		fmt.Fprintf(tw, "container\t%s; %s\n", stored, live)
	}

	drift := "in sync"
	if env.Config.Drifted {
		drift = "drifted"
	}
	applied := "never applied"
	if env.Config.AppliedDigest != nil {
		applied = *env.Config.AppliedDigest
	}
	fmt.Fprintf(tw, "config\t%s (desired %s, applied %s)\n",
		drift, env.Config.DesiredDigest, applied)

	if op := env.LastOperation; op != nil {
		line := fmt.Sprintf("%s %s at %s", op.Operation, op.Outcome, op.FinishedAt)
		if op.ExitStatus != nil {
			line += fmt.Sprintf(" (exit %d)", *op.ExitStatus)
		}
		if op.ErrorSummary != "" {
			line += ": " + op.ErrorSummary
		}
		fmt.Fprintf(tw, "last operation\t%s\n", line)
	}

	fmt.Fprintf(tw, "plan\tsession=%s container=%s reapply=%t record-name=%t\n",
		env.Plan.Session, env.Plan.Container, env.Plan.Reapply, env.Plan.RecordName)
	if env.Plan.Refusal != "" {
		fmt.Fprintf(tw, "refusal\t%s\n", env.Plan.Refusal)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
