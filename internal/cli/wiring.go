package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	runner "github.com/gambtho/projectmux/internal/run"
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

// newSessionActuator is the mutation seam mirroring newSessionObserver.
var newSessionActuator = func() controller.SessionActuator {
	return &tmux.Client{}
}

// containerWindowError reports an explicitly container-located window,
// which this build cannot actuate faithfully: running it on the host
// would silently violate the user's stated intent (open/attach spec §4).
type containerWindowError struct{ window string }

func (e *containerWindowError) Error() string {
	return fmt.Sprintf(
		"window %q is configured with location: container, and container support is not implemented in this build",
		e.window)
}

// windowSpecs derives the actuator windows from merged configuration:
// implicit shell window when none is configured, first window active
// unless one is explicitly focused, relative cwds joined to the
// worktree. It runs before any lock or mutation.
func windowSpecs(cfg config.Config, worktree string) ([]controller.WindowSpec, error) {
	if len(cfg.Windows) == 0 {
		return []controller.WindowSpec{{Name: "shell", Dir: worktree}}, nil
	}
	specs := make([]controller.WindowSpec, 0, len(cfg.Windows))
	for _, w := range cfg.Windows {
		if w.Location != nil && *w.Location == "container" {
			return nil, &containerWindowError{window: w.Name}
		}
		spec := controller.WindowSpec{Name: w.Name, Dir: worktree, Focus: w.Focus}
		switch {
		case w.Agent != nil:
			spec.Command = *w.Agent
		case w.Command != nil:
			spec.Command = *w.Command
		}
		if w.Cwd != nil && *w.Cwd != "" {
			spec.Dir = filepath.Join(worktree, *w.Cwd)
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// hostOnlyContainerObserver is open's container observer while no real
// adapter exists: it answers "does a container apply?" from the
// filesystem alone and funnels everything it cannot honestly answer
// into the unsupported path (open/attach spec §6). Docker is never
// touched. status keeps the plain unprobedObserver.
type hostOnlyContainerObserver struct{}

var _ controller.ContainerObserver = hostOnlyContainerObserver{}

func (hostOnlyContainerObserver) ProbeContainer(context.Context, state.ContainerBinding) (controller.ContainerObservation, error) {
	return controller.ContainerObservation{}, errUnprobed
}

func (hostOnlyContainerObserver) DiscoverContainer(_ context.Context, ws resolve.Workspace, cfg config.Config) (*controller.ContainerObservation, error) {
	if cfg.DevContainer.Enabled == "true" {
		return nil, errUnprobed
	}
	// Observe calls this only for "auto" (and "true", handled above): a
	// container applies exactly when a devcontainer configuration
	// exists on disk.
	for _, p := range devcontainerConfigPaths(ws.Worktree, cfg) {
		if _, err := os.Stat(p); err == nil {
			return nil, errUnprobed
		}
	}
	return nil, nil
}

func devcontainerConfigPaths(worktree string, cfg config.Config) []string {
	if cfg.DevContainer.Config != nil && *cfg.DevContainer.Config != "" {
		return []string{filepath.Join(worktree, *cfg.DevContainer.Config)}
	}
	return []string{
		filepath.Join(worktree, ".devcontainer", "devcontainer.json"),
		filepath.Join(worktree, ".devcontainer.json"),
	}
}

// Terminal attachment seams: a real attach replaces the process and a
// real switch-client needs a live tmux client, so tests substitute all
// three (open/attach spec §8).
var (
	execAttach = func(session string) error {
		path, err := exec.LookPath("tmux")
		if err != nil {
			return fmt.Errorf("finding tmux: %w", err)
		}
		return syscall.Exec(path,
			[]string{"tmux", "attach-session", "-t", "=" + session}, os.Environ())
	}
	switchClient = func(ctx context.Context, session string) error {
		res, err := runner.Run(ctx, runner.Command{
			Argv:    []string{"tmux", "switch-client", "-t", "=" + session},
			Timeout: tmux.DefaultTimeout,
		})
		if err != nil {
			return err
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("tmux switch-client exited %d: %s",
				res.ExitCode, bytes.TrimSpace(res.Stderr))
		}
		return nil
	}
	insideTmux = func() bool { return os.Getenv("TMUX") != "" }
)

// attachTerminal connects the terminal to the session: switch-client
// inside tmux, an exec of attach-session outside (open/attach spec §2).
func attachTerminal(ctx context.Context, session string) error {
	if insideTmux() {
		return switchClient(ctx, session)
	}
	return execAttach(session)
}
