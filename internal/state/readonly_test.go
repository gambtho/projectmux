package state

import (
	"errors"
	"io/fs"
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
	if walStateOf(DBPath(root)) != walComplete {
		t.Fatal("the writer left no complete WAL, so this test proves nothing")
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

// seedUnrecoveredWAL leaves a database beside a -wal with no -shm: what
// a writer that did not shut down cleanly leaves behind. A crash cannot
// be staged inside the test process, so the -wal is copied aside while a
// writer still holds it and restored after that writer closes — the
// close checkpoints the row into the database file, which is why the
// pre-checkpoint bytes are restored along with it.
func seedUnrecoveredWAL(t *testing.T) string {
	t.Helper()
	root := seedDatabase(t)
	path := DBPath(root)

	st, err := Open(root)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
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
	db, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the database: %v", err)
	}
	wal, err := os.ReadFile(path + "-wal")
	if err != nil {
		t.Fatalf("reading the -wal: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(path, db, 0o600); err != nil {
		t.Fatalf("restoring the database: %v", err)
	}
	if err := os.WriteFile(path+"-wal", wal, 0o600); err != nil {
		t.Fatalf("restoring the -wal: %v", err)
	}
	if got := walStateOf(path); got != walIncomplete {
		t.Fatalf("staged WAL state = %v, want walIncomplete", got)
	}
	return root
}

// TestOpenReadOnlyUnrecoveredWALIsUncertainty covers the state no DSN
// serves. Reading it the ordinary way recovers the log and keeps the
// -shm it had to build; reading it immutably silently omits every row
// the log holds — measured as 1 row where 2 were committed, with
// integrity still reporting "ok". A diagnosis may do neither, so it
// reports that it could not look.
func TestOpenReadOnlyUnrecoveredWALIsUncertainty(t *testing.T) {
	root := seedUnrecoveredWAL(t)
	before := rootNames(t, root)

	ro, insp, err := OpenReadOnly(root)
	if err == nil {
		ro.Close()
		t.Fatalf("an unrecovered write-ahead log opened cleanly: %+v", insp)
	}
	if ro != nil {
		t.Error("a failed open returned a store")
	}
	if insp.IntegrityErr != nil {
		t.Errorf("an unread database was reported as corrupt: %v", insp.IntegrityErr)
	}
	if got := rootNames(t, root); !slices.Equal(before, got) {
		t.Errorf("state root = %v, want %v unchanged: the log was recovered", got, before)
	}
}

// TestOpenReadOnlyUnrecoveredWALIsTyped pins the one refusal a mutating
// command must tell apart from all the others. An unrecovered log is the
// crash case rebuild exists to recover — state.Open recovers it — while a
// permission failure is uncertainty rebuild must stop on. Untyped, the two
// are the same error value. The message is asserted byte for byte because
// doctor prints it unchanged and this change must not touch its output.
func TestOpenReadOnlyUnrecoveredWALIsTyped(t *testing.T) {
	root := seedUnrecoveredWAL(t)
	path := DBPath(root)

	ro, _, err := OpenReadOnly(root)
	if err == nil {
		ro.Close()
		t.Fatal("an unrecovered write-ahead log opened cleanly")
	}
	var walErr *IncompleteWALError
	if !errors.As(err, &walErr) {
		t.Fatalf("OpenReadOnly error = %T (%v), want *IncompleteWALError", err, err)
	}
	if walErr.Path != path {
		t.Errorf("Path = %q, want %q", walErr.Path, path)
	}
	if !IsIncompleteWAL(err) {
		t.Error("IsIncompleteWAL = false on an unrecovered write-ahead log")
	}

	want := "the state database at " + path + " has a write-ahead log with no shared-memory index, " +
		"which means a writer did not shut down cleanly; reading it would require " +
		"recovering the log into the state directory, which an inspection must not do. " +
		"The next mutating command recovers it"
	if got := err.Error(); got != want {
		t.Errorf("message changed:\n got %q\nwant %q", got, want)
	}

	// The predicate must be as narrow as the type: every other refusal is
	// still a refusal, and a missing database is the nearest neighbour.
	if IsMissingDatabase(err) {
		t.Error("an unrecovered write-ahead log was reported as a missing database")
	}
	if _, _, missing := OpenReadOnly(t.TempDir()); IsIncompleteWAL(missing) {
		t.Error("a missing database was reported as an unrecovered write-ahead log")
	}
}

// TestOpenReadOnlyUnexaminableSidecarsAreNotTypedAsAnIncompleteWAL is the
// reason walUnknown exists. Every case here is "the filesystem would not
// tell us", which is uncertainty; the typed error means "we looked, and a
// writer crashed", which a mutating command is licensed to proceed past.
// Collapsing the two would let rebuild open a database it never examined.
func TestOpenReadOnlyUnexaminableSidecarsAreNotTypedAsAnIncompleteWAL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(t *testing.T, path string)
	}{
		{
			name: "a -wal that is not a regular file",
			stage: func(t *testing.T, path string) {
				if err := os.Mkdir(path+"-wal", 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
			},
		},
		{
			name: "a -shm that cannot be stat'ed",
			stage: func(t *testing.T, path string) {
				if err := os.WriteFile(path+"-wal", []byte("log"), 0o600); err != nil {
					t.Fatalf("writing the -wal: %v", err)
				}
				// A -shm inside an unsearchable directory: stat fails with
				// EACCES rather than ENOENT, which is "we cannot tell"
				// rather than "it is absent".
				blocked := filepath.Join(t.TempDir(), "blocked")
				if err := os.Mkdir(blocked, 0o000); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
				if err := os.Symlink(filepath.Join(blocked, "target"), path+"-shm"); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				if _, err := os.Stat(filepath.Join(blocked, "probe")); errors.Is(err, fs.ErrNotExist) {
					t.Skip("this filesystem or user ignores directory permissions")
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := seedDatabase(t)
			path := DBPath(root)
			tc.stage(t, path)

			ro, _, err := OpenReadOnly(root)
			if err == nil {
				ro.Close()
				t.Fatal("an unexaminable sidecar opened cleanly")
			}
			if IsIncompleteWAL(err) {
				t.Errorf("uncertainty was typed as a recoverable crash: %v", err)
			}
		})
	}
}

// TestWalStateOfClassifiesSidecars pins the four states apart, since
// which DSN is safe follows entirely from this classification.
func TestWalStateOfClassifiesSidecars(t *testing.T) {
	clean := DBPath(seedDatabase(t))
	if got := walStateOf(clean); got != walNone {
		t.Errorf("a checkpointed database = %v, want walNone", got)
	}

	if got := walStateOf(DBPath(seedUnrecoveredWAL(t))); got != walIncomplete {
		t.Errorf("a -wal with no -shm = %v, want walIncomplete", got)
	}

	// A sidecar that cannot be examined is not an absent one — and it is
	// not a crashed writer either. walIncomplete means "we looked, and a
	// writer left a log behind"; this is "we could not look".
	if err := os.Mkdir(clean+"-wal", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if got := walStateOf(clean); got != walUnknown {
		t.Errorf("an unexaminable -wal = %v, want walUnknown", got)
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

// TestWALStateShmThatIsNotAFileIsUnknown pins the -shm side of the rule
// the -wal side already carries. A directory named state.db-shm is not
// the shared-memory index this code would be reasoning about, so its
// presence establishes nothing: without this, the stat's success alone
// read as walComplete and an unexaminable state root was diagnosed as a
// healthy one.
func TestWALStateShmThatIsNotAFileIsUnknown(t *testing.T) {
	root := seedUnrecoveredWAL(t)
	path := DBPath(root)

	if err := os.Mkdir(path+"-shm", 0o700); err != nil {
		t.Fatalf("creating a directory at the -shm path: %v", err)
	}

	if got := walStateOf(path); got != walUnknown {
		t.Fatalf("walStateOf = %v, want walUnknown", got)
	}

	ro, _, err := OpenReadOnly(root)
	if err == nil {
		ro.Close()
		t.Fatal("an unexaminable -shm opened cleanly")
	}
	// Uncertainty, not the crash case: IncompleteWALError is a licence to
	// recover the log, and nothing here established a writer crashed.
	if IsIncompleteWAL(err) {
		t.Errorf("an unexaminable -shm was reported as an unrecovered log: %v", err)
	}
}
