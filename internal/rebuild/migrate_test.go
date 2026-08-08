package rebuild

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/controller"
	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"
)

// migrateStore is a MigrationStore over literals. The package's other
// tests use fakes for the same reason: the case analysis is the part most
// likely to be wrong, and it is exhaustively testable without SQLite.
type migrateStore struct {
	repos   []state.Repository
	dropped []string
	err     error
}

func (m *migrateStore) Repositories() ([]state.Repository, error) {
	return m.repos, m.err
}

func (m *migrateStore) DropRepository(id string) error {
	m.dropped = append(m.dropped, id)
	return nil
}

// migrateResolver resolves recorded paths from a table. A path absent from
// both maps is one the filesystem no longer has. It is separate from
// apply_test.go's mapResolver because the two model different failures:
// that one distinguishes "will not resolve" with an error table, and this
// one has to answer the existence question the upgrade pass turns on.
type migrateResolver struct {
	roots  map[string]resolve.Workspace
	exists map[string]bool
}

func (r migrateResolver) Resolve(repoRoot string) (resolve.Workspace, error) {
	ws, ok := r.roots[repoRoot]
	if !ok {
		return resolve.Workspace{}, errors.New("no such directory: " + repoRoot)
	}
	return ws, nil
}

func (r migrateResolver) Exists(path string) bool { return r.exists[path] }

type recordingRetagger struct {
	calls [][3]string
	err   error
}

func (r *recordingRetagger) RetagSession(_ context.Context, target, workspaceID, repoRoot string) error {
	r.calls = append(r.calls, [3]string{target, workspaceID, repoRoot})
	return r.err
}

type nopLocker struct{}

func (nopLocker) Lock(context.Context, string) (func(), error) { return func() {}, nil }

type fixedDigest struct{}

func (fixedDigest) Digest(string) (string, error) { return "sha256:d", nil }

type fixedClock struct{}

func (fixedClock) Now() time.Time { return time.Date(2026, 8, 8, 12, 0, 0, 0, time.UTC) }

type registerRecorder struct {
	Store
	registered []resolve.Workspace
}

func (r *registerRecorder) RegisterWorkspace(ws resolve.Workspace, _ string, _ time.Time) error {
	r.registered = append(r.registered, ws)
	return nil
}

func repoWorkspace(root string) resolve.Workspace {
	return resolve.Workspace{
		ID:           "ws-" + root,
		RepositoryID: "repo-" + root,
		Slug:         "slabledger",
		RepoRoot:     root,
		SessionName:  "slabledger",
	}
}

// A row recorded at a linked worktree resolves to its parent repository
// and collapses into it. This is the state migration 0002 leaves behind
// for every worktree that was its own workspace before the change.
func TestMigrateCollapsesALinkedWorktreeRow(t *testing.T) {
	parent := repoWorkspace("/repo")
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/repo/.worktrees/1529", Slug: "slabledger", RepoRoot: "/repo/.worktrees/1529"},
		{ID: "repo-/repo", Slug: "slabledger", RepoRoot: "/repo"},
	}}
	registrar := &registerRecorder{}
	a := &Applier{
		Store:  registrar,
		Repos:  store,
		Config: fixedDigest{},
		Locker: nopLocker{},
		Clock:  fixedClock{},
		Resolver: migrateResolver{
			roots: map[string]resolve.Workspace{
				"/repo":                 parent,
				"/repo/.worktrees/1529": parent,
			},
			exists: map[string]bool{"/repo": true, "/repo/.worktrees/1529": true},
		},
	}

	res := a.Migrate(context.Background(), nil)

	if len(res.Conflicts) != 0 {
		t.Fatalf("conflicts = %+v, want none", res.Conflicts)
	}
	if len(registrar.registered) != 1 || registrar.registered[0].RepoRoot != "/repo" {
		t.Fatalf("registered = %+v, want one registration at /repo", registrar.registered)
	}
	// Registration first, drop second: a crash between them leaves an
	// extra row that resolves to the same repository, which the next run
	// merges. The other order loses the row outright.
	if len(store.dropped) != 1 || store.dropped[0] != "repo-/repo/.worktrees/1529" {
		t.Fatalf("dropped = %v, want the linked-worktree row", store.dropped)
	}
	if len(res.Migrated) != 1 || res.Migrated[0].Action != "collapsed" ||
		res.Migrated[0].Into != "/repo" {
		t.Fatalf("migrated = %+v", res.Migrated)
	}
}

// A row whose recorded path is gone is dropped rather than carried
// forward. Nothing resolves it, so nothing can correct it.
func TestMigrateDropsAVanishedRow(t *testing.T) {
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/gone", Slug: "gone", RepoRoot: "/gone"},
	}}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    store,
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    fixedClock{},
		Resolver: migrateResolver{roots: map[string]resolve.Workspace{}, exists: map[string]bool{}},
	}

	res := a.Migrate(context.Background(), nil)

	if len(store.dropped) != 1 || store.dropped[0] != "repo-/gone" {
		t.Fatalf("dropped = %v, want the vanished row", store.dropped)
	}
	if len(res.Migrated) != 1 || res.Migrated[0].Action != "dropped" {
		t.Fatalf("migrated = %+v", res.Migrated)
	}
}

