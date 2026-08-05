# Container Adapter Implementation Plan

> **Status note:** this slice was implemented and fully verified during
> the plan's materialize-verify pass (all gates green under -race, real
> docker/devcontainer integration tests, and an end-to-end smoke with a
> real devcontainer), then committed directly rather than re-transcribed
> task-by-task. The committed code is authoritative where it and the
> blocks below diverge; the notable divergences are test-package
> qualification (`controller_test`), fixture updates for applicability
> gating, and `fake.ContainerActuator`'s `ExecResult`/`Execs` fields
> added so lifecycle panes run a real command.

**Goal:** Real devcontainer/Docker support: probe, discovery, `devcontainer up` startup, and container-located windows via `docker exec`, wired into `Ensure`'s capability gate.

**Architecture:** A new `internal/container` package owns every docker/devcontainer invocation (observer + actuator on the existing runner, with binary seams). The controller gains the `acquire` plan action, an `Applies` applicability check before stored-binding probes under `auto`, a window intent/rendering split (container commands render only after the binding exists inside the locked loop), and `StartError` persistence with real exit statuses. The CLI swaps its placeholder observers for the real adapter; `status`/`attach` probe live.

**Tech Stack:** Go stdlib only. Spec: `docs/superpowers/specs/2026-08-05-container-adapter-design.md`. Verified tool behaviors (docker 29.7.1, devcontainer CLI 0.86.1) are recorded in the spec §3 and are binding.

## Global Constraints

- No new module dependencies; stdlib only. Linux/WSL only.
- `gofmt -l .` empty, `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux` before every commit.
- Exit codes 0–6 unchanged; JSON envelope changes are additive to schema version 1.
- Health semantics (design §9): `missing` is confirmed absence only; anything unrecognized is `unknown`; a running container is never reported missing (discovery reports `present`-incomplete → plan `acquire`).
- The refusal check precedes the container phase; a refusing plan carries container action `none`; open never mutates on uncertainty; the applied digest is written only on confirmed creation.
- The workspace filesystem lock spans `devcontainer up`; no SQLite transaction ever runs during a subprocess.
- `up`'s result JSON is the last stdout line; `success` requires non-empty `containerId` and `remoteWorkspaceFolder` before anything downstream sees it.
- Probes/discovery use a 5s default timeout; only `up` gets `start_timeout`.
- Commit messages and code comments must not mention Claude or AI assistance.

---

### Task 1: Runner returns partial capture on timeout/cancel

**Files:**
- Modify: `internal/run/run.go` (the tail of `Run`)
- Test: `internal/run/run_test.go` (append)

**Interfaces:**
- Produces: `run.Run` now returns the partially captured `Result` (stdout/stderr/truncation flags, `ExitCode` 0) **alongside** the timeout/cancellation error. Start-failure and empty-argv errors still return a zero `Result`.

- [ ] **Step 1: Write the failing test** — append to `internal/run/run_test.go`:

