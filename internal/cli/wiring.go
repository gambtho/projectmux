package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/container"
	"github.com/gambtho/projectmux/internal/controller"
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

// Container observation and actuation seams; the defaults are the real
// adapter.
var (
	newContainerObserver = func() controller.ContainerObserver {
		return &container.Adapter{}
	}
	newContainerActuator = func() controller.ContainerActuator {
		return &container.Adapter{}
	}
)

// windowIntents derives the actuator window intents purely from merged
// configuration: implicit shell window when none is configured; the
// location tri-state is resolved against the binding inside Ensure.
func windowIntents(cfg config.Config) []controller.WindowIntent {
	if len(cfg.Windows) == 0 {
		return []controller.WindowIntent{{Name: "shell"}}
	}
	intents := make([]controller.WindowIntent, 0, len(cfg.Windows))
	for _, w := range cfg.Windows {
		in := controller.WindowIntent{Name: w.Name, Focus: w.Focus}
		switch {
		case w.Agent != nil:
			in.Command = *w.Agent
		case w.Command != nil:
			in.Command = *w.Command
		}
		if w.Cwd != nil {
			in.RelDir = *w.Cwd
		}
		if w.Location != nil {
			in.Location = controller.WindowLocation(*w.Location)
		}
		intents = append(intents, in)
	}
	return intents
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