// A path that still exists but will not resolve is a conflict, not a
// drop. Deleting a row because git happened to fail would discard state
// the operator can still recover.
func TestMigrateRefusesAnUnresolvableExistingPath(t *testing.T) {
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/broken", Slug: "broken", RepoRoot: "/broken"},
	}}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    store,
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    fixedClock{},
		Resolver: migrateResolver{roots: map[string]resolve.Workspace{}, exists: map[string]bool{"/broken": true}},
	}

	res := a.Migrate(context.Background(), nil)

	if len(store.dropped) != 0 {
		t.Errorf("dropped = %v; an existing path must not be discarded", store.dropped)
	}
	if len(res.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want one", res.Conflicts)
	}
}

// A live session created before the change carries @dev_worktree pointing
// at a linked worktree and a workspace ID derived from it. Both are
// rewritten in place, because the alternative — treating the ID mismatch
// as a different workspace — registers a second row for a session that is
// already running.
func TestMigrateRetagsALiveSessionOntoItsRepository(t *testing.T) {
	parent := repoWorkspace("/repo")
	retagger := &recordingRetagger{}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    &migrateStore{},
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    fixedClock{},
		Retagger: retagger,
		Resolver: migrateResolver{
			roots:  map[string]resolve.Workspace{"/repo/.worktrees/1529": parent},
			exists: map[string]bool{"/repo/.worktrees/1529": true},
		},
	}

	res := a.Migrate(context.Background(), []controller.LiveSession{{
		Name: "slabledger--1529", WorkspaceID: "old-id",
		Slug: "slabledger", Worktree: "/repo/.worktrees/1529",
	}})

	if len(retagger.calls) != 1 {
		t.Fatalf("retag calls = %v, want one", retagger.calls)
	}
	if got := retagger.calls[0]; got[0] != "slabledger--1529" ||
		got[1] != parent.ID || got[2] != "/repo" {
		t.Errorf("retag = %v, want the session retagged onto %s", got, parent.ID)
	}
	// The corrected session is what classification must see; the stale
	// one would classify as an unrelated workspace and register a second
	// row for a session already running.
	if len(res.Live) != 1 || res.Live[0].WorkspaceID != parent.ID ||
		res.Live[0].Worktree != "/repo" {
		t.Fatalf("live = %+v", res.Live)
	}
	if len(res.Migrated) != 1 || res.Migrated[0].Action != "retagged" {
		t.Fatalf("migrated = %+v", res.Migrated)
	}
}

// A session whose keys already agree is left alone: a second run must be
// a silent no-op, the same property Classify's settled case protects.
func TestMigrateLeavesACorrectSessionUntouched(t *testing.T) {
	parent := repoWorkspace("/repo")
	retagger := &recordingRetagger{}
	a := &Applier{
		Store:    &registerRecorder{},
		Repos:    &migrateStore{},
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    fixedClock{},
		Retagger: retagger,
		Resolver: migrateResolver{
			roots:  map[string]resolve.Workspace{"/repo": parent},
			exists: map[string]bool{"/repo": true},
		},
	}

	res := a.Migrate(context.Background(), []controller.LiveSession{{
		Name: "slabledger", WorkspaceID: parent.ID,
		Slug: parent.Slug, Worktree: "/repo",
	}})

	if len(retagger.calls) != 0 {
		t.Errorf("retag calls = %v, want none", retagger.calls)
	}
	if len(res.Migrated) != 0 {
		t.Errorf("migrated = %+v, want nothing reported", res.Migrated)
	}
}

// --dry-run predicts the pass without performing it, matching Apply's
// contract that a preview reports exactly what the real run would do.
func TestMigrateDryRunWritesNothing(t *testing.T) {
	parent := repoWorkspace("/repo")
	store := &migrateStore{repos: []state.Repository{
		{ID: "repo-/repo/.worktrees/1529", Slug: "slabledger", RepoRoot: "/repo/.worktrees/1529"},
	}}
	registrar := &registerRecorder{}
	retagger := &recordingRetagger{}
	a := &Applier{
		Store:    registrar,
		Repos:    store,
		Config:   fixedDigest{},
		Locker:   nopLocker{},
		Clock:    fixedClock{},
		Retagger: retagger,
		DryRun:   true,
		Resolver: migrateResolver{
			roots:  map[string]resolve.Workspace{"/repo/.worktrees/1529": parent},
			exists: map[string]bool{"/repo/.worktrees/1529": true},
		},
	}

	res := a.Migrate(context.Background(), []controller.LiveSession{{
		Name: "slabledger--1529", WorkspaceID: "old-id",
		Slug: "slabledger", Worktree: "/repo/.worktrees/1529",
	}})

	if len(store.dropped) != 0 || len(registrar.registered) != 0 || len(retagger.calls) != 0 {
		t.Errorf("dry run wrote: dropped %v, registered %+v, retagged %v",
			store.dropped, registrar.registered, retagger.calls)
	}
	if len(res.Migrated) != 2 {
		t.Errorf("migrated = %+v, want the collapse and the retag predicted", res.Migrated)
	}
}
