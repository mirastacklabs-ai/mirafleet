-- Schema v1. Times are INTEGER unix milliseconds UTC.
-- AUTOINCREMENT everywhere: ids are never reused (revoke tombstones rely on it).

CREATE TABLE fleets (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT    NOT NULL UNIQUE,
    mode       TEXT    NOT NULL CHECK (mode IN ('actions', 'workspace')),
    confirm    INTEGER NOT NULL DEFAULT 0 CHECK (confirm IN (0, 1)),
    created_at INTEGER NOT NULL
);

CREATE TABLE join_tokens (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    token_id     TEXT    NOT NULL UNIQUE,
    secret_hash  BLOB    NOT NULL CHECK (length(secret_hash) = 32),
    fleet_id     INTEGER NOT NULL REFERENCES fleets (id) ON DELETE CASCADE,
    labels       TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(labels)),
    auto_approve INTEGER NOT NULL DEFAULT 0 CHECK (auto_approve IN (0, 1)),
    max_uses     INTEGER NOT NULL CHECK (max_uses >= 1),
    uses         INTEGER NOT NULL DEFAULT 0 CHECK (uses >= 0 AND uses <= max_uses),
    expires_at   INTEGER NOT NULL,
    revoked_at   INTEGER,
    created_by   TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL
);

CREATE TABLE runners (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    fleet_id      INTEGER NOT NULL REFERENCES fleets (id) ON DELETE RESTRICT,
    name          TEXT    NOT NULL,
    pubkey        BLOB    NOT NULL UNIQUE,
    fingerprint   TEXT    NOT NULL,
    status        TEXT    NOT NULL CHECK (status IN ('pending', 'active', 'disabled', 'revoked')),
    labels        TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(labels)),
    join_token_id INTEGER REFERENCES join_tokens (id) ON DELETE SET NULL,
    info          TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(info)),
    created_at    INTEGER NOT NULL,
    updated_at    INTEGER NOT NULL,
    last_seen_at  INTEGER
);

-- Revoked names are reusable; revoked keys never re-enrol (pubkey stays UNIQUE).
CREATE UNIQUE INDEX runners_fleet_name_live ON runners (fleet_id, name) WHERE status <> 'revoked';
CREATE INDEX runners_fleet ON runners (fleet_id);

CREATE TABLE operators (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    name         TEXT    NOT NULL,
    pubkey       BLOB    NOT NULL UNIQUE,
    fingerprint  TEXT    NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('active', 'revoked')),
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    last_seen_at INTEGER
);

CREATE UNIQUE INDEX operators_name_live ON operators (name) WHERE status <> 'revoked';

CREATE TABLE settings (
    key   TEXT PRIMARY KEY,
    value BLOB NOT NULL
);

-- Append-only: denormalised names, no foreign keys, rows survive what they describe.
CREATE TABLE audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          INTEGER NOT NULL,
    source      TEXT    NOT NULL DEFAULT '',
    actor_kind  TEXT    NOT NULL DEFAULT '',
    actor       TEXT    NOT NULL DEFAULT '',
    remote      TEXT    NOT NULL DEFAULT '',
    fleet       TEXT    NOT NULL DEFAULT '',
    runner      TEXT    NOT NULL DEFAULT '',
    subject     TEXT    NOT NULL DEFAULT '',
    action      TEXT    NOT NULL DEFAULT '',
    params      TEXT    CHECK (params IS NULL OR json_valid(params)),
    outcome     TEXT    NOT NULL DEFAULT '',
    exit_code   INTEGER,
    duration_ms INTEGER NOT NULL DEFAULT 0,
    message     TEXT    NOT NULL DEFAULT '',
    request_id  TEXT    NOT NULL DEFAULT ''
);

CREATE INDEX audit_ts ON audit (ts);
CREATE INDEX audit_fleet_runner ON audit (fleet, runner);

CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;

CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
BEGIN
    SELECT RAISE(ABORT, 'audit is append-only');
END;
