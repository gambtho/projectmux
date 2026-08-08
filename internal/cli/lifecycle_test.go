package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
	"github.com/gambtho/projectmux/internal/tmux"
)

// lifecycleRig points the CLI seams at a real tmux server on an
// isolated socket and the real SQLite store in a temp state root, then
// runs commands through Main exactly as a user would.
func lifecycleRig(t *testing.T, label string) (resolve.Workspace, string) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	workspace(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/slabledger.yaml": validConfig,
	})
	t.Setenv("PROJECTMUX_STATE_ROOT", t.TempDir())

	socket := fmt.Sprintf("projectmux-life-%s-%d", label, os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})

	origObs, origAct := newSessionObserver, newSessionActuator
	t.Cleanup(func() { newSessionObserver, newSessionActuator = origObs, origAct })
	newSessionObserver = func() controller.SessionObserver {
		return &tmux.Client{Socket: socket}
	}
	newSessionActuator = func() controller.SessionActuator {
		return &tmux.Client{Socket: socket}
	}
	origLive := liveSessions
	t.Cleanup(func() { liveSessions = origLive })
	liveSessions = func(ctx context.Context) ([]controller.LiveSession, error) {
		return (&tmux.Client{Socket: socket}).Sessions(ctx)
	}
	installContainerObserver(t, &fake.ContainerObserver{})

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	ws, err := resolve.Resolve("", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ws, socket
}

func openJSON(t *testing.T) openEnvelope {
	t.Helper()
	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("open exit %d, stderr: %s", code, stderr)
	}
	var env openEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding: %v\n%s", err, stdout)
	}
	return env
}

func TestLifecycleOpenReopenAttach(t *testing.T) {
	ws, socket := lifecycleRig(t, "openreopen")

	created := openJSON(t)
	if created.Action != "created" || created.Session != ws.SessionName {
		t.Fatalf("first open = %+v", created)
	}

	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].WorkspaceID != ws.ID || live[0].Worktree != ws.RepoRoot {
		t.Fatalf("live = %+v", live)
	}

	reopened := openJSON(t)
	if reopened.Action != "already-running" || reopened.Session != ws.SessionName {
		t.Errorf("reopen = %+v, want already-running (idempotent reopen)", reopened)
	}
	if reopened.Drifted {
		t.Errorf("reopen drifted; creation should have recorded the applied digest")
	}

	code, stdout, stderr := run(t, "attach", "--json")
	if code != 0 {
		t.Fatalf("attach exit %d, stderr: %s", code, stderr)
	}
	var att attachEnvelope
	if err := json.Unmarshal([]byte(stdout), &att); err != nil {
		t.Fatalf("decoding attach: %v", err)
	}
	if att.Session.State != "live" || att.Session.Identity == nil ||
		*att.Session.Identity != "match" {
		t.Errorf("attach = %+v", att.Session)
	}
}

// TestLifecycleAdoptsPhaseOneSession is design §13 step 7: a live
// session created outside projectmux but carrying the three identity
// keys is adopted — never recreated, renamed, or wrongly attached.
func TestLifecycleAdoptsPhaseOneSession(t *testing.T) {
	ws, socket := lifecycleRig(t, "adopt")

	for _, args := range [][]string{
		{"new-session", "-d", "-s", "bash-era", "-c", ws.RepoRoot},
		{"set-option", "-t", "bash-era", controller.KeyWorkspaceID, ws.ID},
		{"set-option", "-t", "bash-era", controller.KeySlug, ws.Slug},
		{"set-option", "-t", "bash-era", controller.KeyWorktree, ws.RepoRoot},
	} {
		full := append([]string{"-L", socket}, args...)
		if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
			t.Fatalf("tmux %v: %v\n%s", args, err, out)
		}
	}

	env := openJSON(t)
	if env.Action != "adopted" || env.Session != "bash-era" {
		t.Fatalf("open = %+v, want adoption of bash-era", env)
	}
	if !env.Drifted {
		t.Error("adoption cleared drift without applying configuration")
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != "bash-era" {
		t.Errorf("live = %+v; adoption created or renamed sessions", live)
	}
}

func TestLifecycleForeignSquatterRefuses(t *testing.T) {
	ws, socket := lifecycleRig(t, "squat")

	full := []string{"-L", socket, "new-session", "-d", "-s", ws.SessionName}
	if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
		t.Fatalf("seeding the squatter: %v\n%s", err, out)
	}

	code, stdout, stderr := run(t, "open", "--json")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d (stdout %q, stderr %q)", code, ExitRefused, stdout, stderr)
	}

	code, _, _ = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
}

