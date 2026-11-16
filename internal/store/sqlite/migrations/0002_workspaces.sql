-- Schema v2: workspace mode.

CREATE TABLE fleet_workspace (
    fleet_id      INTEGER PRIMARY KEY REFERENCES fleets (id) ON DELETE CASCADE,
    executor      TEXT    NOT NULL CHECK (executor IN ('containerd', 'opensandbox')),
    default_image TEXT    NOT NULL,
    images        TEXT    NOT NULL CHECK (json_valid(images)),
    pull          TEXT    NOT NULL CHECK (pull IN ('if-not-present', 'always', 'never')),
    network       TEXT    NOT NULL CHECK (network IN ('none', 'egress')),
    egress        TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(egress)),
    cpu_milli     INTEGER NOT NULL CHECK (cpu_milli > 0),
    memory_mb     INTEGER NOT NULL CHECK (memory_mb > 0),
    pids          INTEGER NOT NULL CHECK (pids > 0),
    tmpfs_mb      INTEGER NOT NULL CHECK (tmpfs_mb > 0),
    disk_mb       INTEGER NOT NULL CHECK (disk_mb > 0),
    idle_ttl_s    INTEGER NOT NULL CHECK (idle_ttl_s > 0),
    max_ttl_s     INTEGER NOT NULL CHECK (max_ttl_s >= idle_ttl_s),
    exec_default_s INTEGER NOT NULL CHECK (exec_default_s > 0),
    exec_max_s    INTEGER NOT NULL CHECK (exec_max_s >= exec_default_s AND exec_max_s <= 86400),
    updated_at    INTEGER NOT NULL
);

CREATE TABLE workspaces (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    ws_id        TEXT    NOT NULL UNIQUE CHECK (length(ws_id) = 32),
    fleet_id     INTEGER NOT NULL REFERENCES fleets (id) ON DELETE RESTRICT,
    runner_id    INTEGER NOT NULL REFERENCES runners (id) ON DELETE RESTRICT,
    operator_id  INTEGER NOT NULL REFERENCES operators (id) ON DELETE RESTRICT,
    session      TEXT    NOT NULL,
    image        TEXT    NOT NULL,
    spec         TEXT    NOT NULL CHECK (json_valid(spec)),
    state        TEXT    NOT NULL CHECK (state IN ('opening', 'open', 'closing', 'closed', 'failed', 'lost')),
    reason       TEXT    NOT NULL DEFAULT '',
    created_at   INTEGER NOT NULL,
    last_used_at INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL,
    closed_at    INTEGER
);

-- One live workspace per (operator, fleet, session): sticky sessions.
CREATE UNIQUE INDEX workspaces_session_live ON workspaces (operator_id, fleet_id, session)
    WHERE state IN ('opening', 'open');
CREATE INDEX workspaces_runner_live ON workspaces (runner_id)
    WHERE state IN ('opening', 'open', 'closing');
CREATE INDEX workspaces_idle ON workspaces (last_used_at) WHERE state = 'open';

-- The append-only triggers (audit_no_update, audit_no_delete) from v1 stay in force.
ALTER TABLE audit ADD COLUMN workspace TEXT NOT NULL DEFAULT '';
CREATE INDEX audit_workspace ON audit (workspace) WHERE workspace <> '';
