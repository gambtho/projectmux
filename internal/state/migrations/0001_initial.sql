-- Schema version 1: current operational metadata only (design §7).
-- No event stream, no history. Timestamps are RFC3339Nano UTC text
-- supplied by the application.

CREATE TABLE workspaces (
    id               TEXT PRIMARY KEY,
    slug             TEXT NOT NULL,
    worktree         TEXT NOT NULL UNIQUE,
    is_primary       INTEGER NOT NULL CHECK (is_primary IN (0, 1)),
    proposed_session TEXT NOT NULL,
    actual_session   TEXT UNIQUE,
    desired_digest   TEXT,
    applied_digest   TEXT,
    registered_at    TEXT NOT NULL,
    updated_at       TEXT NOT NULL
);

-- A row exists only once a binding has been recorded: no row means no
-- binding has ever existed, and health is non-null whenever one does.
-- health=missing or health=unknown never clears the identity columns;
-- only a successful replacement overwrites them.
CREATE TABLE container_bindings (
    workspace_id   TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL,
    container_id   TEXT NOT NULL,
    container_user TEXT,
    workdir        TEXT,
    health         TEXT NOT NULL CHECK (health IN ('present', 'missing', 'unknown')),
    observed_at    TEXT NOT NULL
);

CREATE TABLE last_operations (
    workspace_id  TEXT PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    operation     TEXT NOT NULL,
    outcome       TEXT NOT NULL CHECK (outcome IN ('ok', 'failed')),
    exit_status   INTEGER,
    error_summary TEXT,
    finished_at   TEXT NOT NULL
);
