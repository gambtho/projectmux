package state

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
	sqlite "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

func encodeTime(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func decodeTime(s string) (time.Time, error) { return time.Parse(time.RFC3339Nano, s) }

// RegisterWorkspace upserts the repository and the session on it.
// Re-registration refreshes everything derivable from resolution and
// configuration while preserving registered_at, the assigned session name,
// the applied digest, and any binding — rebuilding the database is simply
// re-running registration (design §7).
func (s *Store) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Foreign keys are enforced on this connection (internal/state/state.go:65),
	// so deleting the stale repository alone would cascade the rest away. The
	// cleanup is written out statement by statement anyway, in dependency
	// order, so that a reader can see what a stale repository takes with it
	// without reconstructing the schema from memory. Each statement is a no-op
	// when the cascade would have covered it, because the cascade has not run
	// yet — the repository row is deleted last.
	//
	// repo_root is UNIQUE, so a row recorded under a different ID for this
	// same path would fail the insert rather than be refreshed by it. This is
	// *not* the migrated case: the pre-0002 workspace ID was SHA-256 of the
	// canonical path (internal/resolve/resolve.go:107 before Task 1), which is
	// byte-identical to the new repository ID, so a migrated repository row
	// already carries the right ID and this delete matches nothing. It fires
	// when a repository's identity genuinely moved — a path re-cased or
	// re-canonicalized under an ID derived from the old spelling.
	if _, err := tx.Exec(`
		DELETE FROM last_operations WHERE workspace_id IN (
			SELECT w.id FROM workspaces w
			JOIN repositories r ON r.id = w.repository_id
			WHERE r.repo_root = ? AND r.id <> ?)`,
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("clearing operations for the stale repository at %s: %w", ws.RepoRoot, err)
	}
	if _, err := tx.Exec(`
		DELETE FROM workspaces WHERE repository_id IN (
			SELECT id FROM repositories WHERE repo_root = ? AND id <> ?)`,
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("clearing sessions of the stale repository at %s: %w", ws.RepoRoot, err)
	}
	if _, err := tx.Exec(
		"DELETE FROM container_bindings WHERE repository_id IN ("+
			"SELECT id FROM repositories WHERE repo_root = ? AND id <> ?)",
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("clearing the binding of the stale repository at %s: %w", ws.RepoRoot, err)
	}
	if _, err := tx.Exec(
		"DELETE FROM repositories WHERE repo_root = ? AND id <> ?",
		ws.RepoRoot, ws.RepositoryID); err != nil {
		return fmt.Errorf("replacing the stale repository for %s: %w", ws.RepoRoot, err)
	}

	// The *workspace* ID is where migration 0002 does leave a stale value. It
	// carries the old ID into workspaces.id because SQLite cannot compute
	// SHA-256, but the new derivation hashes the session alongside the path,
	// so the default session's ID changes even though its repository's does
	// not. Inserting the new ID beside the carried-over row would violate
	// UNIQUE (repository_id, session) — that row is already the default
	// session of this repository — and the ON CONFLICT(id) clause below cannot
	// absorb a conflict on a different constraint, so the insert would fail
	// outright and every migrated repository would be unopenable.
	//
	// Re-key the row instead of deleting it. actual_session and applied_digest
	// are what let the next reconciliation adopt the tmux session that is
	// already running rather than treat it as a foreign occupant; dropping
	// them would turn every first post-migration open into a name conflict.
	var stale string
	err = tx.QueryRow(
		"SELECT id FROM workspaces WHERE repository_id = ? AND session = ? AND id <> ?",
		ws.RepositoryID, ws.Session, ws.ID).Scan(&stale)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Nothing carried over for this session: the common steady-state path.
	case err != nil:
		return fmt.Errorf("looking for a stale ID for %s: %w", ws.SessionName, err)
	default:
		// last_operations follows on its own: its workspace_id is declared
		// ON UPDATE CASCADE and foreign keys are enforced on this connection
		// (internal/state/state.go:65). An explicit UPDATE here would match
		// nothing, because the cascade has already moved the row by the time
		// it ran. The regression test asserts the operation survives under
		// the new ID, so a schema change that dropped the cascade would fail
		// there rather than silently orphan the row.
		if _, err := tx.Exec(
			"UPDATE workspaces SET id = ?, updated_at = ? WHERE id = ?",
			ws.ID, encodeTime(now), stale); err != nil {
			return fmt.Errorf("re-keying the migrated session %s: %w", ws.SessionName, err)
		}
	}

	_, err = tx.Exec(`
		INSERT INTO repositories (id, slug, repo_root, registered_at, updated_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			slug       = excluded.slug,
			repo_root  = excluded.repo_root,
			updated_at = excluded.updated_at`,
		ws.RepositoryID, ws.Slug, ws.RepoRoot, encodeTime(now), encodeTime(now))
	if err != nil {
		return fmt.Errorf("registering repository %s: %w", ws.RepositoryID, err)
	}

	_, err = tx.Exec(`
		INSERT INTO workspaces
			(id, repository_id, session, proposed_session,
			 desired_digest, registered_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			repository_id    = excluded.repository_id,
			session          = excluded.session,
			proposed_session = excluded.proposed_session,
			desired_digest   = excluded.desired_digest,
			updated_at       = excluded.updated_at`,
		ws.ID, ws.RepositoryID, ws.Session, ws.SessionName,
		desiredDigest, encodeTime(now), encodeTime(now))
	if err != nil {
		return fmt.Errorf("registering workspace %s: %w", ws.ID, err)
	}
	return tx.Commit()
}