```go
func TestRunTimeoutReturnsPartialCapture(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv:    []string{"/bin/sh", "-c", "echo partial-out; echo partial-err 1>&2; sleep 10"},
		Timeout: 300 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := string(res.Stdout); got != "partial-out\n" {
		t.Errorf("Stdout = %q; partial capture was discarded", got)
	}
	if got := string(res.Stderr); got != "partial-err\n" {
		t.Errorf("Stderr = %q; partial capture was discarded", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `go test ./internal/run/ -count=1 -run TestRunTimeoutReturnsPartialCapture` → FAIL (empty Stdout).

- [ ] **Step 3: Implement** — in `internal/run/run.go`, `Run` currently builds `res` *after* the `ctx.Err()` check and returns `Result{}` there. Reorder so `res` is built first and returned with the context error, leaving the other error paths unchanged:

```go
	err := c.Run()
	res := Result{
		Stdout:          stdout.buf,
		Stderr:          stderr.buf,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		// Partial capture rides along with the error so callers can
		// preserve a bounded stderr summary for timed-out subprocesses
		// (container-adapter spec §4).
		return res, fmt.Errorf("running %s: %w", cmd.Argv[0], ctxErr)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	if err != nil {
		return Result{}, fmt.Errorf("running %s: %w", cmd.Argv[0], err)
	}
	return res, nil
```

Also update `Run`'s doc comment sentence about the error return: append "Timeout and cancellation errors carry the partially captured output in the returned Result."

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/run/ -count=1 -race` → PASS.
- [ ] **Step 5: Gates and commit**

```bash
git add internal/run/
git commit -m "Return partial capture alongside runner timeout errors"
```

---

### Task 2: Config validation — numeric window names and the static container contradiction

**Files:**
- Modify: `internal/config/validate.go`
- Test: `internal/config/validate_test.go` or `config_test.go` (follow where existing validation tests live — grep `invalid name` to find the table)

**Interfaces:**
- Produces: two new validation problems; no API changes.

- [ ] **Step 1: Write the failing tests** — add two cases to the existing validation-failure table (match its exact style; the table asserts substrings of the problem list):

```go
		"fully numeric window name": {
			files: map[string]string{"defaults.yaml": "version: 1\nwindows:\n  - name: \"3\"\n    shell: true\n"},
			want:  "fully numeric",
		},
		"container window with devcontainer disabled": {
			files: map[string]string{"defaults.yaml": "version: 1\ndevcontainer:\n  enabled: false\nwindows:\n  - name: agent-1\n    agent: claude\n    location: container\n"},
			want:  "devcontainer.enabled is false",
		},
```

(If the validation tests are unit-level on `validate` rather than file-driven, express the same two cases in that style instead — the assertions are the substrings above.)

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/config/ -count=1` → the two new cases FAIL.

- [ ] **Step 3: Implement** — in `internal/config/validate.go`:

Add beside `windowNamePattern` (validate.go:17):

```go
// numericWindowName rejects fully numeric names: tmux resolves a numeric
// token in a target as a window index before a window name, so a window
// literally named "3" can make focus land on the wrong window.
var numericWindowName = regexp.MustCompile(`^[0-9]+$`)
```

In `validateWindow`, directly after the `windowNamePattern` check (validate.go:74-77):

```go
	if numericWindowName.MatchString(w.Name) {
		problems = append(problems, fmt.Sprintf(
			"window %q has a fully numeric name, which tmux treats as a window index; include a non-digit", w.Name))
	}
```

In `validate`, inside the existing `for _, w := range cfg.Windows` loop (validate.go:58-63), add the cross-field check:

```go
		if cfg.DevContainer.Enabled == "false" && w.Location != nil && *w.Location == "container" {
			problems = append(problems, fmt.Sprintf(
				"window %q sets location: container but devcontainer.enabled is false", w.Name))
		}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/config/ -count=1 -race` → PASS (existing cases unchanged: no shipped default uses numeric names or that contradiction).
- [ ] **Step 5: Gates and commit**

```bash
git add internal/config/
git commit -m "Reject numeric window names and container windows with devcontainer disabled"
```

---

### Task 3: Controller surface — actuator interface, acquire action, intents, errors, fakes

**Files:**
- Modify: `internal/controller/interfaces.go`, `internal/controller/plan.go`, `internal/controller/ensure.go` (error types only), `internal/controller/fake/fake.go`
- Modify: `internal/cli/wiring.go` (two-line `Applies` stubs to keep the build green until Task 7)
- Test: `internal/controller/fake/fake_test.go`, `internal/controller/plan_test.go` (append)

**Interfaces:**
- Produces (consumed by Tasks 4–8):

```go
// interfaces.go
type ContainerObserver interface {
	ProbeContainer(ctx context.Context, binding state.ContainerBinding) (ContainerObservation, error)
	DiscoverContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (*ContainerObservation, error)
	Applies(ctx context.Context, ws resolve.Workspace, cfg config.Config) (bool, error)
}

type ContainerActuator interface {
	StartContainer(ctx context.Context, ws resolve.Workspace, cfg config.Config) (ContainerObservation, error)
	ExecCommand(binding state.ContainerBinding, command, relDir string, env map[string]string) string
}

type WindowLocation string

const (
	WindowAuto      WindowLocation = "" // container when one applies, host otherwise
	WindowHost      WindowLocation = "host"
	WindowContainer WindowLocation = "container"
)

type WindowIntent struct {
	Name     string
	Command  string // empty => shell window
	RelDir   string // config cwd, relative; "" => workspace root
	Focus    bool
	Location WindowLocation
}
```

```go
// plan.go
const ContainerActionAcquire ContainerAction = "acquire"
```

```go
// ensure.go (types only this task)
// ContainerStartError preserves what design §9 requires the recorded
// operation to keep: the real exit status, a bounded stderr summary, and
// whether the start timed out. The container adapter returns it; the
// controller persists it.
type ContainerStartError struct {
	ExitCode int
	Stderr   string
	TimedOut bool
	Reason   string
}

func (e *ContainerStartError) Error() string {
	if e.TimedOut {
		return "devcontainer up timed out: " + e.Reason
	}
	return fmt.Sprintf("devcontainer up failed (exit %d): %s", e.ExitCode, e.Reason)
}

// ContainerWindowError reports a window demanding a container when none
// applies to the workspace.
type ContainerWindowError struct{ Window string }

func (e *ContainerWindowError) Error() string {
	return fmt.Sprintf(
		"window %q requires a container, but no container applies to this workspace", e.Window)
}
```

- fakes: `fake.ContainerObserver` gains `AppliesResult bool`, `AppliesErr error` and the `Applies` method; new `fake.ContainerActuator{StartResult controller.ContainerObservation, StartErr error, Started []string}` whose `ExecCommand` returns the deterministic marker `fmt.Sprintf("fake-exec %s %s %q env=%d", b.ContainerID, path.Join(b.Workdir, relDir), command, len(env))`.
- `internal/cli/wiring.go`: `unprobedObserver` and `hostOnlyContainerObserver` each gain `func (…) Applies(context.Context, resolve.Workspace, config.Config) (bool, error) { return false, errUnprobed }` — deleted in Task 7; they exist only to keep the tree compiling.
- `BuildPlan`: `containerAction` returns `ContainerActionAcquire` when the observation is `present` with an empty `Workdir` (the discovery shape; probes of stored bindings and `up` results always carry one).

- [ ] **Step 1: Write the failing tests**

Append to `internal/controller/plan_test.go` (follow its existing table style; if it is table-driven over snapshots, add cases — otherwise add this standalone test):

```go
func TestContainerActionAcquireOnIncompleteBinding(t *testing.T) {
	snap := Snapshot{
		Desired: Desired{Workspace: resolve.Workspace{ID: "w1", Slug: "s", Worktree: "/w"}},
		Session: SessionSnapshot{State: SessionAbsent},
		Container: ContainerSnapshot{Observed: &ContainerObservation{
			Health:      state.HealthPresent,
			ContainerID: "c1",
			// No Workdir: the discovery shape — running but unbound.
		}},
	}
	if p := BuildPlan(snap); p.Container != ContainerActionAcquire {
		t.Errorf("container action = %q, want %q", p.Container, ContainerActionAcquire)
	}
	snap.Container.Observed.Workdir = "/workspaces/w"
	if p := BuildPlan(snap); p.Container != ContainerActionNone {
		t.Errorf("complete present binding: action = %q, want none", p.Container)
	}
}
```

Append to `internal/controller/fake/fake_test.go`:

```go
func TestFakeContainerObserverApplies(t *testing.T) {
	o := &ContainerObserver{AppliesResult: true}
	ok, err := o.Applies(context.Background(), resolve.Workspace{}, config.Config{})
	if err != nil || !ok {
		t.Errorf("Applies = (%t, %v), want (true, nil)", ok, err)
	}
	o.AppliesErr = errors.New("stat exploded")
	if _, err := o.Applies(context.Background(), resolve.Workspace{}, config.Config{}); err == nil {
		t.Error("configured Applies error was not returned")
	}
}

func TestFakeContainerActuator(t *testing.T) {
	a := &ContainerActuator{
		StartResult: controller.ContainerObservation{
			Health: state.HealthPresent, ContainerID: "c1", Workdir: "/workspaces/w",
		},
	}
	obs, err := a.StartContainer(context.Background(),
		resolve.Workspace{ID: "w1"}, config.Config{})
	if err != nil || obs.ContainerID != "c1" {
		t.Errorf("StartContainer = (%+v, %v)", obs, err)
	}
	if len(a.Started) != 1 || a.Started[0] != "w1" {
		t.Errorf("Started = %v", a.Started)
	}
	cmd := a.ExecCommand(state.ContainerBinding{ContainerID: "c1", Workdir: "/workspaces/w"},
		"make", "sub", map[string]string{"A": "1"})
	if cmd != `fake-exec c1 /workspaces/w/sub "make" env=1` {
		t.Errorf("ExecCommand = %q", cmd)
	}

	a.StartErr = errors.New("boom")
	if _, err := a.StartContainer(context.Background(), resolve.Workspace{}, config.Config{}); err == nil {
		t.Error("configured start error was not returned")
	}
}
```

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/controller/... -count=1` → FAIL to build (`Applies`, `ContainerActuator`, `ContainerActionAcquire` undefined).

- [ ] **Step 3: Implement** — apply the interface/type/const/error additions exactly as the Interfaces block above shows:

1. `interfaces.go`: add `Applies` to `ContainerObserver` (with a doc comment: "Applies reports whether a container applies to the workspace at all — enabled true always applies; auto applies when a devcontainer configuration exists on disk. Observe consults it before probing stored bindings under auto, so removing the configuration de-containerizes the workspace."); add the `ContainerActuator` interface, `WindowLocation` constants, and `WindowIntent` struct with the doc comments shown.
2. `plan.go`: add the `ContainerActionAcquire` constant beside the others, and change `containerAction`'s `HealthPresent` case to:

```go
	case state.HealthPresent:
		if obs.Workdir == "" {
			// The discovery shape: running but unbound (labels cannot
			// supply user/workdir). Acquisition flows through the
			// idempotent devcontainer up (spec §3).
			return ContainerActionAcquire
		}
		return ContainerActionNone
```

3. `ensure.go`: add `ContainerStartError` and `ContainerWindowError` as shown.
4. `fake/fake.go`: add to `ContainerObserver` the fields `AppliesResult bool`, `AppliesErr error` and:

```go
func (o *ContainerObserver) Applies(_ context.Context, _ resolve.Workspace, _ config.Config) (bool, error) {
	if o.AppliesErr != nil {
		return false, o.AppliesErr
	}
	return o.AppliesResult, nil
}
```

Add `fake.ContainerActuator` (import `"path"`):

```go
// ContainerActuator records starts and renders a deterministic exec
// marker so command tests can assert container windows without real
// docker argv.
type ContainerActuator struct {
	StartResult controller.ContainerObservation
	StartErr    error
	Started     []string
}

func (a *ContainerActuator) StartContainer(_ context.Context, ws resolve.Workspace, _ config.Config) (controller.ContainerObservation, error) {
	a.Started = append(a.Started, ws.ID)
	if a.StartErr != nil {
		return controller.ContainerObservation{}, a.StartErr
	}
	return a.StartResult, nil
}

func (a *ContainerActuator) ExecCommand(b state.ContainerBinding, command, relDir string, env map[string]string) string {
	return fmt.Sprintf("fake-exec %s %s %q env=%d",
		b.ContainerID, path.Join(b.Workdir, relDir), command, len(env))
}
```

and `_ controller.ContainerActuator = (*ContainerActuator)(nil)` in the assertions block.
5. `internal/cli/wiring.go`: add the two `Applies` stubs described in Interfaces (both return `false, errUnprobed`).

- [ ] **Step 4: Run to verify it passes** — `go test ./... -count=1 -race` → PASS (no existing behavior changes yet: `Applies` is not called by `Observe` until Task 5).
- [ ] **Step 5: Gates and commit**

```bash
git add internal/controller/ internal/cli/wiring.go
git commit -m "Add container actuator surface, acquire action, and window intents"
```

---

### Task 4: `internal/container` — pure parts (quoting, exec rendering, up-JSON, classification)

**Files:**
- Create: `internal/container/container.go` (package doc, Adapter struct, seams, constants)
- Create: `internal/container/exec.go`, `internal/container/parse.go`
- Test: `internal/container/exec_test.go`, `internal/container/parse_test.go`

**Interfaces:**
- Produces: `container.Adapter{Timeout, StartTimeout time.Duration}` (methods in Task 5); package vars `dockerBinary = "docker"`, `devcontainerBinary = "devcontainer"`; pure `shellQuote(s string) string`, `(*Adapter).ExecCommand(binding, command, relDir, env) string`, `parseUpResult(stdout []byte) (upResult, error)` with `upResult{Outcome, Message, ContainerID, RemoteUser, RemoteWorkspaceFolder}`, `classifyInspect(res run.Result) (state.Health, error)`, `boundedStderr(b []byte) string` (4096-byte bound via `state.MaxErrorSummaryBytes`, valid UTF-8).

- [ ] **Step 1: Write the failing tests**

`internal/container/exec_test.go`:

```go
package container

import (
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/state"
)

func execBinding() state.ContainerBinding {
	return state.ContainerBinding{
		Kind:          "devcontainer",
		ContainerID:   "c0ffee",
		ContainerUser: "vscode",
		Workdir:       "/workspaces/proj",
		Health:        state.HealthPresent,
	}
}

func TestExecCommandRendersFullRequest(t *testing.T) {
	a := &Adapter{}
	got := a.ExecCommand(execBinding(), "make watch", "sub/dir",
		map[string]string{"B_KEY": "2", "A_KEY": "1"})
	want := `docker exec -i -t -u 'vscode' -w '/workspaces/proj/sub/dir' ` +
		`-e 'A_KEY=1' -e 'B_KEY=2' 'c0ffee' sh -lc 'make watch'`
	if got != want {
		t.Errorf("ExecCommand =\n%q\nwant\n%q", got, want)
	}
}

func TestExecCommandShellWindowAndOmittedUser(t *testing.T) {
	a := &Adapter{}
	b := execBinding()
	b.ContainerUser = ""
	got := a.ExecCommand(b, "", "", nil)
	want := `docker exec -i -t -w '/workspaces/proj' 'c0ffee' sh -l`
	if got != want {
		t.Errorf("ExecCommand = %q, want %q", got, want)
	}
	if strings.Contains(got, "-u") {
		t.Error("an empty user must omit -u")
	}
}

func TestExecCommandQuotesHostileValues(t *testing.T) {
	a := &Adapter{}
	got := a.ExecCommand(execBinding(), `echo "$HOME"; touch /tmp/pwned'`, "",
		map[string]string{"EV": `x'; rm -rf / #`})
	for _, want := range []string{
		`sh -lc 'echo "$HOME"; touch /tmp/pwned'\'''`,
		`-e 'EV=x'\''; rm -rf / #'`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("ExecCommand %q\nmissing %q", got, want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"plain":   "'plain'",
		"a b":     "'a b'",
		"it's":    `'it'\''s'`,
		"$(evil)": "'$(evil)'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q, want %q", in, got, want)
		}
	}
}
```

`internal/container/parse_test.go`:

```go
package container

