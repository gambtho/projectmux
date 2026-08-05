# Observation Commands (`list` / `status`) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship `projectmux list` and `projectmux status` backed by a minimal real tmux observer, the design-§5 runner utility, and the existing controller/state foundations.

**Architecture:** A new `internal/run` package executes subprocesses with bounded capture, timeouts, and process-group cancellation. A new `internal/tmux` package implements `controller.SessionObserver` via two-phase observation: a strictly validated `list-sessions -F '#{session_id}'` enumeration, then per-field `display-message -p` calls whose entire output is one raw value — no in-band framing exists for a value to forge. `internal/cli` gains `list` (store ∪ live identity sessions, no config loading) and `status` (config → resolve → `controller.Observe` → `controller.BuildPlan`), both purely observational.

**Tech Stack:** Go (module `github.com/gambtho/projectmux`), stdlib only — no new dependencies. Spec: `docs/superpowers/specs/2026-08-05-observation-commands-design.md`.

## Global Constraints

- No new module dependencies; stdlib only.
- `gofmt -l .` must be empty; run `go vet ./...` and `go test ./... -count=1 -race` before every commit; `CGO_ENABLED=0 go build ./cmd/projectmux` must succeed at the end of every task.
- Exit codes 0–5 are a stable contract (`internal/cli/cli.go:19`); `list`/`status` add none and exit 0 on any successful report, including drift, refusal, and observation uncertainty.
- JSON envelopes carry `schema_version` from the existing `OutputSchemaVersion = 1` (`internal/cli/config.go:22`). Human output is not a contract.
- Observation commands must never mutate operational records: no `RegisterWorkspace`, `AllocateSessionName`, `RecordContainerObservation`, `RecordOperation`, or `CommitReconciliation` calls (design §8/§12). `state.Open` initializing/migrating the database file is allowed.
- A stored container binding with `health=missing`/`unknown` must never render as a live container (design §8).
- tmux identity values may contain newlines and anchor-shaped content; the raw single-value transport must round-trip them (spec §5).
- Every tmux subprocess has a finite timeout (default 5s); no-server is absence only when matched narrowly (`no server running`, or `error connecting to` with `No such file or directory`, exit 1) — permission and other socket failures stay errors.
- Commit messages and code comments must not mention Claude or AI assistance.

---

### Task 1: `internal/run` — bounded subprocess runner

**Files:**
- Create: `internal/run/run.go`
- Test: `internal/run/run_test.go`

**Interfaces:**
- Consumes: nothing project-internal.
- Produces: `run.Command{Argv []string, Dir string, Timeout time.Duration}`, `run.Result{Stdout, Stderr []byte, ExitCode int, StdoutTruncated, StderrTruncated bool}`, `run.Run(ctx context.Context, cmd run.Command) (run.Result, error)`, `run.MaxCaptureBytes = 64 * 1024`. A non-zero exit is not a Go error; the error return covers empty argv, start failure, cancellation, and timeout (wrapping `ctx.Err()` so `errors.Is` works). Cancellation kills the child's whole process group (`Setpgid` + group `SIGKILL`), with a one-second `WaitDelay` backstop for descendants that survive and hold the pipes.

- [ ] **Step 1: Write the failing tests**

Create `internal/run/run_test.go`:

```go
package run

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRunCapturesExitStatusAndOutput(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv: []string{"/bin/sh", "-c", "echo out; echo err 1>&2; exit 3"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", res.ExitCode)
	}
	if got := string(res.Stdout); got != "out\n" {
		t.Errorf("Stdout = %q, want %q", got, "out\n")
	}
	if got := string(res.Stderr); got != "err\n" {
		t.Errorf("Stderr = %q, want %q", got, "err\n")
	}
}

func TestRunZeroExit(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv: []string{"/bin/sh", "-c", "exit 0"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestRunBoundsCapture(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv: []string{"/bin/sh", "-c", "head -c 200000 /dev/zero"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stdout) != MaxCaptureBytes {
		t.Errorf("len(Stdout) = %d, want %d", len(res.Stdout), MaxCaptureBytes)
	}
	if !res.StdoutTruncated {
		t.Error("StdoutTruncated = false, want true")
	}
	if res.StderrTruncated {
		t.Error("StderrTruncated = true, want false")
	}
}

func TestRunTimeoutKillsTheChild(t *testing.T) {
	start := time.Now()
	_, err := Run(context.Background(), Command{
		Argv:    []string{"sleep", "10"},
		Timeout: 100 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %v; the child was not killed at the timeout", elapsed)
	}
}

func TestRunTimeoutKillsDescendantsHoldingPipes(t *testing.T) {
	start := time.Now()
	_, err := Run(context.Background(), Command{
		Argv:    []string{"/bin/sh", "-c", "sleep 30 & sleep 30"},
		Timeout: 100 * time.Millisecond,
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Run took %v; the background grandchild outlived the group kill", elapsed)
	}
}

func TestRunArgvMetacharactersStayLiteral(t *testing.T) {
	res, err := Run(context.Background(), Command{
		Argv: []string{"/bin/echo", "$HOME;`id`|&&"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(res.Stdout); got != "$HOME;`id`|&&\n" {
		t.Errorf("Stdout = %q; shell metacharacters were interpreted", got)
	}
}

func TestRunCancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, err := Run(ctx, Command{Argv: []string{"sleep", "10"}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestRunEmptyArgvIsAnError(t *testing.T) {
	if _, err := Run(context.Background(), Command{}); err == nil {
		t.Fatal("Run accepted an empty argv")
	}
}

func TestRunStartFailureIsAnError(t *testing.T) {
	_, err := Run(context.Background(), Command{
		Argv: []string{"/nonexistent-projectmux-test-binary"},
	})
	if err == nil {
		t.Fatal("Run returned a nil error for a missing binary")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/run/ -count=1`
Expected: FAIL to build — `Run`, `Command`, `Result`, `MaxCaptureBytes` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/run/run.go`:

```go
// Package run executes subprocesses with contexts, timeouts, structured
// argv, bounded output capture, and retained exit status. It is a small
// internal adapter utility (design §5), not a public package or domain
// boundary; adapters own the meaning of the commands they run.
package run

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"
)

// MaxCaptureBytes bounds each captured stream so a chatty subprocess
// cannot balloon memory; truncation is reported, never silent.
const MaxCaptureBytes = 64 * 1024

// waitDelay bounds how long Run waits, after the group kill, for
// descendants that survived and still hold the output pipes.
const waitDelay = time.Second

// Command is one subprocess invocation. Argv is executed directly —
// never through a shell (design §11).
type Command struct {
	Argv    []string
	Dir     string
	Timeout time.Duration
}

// Result is a finished subprocess. A non-zero ExitCode is a result, not
// a Go error: callers decide what an exit status means.
type Result struct {
	Stdout          []byte
	Stderr          []byte
	ExitCode        int
	StdoutTruncated bool
	StderrTruncated bool
}

// boundedBuffer keeps the first MaxCaptureBytes and records overflow.
type boundedBuffer struct {
	buf       []byte
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if room := MaxCaptureBytes - len(b.buf); room >= len(p) {
		b.buf = append(b.buf, p...)
	} else {
		b.buf = append(b.buf, p[:room]...)
		b.truncated = true
	}
	return len(p), nil
}

