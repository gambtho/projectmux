package state

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
)

// seedDatabase creates a healthy database with one registered workspace
// and returns its root.
func seedDatabase(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	st, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ws := resolve.Workspace{
		ID:          "id-1",
		Slug:        "slab",
		Worktree:    "/w/slab",
		SessionName: "slab",
		IsPrimary:   true,
	}
	if err := st.RegisterWorkspace(ws, "sha256:abc", time.Now()); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return root
}

func TestOpenReadOnlyHealthyDatabase(t *testing.T) {
	root := seedDatabase(t)

	ro, insp, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	if insp.IntegrityErr != nil {
		t.Fatalf("integrity: %v", insp.IntegrityErr)
	}
	if insp.UserVersion != SchemaVersion {
		t.Fatalf("user_version = %d, want %d", insp.UserVersion, SchemaVersion)
	}
	if err := insp.Usable(); err != nil {
		t.Fatalf("Usable: %v", err)
	}
	records, err := ro.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(records) != 1 || records[0].ID != "id-1" {
		t.Fatalf("Workspaces = %+v, want one record id-1", records)
	}
	if _, err := ro.Workspace("id-1"); err != nil {
		t.Fatalf("Workspace: %v", err)
	}
	if _, err := ro.Workspace("absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Workspace(absent) = %v, want ErrNotFound", err)
	}
}

// TestOpenReadOnlyLeavesTheFileUntouched is the guarantee doctor rests
// on: inspecting a database neither creates nor rewrites it.
func TestOpenReadOnlyLeavesTheFileUntouched(t *testing.T) {
	root := seedDatabase(t)
	path := DBPath(root)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the database: %v", err)
	}
	ro, _, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if _, err := ro.Workspaces(); err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if err := ro.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-reading the database: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("the read-only inspection modified the database file")
	}
}

// rootNames lists the state root, which the no-sidecar tests compare
// across an inspection.
func rootNames(t *testing.T, root string) []string {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}

// TestOpenReadOnlyCreatesNoSidecars is the load-bearing one for the
// no-mutation contract. Reading a WAL database the ordinary way leaves
// -shm and -wal behind for good, which would make a diagnosis-only
// command a permanent writer to the state root.
func TestOpenReadOnlyCreatesNoSidecars(t *testing.T) {
	root := seedDatabase(t)
	before := rootNames(t, root)

	ro, _, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	if _, err := ro.Workspaces(); err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	during := rootNames(t, root)
	if err := ro.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	after := rootNames(t, root)

	for _, got := range []struct {
		when  string
		names []string
	}{{"during the inspection", during}, {"after it closed", after}} {
		if !slices.Equal(before, got.names) {
			t.Errorf("state root %s = %v, want %v unchanged", got.when, got.names, before)
		}
	}
}

// TestOpenReadOnlyOnAnUnwritableRoot covers the case the sidecars used
// to make impossible: an installation whose state directory cannot be
// written is exactly when someone reaches for doctor.
func TestOpenReadOnlyOnAnUnwritableRoot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	root := seedDatabase(t)
	if err := os.Chmod(root, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	ro, insp, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly on an unwritable root: %v", err)
	}
	defer ro.Close()
	if usable := insp.Usable(); usable != nil {
		t.Fatalf("a healthy database on an unwritable root reported %v", usable)
	}
	rows, err := ro.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("%d workspaces, want the 1 that was registered", len(rows))
	}
}