import (
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/run"
	"github.com/gambtho/projectmux/internal/state"
)

func TestParseUpResultLastLine(t *testing.T) {
	out := []byte("some progress noise\n" +
		`{"outcome":"success","containerId":"c1","remoteUser":"root","remoteWorkspaceFolder":"/workspaces/x"}` + "\n")
	res, err := parseUpResult(out)
	if err != nil {
		t.Fatalf("parseUpResult: %v", err)
	}
	if res.ContainerID != "c1" || res.RemoteUser != "root" ||
		res.RemoteWorkspaceFolder != "/workspaces/x" {
		t.Errorf("res = %+v", res)
	}
}

func TestParseUpResultRejectsBadOutput(t *testing.T) {
	cases := map[string]string{
		"not json":            "definitely not json\n",
		"empty":               "",
		"failed outcome":      `{"outcome":"error","message":"build failed"}`,
		"missing containerId": `{"outcome":"success","remoteWorkspaceFolder":"/w"}`,
		"missing workdir":     `{"outcome":"success","containerId":"c1"}`,
	}
	for label, out := range cases {
		if _, err := parseUpResult([]byte(out)); err == nil {
			t.Errorf("%s: parseUpResult accepted %q", label, out)
		}
	}
	// The failed-outcome error carries the CLI's message.
	_, err := parseUpResult([]byte(`{"outcome":"error","message":"build failed"}`))
	if err == nil || !strings.Contains(err.Error(), "build failed") {
		t.Errorf("failed-outcome error %v does not carry the message", err)
	}
}