// Run executes cmd and waits for it. The error return is reserved for
// failure to start, an empty argv, and context cancellation or timeout
// (wrapping ctx's error so errors.Is sees context.DeadlineExceeded and
// context.Canceled); cancellation kills the child.
func Run(ctx context.Context, cmd Command) (Result, error) {
	if len(cmd.Argv) == 0 {
		return Result{}, errors.New("run: empty argv")
	}
	if cmd.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cmd.Timeout)
		defer cancel()
	}

	var stdout, stderr boundedBuffer
	c := exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...)
	c.Dir = cmd.Dir
	c.Stdout = &stdout
	c.Stderr = &stderr
	// The child gets its own process group and cancellation kills the
	// whole group: killing only the immediate process would leave a
	// grandchild holding the pipes, keeping Wait from returning at the
	// deadline. WaitDelay backstops anything that survives the kill.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		return syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	c.WaitDelay = waitDelay

	err := c.Run()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, fmt.Errorf("running %s: %w", cmd.Argv[0], ctxErr)
	}
	res := Result{
		Stdout:          stdout.buf,
		Stderr:          stderr.buf,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
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
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/run/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/run/
git commit -m "Add the bounded subprocess runner utility"
```

---

### Task 2: `internal/tmux` — ID validation, value transport, matcher, and no-server classifier

**Files:**
- Create: `internal/tmux/decode.go`
- Test: `internal/tmux/decode_test.go`

**Interfaces:**
- Consumes: `controller.LiveSession{Name, WorkspaceID, Slug, Worktree string}`, `controller.SessionQuery{WorkspaceID string, CandidateNames []string}`, `controller.SessionObservation{ByIdentity *LiveSession, ByName []LiveSession}` (all from `internal/controller`).
- Produces (package-private, used by Task 3): `fieldFormats [4]string` (per-field `display-message` formats: name, workspace ID, slug, worktree — in that order), `parseSessionIDs(out string) ([]string, error)`, `valueFromOutput(out []byte) string`, `matchSessions(live []controller.LiveSession, q controller.SessionQuery) (controller.SessionObservation, error)`, `isNoServer(stderr []byte) bool`.

- [ ] **Step 1: Write the failing tests**

Create `internal/tmux/decode_test.go`:

```go
package tmux

import (
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
)

func TestParseSessionIDsWellFormed(t *testing.T) {
	ids, err := parseSessionIDs("$0\n$3\n$12\n")
	if err != nil {
		t.Fatalf("parseSessionIDs: %v", err)
	}
	want := []string{"$0", "$3", "$12"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Errorf("ids[%d] = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestParseSessionIDsEmptyOutput(t *testing.T) {
	for _, out := range []string{"", "\n"} {
		ids, err := parseSessionIDs(out)
		if err != nil {
			t.Fatalf("parseSessionIDs(%q): %v", out, err)
		}
		if len(ids) != 0 {
			t.Errorf("parseSessionIDs(%q) = %v, want none", out, ids)
		}
	}
}

func TestParseSessionIDsRejectsMalformedOutput(t *testing.T) {
	cases := map[string]string{
		"not an id":            "alpha\n",
		"trailing garbage":     "$0 extra\n",
		"embedded blank line":  "$0\n\n$1\n",
		"duplicate id":         "$0\n$0\n",
		"anchor-shaped forger": "$0\n$999\tname\tforged\n",
	}
	for label, out := range cases {
		if _, err := parseSessionIDs(out); err == nil {
			t.Errorf("%s: parseSessionIDs accepted %q", label, out)
		}
	}
}

func TestValueFromOutputRoundTrips(t *testing.T) {
	cases := map[string]string{
		"/w/alpha\n":                  "/w/alpha",
		"/w/evil\npath\n":             "/w/evil\npath",
		"/evil\n$999\tname\tforged\n": "/evil\n$999\tname\tforged",
		"endswithnl\n\n":              "endswithnl\n",
		"\n":                          "",
	}
	for raw, want := range cases {
		if got := valueFromOutput([]byte(raw)); got != want {
			t.Errorf("valueFromOutput(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestFieldFormatsOrderAndKeys(t *testing.T) {
	want := [4]string{
		"#{session_name}",
		"#{" + controller.KeyWorkspaceID + "}",
		"#{" + controller.KeySlug + "}",
		"#{" + controller.KeyWorktree + "}",
	}
	if fieldFormats != want {
		t.Errorf("fieldFormats = %v, want %v", fieldFormats, want)
	}
}

func TestMatchSessions(t *testing.T) {
	live := []controller.LiveSession{
		{Name: "alpha", WorkspaceID: "w1", Slug: "proj", Worktree: "/w/alpha"},
		{Name: "squatter", WorkspaceID: "w2", Slug: "other", Worktree: "/w/other"},
		{Name: "keyless"},
	}
	obs, err := matchSessions(live, controller.SessionQuery{
		WorkspaceID:    "w1",
		CandidateNames: []string{"alpha", "squatter", "keyless"},
	})
	if err != nil {
		t.Fatalf("matchSessions: %v", err)
	}
	if obs.ByIdentity == nil || obs.ByIdentity.Name != "alpha" {
		t.Errorf("ByIdentity = %+v, want alpha", obs.ByIdentity)
	}
	if len(obs.ByName) != 3 {
		t.Errorf("ByName has %d sessions, want 3: %+v", len(obs.ByName), obs.ByName)
	}
}

func TestMatchSessionsNoIdentityMatch(t *testing.T) {
	live := []controller.LiveSession{{Name: "other", WorkspaceID: "w2"}}
	obs, err := matchSessions(live, controller.SessionQuery{
		WorkspaceID:    "w1",
		CandidateNames: []string{"proposed"},
	})
	if err != nil {
		t.Fatalf("matchSessions: %v", err)
	}
	if obs.ByIdentity != nil {
		t.Errorf("ByIdentity = %+v, want nil", obs.ByIdentity)
	}
	if len(obs.ByName) != 0 {
		t.Errorf("ByName = %+v, want none", obs.ByName)
	}
}

func TestMatchSessionsDuplicateClaimIsAnError(t *testing.T) {
	live := []controller.LiveSession{
		{Name: "one", WorkspaceID: "w1"},
		{Name: "two", WorkspaceID: "w1"},
	}
	if _, err := matchSessions(live, controller.SessionQuery{WorkspaceID: "w1"}); err == nil {
		t.Fatal("matchSessions chose between duplicate identity claims")
	}
}

func TestMatchSessionsEmptyWorkspaceIDNeverMatchesKeyless(t *testing.T) {
	live := []controller.LiveSession{{Name: "keyless"}}
	obs, err := matchSessions(live, controller.SessionQuery{WorkspaceID: ""})
	if err != nil {
		t.Fatalf("matchSessions: %v", err)
	}
	if obs.ByIdentity != nil {
		t.Errorf("a keyless session matched an empty workspace ID: %+v", obs.ByIdentity)
	}
}

func TestIsNoServerMatchesOnlyConfirmedAbsence(t *testing.T) {
	for _, s := range []string{
		"no server running on /tmp/tmux-1000/default",
		"error connecting to /tmp/tmux-1000/bs (No such file or directory)",
	} {
		if !isNoServer([]byte(s)) {
			t.Errorf("isNoServer(%q) = false, want true", s)
		}
	}
	// "error connecting to" alone is not absence: tmux emits it for
	// permission and other socket failures too. Reading those as
	// absence would let planning propose creation on uncertainty.
	for _, s := range []string{
		"error connecting to /tmp/tmux-1000/bs (Operation not permitted)",
		"error connecting to /tmp/tmux-1000/bs (Permission denied)",
		"error connecting to /tmp/tmux-1000/bs (Connection refused)",
		"lost server",
		"",
		"unknown option",
	} {
		if isNoServer([]byte(s)) {
			t.Errorf("isNoServer(%q) = true, want false", s)
		}
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tmux/ -count=1`
Expected: FAIL to build — `parseSessionIDs`, `valueFromOutput`, `matchSessions`, `isNoServer`, `fieldFormats` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/tmux/decode.go`:

```go
// Package tmux is the read-only tmux adapter: it owns every tmux command
// this slice issues and translates tmux output into domain types. No
// higher layer parses tmux formats (design §5).
package tmux

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/gambtho/projectmux/internal/controller"
)

// sessionIDPattern is a full tmux session id: "$" plus digits. IDs are
// tmux-generated and cannot be influenced by identity values, which is
// what makes the enumeration phase trustworthy (spec §5).
var sessionIDPattern = regexp.MustCompile(`^\$[0-9]+$`)

// fieldFormats queries one field per subprocess, in this fixed order:
// name, workspace ID, slug, worktree. The whole output of one query is
// one raw value, so no in-band framing exists for a value to forge —
// tmux emits option values verbatim in formats, and identity values are
// not newline-free (spec §5).
var fieldFormats = [4]string{
	"#{session_name}",
	"#{" + controller.KeyWorkspaceID + "}",
	"#{" + controller.KeySlug + "}",
	"#{" + controller.KeyWorktree + "}",
}

// parseSessionIDs validates enumeration output: one well-formed session
// id per line, no duplicates. Anything else is an observation error —
// uncertainty, never a guess about which sessions exist.
func parseSessionIDs(out string) ([]string, error) {
	out = strings.TrimSuffix(out, "\n")
	if out == "" {
		return nil, nil
	}
	var ids []string
	for _, line := range strings.Split(out, "\n") {
		if !sessionIDPattern.MatchString(line) {
			return nil, fmt.Errorf("tmux emitted a malformed session id %q", line)
		}
		if slices.Contains(ids, line) {
			return nil, fmt.Errorf("tmux emitted a duplicate session id %q", line)
		}
		ids = append(ids, line)
	}
	return ids, nil
}

// valueFromOutput recovers one raw value from one display-message call:
// tmux appends exactly one newline, which is stripped; everything else,
// including embedded newlines and anchor-shaped content, is the value.
func valueFromOutput(out []byte) string {
	return strings.TrimSuffix(string(out), "\n")
}

// matchSessions filters a live-session list into the two halves of a
// session query. More than one session claiming the queried workspace ID
// is an observation error — no code path picks a claimant (spec §5).
func matchSessions(live []controller.LiveSession, q controller.SessionQuery) (controller.SessionObservation, error) {
	var obs controller.SessionObservation
	for i := range live {
		s := live[i]
		if q.WorkspaceID != "" && s.WorkspaceID == q.WorkspaceID {
			if obs.ByIdentity != nil {
				return controller.SessionObservation{}, fmt.Errorf(
					"sessions %q and %q both claim workspace %s; refusing to choose between them",
					obs.ByIdentity.Name, s.Name, q.WorkspaceID)
			}
			claimant := s
			obs.ByIdentity = &claimant
		}
		if slices.Contains(q.CandidateNames, s.Name) {
			obs.ByName = append(obs.ByName, s)
		}
	}
	return obs, nil
}

// isNoServer reports whether stderr is tmux confirming no server exists
// — absence, not failure. Matched narrowly: "no server running" (older
// tmux), or "error connecting to" together with "No such file or
// directory" (3.x). "error connecting to" alone also covers permission
// and other socket failures, which must stay errors: an unreadable
// socket never converts to absence (design §9), or planning could
// propose creation on uncertainty.
func isNoServer(stderr []byte) bool {
	s := string(stderr)
	if strings.Contains(s, "no server running") {
		return true
	}
	return strings.Contains(s, "error connecting to") &&
		strings.Contains(s, "No such file or directory")
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tmux/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/tmux/
git commit -m "Add tmux session-id validation, value transport, matcher, and no-server classifier"
```

---

### Task 3: `internal/tmux` — Client with subprocess plumbing and timeout

**Files:**
- Create: `internal/tmux/tmux.go`
- Test: `internal/tmux/client_test.go`

**Interfaces:**
- Consumes: Task 1's `run.Run`/`run.Command`; Task 2's `fieldFormats`, `parseSessionIDs`, `valueFromOutput`, `matchSessions`, `isNoServer`.
- Produces: `tmux.Client{Socket string, Timeout time.Duration}` with `Sessions(ctx context.Context) ([]controller.LiveSession, error)` and `ObserveSession(ctx context.Context, q controller.SessionQuery) (controller.SessionObservation, error)`; `tmux.DefaultTimeout = 5 * time.Second`; package var `tmuxBinary = "tmux"` (test seam). `*tmux.Client` satisfies `controller.SessionObserver`. One observation runs `1 + 4N` subprocesses (enumeration plus four per-field `display-message` calls per session), each bounded by the timeout.

- [ ] **Step 1: Write the failing tests**

Create `internal/tmux/client_test.go`:

```go
package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
)

// fakeTmux installs a shell script as the tmux binary for one test.
func fakeTmux(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	orig := tmuxBinary
	t.Cleanup(func() { tmuxBinary = orig })
	tmuxBinary = path
}

// oneSessionScript answers the two observation phases: enumeration
// returns one session id, and each per-field display-message call
// returns a canned value — the worktree deliberately spans lines to
// prove raw values round-trip through the client untouched.
const oneSessionScript = `#!/bin/sh
while [ "$1" = "-L" ]; do shift 2; done
cmd="$1"
shift
case "$cmd" in
list-sessions)
	printf '$0\n'
	;;
display-message)
	# args: -p -t <target> <format>
	case "$4" in
	'#{session_name}') printf 'alpha\n' ;;
	'#{@dev_workspace_id}') printf 'w1\n' ;;
	'#{@dev_slug}') printf 'proj\n' ;;
	'#{@dev_worktree}') printf '/w/evil\npath\n' ;;
	*) exit 2 ;;
	esac
	;;
*)
	exit 2
	;;
