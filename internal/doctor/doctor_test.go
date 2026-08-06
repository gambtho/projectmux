package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/config"
	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/controller/fake"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// scriptedVersions answers each probe from a table keyed by the joined
// argv, so a test states exactly what the environment looks like.
type scriptedVersions struct {
	results map[string]ProbeResult
	errs    map[string]error
	calls   []string
}

func (s *scriptedVersions) Probe(_ context.Context, argv ...string) (ProbeResult, error) {
	key := strings.Join(argv, " ")
	s.calls = append(s.calls, key)
	if err := s.errs[key]; err != nil {
		return ProbeResult{}, err
	}
	res, ok := s.results[key]
	if !ok {
		// Anything a test does not script is installed and healthy, so
		// each test states only the condition it is about.
		return ProbeResult{Stdout: "1.0", Found: true}, nil
	}
	return res, nil
}

type scriptedSessions struct {
	sessions []controller.LiveSession
	err      error
}

func (s *scriptedSessions) Sessions(context.Context) ([]controller.LiveSession, error) {
	return s.sessions, s.err
}

func findItem(t *testing.T, c Check, subject string) Item {
	t.Helper()
	for _, item := range c.Items {
		if item.Subject == subject {
			return item
		}
	}
	t.Fatalf("item %q missing from check %q (items: %+v)", subject, c.Name, c.Items)
	return Item{}
}