func TestClassifyInspect(t *testing.T) {
	cases := map[string]struct {
		res  run.Result
		want state.Health
		err  bool
	}{
		"running":        {res: run.Result{Stdout: []byte("true\n")}, want: state.HealthPresent},
		"stopped":        {res: run.Result{Stdout: []byte("false\n")}, want: state.HealthMissing},
		"no such lower":  {res: run.Result{ExitCode: 1, Stderr: []byte("error: no such object: c1")}, want: state.HealthMissing},
		"no such older":  {res: run.Result{ExitCode: 1, Stderr: []byte("Error: No such object: c1")}, want: state.HealthMissing},
		"daemon down":    {res: run.Result{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon")}, err: true},
		"garbage stdout": {res: run.Result{Stdout: []byte("maybe\n")}, err: true},
	}
	for label, tc := range cases {
		health, err := classifyInspect(tc.res)
		if tc.err {
			if err == nil {
				t.Errorf("%s: expected an error (unknown funnel)", label)
			}
			continue
		}
		if err != nil || health != tc.want {
			t.Errorf("%s: = (%v, %v), want %v", label, health, err, tc.want)
		}
	}
}

func TestBoundedStderr(t *testing.T) {
	long := strings.Repeat("x", state.MaxErrorSummaryBytes+100)
	if got := boundedStderr([]byte(long)); len(got) > state.MaxErrorSummaryBytes {
		t.Errorf("boundedStderr length = %d", len(got))
	}
	if got := boundedStderr([]byte("  short  \n")); got != "short" {
		t.Errorf("boundedStderr = %q, want trimmed", got)
	}
}
```

- [ ] **Step 2: Run to verify they fail** — `go test ./internal/container/ -count=1` → FAIL to build.

- [ ] **Step 3: Implement**

`internal/container/container.go`:

```go
// Package container is the design-§5 container adapter: it owns every
// docker and devcontainer invocation, translating their output into
// domain observations. No higher layer parses docker output or renders
// docker argv.
package container

import (
	"time"

	"github.com/gambtho/projectmux/internal/controller"
)

// DefaultTimeout bounds probes and discovery. Startup gets the
// configuration's start_timeout instead (spec §3).
const DefaultTimeout = 5 * time.Second

// The executables to invoke; tests substitute scripts.
var (
	dockerBinary       = "docker"
	devcontainerBinary = "devcontainer"
)

// Adapter implements the controller's container observer and actuator.
// Timeout bounds probes and discovery (zero means DefaultTimeout);
// StartTimeout overrides the per-call start_timeout for tests only.
type Adapter struct {
	Timeout      time.Duration
	StartTimeout time.Duration
}

var (
	_ controller.ContainerObserver = (*Adapter)(nil)
	_ controller.ContainerActuator = (*Adapter)(nil)
)

func (a *Adapter) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return DefaultTimeout
}
```

(The interface assertions will not compile until Task 5 adds the methods; within this task, comment the two assertion lines out with `// enabled in the next commit` markers — Task 5 removes the markers. If preferred, simply add the assertions in Task 5 instead; either way the tree must build at this task's commit.)

`internal/container/exec.go`:

```go
package container

import (
	"maps"
	"path"
	"slices"
	"strings"

	"github.com/gambtho/projectmux/internal/state"
)

// shellQuote renders s as one single-quoted POSIX shell word. This is
// the only place quoting happens (spec §3).
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ExecCommand renders the §5 container execution request as a tmux
// window command string: the pane's shell runs docker exec into the
// stored binding. The configured environment must be passed explicitly:
// tmux -e sets pane variables on the host side only, and docker exec
// forwards nothing into the container. Container-relative cwds join the
// binding's workdir with POSIX path semantics. ExecCommand never
// inspects live state; a pane whose container died shows docker's own
// error.
func (a *Adapter) ExecCommand(binding state.ContainerBinding, command, relDir string, env map[string]string) string {
	args := []string{"docker", "exec", "-i", "-t"}
	if binding.ContainerUser != "" {
		args = append(args, "-u", shellQuote(binding.ContainerUser))
	}
	args = append(args, "-w", shellQuote(path.Join(binding.Workdir, relDir)))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		args = append(args, "-e", shellQuote(k+"="+env[k]))
	}
	args = append(args, shellQuote(binding.ContainerID))
	if command == "" {
		args = append(args, "sh", "-l")
	} else {
		args = append(args, "sh", "-lc", shellQuote(command))
	}
	return strings.Join(args, " ")
}
```

`internal/container/parse.go`:

```go
package container

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gambtho/projectmux/internal/run"
	"github.com/gambtho/projectmux/internal/state"
)

// upResult is devcontainer up's result JSON — the last line of stdout
// (verified on CLI 0.86: progress logs go to stderr).
type upResult struct {
	Outcome               string `json:"outcome"`
	Message               string `json:"message"`
	ContainerID           string `json:"containerId"`
	RemoteUser            string `json:"remoteUser"`
	RemoteWorkspaceFolder string `json:"remoteWorkspaceFolder"`
}

// parseUpResult decodes and validates the result. Load-bearing fields
// are checked here so nothing downstream — window rendering or session
// mutation — ever sees an invalid binding (spec §3).
func parseUpResult(stdout []byte) (upResult, error) {
	lines := bytes.Split(bytes.TrimSpace(stdout), []byte("\n"))
	last := bytes.TrimSpace(lines[len(lines)-1])
	if len(last) == 0 {
		return upResult{}, fmt.Errorf("devcontainer up produced no result output")
	}
	var res upResult
	if err := json.Unmarshal(last, &res); err != nil {
		return upResult{}, fmt.Errorf("devcontainer up produced a malformed result: %w", err)
	}
	if res.Outcome != "success" {
		if res.Message != "" {
			return upResult{}, fmt.Errorf("devcontainer up reported %q: %s", res.Outcome, res.Message)
		}
		return upResult{}, fmt.Errorf("devcontainer up reported outcome %q", res.Outcome)
	}
	if res.ContainerID == "" {
		return upResult{}, fmt.Errorf("devcontainer up succeeded without a containerId")
	}
	if res.RemoteWorkspaceFolder == "" {
		return upResult{}, fmt.Errorf("devcontainer up succeeded without a remoteWorkspaceFolder")
	}
	return res, nil
}

// classifyInspect maps a docker inspect result onto tri-state health.
// Narrow on purpose: "no such object" (case-insensitive — docker 29
// lowercases it, older daemons capitalize) is confirmed absence; every
// unrecognized failure is an error, which callers render as unknown —
// uncertainty never converts to absence (design §9).
func classifyInspect(res run.Result) (state.Health, error) {
	if res.ExitCode == 0 {
		switch strings.TrimSpace(string(res.Stdout)) {
		case "true":
			return state.HealthPresent, nil
		case "false":
			return state.HealthMissing, nil
		}
		return "", fmt.Errorf("docker inspect produced unrecognized output %q",
			strings.TrimSpace(string(res.Stdout)))
	}
	if strings.Contains(strings.ToLower(string(res.Stderr)), "no such object") {
		return state.HealthMissing, nil
	}
	return "", fmt.Errorf("docker inspect exited %d: %s",
		res.ExitCode, boundedStderr(res.Stderr))
}

// boundedStderr trims and bounds a stderr capture for error summaries.
func boundedStderr(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > state.MaxErrorSummaryBytes {
		s = strings.ToValidUTF8(s[:state.MaxErrorSummaryBytes], "")
	}
	return s
}
```

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/container/ -count=1 -race` → PASS.
- [ ] **Step 5: Gates and commit**

```bash
git add internal/container/
git commit -m "Add the container adapter's pure rendering, parsing, and classification"
```

---

### Task 5: `internal/container` — subprocess methods, plus Observe's applicability gate

**Files:**
- Create: `internal/container/adapter.go`
- Modify: `internal/controller/observe.go` (`observeContainer`)
- Modify: `internal/controller/observe_test.go`, `internal/controller/ensure_test.go` (fixture updates)
- Test: `internal/container/adapter_test.go` (fake-binary tests)

**Interfaces:**
- Consumes: Tasks 3–4.
- Produces: `(*Adapter) ProbeContainer / DiscoverContainer / Applies / StartContainer` per the spec; `Observe` consults `Applies` before probing stored bindings under `auto`.

- [ ] **Step 1: Write the failing tests**

`internal/container/adapter_test.go`:

```go
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
	return resolve.Workspace{ID: "w1", Slug: "proj", Worktree: t.TempDir()}
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

func writeDevcontainerJSON(t *testing.T, worktree string) {
	t.Helper()
	dir := filepath.Join(worktree, ".devcontainer")
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
	writeDevcontainerJSON(t, ws.Worktree)
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
	writeDevcontainerJSON(t, ws.Worktree)

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
```

Fixture updates for the new `Applies`-before-probe behavior (these fail after Step 3's `observe.go` change, so make them together):

- `internal/controller/observe_test.go`: in `newDeps` (observe_test.go:42-55), initialize `containers: &fake.ContainerObserver{AppliesResult: true}` so existing `auto` cases keep observing. Add two tests:

```go
func TestObserveAutoNotApplicableSkipsContainer(t *testing.T) {
	d := newDeps()
	d.containers.AppliesResult = false
	if err := d.store.RegisterWorkspace(testDesired("auto").Workspace, "sha256:x", testTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := d.store.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, testTime); err != nil {
		t.Fatalf("bind: %v", err)
	}

	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed != nil || snap.Container.Err != nil {
		t.Errorf("container snapshot = %+v; a non-applicable workspace must look disabled, stored binding or not",
			snap.Container)
	}
	if len(d.containers.Probed) != 0 {
		t.Error("Applies=false still probed the stored binding")
	}
}

func TestObserveAutoAppliesErrorIsUnknown(t *testing.T) {
	d := newDeps()
	d.containers.AppliesErr = errors.New("stat exploded")
	snap, err := d.ctrl.Observe(context.Background(), testDesired("auto"))
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if snap.Container.Observed == nil || snap.Container.Observed.Health != state.HealthUnknown ||
		snap.Container.Err == nil {
		t.Errorf("container snapshot = %+v, want unknown with the error", snap.Container)
	}
}
```

(Add the `errors` import if missing.)

- `internal/controller/ensure_test.go`: in `newEnsureRig`, initialize `Containers: &fake.ContainerObserver{AppliesResult: true}` (the rig currently uses the zero value; `TestEnsureContainerGateFiresBeforeActuation` uses `auto` and must still reach discovery).

- [ ] **Step 2: Run to verify failures** — `go test ./internal/container/ ./internal/controller/ -count=1` → container package FAILS to build (methods missing); the two new observe tests FAIL (Applies not consulted).

- [ ] **Step 3: Implement**

`internal/container/adapter.go`:

```go
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
```

Uncomment (or add) the two interface assertions in `container.go`.

In `internal/controller/observe.go`, change the top of `observeContainer`:

```go
	// Observe only on "auto" or "true"; anything else — including "false"
	// and the unnormalized zero value "" — is treated as disabled.
	enabled := d.Config.DevContainer.Enabled
	if enabled != "auto" && enabled != "true" {
		return ContainerSnapshot{}
	}
	if enabled == "auto" {
		// Applicability precedes the stored binding under auto: deleting
		// the devcontainer configuration must de-containerize the
		// workspace even while a binding is retained (spec §4).
		applies, err := c.Containers.Applies(ctx, d.Workspace, d.Config)
		if err != nil {
			return ContainerSnapshot{
				Observed: &ContainerObservation{Health: state.HealthUnknown},
				Err:      err,
			}
		}
		if !applies {
			return ContainerSnapshot{}
		}
	}
```

(The rest of `observeContainer` is unchanged.) Apply the two fixture updates from Step 1.

- [ ] **Step 4: Run to verify it passes** — `go test ./... -count=1 -race` → PASS.
- [ ] **Step 5: Gates and commit**

```bash
git add internal/container/ internal/controller/
git commit -m "Add the container adapter's docker and devcontainer methods with applicability gating"
```

---

### Task 6: `Ensure` — container phase, intent rendering, retries, stale note

**Files:**
- Modify: `internal/controller/ensure.go`
- Modify: `internal/controller/ensure_test.go` (signature migration + new tests)

**Interfaces:**
- Consumes: everything above.
- Produces:

```go
type EnsureResult struct {
	Action                EnsureAction
	Session               string
	Drifted               bool
	Container             *ContainerObservation // nil when no container is in play
	ContainerWindowsStale bool
}

func (c *Controller) Ensure(ctx context.Context, d Desired, intents []WindowIntent, lockDir string, lockTimeout time.Duration) (EnsureResult, error)
```

`Controller` gains `ContainerAct ContainerActuator`. Session-phase behavior, name resolution, squat check, post-create confirmation, and failure recording are unchanged; this task adds the container phase between refusal and session action, renders windows from intents after it, and threads the container observation into every success commit.

- [ ] **Step 1: Migrate and extend the tests**

In `internal/controller/ensure_test.go`:

1. `newEnsureRig`: add `actuatorC *fake.ContainerActuator` to the rig; construct `ContainerAct: nil` by default (gate tests) and provide a helper `withContainerActuator()` that sets both `r.ctrl.ContainerAct = r.actuatorC` and a complete `StartResult`:

```go
func (r *ensureRig) withContainerActuator() *ensureRig {
	r.actuatorC = &fake.ContainerActuator{
		StartResult: controller.ContainerObservation{
			Kind: "devcontainer", ContainerID: "cid-1",
			ContainerUser: "vscode", Workdir: "/workspaces/slab",
			Health: state.HealthPresent,
		},
	}
	r.ctrl.ContainerAct = r.actuatorC
	return r
}
```

2. `r.ensure` migrates from `[]WindowSpec` to intents:

```go
func (r *ensureRig) ensure(t *testing.T, d controller.Desired) (controller.EnsureResult, error) {
	t.Helper()
	intents := []controller.WindowIntent{{Name: "shell"}}
	return r.ctrl.Ensure(context.Background(), d, intents, r.lockDir, time.Second)
}
```

and `TestEnsureRespectsTheWorkspaceLock`'s direct call migrates the same way (`[]controller.WindowIntent{{Name: "shell"}}`).

3. Existing assertions on `spec.Windows[0]` keep working (auto intents on a container-free workspace render to host with `Dir = worktree`).

4. Add new tests:

```go
func containerDesired() controller.Desired {
	d := ensureDesired()
	d.Config.DevContainer.Enabled = "true"
	d.Config.Environment = map[string]string{"FOO": "bar"}
	return d
}

func TestEnsureStartsContainerAndRendersContainerWindows(t *testing.T) {
	r := newEnsureRig(t,
		absentStep(), absentStep(), liveStep(ownSession("slab")),
	).withContainerActuator()
	// enabled true, no binding: Discover reports bare missing.
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}

	d := containerDesired()
	intents := []controller.WindowIntent{
		{Name: "agent-1", Command: "claude", Focus: true}, // auto => container
		{Name: "logs", Command: "tail -f log", RelDir: "sub", Location: controller.WindowContainer},
		{Name: "host-shell", Location: controller.WindowHost},
	}
	res, err := r.ctrl.Ensure(context.Background(), d, intents, r.lockDir, time.Second)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(r.actuatorC.Started) != 1 {
		t.Fatalf("StartContainer calls = %d, want 1", len(r.actuatorC.Started))
	}
	if res.Container == nil || res.Container.ContainerID != "cid-1" {
		t.Errorf("result container = %+v", res.Container)
	}
	spec := r.actuator.Created[0]
	if got := spec.Windows[0].Command; got != `fake-exec cid-1 /workspaces/slab "claude" env=1` {
		t.Errorf("auto window command = %q", got)
	}
	if got := spec.Windows[1].Command; got != `fake-exec cid-1 /workspaces/slab/sub "tail -f log" env=1` {
		t.Errorf("container window command = %q", got)
	}
	if got := spec.Windows[2].Command; got != "" {
		t.Errorf("host window command = %q, want empty (shell)", got)
	}
	rec, _ := r.store.Workspace("w1")
	if rec.Container == nil || rec.Container.ContainerID != "cid-1" ||
		rec.Container.ContainerUser != "vscode" {
		t.Errorf("committed binding = %+v", rec.Container)
	}
	if res.ContainerWindowsStale {
		t.Error("a fresh creation is never stale")
	}
}

func TestEnsureAcquireRunsIdempotentStart(t *testing.T) {
	r := newEnsureRig(t,
		absentStep(), absentStep(), liveStep(ownSession("slab")),
	).withContainerActuator()
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult: true,
		DiscoverResult: &controller.ContainerObservation{
			Health: state.HealthPresent, Kind: "devcontainer", ContainerID: "cid-1",
			// No Workdir: the acquire shape.
		},
	}
	if _, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(r.actuatorC.Started) != 1 {
		t.Errorf("acquire did not run the idempotent start (calls = %d)", len(r.actuatorC.Started))
	}
}