// selectRecord joins the repository for the identity columns and the
// repository's container binding for the projection Record.Container
// exposes. The binding join is on repository_id, not workspace_id: that is
// the whole point — every session on a repository reads the same binding,
// including one a sibling session wrote.
const selectRecord = `
SELECT
	w.id, w.repository_id, r.slug, r.repo_root, w.session, w.proposed_session,
	w.actual_session, w.desired_digest, w.applied_digest,
	w.registered_at, w.updated_at,
	cb.kind, cb.container_id, cb.container_user, cb.workdir,
	cb.health, cb.observed_at,
	o.operation, o.outcome, o.exit_status, o.error_summary, o.finished_at
FROM workspaces w
JOIN repositories r ON r.id = w.repository_id
LEFT JOIN container_bindings cb ON cb.repository_id = w.repository_id
LEFT JOIN last_operations o ON o.workspace_id = w.id`

// Workspace returns the joined record for one workspace, or ErrNotFound.
func (s *Store) Workspace(id string) (Record, error) { return queryWorkspace(s.db, id) }

// Workspaces returns every registered session ordered by slug, repository
// root, then session.
func (s *Store) Workspaces() ([]Record, error) { return queryWorkspaces(s.db) }

// The read queries take the pool rather than the Store so the read-only
// inspection path (readonly.go) can reuse them without also inheriting
// the mutating methods.