esac
`

func TestSessionsObservesRawValues(t *testing.T) {
	fakeTmux(t, oneSessionScript)
	live, err := (&Client{}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := controller.LiveSession{
		Name: "alpha", WorkspaceID: "w1", Slug: "proj", Worktree: "/w/evil\npath",
	}
	if len(live) != 1 || live[0] != want {
		t.Errorf("Sessions = %+v, want [%+v]", live, want)
	}
}

func TestSessionsRejectsEmptySessionName(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
while [ "$1" = "-L" ]; do shift 2; done
case "$1" in
list-sessions) printf '$0\n' ;;
display-message) printf '\n' ;;
esac
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions accepted an empty session name (vanished session)")
	}
}

func TestSessionsRejectsMalformedIDs(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
printf 'not-an-id\n'
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions accepted a malformed session id")
	}
}

func TestSessionsNoServerIsEmptyNotError(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
echo 'no server running on /tmp/tmux-1000/default' 1>&2
exit 1
`)
	live, err := (&Client{}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("Sessions = %+v, want none", live)
	}
}

func TestSessionsOtherFailureIsAnError(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
echo 'lost server' 1>&2
exit 1
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions converted an unrecognized tmux failure into absence")
	}
}

func TestSessionsPermissionFailureIsAnErrorNotAbsence(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
echo 'error connecting to /tmp/tmux-1000/default (Operation not permitted)' 1>&2
exit 1
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions read a permission failure as an absent server")
	}
}

func TestSessionsTimeoutPropagates(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
sleep 10
`)
	start := time.Now()
	_, err := (&Client{Timeout: 100 * time.Millisecond}).Sessions(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Sessions took %v; the subprocess outlived the timeout", elapsed)
	}
}

func TestObserveSessionFiltersSessions(t *testing.T) {
	fakeTmux(t, oneSessionScript)
	obs, err := (&Client{}).ObserveSession(context.Background(), controller.SessionQuery{
		WorkspaceID:    "w1",
		CandidateNames: []string{"alpha"},
	})
	if err != nil {
		t.Fatalf("ObserveSession: %v", err)
	}
	if obs.ByIdentity == nil || obs.ByIdentity.Name != "alpha" {
		t.Errorf("ByIdentity = %+v, want alpha", obs.ByIdentity)
	}
	if len(obs.ByName) != 1 {
		t.Errorf("ByName = %+v, want alpha only", obs.ByName)
	}
}

func TestClientIsASessionObserver(t *testing.T) {
	var _ controller.SessionObserver = (*Client)(nil)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/tmux/ -count=1`
Expected: FAIL to build — `Client`, `tmuxBinary` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/tmux/tmux.go`:

```go
package tmux

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/run"
)

// DefaultTimeout bounds a tmux subprocess when the client does not set
// one. Signal cancellation is for interactive interruption; this finite
// timeout is the hang defense for unattended callers (spec §5).
const DefaultTimeout = 5 * time.Second

// tmuxBinary is the executable to invoke; tests substitute a script.
var tmuxBinary = "tmux"

// Client observes live tmux sessions. Socket, when non-empty, is passed
// as -L for isolated servers (integration tests); Timeout bounds every
// subprocess, with the zero value meaning DefaultTimeout.
type Client struct {
	Socket  string
	Timeout time.Duration
}

var _ controller.SessionObserver = (*Client)(nil)

