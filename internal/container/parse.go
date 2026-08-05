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