func TestLifecycleConcurrentOpensCreateExactlyOnce(t *testing.T) {
	ws, socket := lifecycleRig(t, "race")

	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			code, _, _ := run(t, "open", "--json")
			codes[i] = code
		}(i)
	}
	wg.Wait()

	for i, code := range codes {
		if code != 0 {
			t.Errorf("open %d exited %d", i, code)
		}
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].WorkspaceID != ws.ID {
		t.Errorf("live = %+v, want exactly one session", live)
	}
}

// TestLifecycleContainerWorkspace: real tmux, fake container tooling
// (design §12): open ensures the container, the container window's
// command is the rendered exec request, reopen is idempotent (no second
// start), and a failing start creates no session.
func TestLifecycleContainerWorkspace(t *testing.T) {
	ws, socket := lifecycleRig(t, "container")
	actC := installContainerActuator(t)
	// The default fake marker is not runnable; a real pane would exit
	// and close its window. Substitute a long-running command so the
	// rendered container windows survive for assertion, and assert the
	// original commands through the actuator's Execs record.
	actC.ExecResult = "sleep 300"
	obs := &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}
	installContainerObserver(t, obs)

	env := openJSON(t)
	if env.Action != "created" {
		t.Fatalf("open = %+v", env)
	}
	if env.Container == nil || env.Container.Health != "present" {
		t.Fatalf("container block = %+v", env.Container)
	}
	if len(actC.Started) != 1 {
		t.Fatalf("StartContainer calls = %d, want 1", len(actC.Started))
	}

	// The auto windows rendered through the container actuator: the
	// windows exist in real tmux (running the substituted command), and
	// the Execs record carries the original config commands.
	out, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", ws.SessionName,
		"-F", "#{window_name}").Output()
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	if !strings.Contains(string(out), "agent-1") {
		t.Errorf("windows = %q", out)
	}
	foundClaude := false
	for _, cmd := range actC.Execs {
		if cmd == "claude" {
			foundClaude = true
		}
	}
	if !foundClaude {
		t.Errorf("Execs = %v; the agent window's command never reached ExecCommand", actC.Execs)
	}

	// Reopen: the stored binding probes present -> no second start.
	obs.ProbeResult = controller.ContainerObservation{
		Health: state.HealthPresent, Kind: "devcontainer", ContainerID: "cid-1",
		ContainerUser: "vscode", Workdir: "/workspaces/slabledger",
	}
	env = openJSON(t)
	if env.Action != "already-running" {
		t.Fatalf("reopen = %+v", env)
	}
	if len(actC.Started) != 1 {
		t.Errorf("reopen started the container again (calls = %d)", len(actC.Started))
	}
}

func TestLifecycleContainerStartFailureCreatesNoSession(t *testing.T) {
	_, socket := lifecycleRig(t, "cfail")
	actC := installContainerActuator(t)
	actC.StartErr = &controller.ContainerStartError{
		ExitCode: 3, Stderr: "boom", Reason: "devcontainer up exited 3",
	}
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})

	code, _, stderr := run(t, "open", "--json")
	if code != ExitError {
		t.Fatalf("exit %d, want %d (stderr %s)", code, ExitError, stderr)
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("a session exists despite the failed container start: %+v", live)
	}

	code, stdout, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("status exit %d", code)
	}
	if !strings.Contains(stdout, `"exit_status": 3`) &&
		!strings.Contains(stdout, `"exit_status":3`) {
		t.Errorf("status does not carry the start's exit status: %s", stdout)
	}
}

// TestLifecycleOpenStopReopen: stop kills the real session, a second
// stop is an idempotent no-op, and reopen creates a fresh session.
func TestLifecycleOpenStopReopen(t *testing.T) {
	ws, socket := lifecycleRig(t, "stopcycle")

	if env := openJSON(t); env.Action != "created" {
		t.Fatalf("open = %+v", env)
	}

	code, stdout, stderr := run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("stop exit %d, stderr: %s", code, stderr)
	}
	env := decodeStop(t, stdout)
	if !env.Session.Stopped || env.Session.Name != ws.SessionName {
		t.Fatalf("stop = %+v", env.Session)
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 0 {
		t.Fatalf("live = %+v after stop", live)
	}

	code, stdout, _ = run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("second stop exit %d", code)
	}
	if env := decodeStop(t, stdout); env.Session.Stopped {
		t.Errorf("second stop = %+v, want idempotent no-op", env.Session)
	}

	if env := openJSON(t); env.Action != "created" {
		t.Errorf("reopen after stop = %+v, want a fresh creation", env)
	}
}