// Sessions lists every live session with whatever identity keys it
// carries, in two phases: a strictly validated session-id enumeration,
// then four per-field display-message calls per session whose entire
// output is one raw value (spec §5) — no in-band framing exists for a
// value to forge. No server is absence: an empty list and nil error.
// Any other failure is an error, which callers must render as
// uncertainty, never as absence (design §9).
func (c *Client) Sessions(ctx context.Context) ([]controller.LiveSession, error) {
	res, err := c.exec(ctx, "list-sessions", "-F", "#{session_id}")
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 {
		if res.ExitCode == 1 && isNoServer(res.Stderr) {
			return nil, nil
		}
		return nil, fmt.Errorf("tmux list-sessions exited %d: %s",
			res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	if res.StdoutTruncated {
		return nil, fmt.Errorf("tmux list-sessions output exceeded %d bytes", run.MaxCaptureBytes)
	}
	ids, err := parseSessionIDs(string(res.Stdout))
	if err != nil {
		return nil, err
	}

	live := make([]controller.LiveSession, 0, len(ids))
	for _, id := range ids {
		var values [4]string
		for i, format := range fieldFormats {
			value, err := c.field(ctx, id, format)
			if err != nil {
				return nil, err
			}
			values[i] = value
		}
		if values[0] == "" {
			// Real sessions cannot have empty names. A session that
			// vanished between the phases surfaces here: a dead -t
			// target can exit 0 with empty output.
			return nil, fmt.Errorf("tmux reported an empty name for session %s", id)
		}
		live = append(live, controller.LiveSession{
			Name:        values[0],
			WorkspaceID: values[1],
			Slug:        values[2],
			Worktree:    values[3],
		})
	}
	return live, nil
}

// field reads one raw value for one session.
func (c *Client) field(ctx context.Context, id, format string) (string, error) {
	res, err := c.exec(ctx, "display-message", "-p", "-t", id, format)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("tmux display-message for session %s exited %d: %s",
			id, res.ExitCode, bytes.TrimSpace(res.Stderr))
	}
	if res.StdoutTruncated {
		return "", fmt.Errorf("tmux display-message output exceeded %d bytes", run.MaxCaptureBytes)
	}
	return valueFromOutput(res.Stdout), nil
}

// exec runs one tmux subprocess with the client's socket and timeout.
func (c *Client) exec(ctx context.Context, args ...string) (run.Result, error) {
	argv := []string{tmuxBinary}
	if c.Socket != "" {
		argv = append(argv, "-L", c.Socket)
	}
	argv = append(argv, args...)

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	res, err := run.Run(ctx, run.Command{Argv: argv, Timeout: timeout})
	if err != nil {
		return run.Result{}, fmt.Errorf("observing tmux: %w", err)
	}
	return res, nil
}

// ObserveSession implements controller.SessionObserver by filtering the
// bulk observation in-process.
func (c *Client) ObserveSession(ctx context.Context, q controller.SessionQuery) (controller.SessionObservation, error) {
	live, err := c.Sessions(ctx)
	if err != nil {
		return controller.SessionObservation{}, err
	}
	return matchSessions(live, q)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/tmux/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/tmux/
git commit -m "Add the tmux client with bounded, timed session observation"
```

---

### Task 4: `internal/tmux` — real-tmux integration tests

**Files:**
- Test: `internal/tmux/integration_test.go`

**Interfaces:**
- Consumes: Task 3's `tmux.Client{Socket}`; a real `tmux` binary when installed (CI's ubuntu-latest ships one; tests skip otherwise).
- Produces: nothing new — proof against real tmux on isolated `-L` sockets (design §12).

- [ ] **Step 1: Write the tests**

Create `internal/tmux/integration_test.go`:

```go
package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
)

// isolatedSocket names a per-test tmux server and kills it on cleanup.
func isolatedSocket(t *testing.T, label string) string {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	socket := fmt.Sprintf("projectmux-%s-%d", label, os.Getpid())
	t.Cleanup(func() {
		_ = exec.Command("tmux", "-L", socket, "kill-server").Run()
	})
	return socket
}

func tmuxOK(t *testing.T, socket string, args ...string) {
	t.Helper()
	full := append([]string{"-L", socket}, args...)
	if out, err := exec.Command("tmux", full...).CombinedOutput(); err != nil {
		t.Fatalf("tmux %v: %v\n%s", args, err, out)
	}
}

func TestIntegrationSessionsRoundTrip(t *testing.T) {
	socket := isolatedSocket(t, "roundtrip")
	tmuxOK(t, socket, "new-session", "-d", "-s", "alpha")
	tmuxOK(t, socket, "set-option", "-t", "alpha", controller.KeyWorkspaceID, "w1")
	tmuxOK(t, socket, "set-option", "-t", "alpha", controller.KeySlug, "proj")
	tmuxOK(t, socket, "set-option", "-t", "alpha", controller.KeyWorktree, "/w/evil\n$999\npath")
	tmuxOK(t, socket, "new-session", "-d", "-s", "beta")

	live, err := (&Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	byName := map[string]controller.LiveSession{}
	for _, s := range live {
		byName[s.Name] = s
	}
	if len(byName) != 2 {
		t.Fatalf("observed %d sessions, want 2: %+v", len(byName), live)
	}
	alpha := byName["alpha"]
	if alpha.WorkspaceID != "w1" || alpha.Slug != "proj" {
		t.Errorf("alpha identity = %+v, want w1/proj", alpha)
	}
	if alpha.Worktree != "/w/evil\n$999\npath" {
		t.Errorf("alpha worktree = %q; embedded newline or anchor-shaped content did not round-trip",
			alpha.Worktree)
	}
	beta := byName["beta"]
	if beta.WorkspaceID != "" || beta.Slug != "" || beta.Worktree != "" {
		t.Errorf("beta should carry no identity keys: %+v", beta)
	}
}

func TestIntegrationObserveSessionMatches(t *testing.T) {
	socket := isolatedSocket(t, "observe")
	tmuxOK(t, socket, "new-session", "-d", "-s", "mine")
	tmuxOK(t, socket, "set-option", "-t", "mine", controller.KeyWorkspaceID, "w1")
	tmuxOK(t, socket, "new-session", "-d", "-s", "squatter")

	obs, err := (&Client{Socket: socket}).ObserveSession(context.Background(),
		controller.SessionQuery{WorkspaceID: "w1", CandidateNames: []string{"squatter"}})
	if err != nil {
		t.Fatalf("ObserveSession: %v", err)
	}
	if obs.ByIdentity == nil || obs.ByIdentity.Name != "mine" {
		t.Errorf("ByIdentity = %+v, want mine", obs.ByIdentity)
	}
	if len(obs.ByName) != 1 || obs.ByName[0].Name != "squatter" {
		t.Errorf("ByName = %+v, want squatter", obs.ByName)
	}
}

func TestIntegrationNoServerIsAbsence(t *testing.T) {
	socket := isolatedSocket(t, "noserver")
	live, err := (&Client{Socket: socket}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions on a never-started server: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("Sessions = %+v, want none", live)
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./internal/tmux/ -count=1 -race -v -run Integration`
Expected: PASS (or SKIP if tmux is genuinely absent — it is installed here).

- [ ] **Step 3: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/tmux/integration_test.go
git commit -m "Add real-tmux integration tests on isolated sockets"
```

---

### Task 5: CLI wiring — seams, shared render types, and mutation guard

**Files:**
- Create: `internal/cli/wiring.go`
- Modify: `internal/cli/config.go` (generalize `writeJSON`'s first value parameter)
- Test: `internal/cli/wiring_test.go`

**Interfaces:**
- Consumes: `controller.Store`, `controller.SessionObserver`, `controller.LiveSession`, `state.Root`, `state.Open`, `tmux.Client`, `fake.Store`.
- Produces (package-private, used by Tasks 6–7):
  - `type stateStore interface { controller.Store; Close() error }`
  - seam vars: `openStore func() (stateStore, error)`, `liveSessions func(ctx context.Context) ([]controller.LiveSession, error)`, `newSessionObserver func() controller.SessionObserver`
  - `unprobedObserver{}` (a `controller.ContainerObserver` whose methods return `errUnprobed`), `systemClock{}`, `stamp(t time.Time) string`
  - `storedContainerInfo` JSON struct + `storedContainer(b *state.ContainerBinding) *storedContainerInfo`
  - `writeJSON(w io.Writer, v any, compact bool) error` (was `env envelope`)
  - test helpers in `wiring_test.go`: `guardedStore`, `installFakeStore`, `installLiveSessions`, `installSessionObserver`, `cliTestTime`.

- [ ] **Step 1: Write the failing test**

Create `internal/cli/wiring_test.go`:

```go
package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

var cliTestTime = time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)

// guardedStore fails the test if an observation command mutates the
// store: design §8/§12 — list and status must not mutate workspaces.
type guardedStore struct {
	*fake.Store
	t *testing.T
}

func (g guardedStore) Close() error { return nil }

func (g guardedStore) forbidden(method string) error {
	g.t.Errorf("observation command called %s", method)
	return errors.New("observation commands must not mutate the store")
}

func (g guardedStore) RegisterWorkspace(resolve.Workspace, string, time.Time) error {
	return g.forbidden("RegisterWorkspace")
}

func (g guardedStore) AllocateSessionName(string, time.Time) (string, error) {
	return "", g.forbidden("AllocateSessionName")
}

func (g guardedStore) RecordContainerObservation(string, state.ContainerObservation, time.Time) error {
	return g.forbidden("RecordContainerObservation")
}

func (g guardedStore) RecordOperation(string, state.Operation, time.Time) error {
	return g.forbidden("RecordOperation")
}

func (g guardedStore) CommitReconciliation(string, state.ReconciliationResult, time.Time) error {
	return g.forbidden("CommitReconciliation")
}

func installFakeStore(t *testing.T, s *fake.Store) {
	t.Helper()
	orig := openStore
	t.Cleanup(func() { openStore = orig })
	openStore = func() (stateStore, error) {
		return guardedStore{Store: s, t: t}, nil
	}
}

func installLiveSessions(t *testing.T, sessions []controller.LiveSession, err error) {
	t.Helper()
	orig := liveSessions
	t.Cleanup(func() { liveSessions = orig })
	liveSessions = func(context.Context) ([]controller.LiveSession, error) {
		return sessions, err
	}
}

func installSessionObserver(t *testing.T, obs controller.SessionObservation, err error) {
	t.Helper()
	orig := newSessionObserver
	t.Cleanup(func() { newSessionObserver = orig })
	newSessionObserver = func() controller.SessionObserver {
		return &fake.SessionObserver{Observation: obs, Err: err}
	}
}

func TestUnprobedObserverAlwaysFails(t *testing.T) {
	var obs controller.ContainerObserver = unprobedObserver{}
	if _, err := obs.ProbeContainer(context.Background(), state.ContainerBinding{}); err == nil {
		t.Error("ProbeContainer pretended to probe")
	}
	if _, err := obs.DiscoverContainer(context.Background(), resolve.Workspace{},
		config.Config{Version: 1}); err == nil {
		t.Error("DiscoverContainer pretended to discover")
	}
}

func TestStoredContainerRendersTimestampsUTC(t *testing.T) {
	info := storedContainer(&state.ContainerBinding{
		Kind:        "devcontainer",
		ContainerID: "c1",
		Health:      state.HealthMissing,
		ObservedAt:  cliTestTime,
	})
	if info.Health != "missing" {
		t.Errorf("Health = %q, want missing", info.Health)
	}
	if info.ObservedAt != "2026-08-05T12:00:00Z" {
		t.Errorf("ObservedAt = %q", info.ObservedAt)
	}
	if storedContainer(nil) != nil {
		t.Error("storedContainer(nil) should be nil")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to build — `openStore`, `liveSessions`, `newSessionObserver`, `stateStore`, `unprobedObserver`, `storedContainer` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/wiring.go`:

```go
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
```

In `internal/cli/config.go`, change `writeJSON`'s signature so all envelopes can use it — replace:

```go
func writeJSON(w io.Writer, env envelope, compact bool) error {
```

with:

```go
func writeJSON(w io.Writer, v any, compact bool) error {
```

and inside it change `enc.Encode(env)` to `enc.Encode(v)`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -count=1 -race`
Expected: PASS (including all pre-existing config tests).

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/cli/wiring.go internal/cli/wiring_test.go internal/cli/config.go
git commit -m "Add observation-command wiring seams and shared render types"
```

---

### Task 6: `projectmux list`

**Files:**
- Create: `internal/cli/list.go`
- Modify: `internal/cli/cli.go` (dispatch case, usage text, context plumbing)
- Test: `internal/cli/list_test.go`

**Interfaces:**
- Consumes: Task 5's seams (`openStore`, `liveSessions`), `storedContainerInfo`/`storedContainer`, `writeJSON`, test helpers; `state.Record`; `controller.LiveSession`.
- Produces: `runList(ctx context.Context, args []string, stdout io.Writer) error`; JSON types `listEnvelope{SchemaVersion int, TmuxObserved bool, Workspaces []listRow}` and `listRow{ID, Slug, Worktree string, IsPrimary bool, ProposedSession string, ActualSession *string, SessionState string, LiveSession *string, Container *storedContainerInfo, Recorded bool, IdentityConflict bool}` (JSON names below). `dispatch` gains a `ctx context.Context` first parameter; `Main` builds it with `signal.NotifyContext`.

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/list_test.go`:

```go
package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

func listWorkspace(id, slug string) resolve.Workspace {
	return resolve.Workspace{
		ID:          id,
		Slug:        slug,
		Worktree:    "/w/" + slug,
		SessionName: slug,
		IsPrimary:   true,
	}
}

func seededListStore(t *testing.T) *fake.Store {
	t.Helper()
	s := fake.NewStore()
	if err := s.RegisterWorkspace(listWorkspace("w1", "alpha"), "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, err := s.AllocateSessionName("w1", cliTestTime); err != nil {
		t.Fatalf("allocate: %v", err)
	}
	if err := s.RecordContainerObservation("w1", state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := s.RecordContainerObservation("w1", state.ContainerObservation{
		Health: state.HealthMissing,
	}, cliTestTime); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	if err := s.RegisterWorkspace(listWorkspace("w2", "beta"), "sha256:d", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	return s
}

func decodeList(t *testing.T, stdout string) listEnvelope {
	t.Helper()
	var env listEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding list JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestListUnionsStoredAndLiveSessions(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, []controller.LiveSession{
		{Name: "alpha", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/alpha"},
		{Name: "rogue", WorkspaceID: "w9", Slug: "elsewhere", Worktree: "/w/elsewhere"},
	}, nil)

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeList(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d", env.SchemaVersion)
	}
	if !env.TmuxObserved {
		t.Error("tmux_observed = false, want true")
	}
	if len(env.Workspaces) != 3 {
		t.Fatalf("%d rows, want 3: %+v", len(env.Workspaces), env.Workspaces)
	}

	alpha := env.Workspaces[0]
	if alpha.Slug != "alpha" || !alpha.Recorded || alpha.SessionState != "live" {
		t.Errorf("alpha row = %+v", alpha)
	}
	if alpha.LiveSession == nil || *alpha.LiveSession != "alpha" {
		t.Errorf("alpha live_session = %v, want alpha", alpha.LiveSession)
	}
	if alpha.Container == nil || alpha.Container.Health != "missing" {
		t.Errorf("alpha container = %+v, want retained missing binding", alpha.Container)
	}
	if alpha.IdentityConflict {
		t.Error("alpha identity_conflict = true, want false")
	}

	beta := env.Workspaces[1]
	if beta.Slug != "beta" || beta.SessionState != "absent" || beta.Container != nil {
		t.Errorf("beta row = %+v", beta)
	}
	if beta.ActualSession != nil {
		t.Errorf("beta actual_session = %v, want unassigned", beta.ActualSession)
	}

	rogue := env.Workspaces[2]
	if rogue.Recorded || rogue.SessionState != "live" || rogue.ID != "w9" {
		t.Errorf("rogue row = %+v", rogue)
	}
	if rogue.LiveSession == nil || *rogue.LiveSession != "rogue" {
		t.Errorf("rogue live_session = %v", rogue.LiveSession)
	}
	if rogue.IdentityConflict {
		t.Error("rogue identity_conflict = true, want false")
	}
}

func TestListIdentityConflictOnContradictoryKeys(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, []controller.LiveSession{
		{Name: "alpha", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/other"},
	}, nil)

	code, stdout, _ := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeList(t, stdout)
	if !env.Workspaces[0].IdentityConflict {
		t.Errorf("contradictory worktree key did not set identity_conflict: %+v", env.Workspaces[0])
	}
}

func TestListDuplicateClaimsReportUncertainty(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, []controller.LiveSession{
		{Name: "one", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/alpha"},
		{Name: "two", WorkspaceID: "w1", Slug: "alpha", Worktree: "/w/alpha"},
	}, nil)

	code, stdout, _ := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeList(t, stdout)
	if len(env.Workspaces) != 4 {
		t.Fatalf("%d rows, want 4 (2 stored + 2 claimants): %+v", len(env.Workspaces), env.Workspaces)
	}
	alpha := env.Workspaces[0]
	if alpha.SessionState != "unknown" || !alpha.IdentityConflict || alpha.LiveSession != nil {
		t.Errorf("duplicate-claimed stored row = %+v, want unknown/conflict/no live_session", alpha)
	}
	for _, row := range env.Workspaces[2:] {
		if row.Recorded || !row.IdentityConflict || row.SessionState != "live" {
			t.Errorf("claimant row = %+v, want unrecorded live conflict", row)
		}
	}
}

func TestListTmuxFailureIsUncertainNotFatal(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, nil, errors.New("tmux exploded"))

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeList(t, stdout)
	if env.TmuxObserved {
		t.Error("tmux_observed = true after a failed observation")
	}
	for _, row := range env.Workspaces {
		if row.SessionState != "unknown" {
			t.Errorf("row %s session_state = %q, want unknown", row.Slug, row.SessionState)
		}
	}
}

func TestListHumanNeverRendersRetainedBindingAsLive(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, nil, nil)

	code, stdout, _ := run(t, "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "missing (as of 2026-08-05T12:00:00Z)") {
		t.Errorf("human output does not label the retained binding: %s", stdout)
	}
	if strings.Contains(stdout, "alpha (unassigned)") {
		t.Errorf("alpha has an assigned session yet renders unassigned: %s", stdout)
	}
	if !strings.Contains(stdout, "beta (unassigned)") {
		t.Errorf("beta has no assigned session yet renders without the marker: %s", stdout)
	}
}

func TestListEmpty(t *testing.T) {
	installFakeStore(t, fake.NewStore())
	installLiveSessions(t, nil, nil)

	code, stdout, _ := run(t, "list")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout, "no workspaces") {
		t.Errorf("empty list output: %q", stdout)
	}
}

func TestListRejectsArguments(t *testing.T) {
	code, _, _ := run(t, "list", "extra")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}

func TestListCompactImpliesJSON(t *testing.T) {
	installFakeStore(t, fake.NewStore())
	installLiveSessions(t, nil, nil)

	code, stdout, _ := run(t, "list", "--compact")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.Count(strings.TrimSpace(stdout), "\n") != 0 {
		t.Errorf("compact output is not one line: %q", stdout)
	}
	decodeList(t, stdout)
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to build — `listEnvelope`, `runList` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/list.go`:

```go
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/controller"
)

const listHelp = `usage: projectmux list [--json] [--compact]

List recorded workspaces and live identity-carrying tmux sessions.

  --json     emit the versioned JSON envelope instead of human-readable text
  --compact  emit the JSON on a single line (implies --json)
`

// listEnvelope is the versioned JSON structure for projectmux list.
// tmux_observed is false when the session observation failed; every
// session_state is then "unknown" (a tmux outage is not absence).
type listEnvelope struct {
	SchemaVersion int       `json:"schema_version"`
	TmuxObserved  bool      `json:"tmux_observed"`
	Workspaces    []listRow `json:"workspaces"`
}

// listRow is one workspace or unrecorded live session. live_session is
// present only when exactly one live session corresponds to the row.
type listRow struct {
	ID               string               `json:"id"`
	Slug             string               `json:"slug"`
	Worktree         string               `json:"worktree"`
	IsPrimary        bool                 `json:"is_primary"`
	ProposedSession  string               `json:"proposed_session,omitempty"`
	ActualSession    *string              `json:"actual_session,omitempty"`
	SessionState     string               `json:"session_state"`
	LiveSession      *string              `json:"live_session,omitempty"`
	Container        *storedContainerInfo `json:"container,omitempty"`
	Recorded         bool                 `json:"recorded"`
	IdentityConflict bool                 `json:"identity_conflict"`
}

func runList(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("list")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, listHelp)
			return nil
		}
		return usagef("list: %s", err)
	}
	if fs.NArg() > 0 {
		return usagef("list: unexpected argument %q", fs.Arg(0))
	}
	if *compact {
		*asJSON = true
	}

	env, err := buildList(ctx)
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	return writeListHuman(stdout, env)
}

// buildList unions stored workspaces with live identity-carrying
// sessions. It loads no workspace configuration and resolves nothing: a
// broken workspace YAML cannot break the summary, and any number of
// workspaces costs one tmux subprocess. A failed tmux observation
// renders as uncertainty; only a store failure aborts.
func buildList(ctx context.Context) (listEnvelope, error) {
	st, err := openStore()
	if err != nil {
		return listEnvelope{}, err
	}
	defer st.Close()

	records, err := st.Workspaces()
	if err != nil {
		return listEnvelope{}, fmt.Errorf("reading stored workspaces: %w", err)
	}

	live, obsErr := liveSessions(ctx)
	env := listEnvelope{
		SchemaVersion: OutputSchemaVersion,
		TmuxObserved:  obsErr == nil,
		Workspaces:    []listRow{},
	}

	// Group live sessions by their workspace-ID key. Keyless sessions
	// are not ours and never appear in list output.
	byID := map[string][]controller.LiveSession{}
	for _, s := range live {
		if s.WorkspaceID != "" {
			byID[s.WorkspaceID] = append(byID[s.WorkspaceID], s)
		}
	}

	consumed := map[string]bool{}
	for i := range records {
		rec := records[i]
		row := listRow{
			ID:              rec.ID,
			Slug:            rec.Slug,
			Worktree:        rec.Worktree,
			IsPrimary:       rec.IsPrimary,
			ProposedSession: rec.ProposedSession,
			ActualSession:   rec.ActualSession,
			Container:       storedContainer(rec.Container),
			Recorded:        true,
		}
		switch claimants := byID[rec.ID]; {
		case obsErr != nil:
			row.SessionState = "unknown"
		case len(claimants) == 0:
			row.SessionState = "absent"
		case len(claimants) == 1:
			s := claimants[0]
			name := s.Name
			row.SessionState = "live"
			row.LiveSession = &name
			row.IdentityConflict = s.Slug != rec.Slug || s.Worktree != rec.Worktree
			consumed[rec.ID] = true
		default:
			// Multiple sessions claim this workspace: uncertainty,
			// consistent with ObserveSession — no claimant is picked.
			// The claimants also render below as unrecorded rows.
			row.SessionState = "unknown"
			row.IdentityConflict = true
		}
		env.Workspaces = append(env.Workspaces, row)
	}

	var extras []controller.LiveSession
	for id, sessions := range byID {
		if !consumed[id] {
			extras = append(extras, sessions...)
		}
	}
	sort.Slice(extras, func(i, j int) bool { return extras[i].Name < extras[j].Name })
	for _, s := range extras {
		name := s.Name
		env.Workspaces = append(env.Workspaces, listRow{
			ID:               s.WorkspaceID,
			Slug:             s.Slug,
			Worktree:         s.Worktree,
			SessionState:     "live",
			LiveSession:      &name,
			Recorded:         false,
			IdentityConflict: len(byID[s.WorkspaceID]) > 1,
		})
	}
	return env, nil
}

// writeListHuman renders the summary table. This layout is explicitly
// not a compatibility contract; automation should use --json.
func writeListHuman(w io.Writer, env listEnvelope) error {
	if len(env.Workspaces) == 0 {
		_, err := fmt.Fprintln(w,
			"no workspaces recorded and no identity-carrying tmux sessions found")
		return err
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "WORKSPACE\tSESSION\tTMUX\tCONTAINER\tNOTES")
	for _, row := range env.Workspaces {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			dashIfEmpty(row.Slug), listSessionCell(row), row.SessionState,
			listContainerCell(row.Container), listNotesCell(row))
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}

func listSessionCell(row listRow) string {
	if row.Recorded {
		if row.ActualSession != nil {
			return *row.ActualSession
		}
		return row.ProposedSession + " (unassigned)"
	}
	if row.LiveSession != nil {
		return *row.LiveSession
	}
	return "-"
}

// listContainerCell renders last-observed binding state. A retained
// binding with health missing or unknown must never read as a live
// container (design §8), so health always leads and carries its age.
func listContainerCell(c *storedContainerInfo) string {
	if c == nil {
		return "-"
	}
	return fmt.Sprintf("%s (as of %s)", c.Health, c.ObservedAt)
}

func listNotesCell(row listRow) string {
	var notes []string
	if !row.Recorded {
		notes = append(notes, "unrecorded")
	}
	if row.IdentityConflict {
		notes = append(notes, "conflict")
	}
	if len(notes) == 0 {
		return "-"
	}
	return strings.Join(notes, ", ")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
```

In `internal/cli/cli.go`:

1. Add imports `"context"`, `"os"`, `"os/signal"`, `"syscall"`.
2. Change `Main` to build a signal-aware context and pass it down:

```go
// Main runs one command and returns the process exit code. It writes nothing
// to stdout for a failing command, so callers can pipe stdout without having to
// filter diagnostics out of it.
func Main(args []string, stdout, stderr io.Writer) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	err := dispatch(ctx, args, stdout)
	if err == nil {
		return ExitOK
	}
	fmt.Fprintf(stderr, "projectmux: %s\n", err)

	var usageErr *usageError
	if errors.As(err, &usageErr) {
		fmt.Fprint(stderr, "\n", usage)
	}
	return exitCode(err)
}
```

3. Change `dispatch` to accept the context and route `list`:

```go
// dispatch routes one command. Diagnostics are Main's responsibility, so
// nothing below writes to stderr.
func dispatch(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stdout, usage)
		return nil
	}

	command, rest := args[0], args[1:]
	switch command {
	case "help", "-h", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	case "version", "--version":
		fmt.Fprintln(stdout, versionString())
		return nil
	case "config":
		return runConfig(rest, stdout)
	case "list":
		return runList(ctx, rest, stdout)
	default:
		return usagef("unknown command %q", command)
	}
}
```

4. In the `usage` constant, insert this entry after the `config` entry (keep the rest verbatim):

```text
  list [--json] [--compact]
        list recorded workspaces and live identity-carrying tmux sessions
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/cli/list.go internal/cli/list_test.go internal/cli/cli.go
git commit -m "Add projectmux list over stored workspaces and live identity sessions"
```

---

### Task 7: `projectmux status`

**Files:**
- Create: `internal/cli/status.go`
- Modify: `internal/cli/cli.go` (dispatch case and usage entry)
- Test: `internal/cli/status_test.go`

**Interfaces:**
- Consumes: Task 5's seams and helpers; `config.Root`/`LoadDefaults`/`Load`, `resolve.Resolve`, `controller.Controller`/`Desired`/`Observe`/`BuildPlan`/`Snapshot`/`Plan`, `workspaceInfo` (config.go).
- Produces: `runStatus(ctx context.Context, args []string, stdout io.Writer) error`; JSON types `statusEnvelope`, `storedInfo`, `sessionInfo`, `containerInfo`, `containerObservationInfo`, `configInfo`, `operationInfo`, `planInfo` (shapes below, spec §3).

- [ ] **Step 1: Write the failing tests**

Create `internal/cli/status_test.go`:

```go
package cli

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// statusWorkspace builds the standard test repository and returns its
// resolved identity so store seeding and live-session fixtures agree
// with what buildStatus will resolve.
func statusWorkspace(t *testing.T) resolve.Workspace {
	t.Helper()
	workspace(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/slabledger.yaml": validConfig,
	})
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	ws, err := resolve.Resolve("", nil, cwd)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return ws
}

