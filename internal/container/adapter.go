package container

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/run"
	"github.com/gambtho/projectmux/internal/state"
)

// ProbeContainer checks one stored binding's liveness with a single
// docker inspect. Present carries the binding's identity forward; the
// caller renders errors as unknown, never as absence (design §9).
func (a *Adapter) ProbeContainer(ctx context.Context, binding state.ContainerBinding) (controller.ContainerObservation, error) {
	res, err := run.Run(ctx, run.Command{
		Argv:    []string{dockerBinary, "inspect", "-f", "{{.State.Running}}", binding.ContainerID},
		Timeout: a.timeout(),
	})
	if err != nil {
		return controller.ContainerObservation{}, fmt.Errorf("probing the container: %w", err)
	}
	health, err := classifyInspect(res)
	if err != nil {
		return controller.ContainerObservation{}, err
	}
	obs := controller.ContainerObservation{Health: health}
	if health == state.HealthPresent {
		obs.Kind = binding.Kind
		obs.ContainerID = binding.ContainerID
		obs.ContainerUser = binding.ContainerUser
		obs.Workdir = binding.Workdir
	}
	return obs, nil
}

// Applies reports whether a container applies to the workspace at all:
// enabled true always applies; auto applies exactly when a devcontainer
// configuration exists on disk. Stat errors other than not-exist are
// errors (the unknown funnel), never treated as absence.
func (a *Adapter) Applies(_ context.Context, ws resolve.Workspace, cfg config.Config) (bool, error) {
	switch cfg.DevContainer.Enabled {
	case "true":
		return true, nil
	case "auto":
	default:
		return false, nil
	}
	for _, p := range configPaths(ws.Worktree, cfg) {
		_, err := os.Stat(p)
		switch {
		case err == nil:
			return true, nil
		case os.IsNotExist(err):
		default:
			return false, fmt.Errorf("checking for a devcontainer configuration: %w", err)
		}
	}
	return false, nil
}

func configPaths(worktree string, cfg config.Config) []string {
	if cfg.DevContainer.Config != nil && *cfg.DevContainer.Config != "" {
		return []string{filepath.Join(worktree, *cfg.DevContainer.Config)}
	}
	return []string{
		filepath.Join(worktree, ".devcontainer", "devcontainer.json"),
		filepath.Join(worktree, ".devcontainer.json"),
	}
}

// DiscoverContainer finds an existing container for an unbound
// workspace by the devcontainer CLI's label. Health stays truthful —
// a running match is present with an incomplete binding (no
// user/workdir: labels cannot supply them), which the plan turns into
// acquire; missing means confirmed absence; ambiguity is an error.
func (a *Adapter) DiscoverContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (*controller.ContainerObservation, error) {
	applies, err := a.Applies(ctx, ws, cfg)
	if err != nil {
		return nil, err
	}
	if !applies {
		return nil, nil
	}

	res, err := run.Run(ctx, run.Command{
		Argv: []string{dockerBinary, "ps", "-a",
			"--filter", "label=devcontainer.local_folder=" + ws.Worktree,
			"--format", "{{.ID}}\t{{.State}}"},
		Timeout: a.timeout(),
	})
	if err != nil {
		return nil, fmt.Errorf("discovering containers: %w", err)
	}
	if res.ExitCode != 0 {
		return nil, fmt.Errorf("docker ps exited %d: %s", res.ExitCode, boundedStderr(res.Stderr))
	}

	var running, stopped []string
	for _, line := range strings.Split(strings.TrimSpace(string(res.Stdout)), "\n") {
		if line == "" {
			continue
		}
		id, stateWord, ok := strings.Cut(line, "\t")
		if !ok {
			return nil, fmt.Errorf("docker ps produced an unrecognized line %q", line)
		}
		if stateWord == "running" {
			running = append(running, id)
		} else {
			stopped = append(stopped, id)
		}
	}

	switch {
	case len(running) > 1:
		return nil, fmt.Errorf(
			"%d running containers carry this workspace's devcontainer label; refusing to choose", len(running))
	case len(running) == 1:
		return &controller.ContainerObservation{
			Health: state.HealthPresent, Kind: "devcontainer", ContainerID: running[0],
		}, nil
	case len(stopped) > 0:
		return &controller.ContainerObservation{
			Health: state.HealthMissing, Kind: "devcontainer", ContainerID: stopped[0],
		}, nil
	}
	return &controller.ContainerObservation{
		Health: state.HealthMissing, Kind: "devcontainer",
	}, nil
}

// StartContainer runs the idempotent devcontainer up, bounded by the
// configuration's start_timeout (the struct field overrides for tests),
// and validates the result before anything downstream sees it. All
// failures are typed ContainerStartErrors preserving the real exit
// status and bounded stderr (design §9).
func (a *Adapter) StartContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (controller.ContainerObservation, error) {
	timeout := a.StartTimeout
	if timeout <= 0 {
		timeout = time.Duration(cfg.DevContainer.StartTimeout)
	}
	argv := []string{devcontainerBinary, "up", "--workspace-folder", ws.Worktree}
	if cfg.DevContainer.Config != nil && *cfg.DevContainer.Config != "" {
		argv = append(argv, "--config", filepath.Join(ws.Worktree, *cfg.DevContainer.Config))
	}

	res, err := run.Run(ctx, run.Command{Argv: argv, Timeout: timeout})
	if err != nil {
		return controller.ContainerObservation{}, &controller.ContainerStartError{
			ExitCode: -1,
			Stderr:   boundedStderr(res.Stderr),
			TimedOut: errors.Is(err, context.DeadlineExceeded),
			Reason:   err.Error(),
		}
	}
	if res.ExitCode != 0 {
		reason := fmt.Sprintf("devcontainer up exited %d", res.ExitCode)
		if parsed, perr := parseUpResult(res.Stdout); perr == nil && parsed.Message != "" {
			reason = parsed.Message
		} else if perr != nil {
			reason = perr.Error()
		}
		return controller.ContainerObservation{}, &controller.ContainerStartError{
			ExitCode: res.ExitCode,
			Stderr:   boundedStderr(res.Stderr),
			Reason:   reason,
		}
	}
	parsed, err := parseUpResult(res.Stdout)
	if err != nil {
		return controller.ContainerObservation{}, &controller.ContainerStartError{
			ExitCode: res.ExitCode,
			Stderr:   boundedStderr(res.Stderr),
			Reason:   err.Error(),
		}
	}
	return controller.ContainerObservation{
		Kind:          "devcontainer",
		ContainerID:   parsed.ContainerID,
		ContainerUser: parsed.RemoteUser,
		Workdir:       parsed.RemoteWorkspaceFolder,
		Health:        state.HealthPresent,
	}, nil
}
