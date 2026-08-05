package state

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gambtho/projectmux/internal/resolve"
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
