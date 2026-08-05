package state

import (
	"errors"
	"os"
	"path/filepath"
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