func TestWorstRanksUnknownAboveWarn(t *testing.T) {
	cases := []struct {
		in   []Status
		want Status
	}{
		{nil, StatusOK},
		{[]Status{StatusOK, StatusOK}, StatusOK},
		{[]Status{StatusOK, StatusWarn}, StatusWarn},
		{[]Status{StatusWarn, StatusUnknown}, StatusUnknown},
		{[]Status{StatusUnknown, StatusFail}, StatusFail},
		{[]Status{StatusFail, StatusWarn, StatusUnknown}, StatusFail},
	}
	for _, tc := range cases {
		if got := worst(tc.in...); got != tc.want {
			t.Errorf("worst(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDiagnoseAlwaysReportsEveryCheck is the envelope guarantee: one
// broken dependency never truncates the report.
func TestDiagnoseAlwaysReportsEveryCheck(t *testing.T) {
	r := &Runner{
		ConfigRoot: t.TempDir(),
		Versions:   &scriptedVersions{errs: map[string]error{"tmux -V": errors.New("boom")}},
		DB:         Database{Missing: true, Supported: state.SchemaVersion},
	}
	rep := r.Diagnose(context.Background())

	want := []string{"dependencies", "configuration", "database", "orphaned-sessions", "stale-bindings"}
	if len(rep.Checks) != len(want) {
		t.Fatalf("Diagnose returned %d checks, want %d", len(rep.Checks), len(want))
	}
	for i, name := range want {
		if rep.Checks[i].Name != name {
			t.Errorf("check %d = %q, want %q", i, rep.Checks[i].Name, name)
		}
		if rep.Checks[i].Status == "" {
			t.Errorf("check %q has no status", name)
		}
	}
}

func TestDependenciesMissingToolStatuses(t *testing.T) {
	absent := ProbeResult{}
	versions := &scriptedVersions{results: map[string]ProbeResult{
		"tmux -V":                absent,
		"git --version":          absent,
		"docker --version":       absent,
		"devcontainer --version": absent,
	}}
	r := &Runner{Versions: versions}

	check, dockerAbsent := r.dependencies(context.Background())
	if !dockerAbsent {
		t.Error("a missing docker client did not report as absent")
	}
	if check.Status != StatusFail {
		t.Errorf("check status = %q, want fail (tmux is a core dependency)", check.Status)
	}
	if got := findItem(t, check, "tmux").Status; got != StatusFail {
		t.Errorf("tmux = %q, want fail", got)
	}
	for _, subject := range []string{"git", "docker", "devcontainer"} {
		if got := findItem(t, check, subject).Status; got != StatusWarn {
			t.Errorf("%s = %q, want warn", subject, got)
		}
	}
	// The daemon is not probed behind a missing client.
	for _, call := range versions.calls {
		if strings.HasPrefix(call, "docker version") {
			t.Error("the daemon was probed without a docker client")
		}
	}
}

// TestDependenciesDaemonDownIsUnknown pins the verified shape: exit 1,
// empty stdout, the reason on stderr. Exit status alone would be too
// narrow a seam to tell this from success.
func TestDependenciesDaemonDownIsUnknown(t *testing.T) {
	r := &Runner{Versions: &scriptedVersions{results: map[string]ProbeResult{
		"docker version --format {{.Server.Version}}": {
			Stderr:   "failed to connect to the docker API",
			ExitCode: 1,
			Found:    true,
		},
	}}}

	check, dockerAbsent := r.dependencies(context.Background())
	if dockerAbsent {
		t.Error("an installed client reported as absent because the daemon was down")
	}
	item := findItem(t, check, dockerDaemonSubject)
	if item.Status != StatusUnknown {
		t.Errorf("docker daemon = %q, want unknown", item.Status)
	}
	if !strings.Contains(item.Detail, "failed to connect") {
		t.Errorf("detail = %q, want the stderr reason", item.Detail)
	}
}

func TestDependenciesHealthyEnvironment(t *testing.T) {
	r := &Runner{Versions: &scriptedVersions{results: map[string]ProbeResult{
		"tmux -V": {Stdout: "tmux 3.4", Found: true},
		"docker version --format {{.Server.Version}}": {Stdout: "29.7.1", Found: true},
	}}}

	check, _ := r.dependencies(context.Background())
	if check.Status != StatusOK {
		t.Fatalf("check status = %q (%+v), want ok", check.Status, check.Items)
	}
	if got := findItem(t, check, "tmux").Detail; got != "tmux 3.4" {
		t.Errorf("tmux detail = %q, want the raw version line", got)
	}
	if got := findItem(t, check, dockerDaemonSubject).Detail; got != "29.7.1" {
		t.Errorf("daemon detail = %q, want the server version", got)
	}
}

// TestDependenciesProbeErrorIsUnknown holds the line between "could not
// look" and "is not there".
func TestDependenciesProbeErrorIsUnknown(t *testing.T) {
	r := &Runner{Versions: &scriptedVersions{
		errs: map[string]error{"tmux -V": errors.New("signal: killed")},
	}}
	check, _ := r.dependencies(context.Background())
	if got := findItem(t, check, "tmux").Status; got != StatusUnknown {
		t.Errorf("tmux = %q, want unknown", got)
	}
}

// TestDependenciesClientProbeErrorSkipsTheDaemon holds the other half
// of the tri-state line: a docker client whose probe could not run is
// neither present nor absent, so the daemon behind it must not be
// probed and its absence must not be asserted downstream.
func TestDependenciesClientProbeErrorSkipsTheDaemon(t *testing.T) {
	versions := &scriptedVersions{
		errs: map[string]error{"docker --version": errors.New("signal: killed")},
	}
	r := &Runner{Versions: versions}

	check, dockerAbsent := r.dependencies(context.Background())
	if dockerAbsent {
		t.Error("a client whose probe failed was reported as confirmed absent")
	}
	if got := findItem(t, check, dockerSubject).Status; got != StatusUnknown {
		t.Errorf("docker = %q, want unknown", got)
	}
	for _, item := range check.Items {
		if item.Subject == dockerDaemonSubject {
			t.Errorf("the daemon was probed behind a client that may not exist: %+v", item)
		}
	}
	for _, call := range versions.calls {
		if strings.HasPrefix(call, "docker version") {
			t.Errorf("probe %q ran despite an unknown client", call)
		}
	}
}

// writeConfig lays out a configuration root and returns it with the
// loaded defaults layer.
func writeConfig(t *testing.T, files map[string]string) (string, config.Layer) {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}
	defaults, err := config.LoadDefaults(root)
	if err != nil {
		t.Fatalf("LoadDefaults: %v", err)
	}
	return root, defaults
}

func TestConfigurationAbsentDefaultsIsHealthy(t *testing.T) {
	root, defaults := writeConfig(t, nil)
	r := &Runner{ConfigRoot: root, Defaults: defaults}

	check := r.configuration()
	if check.Status != StatusOK {
		t.Fatalf("status = %q, want ok", check.Status)
	}
	if got := findItem(t, check, "defaults").Detail; got != "defaults.yaml absent" {
		t.Errorf("detail = %q, want the absent note", got)
	}
}

// TestConfigurationValidatesDefaultsStandalone covers the case that
// motivated ValidateDefaults: with no workspace files, nothing else
// would ever read defaults.yaml closely enough to notice.
func TestConfigurationValidatesDefaultsStandalone(t *testing.T) {
	root, defaults := writeConfig(t, map[string]string{
		"defaults.yaml": "version: 99\n",
	})
	r := &Runner{ConfigRoot: root, Defaults: defaults}

	check := r.configuration()
	item := findItem(t, check, "defaults")
	if item.Status != StatusWarn {
		t.Fatalf("defaults = %q, want warn (a workspace layer may override)", item.Status)
	}
	if !strings.Contains(item.Detail, "unsupported schema version 99") {
		t.Errorf("detail = %q, want the version problem", item.Detail)
	}
	if check.Status != StatusWarn {
		t.Errorf("check status = %q, want warn", check.Status)
	}
}

func TestConfigurationReportsEachWorkspace(t *testing.T) {
	root, defaults := writeConfig(t, map[string]string{
		"defaults.yaml":              "version: 1\n",
		"workspaces/good.yaml":       "windows:\n  - name: edit\n    shell: true\n",
		"workspaces/good.local.yaml": "environment:\n  TOKEN: x\n",
		"workspaces/bad.yaml":        "windows:\n  - name: edit\n",
	})
	r := &Runner{ConfigRoot: root, Defaults: defaults}

	check := r.configuration()
	if check.Status != StatusFail {
		t.Fatalf("status = %q, want fail", check.Status)
	}
	if got := findItem(t, check, "good").Status; got != StatusOK {
		t.Errorf("good = %q, want ok", got)
	}
	bad := findItem(t, check, "bad")
	if bad.Status != StatusFail {
		t.Errorf("bad = %q, want fail", bad.Status)
	}
	if !strings.Contains(bad.Detail, "exactly one of agent, command, or shell") {
		t.Errorf("detail = %q, want the window-mode problem", bad.Detail)
	}
	// The machine-local layer is merged under its slug, never a subject.
	for _, item := range check.Items {
		if strings.HasSuffix(item.Subject, ".local") {
			t.Errorf("local layer %q reported as its own item", item.Subject)
		}
	}
}

// TestConfigurationUnreadableWorkspacesIsUnknown covers a workspaces
// directory that cannot be listed. Reporting "no workspace files" there
// would be an affirmative answer over ground nothing examined.
func TestConfigurationUnreadableWorkspacesIsUnknown(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root, defaults := writeConfig(t, map[string]string{
		"workspaces/slab.yaml": "windows:\n  - name: edit\n    shell: true\n",
	})
	dir := filepath.Join(root, "workspaces")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	check := (&Runner{ConfigRoot: root, Defaults: defaults}).configuration()
	if check.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown: %+v", check.Status, check.Items)
	}
	if got := findItem(t, check, "workspaces").Status; got != StatusUnknown {
		t.Errorf("workspaces item = %q, want unknown", got)
	}
}

func TestConfigurationUnreadableDefaultsFailsTheCheck(t *testing.T) {
	r := &Runner{ConfigRoot: t.TempDir(), DefaultsErr: errors.New("defaults.yaml: bad indent")}
	check := r.configuration()
	if check.Status != StatusFail {
		t.Fatalf("status = %q, want fail", check.Status)
	}
	if len(check.Items) != 0 {
		t.Errorf("items = %+v, want none: nothing can be merged", check.Items)
	}
}

func TestDatabaseStatuses(t *testing.T) {
	cases := []struct {
		name string
		db   Database
		want Status
	}{
		{"missing", Database{Missing: true}, StatusOK},
		{"healthy", Database{Version: 1, Supported: 1}, StatusOK},
		{"unreadable", Database{OpenErr: errors.New("permission denied")}, StatusUnknown},
		{"corrupt", Database{IntegrityErr: errors.New("database disk image is malformed (11)")}, StatusFail},
		{"newer", Database{Version: 2, Supported: 1}, StatusFail},
		{"older", Database{Version: 0, Supported: 1}, StatusWarn},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &Runner{DB: tc.db}
			if got := r.database().Status; got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

// seedStore registers one workspace and returns the store.
func seedStore(t *testing.T, slug, worktree string) *fake.Store {
	t.Helper()
	store := fake.NewStore()
	register(t, store, slug, worktree)
	return store
}

func register(t *testing.T, store *fake.Store, slug, worktree string) {
	t.Helper()
	ws := resolve.Workspace{
		ID:          "id-" + slug,
		Slug:        slug,
		Worktree:    worktree,
		SessionName: slug,
		IsPrimary:   true,
	}
	if err := store.RegisterWorkspace(ws, "sha256:abc", time.Now()); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
}

func TestOrphanedSessionsReportsUnregisteredAndMissingWorktree(t *testing.T) {
	store := seedStore(t, "slab", t.TempDir())
	register(t, store, "gone", filepath.Join(t.TempDir(), "deleted"))

	r := &Runner{
		Store: store,
		DB:    Database{Version: 1, Supported: 1},
		Sessions: &scriptedSessions{sessions: []controller.LiveSession{
			{ID: "$1", Name: "slab", WorkspaceID: "id-slab"},
			{ID: "$2", Name: "gone", WorkspaceID: "id-gone"},
			{ID: "$3", Name: "stray", WorkspaceID: "id-unknown"},
			{ID: "$4", Name: "someone-elses"},
		}},
	}

	check := r.orphanedSessions(context.Background())
	if check.Status != StatusWarn {
		t.Fatalf("status = %q, want warn", check.Status)
	}
	if got := findItem(t, check, "slab").Status; got != StatusOK {
		t.Errorf("slab = %q, want ok", got)
	}
	if got := findItem(t, check, "gone"); got.Status != StatusWarn ||
		!strings.Contains(got.Detail, "worktree no longer exists") {
		t.Errorf("gone = %+v, want warn about the worktree", got)
	}
	if got := findItem(t, check, "stray"); got.Status != StatusWarn ||
		!strings.Contains(got.Detail, "not registered") {
		t.Errorf("stray = %+v, want warn about registration", got)
	}
	for _, item := range check.Items {
		if item.Subject == "someone-elses" {
			t.Error("a session without identity keys was diagnosed")
		}
	}
}

// TestOrphanedSessionsMissingDatabaseIsConfirmedEmpty: a fresh
// installation has registered nothing, which is a fact — a live
// identity session is genuinely unregistered.
func TestOrphanedSessionsMissingDatabaseIsConfirmedEmpty(t *testing.T) {
	r := &Runner{
		DB: Database{Missing: true, Supported: 1},
		Sessions: &scriptedSessions{sessions: []controller.LiveSession{
			{ID: "$1", Name: "slab", WorkspaceID: "id-slab"},
		}},
	}
	check := r.orphanedSessions(context.Background())
	if got := findItem(t, check, "slab").Status; got != StatusWarn {
		t.Fatalf("slab = %q, want warn", got)
	}
}

// TestOrphanedSessionsUnusableDatabaseIsUnknown: a corrupt database
// makes the registered set unknowable, so no session can be called an
// orphan.
func TestOrphanedSessionsUnusableDatabaseIsUnknown(t *testing.T) {
	r := &Runner{
		DB: Database{IntegrityErr: errors.New("malformed"), Supported: 1},
		Sessions: &scriptedSessions{sessions: []controller.LiveSession{
			{ID: "$1", Name: "slab", WorkspaceID: "id-slab"},
		}},
	}
	check := r.orphanedSessions(context.Background())
	if check.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown", check.Status)
	}
	if len(check.Items) != 0 {
		t.Errorf("items = %+v, want none", check.Items)
	}
}

func TestOrphanedSessionsNoServerIsHealthy(t *testing.T) {
	r := &Runner{DB: Database{Missing: true}, Sessions: &scriptedSessions{}}
	if got := r.orphanedSessions(context.Background()).Status; got != StatusOK {
		t.Fatalf("status = %q, want ok", got)
	}
}

// TestOrphanedSessionsForeignServerIsHealthy covers a live tmux server
// running only somebody else's sessions: every session is filtered out,
// so the check has no items to aggregate over and must still state a
// status rather than leaving it blank.
func TestOrphanedSessionsForeignServerIsHealthy(t *testing.T) {
	r := &Runner{
		Store: seedStore(t, "slab", t.TempDir()),
		Sessions: &scriptedSessions{sessions: []controller.LiveSession{
			{Name: "somebody-else"},
			{Name: "irc"},
		}},
	}

	check := r.orphanedSessions(context.Background())
	if check.Status != StatusOK {
		t.Fatalf("status = %q, want ok", check.Status)
	}
	if len(check.Items) != 0 {
		t.Errorf("items = %+v, want none", check.Items)
	}
	if check.Detail == "" {
		t.Error("detail is empty; the report must say why there is nothing to show")
	}
}

func TestOrphanedSessionsUnobservableTmuxIsUnknown(t *testing.T) {
	r := &Runner{Sessions: &scriptedSessions{err: errors.New("tmux: command not found")}}
	if got := r.orphanedSessions(context.Background()).Status; got != StatusUnknown {
		t.Fatalf("status = %q, want unknown", got)
	}
}

// bindStore registers a workspace carrying a container binding.
func bindStore(t *testing.T, slug string) *fake.Store {
	t.Helper()
	store := seedStore(t, slug, t.TempDir())
	if err := store.RecordContainerObservation("id-"+slug, state.ContainerObservation{
		Kind:        "devcontainer",
		ContainerID: "c-" + slug,
		Health:      state.HealthPresent,
	}, time.Now()); err != nil {
		t.Fatalf("RecordContainerObservation: %v", err)
	}
	return store
}

func TestStaleBindingsReportsConfirmedAbsence(t *testing.T) {
	r := &Runner{
		Store:      bindStore(t, "slab"),
		DB:         Database{Version: 1, Supported: 1},
		Containers: &fake.ContainerObserver{ProbeResult: controller.ContainerObservation{Health: state.HealthMissing}},
	}
	check := r.staleBindings(context.Background(), false)
	if check.Status != StatusWarn {
		t.Fatalf("status = %q, want warn", check.Status)
	}
	if got := findItem(t, check, "slab").Detail; !strings.Contains(got, "c-slab") {
		t.Errorf("detail = %q, want the retained container ID", got)
	}
}

func TestStaleBindingsProbeErrorIsUnknown(t *testing.T) {
	r := &Runner{
		Store:      bindStore(t, "slab"),
		DB:         Database{Version: 1, Supported: 1},
		Containers: &fake.ContainerObserver{ProbeErr: errors.New("daemon not reachable")},
	}
	if got := r.staleBindings(context.Background(), false).Status; got != StatusUnknown {
		t.Fatalf("status = %q, want unknown", got)
	}
}

func TestStaleBindingsPresentContainerIsHealthy(t *testing.T) {
	r := &Runner{
		Store:      bindStore(t, "slab"),
		DB:         Database{Version: 1, Supported: 1},
		Containers: &fake.ContainerObserver{ProbeResult: controller.ContainerObservation{Health: state.HealthPresent}},
	}
	if got := r.staleBindings(context.Background(), false).Status; got != StatusOK {
		t.Fatalf("status = %q, want ok", got)
	}
}

func TestStaleBindingsWithoutDockerShortCircuits(t *testing.T) {
	observer := &fake.ContainerObserver{}
	r := &Runner{
		Store:      bindStore(t, "slab"),
		DB:         Database{Version: 1, Supported: 1},
		Containers: observer,
	}
	check := r.staleBindings(context.Background(), true)
	if check.Status != StatusUnknown {
		t.Fatalf("status = %q, want unknown", check.Status)
	}
	if len(observer.Probed) != 0 {
		t.Errorf("probed %d bindings with no docker installed", len(observer.Probed))
	}
}

func TestStaleBindingsWithNoBindingsIsHealthy(t *testing.T) {
	r := &Runner{
		Store:      seedStore(t, "slab", t.TempDir()),
		DB:         Database{Version: 1, Supported: 1},
		Containers: &fake.ContainerObserver{},
	}
	if got := r.staleBindings(context.Background(), false).Status; got != StatusOK {
		t.Fatalf("status = %q, want ok", got)
	}
}
