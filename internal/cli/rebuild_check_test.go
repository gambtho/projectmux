package cli

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	"github.com/gambtho/projectmux/internal/state"

	// The driver is registered by internal/state already; naming it here
	// keeps this file's sql.Open honest about what it depends on.
	_ "modernc.org/sqlite"
)

// healthyStateRoot creates a state root holding a database this build
// wrote and closed cleanly, and returns the root and the database path.
func healthyStateRoot(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	st, err := state.Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-1", Slug: "slab", Worktree: "/w/slab", SessionName: "slab", IsPrimary: true,
	}, "sha256:abc", time.Now()); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return root, state.DBPath(root)
}

// A corrupt database is the one case rebuild refuses outright. The
// message is the deliverable: it must name the database and both
// sidecars, because moving state.db alone leaves a stale write-ahead log
// that a freshly created database would inherit.
func TestRebuildDatabaseCheckRefusesACorruptDatabase(t *testing.T) {
	root, path := healthyStateRoot(t)
	if err := os.WriteFile(path, []byte("this is not a database at all"), 0o600); err != nil {
		t.Fatalf("corrupting: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	err = rebuildDatabaseCheck(root)
	if err == nil {
		t.Fatal("rebuildDatabaseCheck accepted a corrupt database")
	}
	for _, want := range []string{path, path + "-wal", path + "-shm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}
	if !strings.Contains(err.Error(), "aside") {
		t.Errorf("refusal %q does not say to move the files aside", err)
	}

	// Rebuild refuses; it never relocates or repairs. The operator
	// performs one inspectable mv.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(before) != string(after) {
		t.Error("the check modified the database file")
	}
}

// A database a newer projectmux wrote is refused, like a corrupt one —
// but for the opposite reason, and the message must say so. Its contents
// are good; they are simply richer than this build understands. Telling
// the operator to move them aside would destroy a working installation to
// resolve a version mismatch, so this test asserts the refusal does not
// say that.
func TestRebuildDatabaseCheckRefusesAFutureSchemaWithoutAdvisingDestruction(t *testing.T) {
	root, path := healthyStateRoot(t)

	// Claim a schema this build does not know. The pragma write goes
	// through the ordinary driver; a clean close checkpoints it into the
	// database file and removes the sidecars.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", state.SchemaVersion+1)); err != nil {
		t.Fatalf("bumping user_version: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	err = rebuildDatabaseCheck(root)
	if err == nil {
		t.Fatal("rebuildDatabaseCheck accepted a database from a newer build")
	}
	msg := err.Error()
	if !strings.Contains(msg, path) {
		t.Errorf("refusal %q does not name the database", msg)
	}
	if !strings.Contains(msg, "newer") {
		t.Errorf("refusal %q does not say a newer build wrote it", msg)
	}
	// The corrupt-database advice must not leak here. These bytes are the
	// operator's state, intact.
	for _, forbidden := range []string{"aside", "cannot be read"} {
		if strings.Contains(msg, forbidden) {
			t.Errorf("refusal %q contains %q, which would advise destroying good data", msg, forbidden)
		}
	}
}

// A missing database is the primary recovery path, not a failure:
// state.Open creates it and rebuild proceeds against a fresh one.
func TestRebuildDatabaseCheckProceedsWhenTheDatabaseIsMissing(t *testing.T) {
	if err := rebuildDatabaseCheck(t.TempDir()); err != nil {
		t.Errorf("rebuildDatabaseCheck on a fresh installation = %v, want nil", err)
	}
}

func TestRebuildDatabaseCheckProceedsOnAHealthyDatabase(t *testing.T) {
	root, _ := healthyStateRoot(t)
	if err := rebuildDatabaseCheck(root); err != nil {
		t.Errorf("rebuildDatabaseCheck = %v, want nil", err)
	}
}

// A -wal with no -shm beside it means a writer died without
// checkpointing — precisely the crash rebuild exists to recover from.
// Refusing here would refuse the main case, so this test is load-bearing
// rather than an edge case.
func TestRebuildDatabaseCheckProceedsWithAnUnrecoveredWAL(t *testing.T) {
	root, path := healthyStateRoot(t)

	// Stage the crash: capture the database and its log while a writer
	// holds them, then restore both after that writer's clean close has
	// removed the sidecars. What is left is a pre-checkpoint database
	// with an orphaned -wal and no -shm.
	st, err := state.Open(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.RegisterWorkspace(resolve.Workspace{
		ID: "id-2", Slug: "other", Worktree: "/w/other", SessionName: "other", IsPrimary: true,
	}, "sha256:def", time.Now()); err != nil {
		t.Fatalf("register: %v", err)
	}
	dbBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	walBytes, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatalf("read wal: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := os.WriteFile(path, dbBytes, 0o600); err != nil {
		t.Fatalf("restore db: %v", err)
	}
	if err := os.WriteFile(path+"-wal", walBytes, 0o600); err != nil {
		t.Fatalf("restore wal: %v", err)
	}

	if err := rebuildDatabaseCheck(root); err != nil {
		t.Fatalf("rebuildDatabaseCheck refused an unrecovered log: %v", err)
	}

	// The point of proceeding is that state.Open recovers the log rather
	// than merely tolerating it: id-2, staged only in the orphaned -wal,
	// survives the recovery.
	st, err = state.Open(root)
	if err != nil {
		t.Fatalf("open after recovery: %v", err)
	}
	if _, err := st.Workspace("id-2"); err != nil {
		t.Errorf("workspace id-2: %v, want the WAL-recovered row", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}
