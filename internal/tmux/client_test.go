package tmux

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
)

// fakeTmux installs a shell script as the tmux binary for one test.
func fakeTmux(t *testing.T, script string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tmux")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	orig := tmuxBinary
	t.Cleanup(func() { tmuxBinary = orig })
	tmuxBinary = path
}

// oneSessionScript answers the two observation phases: enumeration
// returns one session id, and each per-field display-message call
// returns a canned value — the worktree deliberately spans lines to
// prove raw values round-trip through the client untouched.
const oneSessionScript = `#!/bin/sh
while [ "$1" = "-L" ]; do shift 2; done
cmd="$1"
shift
case "$cmd" in
list-sessions)
	printf '$0\n'
	;;
display-message)
	# args: -p -t <target> <format>
	case "$4" in
	'#{session_name}') printf 'alpha\n' ;;
	'#{@dev_workspace_id}') printf 'w1\n' ;;
	'#{@dev_slug}') printf 'proj\n' ;;
	'#{@dev_worktree}') printf '/w/evil\npath\n' ;;
	*) exit 2 ;;
	esac
	;;
*)
	exit 2
	;;
esac
`

func TestSessionsObservesRawValues(t *testing.T) {
	fakeTmux(t, oneSessionScript)
	live, err := (&Client{}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	want := controller.LiveSession{
		Name: "alpha", WorkspaceID: "w1", Slug: "proj", Worktree: "/w/evil\npath",
	}
	if len(live) != 1 || live[0] != want {
		t.Errorf("Sessions = %+v, want [%+v]", live, want)
	}
}

func TestSessionsRejectsEmptySessionName(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
while [ "$1" = "-L" ]; do shift 2; done
case "$1" in
list-sessions) printf '$0\n' ;;
display-message) printf '\n' ;;
esac
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions accepted an empty session name (vanished session)")
	}
}

func TestSessionsRejectsMalformedIDs(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
printf 'not-an-id\n'
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions accepted a malformed session id")
	}
}

func TestSessionsNoServerIsEmptyNotError(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
echo 'no server running on /tmp/tmux-1000/default' 1>&2
exit 1
`)
	live, err := (&Client{}).Sessions(context.Background())
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("Sessions = %+v, want none", live)
	}
}

func TestSessionsOtherFailureIsAnError(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
echo 'lost server' 1>&2
exit 1
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions converted an unrecognized tmux failure into absence")
	}
}

func TestSessionsPermissionFailureIsAnErrorNotAbsence(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
echo 'error connecting to /tmp/tmux-1000/default (Operation not permitted)' 1>&2
exit 1
`)
	if _, err := (&Client{}).Sessions(context.Background()); err == nil {
		t.Fatal("Sessions read a permission failure as an absent server")
	}
}

func TestSessionsTimeoutPropagates(t *testing.T) {
	fakeTmux(t, `#!/bin/sh
sleep 10
`)
	start := time.Now()
	_, err := (&Client{Timeout: 100 * time.Millisecond}).Sessions(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("Sessions took %v; the subprocess outlived the timeout", elapsed)
	}
}

func TestObserveSessionFiltersSessions(t *testing.T) {
	fakeTmux(t, oneSessionScript)
	obs, err := (&Client{}).ObserveSession(context.Background(), controller.SessionQuery{
		WorkspaceID:    "w1",
		CandidateNames: []string{"alpha"},
	})
	if err != nil {
		t.Fatalf("ObserveSession: %v", err)
	}
	if obs.ByIdentity == nil || obs.ByIdentity.Name != "alpha" {
		t.Errorf("ByIdentity = %+v, want alpha", obs.ByIdentity)
	}
	if len(obs.ByName) != 1 {
		t.Errorf("ByName = %+v, want alpha only", obs.ByName)
	}
}

func TestClientIsASessionObserver(t *testing.T) {
	var _ controller.SessionObserver = (*Client)(nil)
}