func decodeStatus(t *testing.T, stdout string) statusEnvelope {
	t.Helper()
	var env statusEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding status JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestStatusLiveMatchingSession(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	actual, err := s.AllocateSessionName(ws.ID, cliTestTime)
	if err != nil {
		t.Fatalf("allocate: %v", err)
	}
	applied := "sha256:stale"
	if err := s.CommitReconciliation(ws.ID, state.ReconciliationResult{
		AppliedDigest: &applied,
		Operation:     state.Operation{Name: "open", Outcome: state.OutcomeOK},
	}, cliTestTime); err != nil {
		t.Fatalf("commit: %v", err)
	}
	installFakeStore(t, s)
	live := controller.LiveSession{
		Name: actual, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{
		ByIdentity: &live,
		ByName:     []controller.LiveSession{live},
	}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if !env.Registered || env.Stored == nil {
		t.Fatalf("registered = %t, stored = %+v", env.Registered, env.Stored)
	}
	if env.Session.State != "live" {
		t.Errorf("session.state = %q", env.Session.State)
	}
	if env.Session.Identity == nil || *env.Session.Identity != "match" {
		t.Errorf("session.identity = %v, want match", env.Session.Identity)
	}
	if env.Plan.Session != "none" {
		t.Errorf("plan.session = %q, want none", env.Plan.Session)
	}
	if !env.Config.Drifted {
		t.Error("config.drifted = false; the applied digest is stale")
	}
	if env.Config.AppliedDigest == nil || *env.Config.AppliedDigest != applied {
		t.Errorf("config.applied_digest = %v", env.Config.AppliedDigest)
	}
	if !env.Plan.Reapply {
		t.Error("plan.reapply = false, want true")
	}
	if env.LastOperation == nil || env.LastOperation.Operation != "open" {
		t.Errorf("last_operation = %+v", env.LastOperation)
	}
	// validConfig enables devcontainer: auto; nothing is bound, and the
	// observer is honest about not probing.
	if env.Container == nil {
		t.Fatal("container section missing while devcontainer is enabled")
	}
	if env.Container.Stored != nil {
		t.Errorf("container.stored = %+v, want none", env.Container.Stored)
	}
	if env.Container.Observation.Attempted {
		t.Error("container.observation.attempted = true; no probe exists in this build")
	}
	if env.Plan.Container != "probe-first" {
		t.Errorf("plan.container = %q, want probe-first", env.Plan.Container)
	}
}

func TestStatusUnknownSessionRefuses(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, errors.New("tmux exploded"))

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeStatus(t, stdout)
	if env.Session.State != "unknown" {
		t.Errorf("session.state = %q, want unknown", env.Session.State)
	}
	if env.Session.Identity != nil {
		t.Errorf("session.identity = %v; an unobserved session has no verdict", env.Session.Identity)
	}
	if env.Plan.Session != "refuse" || env.Plan.Refusal == "" {
		t.Errorf("plan = %+v, want a refusal", env.Plan)
	}
	if env.Registered {
		t.Error("registered = true for an empty store")
	}
}

func TestStatusStoredBindingNeverRendersAsLive(t *testing.T) {
	ws := statusWorkspace(t)
	s := fake.NewStore()
	if err := s.RegisterWorkspace(ws, "sha256:seed", cliTestTime); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.RecordContainerObservation(ws.ID, state.ContainerObservation{
		Kind: "devcontainer", ContainerID: "c1", Health: state.HealthPresent,
	}, cliTestTime); err != nil {
		t.Fatalf("bind: %v", err)
	}
	if err := s.RecordContainerObservation(ws.ID, state.ContainerObservation{
		Health: state.HealthMissing,
	}, cliTestTime); err != nil {
		t.Fatalf("mark missing: %v", err)
	}
	installFakeStore(t, s)
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStatus(t, stdout)
	if env.Container == nil || env.Container.Stored == nil {
		t.Fatalf("container = %+v, want a stored binding", env.Container)
	}
	if env.Container.Stored.Health != "missing" || env.Container.Stored.ContainerID != "c1" {
		t.Errorf("container.stored = %+v, want retained missing c1", env.Container.Stored)
	}
	if env.Container.Observation.Attempted {
		t.Error("container.observation.attempted = true; no probe exists in this build")
	}

	code, human, _ := run(t, "status")
	if code != 0 {
		t.Fatalf("human exit %d", code)
	}
	if !strings.Contains(human, "missing") || !strings.Contains(human, "not probed") {
		t.Errorf("human output hides the missing/unprobed truth: %s", human)
	}
}

func TestStatusForeignOccupantRefuses(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{
		ByName: []controller.LiveSession{{Name: ws.SessionName}},
	}, nil)

	code, stdout, _ := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	env := decodeStatus(t, stdout)
	if env.Plan.Session != "refuse" {
		t.Errorf("plan.session = %q, want refuse", env.Plan.Session)
	}
	if !strings.Contains(env.Plan.Refusal, ws.SessionName) {
		t.Errorf("refusal %q does not name the occupant", env.Plan.Refusal)
	}
}

