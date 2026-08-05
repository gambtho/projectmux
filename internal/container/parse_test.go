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
