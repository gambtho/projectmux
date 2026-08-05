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
	AdoptSessionName(workspaceID, name string, now time.Time) error
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
	// Applies reports whether a container applies to the workspace at
	// all — enabled true always applies; auto applies when a
	// devcontainer configuration exists on disk. Observe consults it
	// before probing stored bindings under auto, so removing the
	// configuration de-containerizes the workspace.
	Applies(ctx context.Context, ws resolve.Workspace, cfg config.Config) (bool, error)
}

// ContainerActuator performs container mutations and renders container
// execution requests (design §5). StartContainer is the idempotent
// devcontainer up; ExecCommand is pure rendering from the stored
// binding.
type ContainerActuator interface {
	StartContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (ContainerObservation, error)
	ExecCommand(binding state.ContainerBinding, command, relDir string, env map[string]string) string
}

// WindowLocation is a window's configured placement. The zero value is
// auto: container when one applies to the workspace, host otherwise.
type WindowLocation string

const (
	WindowAuto      WindowLocation = ""
	WindowHost      WindowLocation = "host"
	WindowContainer WindowLocation = "container"
)

// WindowIntent is a window as configuration describes it, before the
// container binding is known. Ensure renders intents into concrete
// WindowSpecs after its container phase.
type WindowIntent struct {
	Name     string
	Command  string // empty => shell window
	RelDir   string // config cwd, relative; "" => workspace root
	Focus    bool
	Location WindowLocation
}

// Clock supplies the timestamps the store persists.
type Clock interface {
	Now() time.Time
}

// WindowSpec is one window the actuator creates. An empty Command means
// the default shell; Dir is absolute (derivation resolves relative
// cwds against the worktree).
type WindowSpec struct {
	Name    string
	Command string
	Dir     string
	Focus   bool
}

// SessionSpec is everything CreateSession needs. Env carries the
// workspace's configured environment: it is part of the digested desired
// document and must reach every window (open/attach spec §4). Windows is
// never empty — derivation supplies an implicit shell window.
type SessionSpec struct {
	Name        string
	WorkspaceID string
	Slug        string
	Worktree    string
	Env         map[string]string
	Windows     []WindowSpec
}

// SessionActuator creates the workspace session. It is the mutating
// counterpart of SessionObserver; adapters implement both.
type SessionActuator interface {
	CreateSession(ctx context.Context, spec SessionSpec) error
}