// TestLifecycleStopKillsExactlyTheIdentitySession: a same-prefix
// sibling session must survive — kill-session's default prefix matching
// would take it out if the actuator did not pin exact matching.
func TestLifecycleStopKillsExactlyTheIdentitySession(t *testing.T) {
	ws, socket := lifecycleRig(t, "sibling")

	if env := openJSON(t); env.Action != "created" {
		t.Fatalf("open = %+v", env)
	}
	sibling := ws.SessionName + "-scratch"
	if out, err := exec.Command("tmux", "-L", socket,
		"new-session", "-d", "-s", sibling).CombinedOutput(); err != nil {
		t.Fatalf("seeding the sibling: %v\n%s", err, out)
	}

	code, _, stderr := run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("stop exit %d, stderr: %s", code, stderr)
	}
	out, err := exec.Command("tmux", "-L", socket,
		"list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		t.Fatalf("list-sessions: %v", err)
	}
	names := strings.Fields(string(out))
	if len(names) != 1 || names[0] != sibling {
		t.Errorf("surviving sessions = %v, want only %q", names, sibling)
	}
}

// TestLifecycleStopContainerThenAutostart: real tmux and store, fake
// container tooling. stop --container kills the session and stops the
// bound container while retaining the binding as missing; autostart then
// restarts the container from the stored record without touching tmux.
func TestLifecycleStopContainerThenAutostart(t *testing.T) {
	ws, socket := lifecycleRig(t, "restart")
	actC := installContainerActuator(t)
	actC.ExecResult = "sleep 300"
	obs := &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}
	installContainerObserver(t, obs)

	if env := openJSON(t); env.Action != "created" {
		t.Fatalf("open = %+v", env)
	}

	code, stdout, stderr := run(t, "stop", "--container", "--json")
	if code != 0 {
		t.Fatalf("stop exit %d, stderr: %s", code, stderr)
	}
	stopEnv := decodeStop(t, stdout)
	if !stopEnv.Session.Stopped || stopEnv.Container == nil || !stopEnv.Container.Stopped {
		t.Fatalf("stop = %+v / %+v", stopEnv.Session, stopEnv.Container)
	}
	if len(actC.Stopped) != 1 {
		t.Fatalf("StopContainer calls = %d, want 1", len(actC.Stopped))
	}
	// The stored binding now probes as confirmed-stopped.
	obs.ProbeResult = controller.ContainerObservation{
		Health: state.HealthMissing, Kind: "devcontainer",
	}

	// Autostart restarts the container: the workspace is registered in
	// the real store, and its config does not yet enable autostart, so
	// first prove the skip, then enable the flag.
	code, stdout, _ = run(t, "autostart", "--json")
	if code != 0 {
		t.Fatalf("autostart exit %d\n%s", code, stdout)
	}
	env := decodeAutostart(t, stdout)
	if e := entryFor(t, env, ws.Slug); e.Outcome != "skipped" {
		t.Fatalf("autostart without the flag = %+v, want skipped", e)
	}

	configDir := os.Getenv("PROJECTMUX_CONFIG_ROOT")
	path := configDir + "/workspaces/" + ws.Slug + ".yaml"
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if err := os.WriteFile(path, append(raw, []byte("autostart: true\n")...), 0o644); err != nil {
		t.Fatalf("enabling autostart: %v", err)
	}

	code, stdout, _ = run(t, "autostart", "--json")
	if code != 0 {
		t.Fatalf("autostart exit %d\n%s", code, stdout)
	}
	env = decodeAutostart(t, stdout)
	if e := entryFor(t, env, ws.Slug); e.Outcome != "started" {
		t.Fatalf("autostart = %+v, want started", e)
	}
	if len(actC.Started) != 2 {
		t.Errorf("StartContainer calls = %d, want 2 (open + autostart)", len(actC.Started))
	}
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("autostart touched tmux: %+v", live)
	}
}

