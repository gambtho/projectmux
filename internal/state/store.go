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

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// RegisterWorkspace upserts the workspace identity. Re-registration
// refreshes everything derivable from resolution and configuration while
// preserving registered_at, the assigned session name, the applied digest,
// and any binding — rebuilding the database is simply re-running
// registration (design §7).
func (s *Store) RegisterWorkspace(ws resolve.Workspace, desiredDigest string, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()

	_, err = tx.Exec(`
		INSERT INTO workspaces
			(id, slug, worktree, is_primary, proposed_session,
			 desired_digest, registered_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			slug             = excluded.slug,
			worktree         = excluded.worktree,
			is_primary       = excluded.is_primary,
			proposed_session = excluded.proposed_session,
			desired_digest   = excluded.desired_digest,
			updated_at       = excluded.updated_at`,
		ws.ID, ws.Slug, ws.Worktree, boolInt(ws.IsPrimary), ws.SessionName,
		desiredDigest, encodeTime(now), encodeTime(now))
	if err != nil {
		return fmt.Errorf("registering workspace %s: %w", ws.ID, err)
	}
	return tx.Commit()
}

const selectRecord = `
SELECT
	w.id, w.slug, w.worktree, w.is_primary, w.proposed_session,
	w.actual_session, w.desired_digest, w.applied_digest,
	w.registered_at, w.updated_at,
	b.kind, b.container_id, b.container_user, b.workdir, b.health, b.observed_at,
	o.operation, o.outcome, o.exit_status, o.error_summary, o.finished_at
FROM workspaces w
LEFT JOIN container_bindings b ON b.workspace_id = w.id
LEFT JOIN last_operations o ON o.workspace_id = w.id`

// Workspace returns the joined record for one workspace, or ErrNotFound.
func (s *Store) Workspace(id string) (Record, error) {
	rec, err := scanRecord(s.db.QueryRow(selectRecord+" WHERE w.id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, fmt.Errorf("workspace %s: %w", id, ErrNotFound)
	}
	if err != nil {
		return Record{}, fmt.Errorf("reading workspace %s: %w", id, err)
	}
	return rec, nil
}

// Workspaces returns every registered workspace ordered by slug, then
// worktree.
func (s *Store) Workspaces() ([]Record, error) {
	rows, err := s.db.Query(selectRecord + " ORDER BY w.slug, w.worktree")
	if err != nil {
		return nil, fmt.Errorf("listing workspaces: %w", err)
	}
	defer rows.Close()

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

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRecord(r rowScanner) (Record, error) {
	var (
		rec                 Record
		isPrimary           int
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
		&rec.ID, &rec.Slug, &rec.Worktree, &isPrimary, &rec.ProposedSession,
		&actual, &desired, &applied, &registered, &updated,
		&cKind, &cID, &cUser, &cWorkdir, &cHealth, &cObserved,
		&oName, &oOutcome, &oExit, &oSummary, &oFinished)
	if err != nil {
		return Record{}, err
	}

	rec.IsPrimary = isPrimary == 1
	rec.ActualSession = nullable(actual)
	rec.DesiredDigest = nullable(desired)
	rec.AppliedDigest = nullable(applied)
	if rec.RegisteredAt, err = decodeTime(registered); err != nil {
		return Record{}, fmt.Errorf("registered_at: %w", err)
	}
	if rec.UpdatedAt, err = decodeTime(updated); err != nil {
		return Record{}, fmt.Errorf("updated_at: %w", err)
	}

	if cKind.Valid {
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
	defer tx.Rollback()

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

// RecordContainerObservation upserts the container binding. A present
// observation replaces the binding; missing and unknown update health and
// freshness while retaining the stored identity (design §7). With no
// existing binding, missing and unknown record nothing.
func (s *Store) RecordContainerObservation(workspaceID string, obs ContainerObservation, now time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning a transaction: %w", err)
	}
	defer tx.Rollback()
	if err := recordContainer(tx, workspaceID, obs, now); err != nil {
		return err
	}
	return tx.Commit()
}

func recordContainer(tx txExecer, workspaceID string, obs ContainerObservation, now time.Time) error {
	if err := requireWorkspace(tx, workspaceID); err != nil {
		return err
	}
	switch obs.Health {
	case HealthPresent:
		if obs.ContainerID == "" {
			return errors.New("a present container observation must carry a container ID")
		}
		_, err := tx.Exec(`
			INSERT INTO container_bindings
				(workspace_id, kind, container_id, container_user, workdir, health, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(workspace_id) DO UPDATE SET
				kind           = excluded.kind,
				container_id   = excluded.container_id,
				container_user = excluded.container_user,
				workdir        = excluded.workdir,
				health         = excluded.health,
				observed_at    = excluded.observed_at`,
			workspaceID, obs.Kind, obs.ContainerID, obs.ContainerUser,
			obs.Workdir, string(obs.Health), encodeTime(now))
		if err != nil {
			return fmt.Errorf("recording the container binding: %w", err)
		}
		return nil
	case HealthMissing, HealthUnknown:
		_, err := tx.Exec(
			"UPDATE container_bindings SET health = ?, observed_at = ? WHERE workspace_id = ?",
			string(obs.Health), encodeTime(now), workspaceID)
		if err != nil {
			return fmt.Errorf("recording container health: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("invalid container health %q", obs.Health)
	}
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
	defer tx.Rollback()
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
	defer tx.Rollback()

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
		if err := recordContainer(tx, workspaceID, *r.Container, now); err != nil {
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
	defer tx.Rollback()

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
