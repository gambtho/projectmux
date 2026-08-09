// Package target parses the CLI's <target> argument, which is <repo> or
// <repo>/<session>. It is the grammar layer only: it neither resolves a
// repository nor chooses a session, so it makes no git call and reads no
// state.
//
// "/" is the separator because it cannot appear in a git repository directory
// name, so no bare workspace name that worked before becomes ambiguous.
package target

import (
	"fmt"
	"regexp"
	"strings"
)

// MaxSessionLength bounds the session component. Together with
// sessionPattern this is deliberately stricter than tmux's own session-name
// rules. The reason is the bare-workspace shorthand in the CLI: an
// unrecognized first argument is treated as a workspace name and opened. A
// mistyped path such as "docs/commands.md" must therefore fail as a malformed
// target naming the grammar (exit 2), not as an unknown workspace (exit 4).
const MaxSessionLength = 64

var sessionPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Ref is a parsed target argument.
type Ref struct {
	Present    bool   // an argument was given at all
	Name       string // repository component; "" when !Present
	Session    string // session component; "" for the default session
	HasSession bool   // the argument carried a "/<session>"
}

// MalformedError reports an argument that is not a valid target. The CLI maps
// it to exit 2.
type MalformedError struct {
	Arg    string
	Reason string
}

func (e *MalformedError) Error() string {
	return fmt.Sprintf(
		"malformed target %q: %s; a target is <repo> or <repo>/<session>, where "+
			"<session> begins with a letter or a digit, continues with letters, "+
			"digits, \"-\" or \"_\", and is at most %d characters",
		e.Arg, e.Reason, MaxSessionLength)
}

// Parse splits a target argument into its repository and session components.
// An empty argument is the absent target and is not an error: Ref.Present
// distinguishes "no target" from "a target naming the default session", and
// the two select the session differently.
//
// The repository component is checked only for emptiness. resolve.byName
// already rejects path separators, "." and "..", and glob metacharacters, and
// reports an UnknownWorkspaceError naming the searched roots; duplicating that
// rule here would create two sources of truth that could disagree.
func Parse(arg string) (Ref, error) {
	if arg == "" {
		return Ref{}, nil
	}

	name, session, hasSeparator := strings.Cut(arg, "/")
	if !hasSeparator {
		return Ref{Present: true, Name: arg}, nil
	}

	switch {
	case strings.Contains(session, "/"):
		return Ref{}, &MalformedError{Arg: arg, Reason: `it carries more than one "/" separator`}
	case name == "":
		return Ref{}, &MalformedError{Arg: arg, Reason: "the repository component is empty"}
	case session == "":
		return Ref{}, &MalformedError{Arg: arg, Reason: "the session component is empty"}
	case len(session) > MaxSessionLength:
		return Ref{}, &MalformedError{
			Arg:    arg,
			Reason: fmt.Sprintf("the session component is %d characters long", len(session)),
		}
	case !sessionPattern.MatchString(session):
		return Ref{}, &MalformedError{
			Arg:    arg,
			Reason: fmt.Sprintf("the session component %q is not a valid session name", session),
		}
	}

	return Ref{Present: true, Name: name, Session: session, HasSession: true}, nil
}
