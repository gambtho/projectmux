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
