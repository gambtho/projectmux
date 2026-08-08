package controller

import (
	"context"
	"time"

	"github.com/gambtho/projectmux/internal/lock"
)

// lockPhases takes the workspace lock, and the repository lock ahead of
// it when the command has a container phase, in the order the lock
// package documents; the returned release unwinds them in reverse. An
// empty repositoryID means no container phase: a stop that leaves the
// container alone must not queue behind a sibling's devcontainer up.
func lockPhases(ctx context.Context, dir, repositoryID, workspaceID string, timeout time.Duration) (func(), error) {
	releaseRepository := func() {}
	if repositoryID != "" {
		l, err := lock.Acquire(ctx, dir, repositoryID, timeout)
		if err != nil {
			return nil, err
		}
		releaseRepository = func() { _ = l.Release() }
	}
	workspace, err := lock.Acquire(ctx, dir, workspaceID, timeout)
	if err != nil {
		releaseRepository()
		return nil, err
	}
	return func() {
		_ = workspace.Release()
		releaseRepository()
	}, nil
}
