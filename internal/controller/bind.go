package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// SetBind records a session's base directory, or clears it when bind is
// nil. It reports whether the session's record was created by this call.
//
// It takes the workspace lock and nothing else. Binding has no container
// phase, and lockPhases documents an empty repositoryID as exactly that,
// so a bind must not queue behind a sibling's devcontainer up (spec §4).
//
// A session that has never been opened is created here. The desired
// digest is left empty deliberately: bind loads no workspace
// configuration, so it has none to claim, and RegisterWorkspace never
// touches applied_digest — which leaves BuildPlan's nil-digest reapply
// rule (plan.go:71-73) to make the first open converge with no special
// case. Registration runs only when the record is absent, because
// re-registering an existing session would overwrite its desired digest
// with the empty string.
func (c *Controller) SetBind(ctx context.Context, ws resolve.Workspace, bind *string, lockDir string, lockTimeout time.Duration) (bool, error) {
	release, err := lockPhases(ctx, lockDir, "", ws.ID, lockTimeout)
	if err != nil {
		return false, err
	}
	defer release()

	now := c.Clock.Now()
	created := false
	if _, err := c.Store.Workspace(ws.ID); err != nil {
		if !errors.Is(err, state.ErrNotFound) {
			return false, fmt.Errorf("reading the workspace record: %w", err)
		}
		if err := c.Store.RegisterWorkspace(ws, "", now); err != nil {
			return false, fmt.Errorf("registering the workspace: %w", err)
		}
		created = true
	}
	if err := c.Store.SetBind(ws.ID, bind, now); err != nil {
		return created, fmt.Errorf("recording the bind: %w", err)
	}
	return created, nil
}
