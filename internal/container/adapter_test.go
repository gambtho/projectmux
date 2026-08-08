package container

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// fakeBinary installs a script as one of the adapter's executables.
func fakeBinary(t *testing.T, which *string, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bin")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	orig := *which
	t.Cleanup(func() { *which = orig })
	*which = path
}

func adapterWorkspace(t *testing.T) resolve.Workspace {
	return resolve.Workspace{ID: "w1", RepositoryID: "r1", Slug: "proj", RepoRoot: t.TempDir()}
}

func autoConfig() config.Config {
	return config.Config{DevContainer: config.DevContainer{
		Enabled: "auto", StartTimeout: config.Duration(5 * time.Second),
	}}
}

func enabledConfig() config.Config {
	c := autoConfig()
	c.DevContainer.Enabled = "true"
	return c
}

func writeDevcontainerJSON(t *testing.T, repoRoot string) {
	t.Helper()
	dir := filepath.Join(repoRoot, ".devcontainer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "devcontainer.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAppliesMatrix(t *testing.T) {
	a := &Adapter{}
	ws := adapterWorkspace(t)

	if ok, err := a.Applies(context.Background(), ws, enabledConfig()); err != nil || !ok {
		t.Errorf("enabled true: Applies = (%t, %v), want (true, nil)", ok, err)
	}
	if ok, err := a.Applies(context.Background(), ws, autoConfig()); err != nil || ok {
		t.Errorf("auto without config: Applies = (%t, %v), want (false, nil)", ok, err)
	}
	writeDevcontainerJSON(t, ws.RepoRoot)
	if ok, err := a.Applies(context.Background(), ws, autoConfig()); err != nil || !ok {
		t.Errorf("auto with config: Applies = (%t, %v), want (true, nil)", ok, err)
	}
	off := autoConfig()
	off.DevContainer.Enabled = "false"
	if ok, err := a.Applies(context.Background(), ws, off); err != nil || ok {
		t.Errorf("enabled false: Applies = (%t, %v), want (false, nil)", ok, err)
	}
}

func TestProbeClassifications(t *testing.T) {
	a := &Adapter{}
	binding := state.ContainerBinding{
		Kind: "devcontainer", ContainerID: "c1",
		ContainerUser: "u", Workdir: "/workspaces/x",
	}

	fakeBinary(t, &dockerBinary, "#!/bin/sh\nprintf 'true\\n'\n")
	obs, err := a.ProbeContainer(context.Background(), binding)
	if err != nil || obs.Health != state.HealthPresent || obs.Workdir != "/workspaces/x" {
		t.Errorf("running probe = (%+v, %v); present must carry the binding", obs, err)
	}

	fakeBinary(t, &dockerBinary, "#!/bin/sh\nprintf 'false\\n'\n")
	obs, err = a.ProbeContainer(context.Background(), binding)
	if err != nil || obs.Health != state.HealthMissing {
		t.Errorf("stopped probe = (%+v, %v), want missing", obs, err)
	}

	fakeBinary(t, &dockerBinary,
		"#!/bin/sh\necho 'error: no such object: c1' 1>&2\nexit 1\n")
	obs, err = a.ProbeContainer(context.Background(), binding)
	if err != nil || obs.Health != state.HealthMissing {
		t.Errorf("removed probe = (%+v, %v), want missing", obs, err)
	}

	fakeBinary(t, &dockerBinary,
		"#!/bin/sh\necho 'Cannot connect to the Docker daemon' 1>&2\nexit 1\n")
	if _, err := a.ProbeContainer(context.Background(), binding); err == nil {
		t.Error("daemon-down probe did not error (unknown funnel)")
	}
}

func TestDiscoverShapes(t *testing.T) {
	a := &Adapter{}
	ws := adapterWorkspace(t)
	writeDevcontainerJSON(t, ws.RepoRoot)

	fakeBinary(t, &dockerBinary, "#!/bin/sh\nprintf 'abc123\\trunning\\n'\n")
	obs, err := a.DiscoverContainer(context.Background(), ws, autoConfig())
	if err != nil || obs == nil || obs.Health != state.HealthPresent ||
		obs.ContainerID != "abc123" || obs.Workdir != "" {
		t.Errorf("running match = (%+v, %v), want present-incomplete", obs, err)
	}

	fakeBinary(t, &dockerBinary, "#!/bin/sh\nprintf 'abc123\\texited\\n'\n")
	obs, err = a.DiscoverContainer(context.Background(), ws, autoConfig())
	if err != nil || obs == nil || obs.Health != state.HealthMissing || obs.ContainerID != "abc123" {
		t.Errorf("stopped match = (%+v, %v), want missing with id", obs, err)
	}

	fakeBinary(t, &dockerBinary, "#!/bin/sh\nprintf ''\n")
	obs, err = a.DiscoverContainer(context.Background(), ws, autoConfig())
	if err != nil || obs == nil || obs.Health != state.HealthMissing || obs.ContainerID != "" {
		t.Errorf("no match = (%+v, %v), want bare missing", obs, err)
	}

	fakeBinary(t, &dockerBinary,
		"#!/bin/sh\nprintf 'a1\\trunning\\nb2\\trunning\\n'\n")
	if _, err := a.DiscoverContainer(context.Background(), ws, autoConfig()); err == nil {
		t.Error("ambiguous running matches did not error")
	}

	// auto without configuration: no docker call at all.
	bare := adapterWorkspace(t)
	fakeBinary(t, &dockerBinary, "#!/bin/sh\nexit 9\n")
	obs, err = a.DiscoverContainer(context.Background(), bare, autoConfig())
	if err != nil || obs != nil {
		t.Errorf("not-applicable discover = (%+v, %v), want (nil, nil)", obs, err)
	}
}

func TestStartContainerSuccessAndFailures(t *testing.T) {
	a := &Adapter{}
	ws := adapterWorkspace(t)

	fakeBinary(t, &devcontainerBinary, "#!/bin/sh\n"+
		"echo 'progress noise' 1>&2\n"+
		`printf '{"outcome":"success","containerId":"c9","remoteUser":"vscode","remoteWorkspaceFolder":"/workspaces/proj"}\n'`+"\n")
	obs, err := a.StartContainer(context.Background(), ws, enabledConfig())
	if err != nil {
		t.Fatalf("StartContainer: %v", err)
	}
	want := controller.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c9",
		ContainerUser: "vscode", Workdir: "/workspaces/proj",
		Health: state.HealthPresent,
	}
	if obs != want {
		t.Errorf("observation = %+v, want %+v", obs, want)
	}

	fakeBinary(t, &devcontainerBinary, "#!/bin/sh\n"+
		"echo 'stderr detail' 1>&2\n"+
		`printf '{"outcome":"error","message":"build failed"}\n'`+"\nexit 1\n")
	_, err = a.StartContainer(context.Background(), ws, enabledConfig())
	var start *controller.ContainerStartError
	if !errors.As(err, &start) {
		t.Fatalf("err = %v, want *ContainerStartError", err)
	}
	if start.ExitCode != 1 || !strings.Contains(start.Stderr, "stderr detail") || start.TimedOut {
		t.Errorf("StartError = %+v", start)
	}

	fakeBinary(t, &devcontainerBinary, "#!/bin/sh\necho 'partial stderr' 1>&2\nsleep 10\n")
	a.StartTimeout = 300 * time.Millisecond
	t.Cleanup(func() { a.StartTimeout = 0 })
	_, err = a.StartContainer(context.Background(), ws, enabledConfig())
	if !errors.As(err, &start) {
		t.Fatalf("timeout err = %v, want *ContainerStartError", err)
	}
	if !start.TimedOut || !strings.Contains(start.Stderr, "partial stderr") {
		t.Errorf("timeout StartError = %+v; partial stderr must be preserved", start)
	}
}

func TestStopContainerClassifications(t *testing.T) {
	a := &Adapter{}

	fakeBinary(t, &dockerBinary, "#!/bin/sh\nexit 0\n")
	if err := a.StopContainer(context.Background(), "c1"); err != nil {
		t.Fatalf("StopContainer: %v", err)
	}

	// A removed container is the goal state (verified docker error shape).
	fakeBinary(t, &dockerBinary,
		"#!/bin/sh\necho 'Error response from daemon: No such container: c1' 1>&2\nexit 1\n")
	if err := a.StopContainer(context.Background(), "c1"); err != nil {
		t.Fatalf("removed-container stop = %v, want nil", err)
	}

	fakeBinary(t, &dockerBinary,
		"#!/bin/sh\necho 'Cannot connect to the Docker daemon' 1>&2\nexit 1\n")
	if err := a.StopContainer(context.Background(), "c1"); err == nil {
		t.Fatal("a daemon failure was swallowed")
	}
}

// recordArgv installs a script that appends its argv to log and writes
// stdout, so a test can compare the argv two calls produced.
func recordArgv(t *testing.T, which *string, log, stdout string) {
	t.Helper()
	fakeBinary(t, which, "#!/bin/sh\n"+
		"printf '%s\\n' \"$*\" >>'"+log+"'\n"+
		"printf '%s' '"+stdout+"'\n")
}

func argvLines(t *testing.T, log string) []string {
	t.Helper()
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("reading the argv log: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

// TestContainersAreKeyedOnTheRepositoryRoot states the reported defect as
// a test: two sessions on one repository address one container, so every
// docker and devcontainer invocation they produce must be byte-identical
// and must name the repository root rather than any per-session path.
func TestContainersAreKeyedOnTheRepositoryRoot(t *testing.T) {
	a := &Adapter{}
	root := t.TempDir()
	writeDevcontainerJSON(t, root)
	sessions := []resolve.Workspace{
		{ID: "w1", RepositoryID: "r1", Slug: "proj", RepoRoot: root, SessionName: "proj"},
		{ID: "w2", RepositoryID: "r1", Slug: "proj", RepoRoot: root,
			Session: "review", SessionName: "proj--review"},
	}

	upLog := filepath.Join(t.TempDir(), "up.argv")
	recordArgv(t, &devcontainerBinary, upLog,
		`{"outcome":"success","containerId":"c9","remoteUser":"vscode","remoteWorkspaceFolder":"/workspaces/proj"}`)
	for _, ws := range sessions {
		if _, err := a.StartContainer(context.Background(), ws, enabledConfig()); err != nil {
			t.Fatalf("StartContainer(%s): %v", ws.ID, err)
		}
	}
	up := argvLines(t, upLog)
	if len(up) != 2 || up[0] != up[1] {
		t.Fatalf("devcontainer up argv = %q; both sessions must start one container", up)
	}
	if !strings.Contains(up[0], "--workspace-folder "+root) {
		t.Errorf("argv %q does not target the repository root %s", up[0], root)
	}

	psLog := filepath.Join(t.TempDir(), "ps.argv")
	recordArgv(t, &dockerBinary, psLog, "")
	for _, ws := range sessions {
		if _, err := a.DiscoverContainer(context.Background(), ws, autoConfig()); err != nil {
			t.Fatalf("DiscoverContainer(%s): %v", ws.ID, err)
		}
	}
	ps := argvLines(t, psLog)
	if len(ps) != 2 || ps[0] != ps[1] {
		t.Fatalf("docker ps argv = %q; both sessions must discover by one filter", ps)
	}
	if !strings.Contains(ps[0], "label=devcontainer.local_folder="+root) {
		t.Errorf("filter %q does not key on the repository root %s", ps[0], root)
	}
}
