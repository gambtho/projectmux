package container

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("docker daemon is not reachable")
	}
}

// TestIntegrationProbeLifecycle runs a real container through
// running -> stopped -> removed and asserts the classifications.
func TestIntegrationProbeLifecycle(t *testing.T) {
	requireDocker(t)
	name := fmt.Sprintf("projectmux-it-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--rm=false", "--name", name,
		"busybox:latest", "sleep", "300").Output()
	if err != nil {
		t.Skipf("docker run failed (image pull?): %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	a := &Adapter{}
	binding := state.ContainerBinding{
		Kind: "devcontainer", ContainerID: id,
		ContainerUser: "root", Workdir: "/tmp", Health: state.HealthUnknown,
	}

	obs, err := a.ProbeContainer(context.Background(), binding)
	if err != nil || obs.Health != state.HealthPresent {
		t.Fatalf("running probe = (%+v, %v), want present", obs, err)
	}

	if err := exec.Command("docker", "stop", "-t", "0", id).Run(); err != nil {
		t.Fatalf("docker stop: %v", err)
	}
	obs, err = a.ProbeContainer(context.Background(), binding)
	if err != nil || obs.Health != state.HealthMissing {
		t.Fatalf("stopped probe = (%+v, %v), want missing", obs, err)
	}

	if err := exec.Command("docker", "rm", "-f", id).Run(); err != nil {
		t.Fatalf("docker rm: %v", err)
	}
	obs, err = a.ProbeContainer(context.Background(), binding)
	if err != nil || obs.Health != state.HealthMissing {
		t.Fatalf("removed probe = (%+v, %v), want missing", obs, err)
	}
}

// TestIntegrationExecRoundTrip proves a rendered ExecCommand actually
// executes in a container via the pane shell it is written for.
func TestIntegrationExecRoundTrip(t *testing.T) {
	requireDocker(t)
	name := fmt.Sprintf("projectmux-exec-%d", os.Getpid())
	out, err := exec.Command("docker", "run", "-d", "--name", name,
		"busybox:latest", "sleep", "300").Output()
	if err != nil {
		t.Skipf("docker run failed: %v", err)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })

	a := &Adapter{}
	binding := state.ContainerBinding{
		Kind: "devcontainer", ContainerID: id, Workdir: "/tmp", Health: state.HealthPresent,
	}
	cmd := a.ExecCommand(binding, `printf 'RT=%s\n' "$INSIDE"`, "", map[string]string{"INSIDE": "yes"})
	// A pane runs this string through the default shell; -t needs a tty,
	// which `go test` lacks, so drop it for the round trip only.
	cmd = strings.Replace(cmd, " -t ", " ", 1)
	rtOut, err := exec.Command("/bin/sh", "-c", cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("exec round trip: %v\n%s", err, rtOut)
	}
	if !strings.Contains(string(rtOut), "RT=yes") {
		t.Errorf("output %q lacks RT=yes; env did not reach the container", rtOut)
	}
}

// TestIntegrationDevcontainerUp is local-only: GitHub runners lack the
// devcontainer CLI. It exercises up, JSON parsing, discovery by label,
// and the idempotent re-up.
func TestIntegrationDevcontainerUp(t *testing.T) {
	requireDocker(t)
	if _, err := exec.LookPath("devcontainer"); err != nil {
		t.Skip("devcontainer CLI is not installed")
	}
	worktree := t.TempDir()
	if err := os.MkdirAll(worktree+"/.devcontainer", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(worktree+"/.devcontainer/devcontainer.json",
		[]byte(`{"image": "alpine:3.20"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	ws := resolve.Workspace{ID: "it", Slug: "it", Worktree: worktree}
	cfg := config.Config{DevContainer: config.DevContainer{
		Enabled: "true", StartTimeout: config.Duration(4 * time.Minute),
	}}

	a := &Adapter{}
	obs, err := a.StartContainer(context.Background(), ws, cfg)
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", obs.ContainerID).Run() })
	if obs.Health != state.HealthPresent || obs.ContainerID == "" || obs.Workdir == "" {
		t.Fatalf("observation = %+v", obs)
	}

	disc, err := a.DiscoverContainer(context.Background(), ws, cfg)
	if err != nil || disc == nil || disc.Health != state.HealthPresent ||
		disc.ContainerID[:12] != obs.ContainerID[:12] {
		t.Errorf("discovery = (%+v, %v), want present-incomplete with the same container", disc, err)
	}

	again, err := a.StartContainer(context.Background(), ws, cfg)
	if err != nil || again.ContainerID != obs.ContainerID {
		t.Errorf("idempotent re-up = (%+v, %v), want the same container", again, err)
	}
}
