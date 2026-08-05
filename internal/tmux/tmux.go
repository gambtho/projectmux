package tmux

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/run"
)

// DefaultTimeout bounds a tmux subprocess when the client does not set
// one. Signal cancellation is for interactive interruption; this finite
// timeout is the hang defense for unattended callers (spec §5).
const DefaultTimeout = 5 * time.Second

// tmuxBinary is the executable to invoke; tests substitute a script.
var tmuxBinary = "tmux"

// Client observes live tmux sessions. Socket, when non-empty, is passed
// as -L for isolated servers (integration tests); Timeout bounds every
// subprocess, with the zero value meaning DefaultTimeout.
type Client struct {
	Socket  string
	Timeout time.Duration
}

var _ controller.SessionObserver = (*Client)(nil)

// Sessions lists every live session with whatever identity keys it
// carries, in two phases: a strictly validated session-id enumeration,
// then four per-field display-message calls per session whose entire
// output is one raw value (spec §5) — no in-band framing exists for a
// value to forge. No server is absence: an empty list and nil error.
// Any other failure is an error, which callers must render as
// uncertainty, never as absence (design §9).
func (c *Client) Sessions(ctx context.Context) ([]controller.LiveSession, error) {
	res, err := c.exec(ctx, "list-sessions", "-F", "#{session_id}")
	if err != nil {
		return nil, err
	}
	if res.ExitCode == 1 && isNoServer(res.Stderr) {
		return nil, nil
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("tmux list-sessions exited %d: %s",
			res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	if res.StdoutTruncated {
		return nil, fmt.Errorf("tmux list-sessions output exceeded %d bytes", run.MaxCaptureBytes)
	}
	ids, err := parseSessionIDs(string(res.Stdout))
	if err != nil {
		return nil, err
	}

	live := make([]controller.LiveSession, 0, len(ids))
	for _, id := range ids {
		var values [4]string
		for i, format := range fieldFormats {
			value, err := c.field(ctx, id, format)
			if err != nil {
				return nil, err
			}
			values[i] = value
		}
		if values[0] == "" {
			// Real sessions cannot have empty names. A session that
			// vanished between the phases surfaces here: a dead -t
			// target can exit 0 with empty output.
			return nil, fmt.Errorf("tmux reported an empty name for session %s", id)
		}
		live = append(live, controller.LiveSession{
			Name:        values[0],
			WorkspaceID: values[1],
			Slug:        values[2],
			Worktree:    values[3],
		})
	}
	return live, nil
}

// field reads one raw value for one session.
func (c *Client) field(ctx context.Context, id, format string) (string, error) {
	res, err := c.exec(ctx, "display-message", "-p", "-t", id, format)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("tmux display-message for session %s exited %d: %s",
			id, res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	if res.StdoutTruncated {
		return "", fmt.Errorf("tmux display-message output exceeded %d bytes", run.MaxCaptureBytes)
	}
	return valueFromOutput(res.Stdout), nil
}

// exec runs one tmux subprocess with the client's socket and timeout.
func (c *Client) exec(ctx context.Context, args ...string) (run.Result, error) {
	argv := []string{tmuxBinary}
	if c.Socket != "" {
		argv = append(argv, "-L", c.Socket)
	}
	argv = append(argv, args...)

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	res, err := run.Run(ctx, run.Command{Argv: argv, Timeout: timeout})
	if err != nil {
		return run.Result{}, fmt.Errorf("observing tmux: %w", err)
	}
	return res, nil
}

// ObserveSession implements controller.SessionObserver by filtering the
// bulk observation in-process.
func (c *Client) ObserveSession(ctx context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	live, err := c.Sessions(ctx)
	if err != nil {
		return controller.SessionObservation{}, err
	}
	return matchSessions(live, q)
}
