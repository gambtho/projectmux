-- Schema version 2: the repository, not the worktree, is what a container is
-- keyed on, and a repository has many sessions (design §5.2).
--
-- This migration is pure SQL by design (design §9). Telling a main worktree
-- from a linked one requires asking git, and a schema migration that depended
-- on the filesystem and on git's exit status would abort an upgrade because a
-- directory was deleted between runs. Every stored worktree therefore becomes
-- a repository verbatim; `rebuild` collapses the rows whose path is really a
-- linked worktree and drops the rows whose path is gone. The intermediate
-- state is over-counted, never wrong.
--
-- IDs are carried over rather than recomputed: the new derivation is a SHA-256
-- SQLite has no function for. They are stale, not invalid, and the first
-- registration of a repository re-keys its row (see RegisterWorkspace).

-- The rebuild goes through unconstrained copies rather than the usual
-- create-and-rename. Dropping a table that foreign keys point at runs an
-- implicit DELETE, so dropping `workspaces` while any child still references
-- it would cascade the children away — and PRAGMA foreign_keys cannot be
-- turned off here, because it is a no-op inside a transaction and migrations
-- run inside one (migrate.go:57).
CREATE TABLE m0002_workspaces AS SELECT * FROM workspaces;
CREATE TABLE m0002_container_bindings AS SELECT * FROM container_bindings;
CREATE TABLE m0002_last_operations AS SELECT * FROM last_operations;

DROP TABLE container_bindings;
DROP TABLE last_operations;
DROP TABLE workspaces;

CREATE TABLE repositories (
    id            TEXT PRIMARY KEY,
    slug          TEXT NOT NULL,
    repo_root     TEXT NOT NULL UNIQUE,
    registered_at TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- slug lives on the repository because it is a property of the repository;
-- storing it per session would let two sessions on one repository disagree.
CREATE TABLE workspaces (
    id               TEXT PRIMARY KEY,
    repository_id    TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    session          TEXT NOT NULL DEFAULT '',
    proposed_session TEXT NOT NULL,
    actual_session   TEXT UNIQUE,
    desired_digest   TEXT,
    applied_digest   TEXT,
    registered_at    TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    UNIQUE (repository_id, session)
);

-- A row exists only once a binding has been recorded: no row means no
-- binding has ever existed, and health is non-null whenever one does.
-- health=missing or health=unknown never clears the identity columns;
-- only a successful replacement overwrites them.
CREATE TABLE container_bindings (
    repository_id  TEXT PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    container_id   TEXT NOT NULL,
    container_user TEXT,
    workdir        TEXT,
    health         TEXT NOT NULL CHECK (health IN ('present', 'missing', 'unknown')),
    observed_at    TEXT NOT NULL
);

-- last_operations stays keyed on workspace_id: an operation is performed by a
-- session, not by a repository.
--
-- ON UPDATE CASCADE is what carries this row along when step 9 re-keys a
-- migrated workspace. Foreign keys are enforced here: Open puts
-- _pragma=foreign_keys(1) on the DSN so every pooled connection has them on
-- (internal/state/state.go:65), and migrate_test.go:152-156 asserts it per
-- connection. The declaration is therefore load-bearing, not documentation.
--
-- Step 9 still writes its cleanup out statement by statement rather than
-- leaning on ON DELETE CASCADE. That is a readability choice, not a
-- correctness one: a reader of RegisterWorkspace should be able to see which
-- rows a stale repository takes with it without reconstructing the schema
-- from memory, and the explicit order is the same order the cascade would
-- have used.
CREATE TABLE last_operations (
    workspace_id  TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE ON UPDATE CASCADE,
    operation     TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN ('ok', 'failed')),
    exit_status   INTEGER,
    error_summary TEXT,
    finished_at   TEXT NOT NULL
);

INSERT INTO repositories (id, slug, repo_root, registered_at, updated_at)
SELECT id, slug, worktree, registered_at, updated_at FROM m0002_workspaces;

-- Every migrated row becomes the default session of its repository. is_primary
-- is not carried: every repository row is a main worktree by construction, so
-- the flag would always be true.
INSERT INTO workspaces
    (id, repository_id, session, proposed_session, actual_session,
     desired_digest, applied_digest, registered_at, updated_at)
SELECT id, id, '', proposed_session, actual_session,
       desired_digest, applied_digest, registered_at, updated_at
FROM m0002_workspaces;

-- One binding per repository, keeping the most recently observed one. The
-- grouping is defensive today — 0001 made workspaces.worktree UNIQUE, so the
-- mapping is one-to-one — but the tie-break is stated rather than left to scan
-- order, because `rebuild` collapses siblings onto one repository next. The
-- non-aggregated columns come from the MAX row: SQLite defines bare columns
-- that way when the query uses exactly one min/max aggregate.
INSERT INTO container_bindings
    (repository_id, kind, container_id, container_user, workdir, health, observed_at)
SELECT w.repository_id, b.kind, b.container_id, b.container_user, b.workdir,
       b.health, MAX(b.observed_at)
FROM m0002_container_bindings b
JOIN workspaces w ON w.id = b.workspace_id
GROUP BY w.repository_id;

INSERT INTO last_operations
    (workspace_id, operation, outcome, exit_status, error_summary, finished_at)
SELECT workspace_id, operation, outcome, exit_status, error_summary, finished_at
FROM m0002_last_operations;

DROP TABLE m0002_container_bindings;
DROP TABLE m0002_last_operations;
DROP TABLE m0002_workspaces;
