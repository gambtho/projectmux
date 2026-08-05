package controller

import (
	"context"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// ContainerObservation is a live observation from the container adapter,
// as opposed to state.ContainerObservation, which is the form the store
// records.
type ContainerObservation struct {
	Kind          string
	ContainerID   string
	ContainerUser string
	Workdir       string
	Health        state.Health
}

// Store is the slice of the state store the controller uses. *state.Store
// satisfies it; fakes mirror its semantics for tests.
type Store interface {
	RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error
	AllocateSessionName(workspaceID string, now time.Time) (string, error)
	RecordContainerObservation(workspaceID string, obs state.ContainerObservation, now time.Time) error
	RecordOperation(workspaceID string, op state.Operation, now time.Time) error
	CommitReconciliation(workspaceID string, r state.ReconciliationResult, now time.Time) error
	Workspace(id string) (state.Record, error)
	Workspaces() ([]state.Record, error)
}

var _ Store = (*state.Store)(nil)

// SessionObserver reports live tmux sessions. Its granularity is
// provisional until the tmux adapter slice exists.
type SessionObserver interface {
	ObserveSession(ctx context.Context, q SessionQuery) (SessionObservation, error)
}

// ContainerObserver probes an existing binding or discovers a container
// for a workspace that has none — post-rebuild reacquisition and
// devcontainer.enabled "auto" both need the workspace and configuration.
//
// DiscoverContainer returning (nil, nil) means no container applies to
// this workspace: "auto" resolved to none. Observe then treats the
// workspace exactly like enabled "false". ProbeContainer returns a value
// because a binding always applies.
type ContainerObserver interface {
	ProbeContainer(ctx context.Context, binding state.ContainerBinding) (ContainerObservation, error)
	DiscoverContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (*ContainerObservation, error)
}

// Clock supplies the timestamps the store persists.
type Clock interface {
	Now() time.Time
}
