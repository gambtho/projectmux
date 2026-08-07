package cli

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/tmux"
)

func decodeAttach(t *testing.T, stdout string) attachEnvelope {
	t.Helper()
	var env attachEnvelope
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("decoding attach JSON: %v\n%s", err, stdout)
	}
	return env
}

func TestAttachLiveSessionJSON(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{
		ByIdentity: &live, ByName: []controller.LiveSession{live},
	}, nil)

	code, stdout, stderr := run(t, "attach", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	env := decodeAttach(t, stdout)
	if env.SchemaVersion != OutputSchemaVersion {
		t.Errorf("schema_version = %d", env.SchemaVersion)
	}
	if env.Session.State != "live" || env.Session.Name == nil ||
		*env.Session.Name != ws.SessionName {
		t.Errorf("session = %+v", env.Session)
	}
	if env.Session.Identity == nil || *env.Session.Identity != "match" {
		t.Errorf("identity = %v, want match", env.Session.Identity)
	}
}

func TestAttachPerformsTerminalAttachment(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{ByIdentity: &live}, nil)
	execs, switches := installAttachSpies(t)

	code, _, stderr := run(t, "attach")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	if len(*execs) != 1 || (*execs)[0] != ws.SessionName || len(*switches) != 0 {
		t.Errorf("exec = %v, switch = %v", *execs, *switches)
	}
}

// TestAttachAcrossServersRefusesBeforeAnnouncing pins both halves: the
// exit code, and that stdout stays empty. A successful attach execs
// away and can never print afterwards, so "attaching to X" has to be
// written first — which makes it a lie unless the refusal precedes it.
func TestAttachAcrossServersRefusesBeforeAnnouncing(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{ByIdentity: &live}, nil)
	execs, switches := installAttachSpies(t)
	currentSocket = func() string { return tmux.DefaultSocket }
	t.Setenv(tmux.SocketEnv, "pmxvalidate")

	code, stdout, stderr := run(t, "attach")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d; stderr: %s", code, ExitRefused, stderr)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on a refusal", stdout)
	}
	if len(*execs) != 0 || len(*switches) != 0 {
		t.Errorf("refusal ran tmux anyway: exec = %v, switch = %v", *execs, *switches)
	}
}

func TestAttachAbsentSessionFailsWithHint(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "attach")
	if code != ExitError {
		t.Fatalf("exit %d, want %d", code, ExitError)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty on failure", stdout)
	}
	if !strings.Contains(stderr, "projectmux open") {
		t.Errorf("stderr %q lacks the open hint", stderr)
	}
}

func TestAttachUnknownSessionStateRefuses(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, errors.New("tmux exploded"))

	code, _, _ := run(t, "attach")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
}

func TestAttachContradictoryIdentityRefuses(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: "/somewhere/else",
	}
	installSessionObserver(t, controller.SessionObservation{ByIdentity: &live}, nil)

	code, _, _ := run(t, "attach")
	if code != ExitRefused {
		t.Fatalf("exit %d, want %d", code, ExitRefused)
	}
}

func TestAttachNeverMutates(t *testing.T) {
	// installFakeStore wraps the store in guardedStore, which fails the
	// test on any mutating call — running attach at all is the
	// assertion (design §8/§12).
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := controller.LiveSession{
		Name: ws.SessionName, WorkspaceID: ws.ID, Slug: ws.Slug, Worktree: ws.Worktree,
	}
	installSessionObserver(t, controller.SessionObservation{ByIdentity: &live}, nil)
	installAttachSpies(t)

	if code, _, stderr := run(t, "attach"); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
}
