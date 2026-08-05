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
