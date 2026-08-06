package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/container"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/doctor"
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

// sessionListerFunc adapts the liveSessions seam to doctor's bulk
// enumeration interface.
type sessionListerFunc func(ctx context.Context) ([]controller.LiveSession, error)

func (f sessionListerFunc) Sessions(ctx context.Context) ([]controller.LiveSession, error) {
	return f(ctx)
}

// inspectDatabase is doctor's database seam. It never returns an error:
// every outcome — missing, unreadable, corrupt, drifted — is a finding
// the report carries. The store is non-nil only when the view is safe to
// query, and the returned close is always safe to call.
//
// Doctor deliberately does not go through openStore: state.Open creates
// the file, enables WAL, and migrates, and a command that reports
// "migrations pending" must not perform them in the same breath. The
// database's own bytes are never written, and nothing new appears beside
// it either — see state.OpenReadOnly for how the -shm/-wal sidecars a WAL
// reader would otherwise leave behind are avoided.
var inspectDatabase = func(root string) (doctor.Database, doctor.Store, func()) {
	db := doctor.Database{Path: state.DBPath(root), Supported: state.SchemaVersion}
	ro, insp, err := state.OpenReadOnly(root)
	if err != nil {
		if state.IsMissingDatabase(err) {
			// A fresh installation: nothing is registered, which is a
			// fact rather than uncertainty.
			db.Missing = true
		} else {
			db.OpenErr = err
		}
		return db, nil, func() {}
	}
	closeDB := func() { _ = ro.Close() }
	db.IntegrityErr = insp.IntegrityErr
	db.Version = insp.UserVersion
	if insp.Usable() != nil {
		return db, nil, closeDB
	}
	return db, ro, closeDB
}

// newVersionRunner is doctor's dependency-probe seam.
var newVersionRunner = func() doctor.VersionRunner { return toolProbe{} }

// toolProbe runs one tool's version command. PATH resolution is separate
// from execution so an absent binary is reported as confirmed absence
// rather than as an execution failure.
type toolProbe struct{}

func (toolProbe) Probe(ctx context.Context, argv ...string) (doctor.ProbeResult, error) {
	if len(argv) == 0 {
		return doctor.ProbeResult{}, errors.New("probe requires a command")
	}
	if _, err := exec.LookPath(argv[0]); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return doctor.ProbeResult{}, nil
		}
		return doctor.ProbeResult{}, fmt.Errorf("finding %s: %w", argv[0], err)
	}
	res, err := runner.Run(ctx, runner.Command{Argv: argv, Timeout: tmux.DefaultTimeout})
	if err != nil {
		return doctor.ProbeResult{}, err
	}
	return doctor.ProbeResult{
		Stdout:   string(bytes.TrimSpace(res.Stdout)),
		Stderr:   string(bytes.TrimSpace(res.Stderr)),
		ExitCode: res.ExitCode,
		Found:    true,
	}, nil
}
