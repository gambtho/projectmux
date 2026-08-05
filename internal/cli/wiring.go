package cli

import (
	"context"
	"errors"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
)

// stateStore is what the observation commands need from the state store.
type stateStore interface {
	controller.Store
	Close() error
}

// The observation commands' dependencies are package variables so command
// tests can substitute fakes; the defaults are the production wiring.
var (
	openStore = func() (stateStore, error) {
		root, err := state.Root()
		if err != nil {
			return nil, err
		}
		return state.Open(root)
	}
	liveSessions = func(ctx context.Context) ([]controller.LiveSession, error) {
		return (&tmux.Client{}).Sessions(ctx)
	}
	newSessionObserver = func() controller.SessionObserver {
		return &tmux.Client{}
	}
)

// errUnprobed explains why no live container observation exists yet.
var errUnprobed = errors.New("container probing is not implemented in this build")

// unprobedObserver is the honest stand-in for the future container
// adapter: every observation fails, so snapshots carry health=unknown
// and plans say probe-first, while rendered container facts come from
// the stored binding, explicitly labeled as unprobed. It never pretends
// to be a live probe (spec §6).
type unprobedObserver struct{}

var _ controller.ContainerObserver = unprobedObserver{}

func (unprobedObserver) ProbeContainer(context.Context, state.ContainerBinding) (controller.ContainerObservation, error) {
	return controller.ContainerObservation{}, errUnprobed
}

func (unprobedObserver) DiscoverContainer(context.Context, resolve.Workspace, config.Config) (*controller.ContainerObservation, error) {
	return nil, errUnprobed
}

// systemClock satisfies controller.Clock. Nothing is persisted by the
// observation commands, but the controller requires a clock.
type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// stamp renders a stored timestamp for output, matching the store's
// RFC3339Nano UTC convention.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

// storedContainerInfo is the stored binding as rendered in JSON
// envelopes. It is always last-observed state, never a live probe.
type storedContainerInfo struct {
	Kind          string `json:"kind"`
	ContainerID   string `json:"container_id"`
	ContainerUser string `json:"container_user,omitempty"`
	Workdir       string `json:"workdir,omitempty"`
	Health        string `json:"health"`
	ObservedAt    string `json:"observed_at"`
}

func storedContainer(b *state.ContainerBinding) *storedContainerInfo {
	if b == nil {
		return nil
	}
	return &storedContainerInfo{
		Kind:          b.Kind,
		ContainerID:   b.ContainerID,
		ContainerUser: b.ContainerUser,
		Workdir:       b.Workdir,
		Health:        string(b.Health),
		ObservedAt:    stamp(b.ObservedAt),
	}
}