func queryWorkspace(db *sql.DB, id string) (Record, error) {
	rec, err := scanRecord(db.QueryRow(selectRecord+" WHERE w.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("workspace %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Record{}, fmt.Errorf("reading workspace %s: %w", id, err)
	}
	return rec, nil
}

func queryWorkspaces(db *sql.DB) ([]Record, error) {
	rows, err := db.Query(selectRecord + " ORDER BY r.slug, r.repo_root, w.session")
	if err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Record
	for rows.Next() {
		rec, err := scanRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a workspace row: %w", err)
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	return out, nil
}

const selectRepository = `
SELECT
	r.id, r.slug, r.repo_root, r.registered_at, r.updated_at,
	b.kind, b.container_id, b.container_user, b.workdir, b.health, b.observed_at
FROM repositories r
LEFT JOIN container_bindings b ON b.repository_id = r.id`

// Repositories returns every registered repository ordered by slug, then
// repository root. Autostart iterates this rather than filtering sessions, so
// a shared container is started once per repository by construction.
func (s *Store) Repositories() ([]Repository, error) { return queryRepositories(s.db) }

func queryRepositories(db *sql.DB) ([]Repository, error) {
	rows, err := db.Query(selectRepository + " ORDER BY r.slug, r.repo_root")
	if err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Repository
	for rows.Next() {
		repo, err := scanRepository(rows)
		if err != nil {
			return nil, fmt.Errorf("reading a repository row: %w", err)
		}
		out = append(out, repo)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing repositories: %w", err)
	}
	return out, nil
}

func scanRepository(r rowScanner) (Repository, error) {
	var (
		repo                Repository
		registered, updated string
		cKind, cID          sql.NullString
		cUser, cWorkdir     sql.NullString
		cHealth, cObserved  sql.NullString
	)
	err := r.Scan(
		&repo.ID, &repo.Slug, &repo.RepoRoot, &registered, &updated,
		&cKind, &cID, &cUser, &cWorkdir, &cHealth, &cObserved)
	if err != nil {
		return Repository{}, err
	}
	if repo.RegisteredAt, err = decodeTime(registered); err != nil {
		return Repository{}, fmt.Errorf("registered_at: %w", err)
	}
	if repo.UpdatedAt, err = decodeTime(updated); err != nil {
		return Repository{}, fmt.Errorf("updated_at: %w", err)
	}
	if cKind.Valid {
		observedAt, err := decodeTime(cObserved.String)
		if err != nil {
			return Repository{}, fmt.Errorf("observed_at: %w", err)
		}
		repo.Container = &ContainerBinding{
			Kind:          cKind.String,
			ContainerID:   cID.String,
			ContainerUser: cUser.String,
			Workdir:       cWorkdir.String,
			Health:        Health(cHealth.String),
			ObservedAt:    observedAt,
		}
	}
	return repo, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(r rowScanner) (Record, error) {
	var (
		rec                 Record
		actual, desired     sql.NullString
		applied             sql.NullString
		registered, updated string
		cKind, cID          sql.NullString
		cUser, cWorkdir     sql.NullString
		cHealth, cObserved  sql.NullString
		oName, oOutcome     sql.NullString
		oSummary, oFinished sql.NullString
		oExit               sql.NullInt64
	)
	err := r.Scan(
		&rec.ID, &rec.RepositoryID, &rec.Slug, &rec.RepoRoot, &rec.Session,
		&rec.ProposedSession, &actual, &desired, &applied, &registered, &updated,
		&cKind, &cID, &cUser, &cWorkdir, &cHealth, &cObserved,
		&oName, &oOutcome, &oExit, &oSummary, &oFinished)
	if err != nil {
		return Record{}, err
	}

	rec.ActualSession = nullable(actual)
	rec.DesiredDigest = nullable(desired)
	rec.AppliedDigest = nullable(applied)
	if rec.RegisteredAt, err = decodeTime(registered); err != nil {
		return Record{}, fmt.Errorf("registered_at: %w", err)
	}
	if rec.UpdatedAt, err = decodeTime(updated); err != nil {
		return Record{}, fmt.Errorf("updated_at: %w", err)
	}

	// container_id is NOT NULL in the table, so its validity is a faithful
	// test for "the LEFT JOIN matched a binding" rather than for "the column
	// happened to be set".
	if cID.Valid {
		observedAt, err := decodeTime(cObserved.String)
		if err != nil {
			return Record{}, fmt.Errorf("observed_at: %w", err)
		}
		rec.Container = &ContainerBinding{
			Kind:          cKind.String,
			ContainerID:   cID.String,
			ContainerUser: cUser.String,
			Workdir:       cWorkdir.String,
			Health:        Health(cHealth.String),
			ObservedAt:    observedAt,
		}
	}

	if oName.Valid {
		finishedAt, err := decodeTime(oFinished.String)
		if err != nil {
			return Record{}, fmt.Errorf("finished_at: %w", err)
		}
		op := &Operation{
			Name:         oName.String,
			Outcome:      Outcome(oOutcome.String),
			ErrorSummary: oSummary.String,
			FinishedAt:   finishedAt,
		}
		if oExit.Valid {
			exit := int(oExit.Int64)
			op.ExitStatus = &exit
		}
		rec.LastOperation = op
	}
	return rec, nil
}

func nullable(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

// maxNameCandidates bounds the collision scan; hitting it means something
// is systematically wrong, not that the loop should keep going.
const maxNameCandidates = 100

func isUniqueViolation(err error) bool {
	var se *sqlite.Error
	return errors.As(err, &se) && se.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}

// AllocateSessionName assigns the workspace's actual session name inside
// one immediate transaction. The proposed name is tried first, then
// numbered variants; the UNIQUE constraint on actual_session is the
// collision-prevention mechanism — there is deliberately no
// SELECT-then-INSERT check (design §7). An already-assigned workspace gets
// its existing name back.
func (s *Store) AllocateSessionName(workspaceID string, now time.Time) (string, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return "", fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var proposed string
	var actual sql.NullString
	err = tx.QueryRow(
		"SELECT proposed_session, actual_session FROM workspaces WHERE id = ?",
		workspaceID).Scan(&proposed, &actual)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
	}
	if err != nil {
		return "", fmt.Errorf("reading workspace %s: %w", workspaceID, err)
	}
	if actual.Valid {
		return actual.String, tx.Commit()
	}

	for i := 1; i <= maxNameCandidates; i++ {
		candidate := proposed
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", proposed, i)
		}
		_, err := tx.Exec(
			"UPDATE workspaces SET actual_session = ?, updated_at = ? WHERE id = ?",
			candidate, encodeTime(now), workspaceID)
		if isUniqueViolation(err) {
			// SQLite rolls back only the failed statement; the
			// transaction stays usable for the next candidate.
			continue
		}
		if err != nil {
			return "", fmt.Errorf("assigning session name %q: %w", candidate, err)
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("committing the session name: %w", err)
		}
		return candidate, nil
	}
	return "", fmt.Errorf(
		"no free session name near %q after %d candidates", proposed, maxNameCandidates)
}

// txExecer is the slice of *sql.Tx the record helpers need, so
// CommitReconciliation can reuse them inside its own transaction.
type txExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
	QueryRow(query string, args ...any) *sql.Row
}

// RecordContainerObservation upserts the repository's container binding. A
// present observation replaces the binding; missing and unknown update health
// and freshness while retaining the stored identity (design §7). With no
// existing binding, missing and unknown record nothing.
func (s *Store) RecordContainerObservation(repositoryID string, obs ContainerObservation, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordContainer(tx, repositoryID, obs, now); err != nil {
		return err
	}
	return tx.Commit()
}

func recordContainer(tx txExecer, repositoryID string, obs ContainerObservation, now time.Time) error {
	if err := requireRepository(tx, repositoryID); err != nil {
		return err
	}
	switch obs.Health {
	case HealthPresent:
		if obs.ContainerID == "" {
			return errors.New("a present container observation must carry a container ID")
		}
		_, err := tx.Exec(`
			INSERT INTO container_bindings
				(repository_id, kind, container_id, container_user, workdir, health, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(repository_id) DO UPDATE SET
				kind           = excluded.kind,
				container_id   = excluded.container_id,
				container_user = excluded.container_user,
				workdir        = excluded.workdir,
				health         = excluded.health,
				observed_at    = excluded.observed_at`,
			repositoryID, obs.Kind, obs.ContainerID, obs.ContainerUser,
			obs.Workdir, string(obs.Health), encodeTime(now))
		if err != nil {
			return fmt.Errorf("recording the container binding: %w", err)
		}
		return nil
	case HealthMissing, HealthUnknown:
		_, err := tx.Exec(
			"UPDATE container_bindings SET health = ?, observed_at = ? WHERE repository_id = ?",
			string(obs.Health), encodeTime(now), repositoryID)
		if err != nil {
			return fmt.Errorf("recording container health: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid container health %q", obs.Health)
	}
}

func requireRepository(tx txExecer, repositoryID string) error {
	var n int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM repositories WHERE id = ?", repositoryID).Scan(&n); err != nil {
		return fmt.Errorf("checking repository %s: %w", repositoryID, err)
	}
	if n == 0 {
		return fmt.Errorf("repository %s: %w", repositoryID, ErrNotFound)
	}
	return nil
}

func requireWorkspace(tx txExecer, workspaceID string) error {
	var n int
	if err := tx.QueryRow(
		"SELECT COUNT(*) FROM workspaces WHERE id = ?", workspaceID).Scan(&n); err != nil {
		return fmt.Errorf("checking workspace %s: %w", workspaceID, err)
	}
	if n == 0 {
		return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
	}
	return nil
}

// boundedSummary enforces the 4 KiB error-summary bound, trimming any
// UTF-8 rune the byte cut split.
func boundedSummary(s string) string {
	if len(s) <= MaxErrorSummaryBytes {
		return s
	}
	return strings.ToValidUTF8(s[:MaxErrorSummaryBytes], "")
}

// RecordOperation upserts the workspace's last operation outcome. The
// store sets finished_at from now.
func (s *Store) RecordOperation(workspaceID string, op Operation, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := recordOperation(tx, workspaceID, op, now); err != nil {
		return err
	}
	return tx.Commit()
}

func recordOperation(tx txExecer, workspaceID string, op Operation, now time.Time) error {
	if err := requireWorkspace(tx, workspaceID); err != nil {
		return err
	}
	var exit any
	if op.ExitStatus != nil {
		exit = *op.ExitStatus
	}
	_, err := tx.Exec(`
		INSERT INTO last_operations
			(workspace_id, operation, outcome, exit_status, error_summary, finished_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(workspace_id) DO UPDATE SET
			operation     = excluded.operation,
			outcome       = excluded.outcome,
			exit_status   = excluded.exit_status,
			error_summary = excluded.error_summary,
			finished_at   = excluded.finished_at`,
		workspaceID, op.Name, string(op.Outcome), exit,
		boundedSummary(op.ErrorSummary), encodeTime(now))
	if err != nil {
		return fmt.Errorf("recording the operation outcome: %w", err)
	}
	return nil
}

// CommitReconciliation is design §9 step 5 as one transaction: the applied
// digest (only when the desired configuration was fully applied), any
// container observation, and the operation outcome commit together. A nil
// AppliedDigest leaves applied_digest untouched, so a failed
// reconciliation never clears recorded drift.
func (s *Store) CommitReconciliation(workspaceID string, r ReconciliationResult, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if r.AppliedDigest != nil {
		res, err := tx.Exec(
			"UPDATE workspaces SET applied_digest = ?, updated_at = ? WHERE id = ?",
			*r.AppliedDigest, encodeTime(now), workspaceID)
		if err != nil {
			return fmt.Errorf("recording the applied digest: %w", err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("recording the applied digest: %w", err)
		}
		if affected == 0 {
			return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
		}
	}
	if r.Container != nil {
		// A reconciliation is a session's, but the container it observed is
		// the repository's. Looking the owner up here keeps the caller from
		// having to carry both IDs into a result it assembles per session.
		var repositoryID string
		err := tx.QueryRow(
			"SELECT repository_id FROM workspaces WHERE id = ?", workspaceID).Scan(&repositoryID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("reading workspace %s: %w", workspaceID, err)
		}
		if err := recordContainer(tx, repositoryID, *r.Container, now); err != nil {
			return err
		}
	}
	if err := recordOperation(tx, workspaceID, r.Operation, now); err != nil {
		return err
	}
	return tx.Commit()
}

// AdoptSessionName records a live session's observed name as the
// workspace's actual session inside one transaction. The UNIQUE
// constraint still governs: a name recorded for another workspace is a
// typed conflict, never an overwrite. Re-adopting the workspace's own
// current name is a no-op; adopting over a stale assignment repairs the
// record to match reality (design §9 crash recovery, §13 step 7
// adoption).
func (s *Store) AdoptSessionName(workspaceID, name string, now time.Time) error {
	if name == "" {
		return fmt.Errorf("adopting an empty session name for workspace %s", workspaceID)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var current sql.NullString
	err = tx.QueryRow(
		"SELECT actual_session FROM workspaces WHERE id = ?",
		workspaceID).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("workspace %s: %w", workspaceID, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("reading workspace %s: %w", workspaceID, err)
	}
	if current.Valid && current.String == name {
		return tx.Commit()
	}

	_, err = tx.Exec(
		"UPDATE workspaces SET actual_session = ?, updated_at = ? WHERE id = ?",
		name, encodeTime(now), workspaceID)
	if isUniqueViolation(err) {
		return &SessionNameConflictError{Name: name}
	}
	if err != nil {
		return fmt.Errorf("adopting session name %q: %w", name, err)
	}
	return tx.Commit()
}