func TestStatusUnknownWorkspaceExitCode(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, _ := run(t, "status", "no-such-workspace")
	if code != ExitUnknownWorkspace {
		t.Errorf("exit %d, want %d", code, ExitUnknownWorkspace)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
}

func TestStatusRejectsExtraArguments(t *testing.T) {
	code, _, _ := run(t, "status", "one", "two")
	if code != ExitUsage {
		t.Errorf("exit %d, want %d", code, ExitUsage)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/cli/ -count=1`
Expected: FAIL to build — `statusEnvelope`, `runStatus` undefined.

- [ ] **Step 3: Write the implementation**

Create `internal/cli/status.go`:

```go
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
)

const statusHelp = `usage: projectmux status [--json] [--compact] [<workspace>]

Observe one workspace and explain configuration drift and dependency
failures, resolved either from <workspace> or from the current directory.

  --json     emit the versioned JSON envelope instead of human-readable text
  --compact  emit the JSON on a single line (implies --json)
`

// unprobedReason is the fixed observation outcome while no container
// adapter exists (spec §3).
const unprobedReason = "probe-not-implemented"

// statusEnvelope is the versioned JSON structure for projectmux status.
type statusEnvelope struct {
	SchemaVersion int            `json:"schema_version"`
	Workspace     workspaceInfo  `json:"workspace"`
	Registered    bool           `json:"registered"`
	Stored        *storedInfo    `json:"stored,omitempty"`
	Session       sessionInfo    `json:"session"`
	Container     *containerInfo `json:"container,omitempty"`
	Config        configInfo     `json:"config"`
	LastOperation *operationInfo `json:"last_operation,omitempty"`
	Plan          planInfo       `json:"plan"`
}

type storedInfo struct {
	ProposedSession string  `json:"proposed_session"`
	ActualSession   *string `json:"actual_session,omitempty"`
	RegisteredAt    string  `json:"registered_at"`
	UpdatedAt       string  `json:"updated_at"`
}

// sessionInfo reports tri-state session knowledge. Name and Identity
// ("match" or "conflict") are present only when State is "live": an
// unobserved session never carries a verdict (spec §3).
type sessionInfo struct {
	State    string  `json:"state"`
	Name     *string `json:"name,omitempty"`
	Identity *string `json:"identity,omitempty"`
}

// containerInfo separates the stored binding (last-observed state) from
// the current observation's outcome, so a stored "present" can never
// hide that the live observation failed or was unsupported (spec §3).
type containerInfo struct {
	Stored      *storedContainerInfo     `json:"stored,omitempty"`
	Observation containerObservationInfo `json:"observation"`
}

type containerObservationInfo struct {
	Attempted bool   `json:"attempted"`
	Reason    string `json:"reason,omitempty"`
}

type configInfo struct {
	DesiredDigest string  `json:"desired_digest"`
	AppliedDigest *string `json:"applied_digest,omitempty"`
	Drifted       bool    `json:"drifted"`
}

type operationInfo struct {
	Operation    string `json:"operation"`
	Outcome      string `json:"outcome"`
	ExitStatus   *int   `json:"exit_status,omitempty"`
	ErrorSummary string `json:"error_summary,omitempty"`
	FinishedAt   string `json:"finished_at"`
}

type planInfo struct {
	Session    string `json:"session"`
	Container  string `json:"container"`
	Reapply    bool   `json:"reapply"`
	RecordName bool   `json:"record_name"`
	Refusal    string `json:"refusal,omitempty"`
}

func runStatus(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlagSet("status")
	asJSON := fs.Bool("json", false, "emit the versioned JSON envelope")
	compact := fs.Bool("compact", false, "emit the JSON on a single line")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, statusHelp)
			return nil
		}
		return usagef("status: %s", err)
	}
	if fs.NArg() > 1 {
		return usagef("status: expected at most one workspace, got %d", fs.NArg())
	}
	if *compact {
		*asJSON = true
	}

	env, err := buildStatus(ctx, fs.Arg(0))
	if err != nil {
		return err
	}
	if *asJSON {
		return writeJSON(stdout, env, *compact)
	}
	return writeStatusHuman(stdout, env)
}

