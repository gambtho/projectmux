package cli

import (
	"errors"
	"testing"

	"github.com/gambtho/projectmux/internal/config"
)

// A command whose report is its output still exits on what actually went
// wrong. The exit code is a property of the failure, not of which command
// noticed it, so classification looks through the reporting wrapper.
func TestReportedErrorExitsOnTheWrappedCause(t *testing.T) {
	err := &reportedError{
		msg: "configuration is invalid; details are in the report above",
		err: &config.InvalidConfigError{Problems: []config.Problem{{Message: "bad"}}},
	}

	if got := exitCode(err); got != ExitInvalidConfig {
		t.Errorf("exitCode = %d, want %d", got, ExitInvalidConfig)
	}
}

// A reporting wrapper with nothing underneath keeps the default failure
// code, which is what the existing stop and autostart reports rely on.
func TestReportedErrorWithNoCauseExitsOne(t *testing.T) {
	if got := exitCode(&reportedError{msg: "stop partially failed"}); got != ExitError {
		t.Errorf("exitCode = %d, want %d", got, ExitError)
	}
}

// The wrapper stays transparent to errors.As, so callers can inspect the
// cause rather than parsing the summary text.
func TestReportedErrorUnwrapsToItsCause(t *testing.T) {
	cause := &config.InvalidConfigError{Problems: []config.Problem{{Message: "bad"}}}
	wrapped := &reportedError{msg: "summary", err: cause}

	var found *config.InvalidConfigError
	if !errors.As(wrapped, &found) {
		t.Fatal("errors.As did not reach the wrapped cause")
	}
	if found != cause {
		t.Errorf("errors.As found %p, want %p", found, cause)
	}
	// The summary is what reaches stderr; it must not grow the cause's text.
	if wrapped.Error() != "summary" {
		t.Errorf("Error() = %q, want the one-line summary alone", wrapped.Error())
	}
}
