package target

import (
	"errors"
	"strings"
	"testing"
)

func TestParseAcceptsValidTargets(t *testing.T) {
	long := strings.Repeat("s", MaxSessionLength)

	cases := []struct {
		arg  string
		want Ref
	}{
		{"", Ref{}},
		{"myrepo", Ref{Present: true, Name: "myrepo"}},
		{"myrepo/feature-a", Ref{Present: true, Name: "myrepo", Session: "feature-a", HasSession: true}},
		{"myrepo/A1", Ref{Present: true, Name: "myrepo", Session: "A1", HasSession: true}},
		{"myrepo/1", Ref{Present: true, Name: "myrepo", Session: "1", HasSession: true}},
		{"myrepo/a_b-c", Ref{Present: true, Name: "myrepo", Session: "a_b-c", HasSession: true}},
		{"myrepo/" + long, Ref{Present: true, Name: "myrepo", Session: long, HasSession: true}},
		// The repository component is not validated here; resolve.byName owns
		// that rule and reports its own error.
		{"euro_trip.old", Ref{Present: true, Name: "euro_trip.old"}},
	}

	for _, tc := range cases {
		got, err := Parse(tc.arg)
		if err != nil {
			t.Errorf("Parse(%q): unexpected error %v", tc.arg, err)
			continue
		}
		if got != tc.want {
			t.Errorf("Parse(%q) = %+v, want %+v", tc.arg, got, tc.want)
		}
	}
}

func TestParseRejectsMalformedTargets(t *testing.T) {
	cases := []struct {
		name string
		arg  string
	}{
		{"empty session", "repo/"},
		{"empty repository", "/session"},
		{"more than one separator", "a/b/c"},
		{"leading dash", "repo/-feature"},
		{"leading underscore", "repo/_feature"},
		{"session over the length limit", "repo/" + strings.Repeat("s", MaxSessionLength+1)},
		{"session with a dot", "repo/feature.a"},
		{"session with a space", "repo/feature a"},
		// The case the restrictive grammar exists for: a mistyped path must
		// report the grammar, not "unknown workspace".
		{"a path mistaken for a target", "docs/commands.md"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Parse(tc.arg)
			var malformed *MalformedError
			if !errors.As(err, &malformed) {
				t.Fatalf("Parse(%q) = %+v, %v; want *MalformedError", tc.arg, got, err)
			}
			if got != (Ref{}) {
				t.Errorf("Parse(%q) returned %+v alongside its error; want the zero Ref", tc.arg, got)
			}
			if malformed.Arg != tc.arg {
				t.Errorf("MalformedError.Arg = %q, want %q", malformed.Arg, tc.arg)
			}
			// Every message names the grammar, because the whole point of the
			// restrictive grammar is that the user is told what a target is.
			msg := err.Error()
			for _, want := range []string{tc.arg, "<repo>/<session>"} {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not mention %q", msg, want)
				}
			}
			if malformed.Reason == "" {
				t.Error("MalformedError carries no reason")
			}
		})
	}
}