func TestEnsureStartFailurePersistsExitStatus(t *testing.T) {
	r := newEnsureRig(t, absentStep()).withContainerActuator()
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}
	r.actuatorC.StartErr = &controller.ContainerStartError{
		ExitCode: 47, Stderr: "build exploded", Reason: "devcontainer up exited 47",
	}

	_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
	if err == nil {
		t.Fatal("Ensure succeeded despite a failing start")
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a failed container start reached the session actuator")
	}
	op := lastOp(t, r.store, "w1")
	if op == nil || op.Outcome != state.OutcomeFailed {
		t.Fatalf("last operation = %+v", op)
	}
	if op.ExitStatus == nil || *op.ExitStatus != 47 {
		t.Errorf("ExitStatus = %v, want 47 (design §9)", op.ExitStatus)
	}
	if !strings.Contains(op.ErrorSummary, "build exploded") {
		t.Errorf("summary %q lacks the stderr", op.ErrorSummary)
	}
}

func TestEnsureProbeFirstRetriesBoundAndUnbound(t *testing.T) {
	t.Run("bound retries probe", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		if err := r.store.RegisterWorkspace(containerDesired().Workspace, "sha256:x", ensureTime); err != nil {
			t.Fatal(err)
		}
		if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
			t.Fatal(err)
		}
		if err := r.store.RecordContainerObservation("w1", state.ContainerObservation{
			Kind: "devcontainer", ContainerID: "cid-1", ContainerUser: "vscode",
			Workdir: "/workspaces/slab", Health: state.HealthPresent,
		}, ensureTime); err != nil {
			t.Fatal(err)
		}
		// First probe (inside Observe) errors -> probe-first; the retry succeeds.
		obs := &fake.ContainerObserver{
			AppliesResult: true,
			ProbeErr:      errors.New("docker hiccup"),
		}
		r.ctrl.Containers = obs

		// The retry must NOT reuse ProbeErr: clear it after Observe by
		// scripting — the fake returns ProbeErr every call, so instead
		// assert the failure path: a persistently failing probe fails
		// Ensure without mutating.
		_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
		if err == nil {
			t.Fatal("Ensure succeeded with an unobservable container")
		}
		if len(r.actuator.Created) != 0 {
			t.Error("uncertainty reached the session actuator")
		}
		if got := len(obs.Probed); got != 2 {
			t.Errorf("probe calls = %d, want 2 (observe + one retry)", got)
		}
	})

	t.Run("unbound retries discover", func(t *testing.T) {
		r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
		obs := &fake.ContainerObserver{
			AppliesResult: true,
			DiscoverErr:   errors.New("docker hiccup"),
		}
		r.ctrl.Containers = obs
		_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
			[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
		if err == nil {
			t.Fatal("Ensure succeeded with an unobservable container")
		}
		if got := len(obs.Discovered); got != 2 {
			t.Errorf("discover calls = %d, want 2 (observe + one retry)", got)
		}
	})
}

func TestEnsureContainerWindowWithoutContainerFails(t *testing.T) {
	r := newEnsureRig(t, absentStep()).withContainerActuator()
	// auto that resolves to none.
	r.ctrl.Containers = &fake.ContainerObserver{AppliesResult: false}
	d := containerDesired()
	d.Config.DevContainer.Enabled = "auto"

	_, err := r.ctrl.Ensure(context.Background(), d,
		[]controller.WindowIntent{{Name: "agent-1", Command: "claude", Location: controller.WindowContainer}},
		r.lockDir, time.Second)
	var cw *controller.ContainerWindowError
	if !errors.As(err, &cw) {
		t.Fatalf("err = %v, want *ContainerWindowError", err)
	}
	if len(r.actuator.Created) != 0 {
		t.Error("the failing window demand reached the session actuator")
	}
	if op := lastOp(t, r.store, "w1"); op == nil || op.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", op)
	}
}

func TestEnsureReplacementIntoLiveSessionIsStale(t *testing.T) {
	r := newEnsureRig(t, liveStep(ownSession("slab"))).withContainerActuator()
	if err := r.store.RegisterWorkspace(containerDesired().Workspace, "sha256:x", ensureTime); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.AllocateSessionName("w1", ensureTime); err != nil {
		t.Fatal(err)
	}
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}

	res, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "agent-1", Command: "claude"}}, r.lockDir, time.Second)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if res.Action != controller.EnsureAlreadyRunning {
		t.Fatalf("action = %v", res.Action)
	}
	if !res.ContainerWindowsStale {
		t.Error("a container started into a live session must report stale container windows")
	}
	if len(r.actuator.Created) != 0 {
		t.Error("a live session was re-created")
	}
}

func TestEnsureNilActuatorStillRefusesContainerActions(t *testing.T) {
	r := newEnsureRig(t, absentStep())
	r.ctrl.Containers = &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	}
	_, err := r.ctrl.Ensure(context.Background(), containerDesired(),
		[]controller.WindowIntent{{Name: "shell"}}, r.lockDir, time.Second)
	if !errors.Is(err, controller.ErrContainerActionUnsupported) {
		t.Fatalf("err = %v, want ErrContainerActionUnsupported", err)
	}
}
```

(Add `"strings"` to the imports. `TestEnsureContainerGateFiresBeforeActuation` — the pre-existing nil-actuator gate test — stays as-is apart from the `AppliesResult: true` rig change from Task 5.)

- [ ] **Step 2: Run to verify failures** — `go test ./internal/controller/ -count=1` → FAIL (signature and behaviors missing).

- [ ] **Step 3: Implement** — rewrite `internal/controller/ensure.go`'s flow (types from Task 3 stay; session-phase helpers `createSession`, `confirmCreation`, `recordFailure`, `recordOK` keep their bodies except as noted):

Add the field to `Controller` (observe.go, after `Actuator`):

```go
	// ContainerAct performs container mutations for Ensure. Nil refuses
	// any container action (the pre-adapter capability gate).
	ContainerAct ContainerActuator
