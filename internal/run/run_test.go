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
