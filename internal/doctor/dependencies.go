package doctor

import (
	"context"
	"fmt"
	"strings"
)

// dockerSubject is the docker client item's subject, shared between the
// dependencies check and the short-circuit stale-bindings consults.
const dockerSubject = "docker"

// dependency is one probed tool: the argv to run and the status a
// confirmed absence carries.
type dependency struct {
	subject string
	argv    []string
	// absent is the status when the binary is not on PATH. Only tmux
	// is fatal; the rest degrade a feature rather than the tool.
	absent Status
}

var dependencyProbes = []dependency{
	{subject: "tmux", argv: []string{"tmux", "-V"}, absent: StatusFail},
	{subject: "git", argv: []string{"git", "--version"}, absent: StatusWarn},
	{subject: dockerSubject, argv: []string{"docker", "--version"}, absent: StatusWarn},
	{subject: "devcontainer", argv: []string{"devcontainer", "--version"}, absent: StatusWarn},
}

// dockerDaemonSubject names the reachability item, which is not a
// binary and so is probed separately.
const dockerDaemonSubject = "docker daemon"

// dependencies probes each tool and also reports whether the docker
// client was confirmed absent — stale-bindings needs that one fact, and
// only a confirmed absence counts: an unfinished probe leaves the
// question open.
func (r *Runner) dependencies(ctx context.Context) (Check, bool) {
	check := Check{Name: "dependencies"}
	if r.Versions == nil {
		return verdict(check.Name, StatusUnknown, "no version probe is configured"), false
	}

	dockerAbsent := false
	for _, dep := range dependencyProbes {
		item, found := r.probeVersion(ctx, dep)
		check.Items = append(check.Items, item)
		if dep.subject != dockerSubject {
			continue
		}
		dockerAbsent = !found
		// The daemon is only meaningfully probed behind a client that
		// exists; without one the reachability question has no answer
		// to give, rather than a negative one.
		if found {
			check.Items = append(check.Items, r.probeDockerDaemon(ctx))
		}
	}
	return check.aggregate(), dockerAbsent
}

// probeVersion reports the item and whether the binary was found. A
// probe that could not run reports found: absence was not established.
func (r *Runner) probeVersion(ctx context.Context, dep dependency) (Item, bool) {
	item := Item{Subject: dep.subject}
	res, err := r.Versions.Probe(ctx, dep.argv...)
	switch {
	case err != nil:
		// A probe that could not be run says nothing about whether the
		// tool is installed.
		item.Status = StatusUnknown
		item.Detail = err.Error()
	case !res.Found:
		item.Status = dep.absent
		item.Detail = "not installed"
		return item, false
	case res.ExitCode != 0:
		item.Status = StatusUnknown
		item.Detail = fmt.Sprintf("%s exited %d: %s",
			strings.Join(dep.argv, " "), res.ExitCode, summarize(res.Stderr))
	case res.Stdout == "":
		item.Status = StatusUnknown
		item.Detail = strings.Join(dep.argv, " ") + " printed nothing"
	default:
		item.Status = StatusOK
		item.Detail = firstLine(res.Stdout)
	}
	return item, true
}

// probeDockerDaemon asks the client whether the daemon answers.
// Reachable is exit 0 AND non-empty stdout: with the daemon down the
// server-version template prints an empty line and the reason lands on
// stderr, so exit status alone would misread as success on some
// versions.
func (r *Runner) probeDockerDaemon(ctx context.Context) Item {
	item := Item{Subject: dockerDaemonSubject}
	res, err := r.Versions.Probe(ctx, "docker", "version", "--format", "{{.Server.Version}}")
	switch {
	case err != nil:
		item.Status = StatusUnknown
		item.Detail = err.Error()
	case res.ExitCode == 0 && res.Stdout != "":
		item.Status = StatusOK
		item.Detail = firstLine(res.Stdout)
	default:
		// The daemon may simply be stopped, which is a legitimate state
		// for a host-only installation — never a finding.
		item.Status = StatusUnknown
		item.Detail = "not reachable: " + summarize(res.Stderr)
	}
	return item
}

// summarize reduces captured stderr to one line for a report.
func summarize(stderr string) string {
	if line := firstLine(stderr); line != "" {
		return line
	}
	return "no output"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