```

Replace `Ensure` and add the container-phase helpers:

```go
// Ensure is the design-§9 convergence loop: lock, register, final
// observation under the lock, plan, refusal first, then the container
// phase (its windows need the binding), window rendering, one session
// mutation, and a transactional commit.
func (c *Controller) Ensure(ctx context.Context, d Desired, intents []WindowIntent, lockDir string, lockTimeout time.Duration) (EnsureResult, error) {
	lk, err := lock.Acquire(ctx, lockDir, d.Workspace.ID, lockTimeout)
	if err != nil {
		return EnsureResult{}, err
	}
	defer lk.Release()

	if err := c.Store.RegisterWorkspace(d.Workspace, d.Digest, c.Clock.Now()); err != nil {
		return EnsureResult{}, fmt.Errorf("registering the workspace: %w", err)
	}

	snap, err := c.Observe(ctx, d)
	if err != nil {
		c.recordFailure(d.Workspace.ID, "observing the workspace: "+err.Error())
		return EnsureResult{}, err
	}
	plan := BuildPlan(snap)

	if plan.Session == SessionActionRefuse {
		c.recordFailure(d.Workspace.ID, plan.Refusal)
		return EnsureResult{}, &RefusalError{Reason: plan.Refusal}
	}

	containerObs, started, err := c.ensureContainer(ctx, d, snap, plan.Container)
	if err != nil {
		return EnsureResult{}, err
	}

	windows, err := renderWindows(intents, d, containerObs, c.ContainerAct)
	if err != nil {
		c.recordFailure(d.Workspace.ID, err.Error())
		return EnsureResult{}, err
	}

	drifted := snap.Stored == nil || snap.Stored.AppliedDigest == nil ||
		*snap.Stored.AppliedDigest != d.Digest
	stale := started && wantsContainerWindows(intents, containerObs != nil)

	switch plan.Session {
	case SessionActionNone:
		if err := c.commitOutcome(d.Workspace.ID, containerObs); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:                EnsureAlreadyRunning,
			Session:               snap.Session.ByIdentity.Name,
			Drifted:               drifted,
			Container:             containerObs,
			ContainerWindowsStale: stale,
		}, nil

	case SessionActionAdopt:
		name := snap.Session.ByIdentity.Name
		if err := c.Store.AdoptSessionName(d.Workspace.ID, name, c.Clock.Now()); err != nil {
			var conflict *state.SessionNameConflictError
			if errors.As(err, &conflict) {
				reason := fmt.Sprintf(
					"session name %q is already recorded for another workspace; refusing to adopt it", name)
				c.recordFailure(d.Workspace.ID, reason)
				return EnsureResult{}, &RefusalError{Reason: reason}
			}
			c.recordFailure(d.Workspace.ID, "recording the adopted session name: "+err.Error())
			return EnsureResult{}, fmt.Errorf("recording the adopted session name: %w", err)
		}
		if err := c.commitOutcome(d.Workspace.ID, containerObs); err != nil {
			return EnsureResult{}, err
		}
		return EnsureResult{
			Action:                EnsureAdopted,
			Session:               name,
			Drifted:               drifted,
			Container:             containerObs,
			ContainerWindowsStale: stale,
		}, nil

	case SessionActionCreate:
		res, err := c.createSession(ctx, d, windows, containerObs)
		if err != nil {
			return EnsureResult{}, err
		}
		res.Container = containerObs
		return res, nil
	}
	return EnsureResult{}, fmt.Errorf("unexpected session action %q", plan.Session)
}

// ensureContainer executes the plan's container action and returns the
// observation the rest of the pass uses (nil when no container is in
// play) plus whether devcontainer up ran.
func (c *Controller) ensureContainer(ctx context.Context, d Desired, snap Snapshot, action ContainerAction) (*ContainerObservation, bool, error) {
	if action == ContainerActionProbeFirst {
		// One retry of the observation kind that failed (spec §4): a
		// stored binding re-probes; an unbound workspace re-discovers.
		retried, err := c.retryContainerObservation(ctx, d, snap)
		if err != nil {
			c.recordFailure(d.Workspace.ID, "re-observing the container: "+err.Error())
			return nil, false, fmt.Errorf("re-observing the container: %w", err)
		}
		snap.Container = ContainerSnapshot{Observed: retried}
		action = containerAction(snap)
	}

	switch action {
	case ContainerActionNone:
		if o := snap.Container.Observed; o != nil && o.Health == state.HealthPresent {
			return o, false, nil
		}
		return nil, false, nil
	case ContainerActionStart, ContainerActionAcquire:
		if c.ContainerAct == nil {
			c.recordFailure(d.Workspace.ID, ErrContainerActionUnsupported.Error())
			return nil, false, ErrContainerActionUnsupported
		}
		obs, err := c.ContainerAct.StartContainer(ctx, d.Workspace, d.Config)
		if err != nil {
			c.recordStartFailure(d.Workspace.ID, err)
			return nil, false, fmt.Errorf("starting the container: %w", err)
		}
		return &obs, true, nil
	}
	return nil, false, fmt.Errorf("unexpected container action %q", action)
}

func (c *Controller) retryContainerObservation(ctx context.Context, d Desired, snap Snapshot) (*ContainerObservation, error) {
	if snap.Stored != nil && snap.Stored.Container != nil {
		obs, err := c.Containers.ProbeContainer(ctx, *snap.Stored.Container)
		if err != nil {
			return nil, err
		}
		return &obs, nil
	}
	return c.Containers.DiscoverContainer(ctx, d.Workspace, d.Config)
}

// renderWindows turns intents into concrete window specs, now that the
// binding (if any) exists. Auto follows the container; an explicit
// container demand without one is a typed error.
func renderWindows(intents []WindowIntent, d Desired, container *ContainerObservation, act ContainerActuator) ([]WindowSpec, error) {
	specs := make([]WindowSpec, 0, len(intents))
	for _, in := range intents {
		inContainer := false
		switch in.Location {
		case WindowContainer:
			if container == nil {
				return nil, &ContainerWindowError{Window: in.Name}
			}
			inContainer = true
		case WindowAuto:
			inContainer = container != nil
		}
		if inContainer {
			if act == nil {
				return nil, ErrContainerActionUnsupported
			}
			binding := state.ContainerBinding{
				Kind:          container.Kind,
				ContainerID:   container.ContainerID,
				ContainerUser: container.ContainerUser,
				Workdir:       container.Workdir,
			}
			specs = append(specs, WindowSpec{
				Name:    in.Name,
				Command: act.ExecCommand(binding, in.Command, in.RelDir, d.Config.Environment),
				Dir:     d.Workspace.Worktree,
				Focus:   in.Focus,
			})
			continue
		}
		dir := d.Workspace.Worktree
		if in.RelDir != "" {
			dir = filepath.Join(d.Workspace.Worktree, in.RelDir)
		}
		specs = append(specs, WindowSpec{
			Name: in.Name, Command: in.Command, Dir: dir, Focus: in.Focus,
		})
	}
	return specs, nil
}

// wantsContainerWindows reports whether any intent resolves to the
// container, given whether one applies.
func wantsContainerWindows(intents []WindowIntent, containerApplies bool) bool {
	for _, in := range intents {
		if in.Location == WindowContainer ||
			(in.Location == WindowAuto && containerApplies) {
			return true
		}
	}
	return false
}

// commitOutcome records a successful open, carrying the container
// observation into the same transaction when one exists.
func (c *Controller) commitOutcome(workspaceID string, obs *ContainerObservation) error {
	op := state.Operation{Name: "open", Outcome: state.OutcomeOK}
	if obs == nil {
		if err := c.Store.RecordOperation(workspaceID, op, c.Clock.Now()); err != nil {
			return fmt.Errorf("recording the operation: %w", err)
		}
		return nil
	}
	if err := c.Store.CommitReconciliation(workspaceID, state.ReconciliationResult{
		Container: toStateObservation(obs),
		Operation: op,
	}, c.Clock.Now()); err != nil {
		return fmt.Errorf("committing the outcome: %w", err)
	}
	return nil
}

func toStateObservation(obs *ContainerObservation) *state.ContainerObservation {
	if obs == nil {
		return nil
	}
	return &state.ContainerObservation{
		Kind:          obs.Kind,
		ContainerID:   obs.ContainerID,
		ContainerUser: obs.ContainerUser,
		Workdir:       obs.Workdir,
		Health:        obs.Health,
	}
}