// TestLifecycleRebuildThenAutostart performs the disaster this slice
// exists for, against a real tmux server and the real store: the state
// database is destroyed while the session it described is still running.
// Rebuild re-registers the repository and its session from the live
// session's identity keys, and autostart then starts that repository's
// container. This is the successor to the is_primary regression: the flag
// used to keep autostart from starting a shared container once per
// worktree, and one row per repository now does it structurally (spec
// §6.3). If the repositories table came back wrong, autostart would stop
// starting this container — or start it twice.
func TestLifecycleRebuildThenAutostart(t *testing.T) {
	ws, socket := lifecycleRig(t, "rebuild")
	actC := installContainerActuator(t)
	actC.ExecResult = "sleep 300"
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})

	if env := openJSON(t); env.Action != "created" {
		t.Fatalf("open = %+v", env)
	}

	// The disaster: the database and its sidecars are gone; the tmux
	// session is not.
	stateRoot := os.Getenv("PROJECTMUX_STATE_ROOT")
	path := state.DBPath(stateRoot)
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			t.Fatalf("removing %s: %v", p, err)
		}
	}

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("rebuild exit %d, stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	env := decodeRebuild(t, stdout)
	if len(env.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", env.Conflicts)
	}
	if len(env.Registered) != 1 {
		t.Fatalf("registered = %+v, want exactly one workspace", env.Registered)
	}
	// The envelope names the field repo_root as of Task 7 (schema_version
	// 2); the value it carries is the repository root.
	if got := env.Registered[0]; got.ID != ws.ID || got.Slug != ws.Slug ||
		got.RepoRoot != ws.RepoRoot || got.SessionName != ws.SessionName {
		t.Fatalf("registered = %+v, want %s at %s, session %s", got, ws.Slug, ws.RepoRoot, ws.SessionName)
	}

	// Exactly one repository row, carrying the root autostart will use.
	st, err := state.Open(stateRoot)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	repos, err := st.Repositories()
	if err != nil {
		t.Fatalf("Repositories: %v", err)
	}
	if len(repos) != 1 || repos[0].ID != ws.RepositoryID || repos[0].RepoRoot != ws.RepoRoot {
		t.Fatalf("repositories = %+v, want exactly one row for %s", repos, ws.RepoRoot)
	}
	rec, err := st.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.ActualSession == nil || *rec.ActualSession != ws.SessionName {
		t.Errorf("actual_session = %v, want %q adopted", rec.ActualSession, ws.SessionName)
	}
	if rec.ProposedSession != ws.SessionName {
		t.Errorf("proposed_session = %q, want %q", rec.ProposedSession, ws.SessionName)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// Idempotence: a fully recovered installation has nothing to do.
	code, stdout, stderr = run(t, "rebuild", "--json")
	if code != ExitOK {
		t.Fatalf("second rebuild exit %d, stderr: %s", code, stderr)
	}
	second := decodeRebuild(t, stdout)
	if len(second.Registered) != 0 || len(second.Conflicts) != 0 {
		t.Errorf("second rebuild = %+v, want an empty report", second)
	}

	// The recovered installation still autostarts, once.
	configDir := os.Getenv("PROJECTMUX_CONFIG_ROOT")
	cfgPath := configDir + "/workspaces/" + ws.Slug + ".yaml"
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("reading config: %v", err)
	}
	if err := os.WriteFile(cfgPath, append(raw, []byte("autostart: true\n")...), 0o644); err != nil {
		t.Fatalf("enabling autostart: %v", err)
	}
	before := len(actC.Started)
	code, stdout, stderr = run(t, "autostart", "--json")
	if code != ExitOK {
		t.Fatalf("autostart exit %d, stderr: %s\nstdout: %s", code, stderr, stdout)
	}
	auto := decodeAutostart(t, stdout)
	if len(auto.Repositories) != 1 {
		t.Fatalf("autostart report = %+v, want one repository", auto.Repositories)
	}
	if e := auto.Repositories[0]; e.ID != ws.RepositoryID || e.Outcome != "started" {
		t.Fatalf("autostart entry = %+v, want %s started", e, ws.RepositoryID)
	}
	if len(actC.Started)-before != 1 {
		t.Errorf("autostart made %d starts, want exactly 1", len(actC.Started)-before)
	}

	// The session was adopted, never recreated or renamed, and autostart
	// never touched tmux.
	live, err := (&tmux.Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 1 || live[0].Name != ws.SessionName || live[0].WorkspaceID != ws.ID {
		t.Errorf("live = %+v; rebuild or autostart created or renamed sessions", live)
	}
}