// buildStatus runs the full observation pipeline: the config command's
// read-only resolution, then Observe and BuildPlan. Rendering never
// re-implements planning logic (spec §2).
func buildStatus(ctx context.Context, name string) (statusEnvelope, error) {
	root, err := config.Root()
	if err != nil {
		return statusEnvelope{}, err
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		return statusEnvelope{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return statusEnvelope{}, fmt.Errorf("determining the current directory: %w", err)
	}
	ws, err := resolve.Resolve(name, defaults.RepositoryRoots, cwd)
	if err != nil {
		return statusEnvelope{}, err
	}
	effective, err := config.Load(root, defaults, ws.Slug)
	if err != nil {
		return statusEnvelope{}, err
	}

	st, err := openStore()
	if err != nil {
		return statusEnvelope{}, err
	}
	defer st.Close()

	ctrl := controller.Controller{
		Store:      st,
		Sessions:   newSessionObserver(),
		Containers: unprobedObserver{},
		Clock:      systemClock{},
	}
	snap, err := ctrl.Observe(ctx, controller.Desired{
		Workspace: ws,
		Config:    effective.Config,
		Digest:    effective.Digest,
	})
	if err != nil {
		return statusEnvelope{}, err
	}
	return statusEnvelopeFrom(ws, effective, snap, controller.BuildPlan(snap)), nil
}

func statusEnvelopeFrom(ws resolve.Workspace, effective config.Effective, snap controller.Snapshot, plan controller.Plan) statusEnvelope {
	env := statusEnvelope{
		SchemaVersion: OutputSchemaVersion,
		Workspace: workspaceInfo{
			ID:          ws.ID,
			Slug:        ws.Slug,
			Worktree:    ws.Worktree,
			SessionName: ws.SessionName,
			IsPrimary:   ws.IsPrimary,
		},
		Session: sessionInfo{State: string(snap.Session.State)},
		Config:  configInfo{DesiredDigest: effective.Digest},
		Plan: planInfo{
			Session:    string(plan.Session),
			Container:  string(plan.Container),
			Reapply:    plan.Reapply,
			RecordName: plan.RecordName,
			Refusal:    plan.Refusal,
		},
	}

	if live := snap.Session.ByIdentity; live != nil && snap.Session.State == controller.SessionLive {
		name := live.Name
		env.Session.Name = &name
		verdict := "conflict"
		if live.WorkspaceID == ws.ID && live.Slug == ws.Slug && live.Worktree == ws.Worktree {
			verdict = "match"
		}
		env.Session.Identity = &verdict
	}

	var storedBinding *storedContainerInfo
	if rec := snap.Stored; rec != nil {
		env.Registered = true
		env.Stored = &storedInfo{
			ProposedSession: rec.ProposedSession,
			ActualSession:   rec.ActualSession,
			RegisteredAt:    stamp(rec.RegisteredAt),
			UpdatedAt:       stamp(rec.UpdatedAt),
		}
		if rec.AppliedDigest != nil {
			applied := *rec.AppliedDigest
			env.Config.AppliedDigest = &applied
		}
		if op := rec.LastOperation; op != nil {
			env.LastOperation = &operationInfo{
				Operation:    op.Name,
				Outcome:      string(op.Outcome),
				ExitStatus:   op.ExitStatus,
				ErrorSummary: op.ErrorSummary,
				FinishedAt:   stamp(op.FinishedAt),
			}
		}
		storedBinding = storedContainer(rec.Container)
	}

	// Drift is string inequality on digests (design §7). plan.Reapply
	// cannot serve here: a refusing plan clears every mutating flag.
	env.Config.Drifted = env.Config.AppliedDigest == nil ||
		*env.Config.AppliedDigest != effective.Digest

	if storedBinding != nil || snap.Container.Observed != nil {
		env.Container = &containerInfo{
			Stored: storedBinding,
			Observation: containerObservationInfo{
				Attempted: false,
				Reason:    unprobedReason,
			},
		}
	}
	return env
}

// writeStatusHuman renders a readable report. This layout is explicitly
// not a compatibility contract; automation should use --json.
func writeStatusHuman(w io.Writer, env statusEnvelope) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)

	fmt.Fprintf(tw, "workspace\t%s\n", env.Workspace.Slug)
	fmt.Fprintf(tw, "worktree\t%s\n", env.Workspace.Worktree)
	fmt.Fprintf(tw, "id\t%s\n", env.Workspace.ID)

	if env.Registered {
		recorded := env.Stored.ProposedSession + " (proposed, unassigned)"
		if env.Stored.ActualSession != nil {
			recorded = *env.Stored.ActualSession
		}
		fmt.Fprintf(tw, "recorded session\t%s\n", recorded)
	} else {
		fmt.Fprint(tw, "recorded session\tnot registered\n")
	}

	sessionLine := env.Session.State
	if env.Session.Name != nil {
		sessionLine = fmt.Sprintf("%s (%s, identity %s)",
			env.Session.State, *env.Session.Name, *env.Session.Identity)
	}
	fmt.Fprintf(tw, "tmux session\t%s\n", sessionLine)

	if env.Container == nil {
		fmt.Fprint(tw, "container\tnone\n")
	} else {
		stored := "no binding recorded"
		if c := env.Container.Stored; c != nil {
			stored = fmt.Sprintf("%s %s (as of %s)", c.Health, c.ContainerID, c.ObservedAt)
		}
		fmt.Fprintf(tw, "container\t%s; not probed: container probing is not implemented in this build\n", stored)
	}

	drift := "in sync"
	if env.Config.Drifted {
		drift = "drifted"
	}
	applied := "never applied"
	if env.Config.AppliedDigest != nil {
		applied = *env.Config.AppliedDigest
	}
	fmt.Fprintf(tw, "config\t%s (desired %s, applied %s)\n",
		drift, env.Config.DesiredDigest, applied)

	if op := env.LastOperation; op != nil {
		line := fmt.Sprintf("%s %s at %s", op.Operation, op.Outcome, op.FinishedAt)
		if op.ExitStatus != nil {
			line += fmt.Sprintf(" (exit %d)", *op.ExitStatus)
		}
		if op.ErrorSummary != "" {
			line += ": " + op.ErrorSummary
		}
		fmt.Fprintf(tw, "last operation\t%s\n", line)
	}

	fmt.Fprintf(tw, "plan\tsession=%s container=%s reapply=%t record-name=%t\n",
		env.Plan.Session, env.Plan.Container, env.Plan.Reapply, env.Plan.RecordName)
	if env.Plan.Refusal != "" {
		fmt.Fprintf(tw, "refusal\t%s\n", env.Plan.Refusal)
	}

	if err := tw.Flush(); err != nil {
		return fmt.Errorf("writing output: %w", err)
	}
	return nil
}
```

In `internal/cli/cli.go`:

1. Add the dispatch case directly after the `list` case:

```go
	case "status":
		return runStatus(ctx, rest, stdout)