// recordStartFailure persists a failed container start with the real
// exit status and bounded stderr (design §9).
func (c *Controller) recordStartFailure(workspaceID string, err error) {
	op := state.Operation{Name: "open", Outcome: state.OutcomeFailed, ErrorSummary: err.Error()}
	var start *ContainerStartError
	if errors.As(err, &start) {
		code := start.ExitCode
		op.ExitStatus = &code
		if start.Stderr != "" {
			op.ErrorSummary = start.Reason + ": " + start.Stderr
		}
	}
	_ = c.Store.RecordOperation(workspaceID, op, c.Clock.Now())
}
```

Adapt `createSession`: signature becomes `createSession(ctx, d, windows []WindowSpec, containerObs *ContainerObservation)`; the `recordOK`-style success commit passes the container observation:

```go
	digest := d.Digest
	if err := c.Store.CommitReconciliation(id, state.ReconciliationResult{
		AppliedDigest: &digest,
		Container:     toStateObservation(containerObs),
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, c.Clock.Now()); err != nil {
		c.recordFailure(id, "committing the reconciliation: "+err.Error())
		return EnsureResult{}, fmt.Errorf("committing the reconciliation: %w", err)
	}
	return EnsureResult{Action: EnsureCreated, Session: name, Drifted: false}, nil
```

Delete the old `recordOK` if `commitOutcome` fully replaces it (grep for remaining callers). Add `"path/filepath"` to ensure.go's imports.

- [ ] **Step 4: Run to verify it passes** — `go test ./internal/controller/... -count=1 -race` → PASS.
- [ ] **Step 5: Gates and commit** — note `internal/cli` still compiles: `open.go` calls `Ensure` with `[]WindowSpec` → it will NOT compile. To keep the tree green, this task also mechanically migrates the call site: in `internal/cli/open.go`, replace the `windowSpecs` call and `Ensure` invocation with the Task 7 form shown below under "open.go bridge", but keep everything else untouched. The bridge:

```go
	intents := windowIntents(effective.Config)
```

and add to `internal/cli/wiring.go` (Task 7 refines nothing about it):

```go
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
```

Delete `windowSpecs` and `containerWindowError` from `wiring.go`, and delete/adapt their `wiring_test.go` tests: `TestWindowSpecsDerivation` becomes `TestWindowIntentsDerivation` asserting intents (same fixture, expectations now `controller.WindowIntent{Name: "agent-1", Command: "claude", Focus: true}` etc. with `RelDir: "sub"` and `Location: controller.WindowContainer` passing through); `TestWindowSpecsImplicitShellWindow` becomes the intent equivalent; `TestWindowSpecsRejectContainerLocation` is deleted (rejection now happens in validation and in Ensure, both tested elsewhere). `open_test.go`'s `TestOpenContainerWindowIsRejectedBeforeAnyMutation` changes expectation: with `location: container` + no devcontainer config and `enabled` unset (normalizes to auto), open now fails at **Ensure** with the `ContainerWindowError` (exit 1) and the store **does** carry a registration and a failed op — update its assertions to `code != ExitError` → keep, `strings.Contains(stderr, "requires a container")`, drop the empty-store assertion, and assert the failed op instead:

```go
	rec, err := s.Workspace(ws.ID)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	if rec.LastOperation == nil || rec.LastOperation.Outcome != state.OutcomeFailed {
		t.Errorf("last operation = %+v, want open/failed", rec.LastOperation)
	}
```

(that test therefore needs `ws := openWorkspace(t)`-style resolution — mirror `TestOpenCreatesAndReportsJSON`'s setup with the container-window config, `installScriptedSessions(t, cliAbsent())`, and `state` import).

Run the full gates, then:

```bash
git add internal/controller/ internal/cli/
git commit -m "Execute container plans in Ensure with intent-rendered windows"
```

---

### Task 7: CLI wiring — real adapter, status live probes, SessionBelongsTo, envelope additions

**Files:**
- Modify: `internal/cli/wiring.go` (delete placeholder observers; adapter seams), `internal/cli/open.go` (actuator wiring, envelope), `internal/cli/status.go` (observer + observation info), `internal/cli/attach.go` (observer + predicate), `internal/controller/plan.go` (export predicate)
- Test: `internal/cli/wiring_test.go`, `open_test.go`, `status_test.go`, `attach_test.go` adjustments + additions

**Interfaces:**
- Produces: seams `newContainerObserver = func() controller.ContainerObserver { return &container.Adapter{} }` and `newContainerActuator = func() controller.ContainerActuator { return &container.Adapter{} }`; `controller.SessionBelongsTo(s LiveSession, ws resolve.Workspace) bool` (the renamed `belongsTo`; plan.go call sites updated); `openEnvelope` gains `Container *openContainerInfo` (`{"kind","container_id","health"}`) and `ContainerWindowsStale bool` (`container_windows_stale,omitempty`); `statusEnvelope.Container.Observation` becomes `{Attempted bool, Health string omitempty, Error string omitempty, Reason string omitempty}`.

- [ ] **Step 1: Write the failing tests**

`wiring_test.go` — delete the `TestHostOnlyContainerObserver` and `TestUnprobedObserverAlwaysFails` tests (their subjects disappear); the `guardedStore` and install helpers stay. Add install helpers:

```go
func installContainerObserver(t *testing.T, o controller.ContainerObserver) {
	t.Helper()
	orig := newContainerObserver
	t.Cleanup(func() { newContainerObserver = orig })
	newContainerObserver = func() controller.ContainerObserver { return o }
}

func installContainerActuator(t *testing.T) *fake.ContainerActuator {
	t.Helper()
	orig := newContainerActuator
	t.Cleanup(func() { newContainerActuator = orig })
	a := &fake.ContainerActuator{
		StartResult: controller.ContainerObservation{
			Kind: "devcontainer", ContainerID: "cid-1",
			ContainerUser: "vscode", Workdir: "/workspaces/slabledger",
			Health: state.HealthPresent,
		},
	}
	newContainerActuator = func() controller.ContainerActuator { return a }
	return a
}
```

`open_test.go` — existing tests gain `installContainerObserver(t, &fake.ContainerObserver{})` only where the default adapter would touch the filesystem/docker: `openWorkspace`-based tests use `validConfig` (`enabled: auto`) — install `&fake.ContainerObserver{AppliesResult: false}` in `openWorkspace` itself (one line in the helper) so every existing open test stays container-free by default. Then add:

```go
func TestOpenStartsContainerAndReportsIt(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	installOpenStore(t, s)
	installFakeActuator(t)
	actC := installContainerActuator(t)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeOpen(t, stdout)
	if env.Container == nil || env.Container.ContainerID != "cid-1" ||
		env.Container.Health != "present" {
		t.Errorf("container block = %+v", env.Container)
	}
	if env.ContainerWindowsStale {
		t.Error("fresh creation reported stale windows")
	}
	if len(actC.Started) != 1 {
		t.Errorf("StartContainer calls = %d, want 1", len(actC.Started))
	}
	rec, _ := s.Workspace(ws.ID)
	if rec.Container == nil || rec.Container.ContainerUser != "vscode" {
		t.Errorf("committed binding = %+v", rec.Container)
	}
}

func TestOpenReplacementReportsStaleWindows(t *testing.T) {
	ws := openWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatal(err)
	}
	actual, err := s.AllocateSessionName(ws.ID, cliTestTime)
	if err != nil {
		t.Fatal(err)
	}
	installOpenStore(t, s)
	installFakeActuator(t)
	installContainerActuator(t)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult:  true,
		DiscoverResult: &controller.ContainerObservation{Health: state.HealthMissing, Kind: "devcontainer"},
	})
	installScriptedSessions(t, cliLive(ownLive(ws, actual)))

	code, stdout, _ := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeOpen(t, stdout)
	if env.Action != "already-running" || !env.ContainerWindowsStale {
		t.Errorf("envelope = %+v, want already-running with stale container windows", env)
	}
}
```

(`validConfig`'s windows are location-unset → auto → container when one applies, so the stale flag fires.)

`status_test.go` — existing tests: `statusWorkspace(t)` gains `installContainerObserver(t, &fake.ContainerObserver{AppliesResult: false})` inside the helper (default container-free; the `TestStatusLiveMatchingSession` expectations change: with `AppliesResult: false` and `enabled: auto`, the container section is now **absent** and `plan.container` is `none` — update that test to install `&fake.ContainerObserver{AppliesResult: true, DiscoverErr: errors.New("docker down")}` explicitly and keep its probe-first/unknown expectations, with `env.Container.Observation.Attempted` now **true** and `Error` non-empty). `TestStatusStoredBindingNeverRendersAsLive` installs `&fake.ContainerObserver{AppliesResult: true, ProbeErr: errors.New("docker down")}` and asserts `Observation.Attempted == true`, `Observation.Error != ""`, stored still `missing`/`c1`. Add:

```go
func TestStatusLiveProbeContradictsStalePresent(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordContainerObservation(ws.ID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatal(err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)
	installContainerObserver(t, &fake.ContainerObserver{
		AppliesResult: true,
		ProbeResult:   controller.ContainerObservation{Health: state.HealthMissing},
	})

	code, stdout, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStatus(t, stdout)
	if env.Container.Stored == nil || env.Container.Stored.Health != "present" {
		t.Fatalf("stored = %+v (last-observed present)", env.Container.Stored)
	}
	if !env.Container.Observation.Attempted || env.Container.Observation.Health != "missing" {
		t.Errorf("observation = %+v, want an attempted live missing", env.Container.Observation)
	}
}
```

`attach_test.go` — `statusWorkspace` change covers the observer; no assertion changes (attach ignores containers). Verify `TestAttachNeverMutates` still passes unmodified.

- [ ] **Step 2: Run to verify failures** — `go test ./internal/cli/ -count=1` → FAIL (seams, envelope fields, observation fields missing).

- [ ] **Step 3: Implement**

1. `internal/controller/plan.go`: rename `belongsTo` → `SessionBelongsTo` with an exported doc comment ("SessionBelongsTo compares all three load-bearing identity keys (design §7)…"); update its two call sites in plan.go.
2. `internal/cli/wiring.go`: delete `unprobedObserver`, `hostOnlyContainerObserver`, their `Applies` stubs, `errUnprobed`, and `devcontainerConfigPaths`; add:

```go
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
```

with the `internal/container` import (drop now-unused imports).
3. `internal/cli/open.go`: `ensureWorkspace` wires `Containers: newContainerObserver(), ContainerAct: newContainerActuator()`; `openEnvelope` gains:

```go
	Container             *openContainerInfo `json:"container,omitempty"`
	ContainerWindowsStale bool               `json:"container_windows_stale,omitempty"`
```

```go
// openContainerInfo is the ensured container as reported by open.
type openContainerInfo struct {
	Kind        string `json:"kind"`
	ContainerID string `json:"container_id"`
	Health      string `json:"health"`
}
```

populated from `res.Container`; the human path prints, when `res.Container != nil`, `fmt.Fprintf(stdout, "container %s (%s)\n", res.Container.ContainerID, res.Container.Health)` after the session line, and when `res.ContainerWindowsStale`:

```go
		fmt.Fprintln(stdout, "container replaced; existing session keeps its old windows — run `projectmux stop` (once available) or kill the session and reopen to rebuild them")
```

4. `internal/cli/status.go`: `buildStatus` wires `Containers: newContainerObserver()`; `containerObservationInfo` becomes:

```go
type containerObservationInfo struct {
	Attempted bool   `json:"attempted"`
	Health    string `json:"health,omitempty"`
	Error     string `json:"error,omitempty"`
	Reason    string `json:"reason,omitempty"`
}
```

`statusEnvelopeFrom` populates it from the snapshot instead of the fixed unprobed values:

```go
	if storedBinding != nil || snap.Container.Observed != nil || snap.Container.Err != nil {
		obs := containerObservationInfo{}
		switch {
		case snap.Container.Observed != nil || snap.Container.Err != nil:
			obs.Attempted = true
			if snap.Container.Observed != nil {
				obs.Health = string(snap.Container.Observed.Health)
			}
			if snap.Container.Err != nil {
				obs.Error = snap.Container.Err.Error()
			}
		default:
			obs.Reason = "no container applies to this workspace"
		}
		env.Container = &containerInfo{Stored: storedBinding, Observation: obs}
	}
```

(delete the `unprobedReason` constant and its uses; the human rendering's "not probed: container probing is not implemented in this build" suffix becomes a live rendering: when `Observation.Attempted`, append `"; observed " + obs.Health` or `"; observation failed: " + obs.Error`, else `"; " + obs.Reason`).
5. `internal/cli/status.go` and `attach.go`: replace the inline three-key comparisons with `controller.SessionBelongsTo(*live, ws)`.
6. `internal/cli/attach.go`: `buildAttach` wires `Containers: newContainerObserver()`.

- [ ] **Step 4: Run to verify it passes** — `go test ./... -count=1 -race` → PASS.
- [ ] **Step 5: Gates and commit**

```bash
git add internal/cli/ internal/controller/
git commit -m "Wire the real container adapter into open, status, and attach"
```

---

### Task 8: Real-Docker and real-devcontainer integration tests

**Files:**
- Test: `internal/container/integration_test.go`

- [ ] **Step 1: Write the tests**

```go
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
```

- [ ] **Step 2: Run** — `go test ./internal/container/ -count=1 -race -run Integration -v` → PASS locally (docker + devcontainer both installed; the up test takes ~10-60s warm).
- [ ] **Step 3: Gates and commit**

```bash
git add internal/container/integration_test.go
git commit -m "Add real docker and devcontainer integration tests"
```

---

### Task 9: Lifecycle — open with fake container tooling end to end

**Files:**
- Test: `internal/cli/lifecycle_test.go` (append)

The §12 requirement verbatim: lifecycle against **real tmux and fake container tooling**. These tests reuse `lifecycleRig` and install fake `docker`/`devcontainer` binaries through the *container package* seams — export nothing; use the cli seams instead: install a `container.Adapter`? No — the seams are package-private to `internal/container`. Instead these tests use the **cli** seams with `fake.ContainerObserver`/`fake.ContainerActuator` (behavioral fakes), which §12's "fake container tooling" permits and which the adapter's own fake-binary tests already complement.

- [ ] **Step 1: Write the tests** — append to `internal/cli/lifecycle_test.go`:

```go
// TestLifecycleContainerWorkspace: real tmux, fake container tooling
// (design §12): open ensures the container, the container window's
// command is the rendered exec request, reopen is idempotent (no second
// start), and a failing start creates no session.
func TestLifecycleContainerWorkspace(t *testing.T) {
	ws, socket := lifecycleRig(t, "container")
	actC := installContainerActuator(t)
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

	// The agent window (auto location) must carry the rendered exec
	// marker as its command; verify through real tmux.
	out, err := exec.Command("tmux", "-L", socket, "list-windows", "-t", ws.SessionName,
		"-F", "#{window_name}").Output()
	if err != nil {
		t.Fatalf("list-windows: %v", err)
	}
	if !strings.Contains(string(out), "agent-1") {
		t.Errorf("windows = %q", out)
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
```

(`lifecycleRig` uses `validConfig` — `enabled: auto` — and the fake observer's `AppliesResult: true` makes it container-shaped. Add `"context"` and `"github.com/gambtho/projectmux/internal/state"` imports as needed. Existing lifecycle tests need `installContainerObserver(t, &fake.ContainerObserver{AppliesResult: false})` inside `lifecycleRig` so they stay container-free — add it there, and the two new tests' explicit installs override it.)

- [ ] **Step 2: Run** — `go test ./internal/cli/ -count=1 -race -run TestLifecycle -v` → PASS.
- [ ] **Step 3: Gates and commit**

```bash
git add internal/cli/lifecycle_test.go
git commit -m "Add container lifecycle tests over real tmux with fake tooling"
```

---

### Task 10: Final verification sweep

- [ ] **Step 1: Full gates** — `gofmt -l .` empty; `go vet ./...`; `go test ./... -count=1 -race` (all integration suites run locally); `CGO_ENABLED=0 go build ./cmd/projectmux`.

- [ ] **Step 2: Isolated smoke test with a real devcontainer** (local tools present; every exit code asserted):

```bash
SMOKE=$(mktemp -d)
export PROJECTMUX_STATE_ROOT="$SMOKE/state"
export PROJECTMUX_CONFIG_ROOT="$SMOKE/config"
export TMUX_TMPDIR="$SMOKE/tmux"
mkdir -p "$PROJECTMUX_CONFIG_ROOT" "$TMUX_TMPDIR" "$SMOKE/repo/.devcontainer"
printf 'version: 1\n' > "$PROJECTMUX_CONFIG_ROOT/defaults.yaml"
printf '{"image": "alpine:3.20"}\n' > "$SMOKE/repo/.devcontainer/devcontainer.json"
git -C "$SMOKE/repo" init -q && git -C "$SMOKE/repo" -c user.email=s@e -c user.name=s commit -q --allow-empty -m init
go build -o "$SMOKE/projectmux" ./cmd/projectmux
cd "$SMOKE/repo"

"$SMOKE/projectmux" open --no-attach;  echo "open exit: $?"     # want 0; devcontainer up runs
"$SMOKE/projectmux" status --json | grep -o '"health": *"present"' | head -1
"$SMOKE/projectmux" open --no-attach;  echo "reopen exit: $?"   # want 0, already-running, no second up
docker ps --filter "label=devcontainer.local_folder=$SMOKE/repo" --format '{{.ID}}' | head -1
tmux kill-server 2>/dev/null
docker rm -f $(docker ps -aq --filter "label=devcontainer.local_folder=$SMOKE/repo") 2>/dev/null
cd - >/dev/null
```

Expected: open 0 with a real container started; status shows a live `present` observation; reopen 0 without a second `up` (watch the elapsed time); cleanup removes the container.

- [ ] **Step 3: Spec cross-check** — re-read the spec §2–§7; each behavior maps to a test or the smoke transcript. Grep `hostOnlyContainerObserver\|unprobedObserver\|errUnprobed` in `internal/cli/` → nothing.

- [ ] **Step 4: Commit any fixes** — if clean, nothing; otherwise fix, gates, commit.

---

## Self-review notes

- Spec §2 behaviors → Tasks 6–7, 9–10; §3 adapter → Tasks 4–5, 8; §4 controller (acquire, Applies-before-probe, intents, retries, StartError persistence, stale note, runner change) → Tasks 1, 3, 5, 6; §5 CLI → Task 7; §6 validation → Task 2; §7 testing rows → distributed as annotated; §8 exclusions untouched.
- Type consistency: `ContainerStartError`/`ContainerWindowError` (Task 3) consumed in Tasks 5–7 and 9; `WindowIntent`/`WindowLocation` (Task 3) consumed in 6–7; fake actuator marker string identical in Tasks 3, 6 expectations; seam names `newContainerObserver`/`newContainerActuator` identical in 7 and 9.
- Compile-greenness at every commit: Task 3 stubs the CLI observers' `Applies`; Task 6 carries the CLI call-site bridge; Task 7 deletes the placeholders.
