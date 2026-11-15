-- Schema v3 (docs/M4.md section 2.9): admin GUI. Times are INTEGER unix milliseconds UTC.
-- Only SHA-256 hashes of the setup token and of session tokens are stored.

CREATE TABLE gui_admins (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    username    TEXT    NOT NULL,
    pw_hash     TEXT    NOT NULL,
    generation  INTEGER NOT NULL DEFAULT 1 CHECK (generation >= 1),
    status      TEXT    NOT NULL CHECK (status IN ('active', 'reset')),
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL
);

-- At most one active admin.
CREATE UNIQUE INDEX gui_admins_one_active ON gui_admins (status) WHERE status = 'active';

CREATE TABLE gui_setup (
    id          INTEGER PRIMARY KEY CHECK (id = 1),
    token_hash  BLOB    NOT NULL CHECK (length(token_hash) = 32),
    expires_at  INTEGER NOT NULL,
    created_at  INTEGER NOT NULL
);

CREATE TABLE gui_sessions (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    token_hash     BLOB    NOT NULL UNIQUE CHECK (length(token_hash) = 32),
    admin_id       INTEGER NOT NULL REFERENCES gui_admins (id) ON DELETE CASCADE,
    generation     INTEGER NOT NULL CHECK (generation >= 1),
    created_at     INTEGER NOT NULL,
    last_active_at INTEGER NOT NULL,
    expires_at     INTEGER NOT NULL,   -- absolute expiry (created + 12 h)
    remote         TEXT    NOT NULL DEFAULT '',
    user_agent     TEXT    NOT NULL DEFAULT '' CHECK (length(user_agent) <= 128)
);

CREATE INDEX gui_sessions_admin   ON gui_sessions (admin_id);
CREATE INDEX gui_sessions_expires ON gui_sessions (expires_at);

CREATE INDEX audit_actor   ON audit (actor);
CREATE INDEX audit_request ON audit (request_id) WHERE request_id <> '';
