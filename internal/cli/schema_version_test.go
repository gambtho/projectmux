package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
)

// assertSchemaV2 checks the two properties every migrated envelope shares:
// schema_version and the absence of is_primary. It says nothing about the
// session field on purpose — session decodes to a different shape, and in
// several carriers to a decoy key entirely, so it needs its own per-carrier
// assertion. See assertWorkspaceHasSession and assertEnvelopeHasSessionField
// below, and their call sites in each test.
//
// The version is compared against the literal 2, not against
// OutputSchemaVersion: every existing envelope test asserts equality with
// the constant, which holds whatever the constant says and therefore
// cannot catch a bump that never happened.
func assertSchemaV2(t *testing.T, stdout string) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &top); err != nil {
		t.Fatalf("decoding the envelope: %v\n%s", err, stdout)
	}
	raw, ok := top["schema_version"]
	if !ok {
		t.Fatalf("envelope carries no schema_version:\n%s", stdout)
	}
	var version int
	if err := json.Unmarshal(raw, &version); err != nil {
		t.Fatalf("decoding schema_version: %v", err)
	}
	if version != 2 {
		t.Errorf("schema_version = %d, want 2", version)
	}
	if strings.Contains(stdout, "is_primary") {
		t.Errorf("envelope still carries is_primary:\n%s", stdout)
	}
}

// assertWorkspaceHasSession decodes the top-level "workspace" object and
// asserts it carries the key "session". Use it for the five commands whose
// envelope embeds workspaceInfo: config, open, attach, stop, status.
//
// Those five commands each ALSO render their own, unrelated top-level
// "session" key — openEnvelope.Session (open.go:39), attachEnvelope.Session
// (attach.go:30), stopEnvelope.Session (stop.go:36), and
// statusEnvelope.Session (status.go:32) all exist independently of
// workspace.session. A substring search over the whole document is
// satisfied by one of those decoys even if workspaceInfo.Session is
// removed entirely, which is exactly the false pass the review caught.
// Decoding to the "workspace" object specifically and checking its own key
// set rules that out: only workspaceInfo's own session field can satisfy
// this assertion.
func assertWorkspaceHasSession(t *testing.T, stdout string) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &top); err != nil {
		t.Fatalf("decoding the envelope: %v\n%s", err, stdout)
	}
	wsRaw, ok := top["workspace"]
	if !ok {
		t.Fatalf("envelope has no workspace object:\n%s", stdout)
	}
	var ws map[string]json.RawMessage
	if err := json.Unmarshal(wsRaw, &ws); err != nil {
		t.Fatalf("decoding workspace: %v\n%s", err, stdout)
	}
	if _, ok := ws["session"]; !ok {
		t.Errorf("workspace object has no session key:\n%s", stdout)
	}
}

// assertEnvelopeHasSessionField checks, by quote-and-colon-delimited
// substring, that the rendered document contains the given key. It is
// safe for list and rebuild only: neither embeds workspaceInfo, and in
// both, listRow.Session (list.go:39) / rebuildRegistered.SessionName
// (rebuild.go:60) is the only field whose key matches exactly —
// "proposed_session", "actual_session", "session_state", and
// "live_session" all fail the delimiter, since none of them ends its key
// in a closing quote immediately followed by a colon the way "session"
// itself does.
//
// The two envelopes name different things and so use different keys: list
// carries the session component, rebuild the tmux session name.
func assertEnvelopeHasSessionField(t *testing.T, stdout, key string) {
	t.Helper()
	if !strings.Contains(stdout, `"`+key+`":`) {
		t.Errorf("envelope has no %s field:\n%s", key, stdout)
	}
}

// Every carrier is asserted separately rather than through one shared
// fixture: workspaceInfo covers five of the seven commands, so a single
// assertion would pass for list and rebuild by inheritance and a missed
// carrier would ship silently.
func TestConfigEnvelopeIsSchemaV2(t *testing.T) {
	workspace(t, map[string]string{"defaults.yaml": validConfig})

	code, stdout, stderr := run(t, "config", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	assertWorkspaceHasSession(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("config envelope has no repo_root:\n%s", stdout)
	}
}

func TestOpenEnvelopeIsSchemaV2(t *testing.T) {
	ws := openWorkspace(t)
	installOpenStore(t, fake.NewStore())
	installFakeActuator(t)
	installScriptedSessions(t,
		cliAbsent(), cliAbsent(), cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "open", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	assertWorkspaceHasSession(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("open envelope has no repo_root:\n%s", stdout)
	}
}

func TestAttachEnvelopeIsSchemaV2(t *testing.T) {
	ws := statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	live := ownLive(ws, ws.SessionName)
	installSessionObserver(t, controller.SessionObservation{
		ByIdentity: &live, ByName: []controller.LiveSession{live},
	}, nil)

	code, stdout, stderr := run(t, "attach", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	assertWorkspaceHasSession(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("attach envelope has no repo_root:\n%s", stdout)
	}
}

func TestStopEnvelopeIsSchemaV2(t *testing.T) {
	ws := openWorkspaceIdentity(t)
	stopFixtureFor(t, ws)
	installScriptedSessions(t, cliLive(ownLive(ws, ws.SessionName)))

	code, stdout, stderr := run(t, "stop", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	assertWorkspaceHasSession(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("stop envelope has no repo_root:\n%s", stdout)
	}
}

func TestStatusEnvelopeIsSchemaV2(t *testing.T) {
	statusWorkspace(t)
	installFakeStore(t, fake.NewStore())
	installSessionObserver(t, controller.SessionObservation{}, nil)

	code, stdout, stderr := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	assertWorkspaceHasSession(t, stdout)
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("status envelope has no repo_root:\n%s", stdout)
	}
}

func TestListEnvelopeIsSchemaV2(t *testing.T) {
	installFakeStore(t, seededListStore(t))
	installLiveSessions(t, nil, nil)

	code, stdout, stderr := run(t, "list", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	assertEnvelopeHasSessionField(t, stdout, "session")
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("list rows have no repo_root:\n%s", stdout)
	}
}

func TestRebuildEnvelopeIsSchemaV2(t *testing.T) {
	ws := rebuildEnv(t, fake.NewStore(), nil)
	live := ownLive(ws, ws.SessionName)
	installLiveSessions(t, []controller.LiveSession{live}, nil)
	installScriptedSessions(t, cliLive(live))

	code, stdout, stderr := run(t, "rebuild", "--json")
	if code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, stderr)
	}
	assertSchemaV2(t, stdout)
	assertEnvelopeHasSessionField(t, stdout, "session_name")
	// The fixture seeds one live session, so the registered array is
	// guaranteed non-empty here; the repo_root check below therefore lands
	// on an actual rebuildRegistered entry rather than passing vacuously
	// over an empty array.
	if !strings.Contains(stdout, `"repo_root"`) {
		t.Errorf("rebuild registration has no repo_root:\n%s", stdout)
	}
}