// TestOpenReadOnlySeesUncheckpointedRows is the other half of the
// contract the sidecar fix must not break. An immutable read of a live
// WAL database silently omits committed rows, so a database with a -wal
// beside it has to be read the ordinary way — where the sidecars
// already exist, so nothing new is created there either.
func TestOpenReadOnlySeesUncheckpointedRows(t *testing.T) {
	root := seedDatabase(t)
	// Hold the writer open so its -wal is not checkpointed away.
	st, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	ws := resolve.Workspace{
		ID:          "id-2",
		Slug:        "slab2",
		Worktree:    "/w/slab2",
		SessionName: "slab2",
		IsPrimary:   true,
	}
	if err := st.RegisterWorkspace(ws, "sha256:def", time.Now()); err != nil {
		t.Fatalf("RegisterWorkspace: %v", err)
	}
	if !walPresent(DBPath(root)) {
		t.Fatal("the writer left no -wal, so this test proves nothing")
	}
	before := rootNames(t, root)

	ro, insp, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if usable := insp.Usable(); usable != nil {
		t.Fatalf("a healthy database reported %v", usable)
	}
	rows, err := ro.Workspaces()
	if err != nil {
		t.Fatalf("Workspaces: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("%d workspaces, want 2: the uncheckpointed row was missed", len(rows))
	}
	if got := rootNames(t, root); !slices.Equal(before, got) {
		t.Errorf("state root = %v, want %v unchanged", got, before)
	}
}

func TestOpenReadOnlyMissingDatabaseNeitherCreatesNorReports(t *testing.T) {
	root := t.TempDir()

	_, _, err := OpenReadOnly(root)
	if err == nil {
		t.Fatal("OpenReadOnly on a missing database returned no error")
	}
	if !IsMissingDatabase(err) {
		t.Fatalf("IsMissingDatabase(%v) = false, want true", err)
	}
	if _, statErr := os.Stat(DBPath(root)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("OpenReadOnly created the database file")
	}
}

func TestOpenReadOnlyCorruptDatabaseReportsIntegrity(t *testing.T) {
	root := seedDatabase(t)
	path := DBPath(root)

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the database: %v", err)
	}
	// Keep the header so the file is still recognizably SQLite, and
	// scribble over the pages behind it.
	for i := 100; i < len(raw); i++ {
		raw[i] = 0xFF
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("writing the corrupt database: %v", err)
	}

	ro, insp, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if insp.IntegrityErr == nil {
		t.Fatal("a corrupt database reported no integrity error")
	}
	if usable := insp.Usable(); usable == nil {
		t.Fatal("a corrupt database reported itself usable")
	}
}

// TestOpenReadOnlyUnreadableDatabaseIsNotCorruption separates the two
// ways a first query can fail. A file the process may not read says
// nothing about its contents, so it must surface as an error the caller
// reports as uncertainty — never as an integrity finding, which asserts
// the bytes were examined and found malformed.
func TestOpenReadOnlyUnreadableDatabaseIsNotCorruption(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores file permissions")
	}
	root := seedDatabase(t)
	path := DBPath(root)
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	ro, insp, err := OpenReadOnly(root)
	if err == nil {
		ro.Close()
		t.Fatalf("an unreadable database opened cleanly: %+v", insp)
	}
	if ro != nil {
		t.Error("a failed open returned a store")
	}
	if insp.IntegrityErr != nil {
		t.Errorf("an unreadable database was reported as corrupt: %v", insp.IntegrityErr)
	}
}

func TestOpenReadOnlyNotADatabase(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(DBPath(root), []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("writing the file: %v", err)
	}

	ro, insp, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()
	if insp.Usable() == nil {
		t.Fatal("a non-database file reported itself usable")
	}
}

func TestInspectionUsableReportsSchemaDrift(t *testing.T) {
	pending := Inspection{UserVersion: SchemaVersion - 1}
	var pendingErr *PendingMigrationError
	if err := pending.Usable(); !errors.As(err, &pendingErr) {
		t.Fatalf("Usable() = %v, want PendingMigrationError", err)
	}

	future := Inspection{UserVersion: SchemaVersion + 1}
	var futureErr *FutureSchemaError
	if err := future.Usable(); !errors.As(err, &futureErr) {
		t.Fatalf("Usable() = %v, want FutureSchemaError", err)
	}
}

// TestOpenReadOnlyRefusesWrites confirms the connection itself is
// read-only, not merely used carefully.
func TestOpenReadOnlyRefusesWrites(t *testing.T) {
	root := seedDatabase(t)
	ro, _, err := OpenReadOnly(root)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer ro.Close()

	if _, err := ro.db.Exec("DELETE FROM workspaces"); err == nil {
		t.Fatal("a write through the read-only connection succeeded")
	}
}

func TestDBPath(t *testing.T) {
	if got, want := DBPath("/state"), filepath.Join("/state", "state.db"); got != want {
		t.Fatalf("DBPath = %q, want %q", got, want)
	}
}
