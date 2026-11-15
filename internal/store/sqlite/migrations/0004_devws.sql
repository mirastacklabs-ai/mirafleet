-- Schema v4 (docs/M7.md section 4.1): host-mode executor and central-held credentials.
-- Times are INTEGER unix milliseconds UTC. Only sealed ciphertext (AES-256-GCM, sealed by
-- internal/secrets) is stored; the key never enters the database, only its key-check value.

-- Rebuild fleet_workspace so the executor may be 'host'. A host fleet has no image allowlist and no
-- resource limits (the VM is the boundary), so those checks hold for every other executor only.
CREATE TABLE fleet_workspace_new (
    fleet_id      INTEGER PRIMARY KEY REFERENCES fleets (id) ON DELETE CASCADE,
    executor      TEXT    NOT NULL CHECK (executor IN ('containerd', 'opensandbox', 'host')),
    default_image TEXT    NOT NULL,
    images        TEXT    NOT NULL CHECK (json_valid(images)),
    pull          TEXT    NOT NULL CHECK (pull IN ('if-not-present', 'always', 'never')),
    network       TEXT    NOT NULL CHECK (network IN ('none', 'egress')),
    egress        TEXT    NOT NULL DEFAULT '[]' CHECK (json_valid(egress)),
    cpu_milli     INTEGER NOT NULL CHECK (executor = 'host' OR cpu_milli > 0),
    memory_mb     INTEGER NOT NULL CHECK (executor = 'host' OR memory_mb > 0),
    pids          INTEGER NOT NULL CHECK (executor = 'host' OR pids > 0),
    tmpfs_mb      INTEGER NOT NULL CHECK (executor = 'host' OR tmpfs_mb > 0),
    disk_mb       INTEGER NOT NULL CHECK (executor = 'host' OR disk_mb > 0),
    idle_ttl_s    INTEGER NOT NULL CHECK (idle_ttl_s > 0),
    max_ttl_s     INTEGER NOT NULL CHECK (max_ttl_s >= idle_ttl_s),
    exec_default_s INTEGER NOT NULL CHECK (exec_default_s > 0),
    exec_max_s    INTEGER NOT NULL CHECK (exec_max_s >= exec_default_s AND exec_max_s <= 86400),
    updated_at    INTEGER NOT NULL
);
INSERT INTO fleet_workspace_new SELECT * FROM fleet_workspace;
DROP TABLE fleet_workspace;
ALTER TABLE fleet_workspace_new RENAME TO fleet_workspace;

CREATE TABLE credentials (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT    NOT NULL UNIQUE,
    kind        TEXT    NOT NULL CHECK (kind IN ('ssh-key', 'https-token', 'registry')),
    hosts       TEXT    NOT NULL CHECK (json_valid(hosts) AND json_type(hosts) = 'array' AND json_array_length(hosts) > 0),
    meta        TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(meta) AND json_type(meta) = 'object'),
    fingerprint TEXT    NOT NULL DEFAULT '',
    nonce       BLOB    NOT NULL CHECK (length(nonce) = 12),
    ciphertext  BLOB    NOT NULL CHECK (length(ciphertext) > 0),
    version     INTEGER NOT NULL CHECK (version >= 1),   -- part of the AEAD associated data
    created_at  INTEGER NOT NULL,
    rotated_at  INTEGER,                                 -- last value replacement, NULL until the first
    expires_at  INTEGER
);

-- A credential goes to one runner or to a whole fleet, never both in one row.
CREATE TABLE credential_assignments (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    credential_id INTEGER NOT NULL REFERENCES credentials (id) ON DELETE CASCADE,
    runner_id     INTEGER REFERENCES runners (id) ON DELETE CASCADE,
    fleet_id      INTEGER REFERENCES fleets (id) ON DELETE CASCADE,
    created_at    INTEGER NOT NULL,
    CHECK ((runner_id IS NULL) <> (fleet_id IS NULL))
);

CREATE UNIQUE INDEX credential_assign_runner ON credential_assignments (credential_id, runner_id) WHERE runner_id IS NOT NULL;
CREATE UNIQUE INDEX credential_assign_fleet  ON credential_assignments (credential_id, fleet_id)  WHERE fleet_id IS NOT NULL;
CREATE INDEX credential_assign_runner_id ON credential_assignments (runner_id) WHERE runner_id IS NOT NULL;
CREATE INDEX credential_assign_fleet_id  ON credential_assignments (fleet_id)  WHERE fleet_id IS NOT NULL;

-- HMAC of a constant under the secrets key: lets central detect a missing or wrong key.
CREATE TABLE secrets_meta (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    key_check  BLOB    NOT NULL CHECK (length(key_check) > 0),
    updated_at INTEGER NOT NULL
);