```

2. In the `usage` constant, insert this entry after the `list` entry:

```text
  status [--json] [--compact] [<workspace>]
        observe one workspace and explain drift and dependency failures
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/cli/ -count=1 -race`
Expected: PASS.

- [ ] **Step 5: Gates and commit**

Run: `gofmt -l .` (expect empty), `go vet ./...`, `go test ./... -count=1 -race`, `CGO_ENABLED=0 go build ./cmd/projectmux`

```bash
git add internal/cli/status.go internal/cli/status_test.go internal/cli/cli.go
git commit -m "Add projectmux status rendering observation and plan output"
```

---

### Task 8: Final verification sweep

**Files:**
- Modify: none expected; fix anything the sweep finds.

**Interfaces:**
- Consumes: everything above.
- Produces: a verified branch ready for review.

- [ ] **Step 1: Run the full gates**

Run each and confirm:

- `gofmt -l .` → empty output
- `go vet ./...` → no findings
- `go test ./... -count=1 -race` → all packages PASS (including the real-tmux integration tests)
- `CGO_ENABLED=0 go build ./cmd/projectmux` → builds

- [ ] **Step 2: Manual smoke test, fully isolated and exit-code-sensitive**

Everything is isolated — state, config, and the tmux server (via
`TMUX_TMPDIR`) — so the smoke test never observes or disturbs the user's
real sessions, and every exit code is asserted explicitly:

```bash
SMOKE=$(mktemp -d)
export PROJECTMUX_STATE_ROOT="$SMOKE/state"
export PROJECTMUX_CONFIG_ROOT="$SMOKE/config"
export TMUX_TMPDIR="$SMOKE/tmux"
mkdir -p "$PROJECTMUX_CONFIG_ROOT" "$TMUX_TMPDIR"
printf 'version: 1\n' > "$PROJECTMUX_CONFIG_ROOT/defaults.yaml"
go build -o "$SMOKE/projectmux" ./cmd/projectmux

"$SMOKE/projectmux" list;              echo "list exit: $?"          # want 0
"$SMOKE/projectmux" list --json;       echo "list json exit: $?"     # want 0
"$SMOKE/projectmux" status;            echo "status exit: $?"        # want 0
"$SMOKE/projectmux" status --json;     echo "status json exit: $?"   # want 0
"$SMOKE/projectmux" status no-such-ws; echo "unknown exit: $?"       # want 4

tmux -f /dev/null new-session -d -s smoke-live
tmux set-option -t smoke-live @dev_workspace_id 0000
tmux set-option -t smoke-live @dev_slug smokeslug
"$SMOKE/projectmux" list;              echo "list live exit: $?"     # want 0
tmux kill-server

"$SMOKE/projectmux" list --json > "$SMOKE/a.json"
"$SMOKE/projectmux" list --json > "$SMOKE/b.json"
diff "$SMOKE/a.json" "$SMOKE/b.json"; echo "idempotent: $?"          # want 0
```

Expected: every `want` matches; the empty database yields "no
workspaces" initially; with the isolated server up, `list` shows the
`smoke-live` session as `unrecorded`; after `kill-server`, absence again
(the `TMUX_TMPDIR` server is the one the binary observes, since it
inherits the variable); the double `list --json` diff proves observation
performs no store writes.

- [ ] **Step 3: Spec cross-check**

Re-read `docs/superpowers/specs/2026-08-05-observation-commands-design.md` §2–§7 and confirm each observable behavior has a test or was exercised in the smoke test. Confirm no operational-record mutation can occur: `grep -rn "RegisterWorkspace\|AllocateSessionName\|RecordContainerObservation\|RecordOperation\|CommitReconciliation" internal/cli/` must show only the `guardedStore` test doubles and seeding inside `_test.go` files.

- [ ] **Step 4: Commit any fixes**

```bash
git status
```

If clean, done. Otherwise fix, re-run the gates, and commit with a message describing the fix.

---

## Self-review notes

- Spec §2 `list` behavior → Task 6; §2 `status` behavior → Task 7; §2 contract (exit 0, no new codes, no mutations) → Tasks 6–7 tests plus the Task 5 `guardedStore`; §3 envelopes → Tasks 6–7 types; §4 runner → Task 1; §5 tmux adapter/decoder/timeout → Tasks 2–4; §6 wiring/seams/unprobed observer → Task 5; §7 failure behavior → Tasks 6–7 tests (tmux failure, unknown workspace, empty store) and existing exit-code mapping; §8 testing → distributed as listed; §9 exclusions → nothing here implements them.
- The `status` human-output rule "missing never reads as live" is asserted in `TestStatusStoredBindingNeverRendersAsLive`; the `list` equivalent in `TestListHumanNeverRendersRetainedBindingAsLive`.
- `controller`, `state`, and `fake` APIs are consumed unchanged, as the spec requires.
