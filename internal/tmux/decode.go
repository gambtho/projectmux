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
