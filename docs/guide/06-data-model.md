# 6. Data model

## What you will learn

- Every table and column of central's SQLite database: its type, unit, constraints and why it is
  there.
- How runners, operators and join tokens move through their states, and which transitions the
  code allows.
- How the append-only audit table works, with real example rows.
- How the database is opened safely, migrated, and kept consistent (one writer, many readers,
  `synchronous=FULL`).
- What the `Store` interface promises about atomicity and errors (summary here, every method in
  the appendix at the end).

Each new database word is explained where it first appears, and the main ones are also in the
[glossary](glossary.md). Quick versions: a **transaction** is a group of changes that all
happen or none do; a **foreign key** is a column that must point at a row in another table; a
**tombstone** (here) is an in-memory "this id is dead" marker. M1 and M2 are done (13 Nov 2026);
this chapter describes the M2 schema. The code lives in `internal/store` (the interface, pure Go with no SQL)
and `internal/store/sqlite` (the implementation). Central is the only component with a
database; runners keep a small local JSON-lines audit file and a `runner.yaml`, nothing more.

## 6.1 Why SQLite, and why behind an interface

Central is **one process on one host** in the open-source core, so it does not need a database
server. **SQLite** is a database in a single file, used through a library. We use the pure-Go
driver `modernc.org/sqlite`, which keeps the binary static (`CGO_ENABLED=0`).

Nobody outside `internal/store/sqlite` writes SQL. The rest of central talks to the
**`Store` interface**, whose methods are *domain-level* ("enrol this runner using this token"),
not generic ("run this SQL"). Two reasons:

1. **Atomicity lives inside the method.** "Consume one use of the token *and* insert the runner"
   must be one all-or-nothing step. A generic `WithTx(func(tx))` would let callers forget that.
2. **A different database can be swapped in later** (the enterprise plan mentions Postgres for
   high availability) by implementing the same interface; `internal/store/storetest` is the
   shared test-suite any implementation must pass.

Rejected alternatives (from `docs/M2.md` section 1.1): a generic `WithTx` (leaks SQL, harder
fakes); `goose`/`golang-migrate` (new dependencies); ad-hoc DDL at start-up (no versioning).

## 6.2 The tables at a glance

```mermaid
erDiagram
    fleets ||--o{ runners : "fleet_id (RESTRICT)"
    fleets ||--o{ join_tokens : "fleet_id (CASCADE)"
    join_tokens |o--o{ runners : "join_token_id (SET NULL)"

    fleets {
        integer id PK
        text name UK
        text mode "actions | workspace"
        integer confirm "0 or 1"
        integer created_at "unix ms"
    }
    runners {
        integer id PK
        integer fleet_id FK
        text name "unique per fleet while not revoked"
        blob pubkey UK "SSH wire format"
        text fingerprint
        text status "pending active disabled revoked"
        text labels "JSON object"
        integer join_token_id FK "nullable"
        text info "JSON object"
        integer created_at "unix ms"
        integer updated_at "unix ms"
        integer last_seen_at "unix ms, nullable"
    }
    join_tokens {
        integer id PK
        text token_id UK "16 hex"
        blob secret_hash "SHA-256, 32 bytes"
        integer fleet_id FK
        text labels "JSON object"
        integer auto_approve "0 or 1"
        integer max_uses "at least 1"
        integer uses "0 to max_uses"
        integer expires_at "unix ms"
        integer revoked_at "unix ms, nullable"
        text created_by
        integer created_at "unix ms"
    }
    operators {
        integer id PK
        text name "unique while not revoked"
        blob pubkey UK "SSH wire format"
        text fingerprint
        text status "active revoked"
        integer created_at "unix ms"
        integer updated_at "unix ms"
        integer last_seen_at "unix ms, nullable"
    }
    settings {
        text key PK
        blob value
    }
    audit {
        integer id PK
        integer ts "unix ms"
        text source
        text actor_kind
        text actor
        text remote
        text fleet "name, not an id"
        text runner "name, not an id"
        text subject
        text action
        text params "JSON, nullable"
        text outcome
        integer exit_code "nullable"
        integer duration_ms
        text message
        text request_id
    }
```

*Look at: `operators`, `settings` and `audit` have no lines. They are not linked by foreign keys.
`audit` is deliberately disconnected so history outlives what it describes.*

What the three words in brackets mean. They say what the database does when someone deletes the
row a foreign key points at (the "parent"):

- **RESTRICT:** refuse the delete. A fleet that still has runners (even revoked ones) cannot be
  deleted.
- **CASCADE:** delete the dependent rows too. Deleting a fleet would delete its join tokens.
- **SET NULL:** keep the dependent row, but blank the pointer. Deleting a token leaves its
  runners, with `join_token_id` empty.

(The current code never deletes fleets or tokens; these rules are a safety net.)

### Conventions used by every table

- **Times are `INTEGER` unix milliseconds, UTC.** `1794562867512` means 2026-11-13T09:41:07.512Z.
  Go converts with `time.UnixMilli` (see `fromMS` in `runners.go`); nothing in the database
  depends on a time zone. The audit table uses the same unit.
- **`NULL` means "not set"**, used for `last_seen_at`, `revoked_at`, `join_token_id`,
  `audit.params` and `audit.exit_code`. In Go a zero `time.Time` stands for `NULL`.
- **Every primary key is `INTEGER PRIMARY KEY AUTOINCREMENT`.** Plain SQLite may reuse the id
  of a deleted row. `AUTOINCREMENT` never does. The revoke design depends on this: central keeps
  an in-memory "tombstone" of revoked runner ids, and an id that could be recycled by a new runner
  would put the tombstone on the wrong host.
- **JSON columns** (`labels`, `info`, `params`) are `TEXT` with a `CHECK (json_valid(...))`. They
  hold flat `{"key":"value"}` string maps. The database refuses malformed JSON; Go marshals and
  unmarshals with `marshalMap`/`unmarshalMap`.
- **Keys are stored as SSH wire-format `BLOB`s** (what `ssh.PublicKey.Marshal()` returns) next
  to a text `fingerprint` (`SHA256:...` as printed by `ssh-keygen -l`). The blob is what auth
  compares; the fingerprint is for humans and audit rows. Only `ssh-ed25519` ever gets here.

## 6.3 `fleets`

A fleet is a named group of runners and the unit of isolation.

| Column | Type | Meaning |
|---|---|---|
| `id` | `INTEGER` PK AUTOINCREMENT | Internal id; never shown to users. |
| `name` | `TEXT NOT NULL UNIQUE` | `dev`, `prod`. Regex `^[a-z0-9][a-z0-9._-]{0,62}$` (`store.FleetNameRE`), checked in Go before the insert. |
| `mode` | `TEXT NOT NULL`, `CHECK IN ('actions','workspace')` | `actions` runs only named actions (the only mode `action.run` accepts today). `workspace` is planned for [M3](../M3.md); central refuses `action.run` on it with `conflict: fleet is not in actions mode`. |
| `confirm` | `INTEGER NOT NULL DEFAULT 0`, `CHECK IN (0,1)` | SQLite has no boolean type; 0/1. `1` = **guarded fleet**: `action.run` must carry `confirm == fleet name` ([chapter 5](05-security.md#537-guarded-fleets-confirm-must-equal-the-fleet-name)). |
| `created_at` | `INTEGER NOT NULL` | Unix ms. |

Fleets are never updated or deleted by the current code; there is no `fleet.delete` method.
(`join_tokens.fleet_id` is `ON DELETE CASCADE`, so deleting a fleet by hand would also delete its
tokens; `runners.fleet_id` is `ON DELETE RESTRICT`, so a fleet that still has runners, even revoked
ones, cannot be deleted.)

## 6.4 `runners`

One row per enrolled host, **including revoked ones** (history).

| Column | Type | Meaning |
|---|---|---|
| `id` | PK | Used as the session key in the live registry and in `Principal.ID`. |
| `fleet_id` | `INTEGER NOT NULL` FK to `fleets(id)` `ON DELETE RESTRICT` | The one fleet this runner belongs to. |
| `name` | `TEXT NOT NULL` | `build-1`, `db-1`. Same regex as fleets. Unique **within the fleet while not revoked** (see index below). |
| `pubkey` | `BLOB NOT NULL UNIQUE` | The runner's Ed25519 key in SSH wire format. Globally unique, **including revoked rows**, so a revoked key can never be enrolled again. |
| `fingerprint` | `TEXT NOT NULL` | `SHA256:...` of the key. |
| `status` | `TEXT NOT NULL`, `CHECK IN ('pending','active','disabled','revoked')` | Lifecycle state, section 6.8. |
| `labels` | `TEXT NOT NULL DEFAULT '{}'`, `json_valid` | Copied from the join token at enrol time. **Trusted**: set by the admin who made the token. |
| `join_token_id` | `INTEGER` FK to `join_tokens(id)` `ON DELETE SET NULL` | Which token enrolled this runner, kept for forensics. `NULL` if the token row is later deleted. |
| `info` | `TEXT NOT NULL DEFAULT '{}'`, `json_valid` | `hostname`, `os`, `arch`, `version` from the last heartbeat, each clipped to 256 bytes; the JSON is capped at 16 KiB by `TouchRunner`. **Informational, untrusted.** |
| `created_at` / `updated_at` | `INTEGER NOT NULL` | `updated_at` changes on status changes only. |
| `last_seen_at` | `INTEGER`, nullable | Last time central saw the runner, written on connect, disconnect and at most every 5 minutes (`Config.TouchInterval`). Heartbeats themselves stay **in memory**; writing a row every 15 s per runner would be wasteful. |

### Indexes on `runners`

```sql
CREATE UNIQUE INDEX runners_fleet_name_live ON runners (fleet_id, name) WHERE status <> 'revoked';
CREATE INDEX runners_fleet ON runners (fleet_id);
```

- **`runners_fleet_name_live` is a *partial unique index*:** it covers only rows whose status is
  not `revoked`. Result: two *live* runners cannot share a name in a fleet, but once `db-1` is
  revoked a replacement can be enrolled as `db-1` again. A plain `UNIQUE(fleet_id, name)` would
  block that forever and force admins to invent `db-1b`. Because only the key is unique over
  *all* rows, the old key stays dead.
- `runners_fleet` speeds up "list the runners of a fleet" and the runner count per fleet.
- `pubkey UNIQUE` creates an implicit index, used by `GetRunnerByKey` on every SSH login.

## 6.5 `join_tokens`

| Column | Type | Meaning |
|---|---|---|
| `id` | PK | Internal; referenced by `runners.join_token_id`. |
| `token_id` | `TEXT NOT NULL UNIQUE` | The public 16-hex-character id (8 random bytes). Appears in the token string, in audit rows (`subject`) and in `token list`. Not secret. |
| `secret_hash` | `BLOB NOT NULL`, `CHECK (length(secret_hash) = 32)` | SHA-256 of the 32-byte secret. The secret itself is never stored. |
| `fleet_id` | `INTEGER NOT NULL` FK to `fleets(id)` `ON DELETE CASCADE` | Fleet the joining runner enters. |
| `labels` | JSON text | Labels given to runners that join with this token. |
| `auto_approve` | `INTEGER` 0/1 | `1` = the runner starts `active` instead of `pending` (`token create -approve`). |
| `max_uses` | `INTEGER NOT NULL`, `CHECK (max_uses >= 1)` | Default 1, at most 1000 (service limit). Never "unlimited". |
| `uses` | `INTEGER NOT NULL DEFAULT 0`, `CHECK (uses >= 0 AND uses <= max_uses)` | Joins so far. The `CHECK` is a backstop: even a bug cannot push `uses` past `max_uses`. |
| `expires_at` | `INTEGER NOT NULL` | Unix ms. Default now + 24 h, at most now + 30 days. |
| `revoked_at` | `INTEGER`, nullable | Set by `token revoke`. |
| `created_by` | `TEXT NOT NULL DEFAULT ''` | Admin principal name, for example `uid:1000`. |
| `created_at` | `INTEGER NOT NULL` | Unix ms. |

A token has no `status` column. Its state is **derived** from columns at the moment of use, in one
conditional UPDATE (`store.Enroll`):

```sql
UPDATE join_tokens SET uses = uses + 1
 WHERE id = ? AND revoked_at IS NULL AND expires_at > ? AND uses < max_uses
```

If that affects one row, the use is consumed in the same transaction that inserts the runner.
If it affects zero rows the token is stale (`store.ErrStale`). Because SQLite runs one writer at a
time and the UPDATE checks and increments in a single statement, 20 goroutines racing for a
single-use token produce exactly one runner (the `EnrollRaceSingleUse` test in `storetest` does exactly this).

## 6.6 `operators`

| Column | Type | Meaning |
|---|---|---|
| `id` | PK | `Principal.ID` for operators. |
| `name` | `TEXT NOT NULL` | `alice`, `bob`. Unique while not revoked (index below). |
| `pubkey` | `BLOB NOT NULL UNIQUE` | Ed25519 key, SSH wire format. Unique over all rows: a revoked operator's key is dead. |
| `fingerprint` | `TEXT NOT NULL` | `SHA256:...`. |
| `status` | `TEXT`, `CHECK IN ('active','revoked')` | There is **no `pending` state**: registering with a valid code makes the operator `active` immediately. |
| `created_at`, `updated_at`, `last_seen_at` | `INTEGER` | `last_seen_at` is written when an operator connects. |

```sql
CREATE UNIQUE INDEX operators_name_live ON operators (name) WHERE status <> 'revoked';
```

Same trick as runners: a revoked `alice` frees the name, so Alice can register again from a new
laptop with a new key.

## 6.7 `settings`

A tiny key/value table.

| Column | Type | Meaning |
|---|---|---|
| `key` | `TEXT PRIMARY KEY` | Today only `registration_secret` (`store.SettingRegistrationSecret`). |
| `value` | `BLOB NOT NULL` | For the registration secret: 32 random bytes. |

- `CreateSetting` inserts and returns `ErrConflict` if the key exists. `central init` uses it, so
  running init twice **cannot** replace the secret.
- `PutSetting` is an upsert and is used only by `operator rotate-secret`.
- Both write their audit row in the same transaction (`operator.init_secret`,
  `operator.rotate_secret`). The value is never in the audit row or any log.
- `checkSetting` rejects an empty key or empty value (`ErrInvalid`).

The secret is stored in plaintext because central must compute codes from it. The protection is
the `0600` file in a `0700` directory ([chapter 5](05-security.md#5316-file-modes-umask-flock-and-the-admin-socket)).

## 6.8 State machines

### Runner

```mermaid
stateDiagram-v2
    [*] --> pending: enroll.join (token without approve)
    [*] --> active: enroll.join (token with approve)
    pending --> active: approve
    active --> disabled: disable
    disabled --> active: enable
    pending --> revoked: revoke
    active --> revoked: revoke
    disabled --> revoked: revoke
    revoked --> [*]
```

*Look at: `revoked` is terminal. `pending` cannot go to `disabled`; `disabled` cannot go back to
`pending`.*

The rules are one function, `store.CanTransition(from, to)`:

| From \ To | pending | active | disabled | revoked |
|---|---|---|---|---|
| pending | no | yes (approve) | no | yes |
| active | no | no | yes (disable) | yes |
| disabled | no | yes (enable) | no | yes |
| revoked | no | no | no | no |

What each state means to a *live* connection (`internal/central/registry`):

| Status | May it connect? | May it heartbeat? | May it run actions? |
|---|---|---|---|
| `pending` | yes | yes | **no**: `ErrPending` |
| `active` | yes | yes | yes |
| `disabled` | yes (visible) | yes | **no** for new work; in-flight work finishes |
| `revoked` | **no**: key rejected at auth | n/a | no |

Two layers enforce a transition, and both must agree: the service checks the `from` set and
`CanTransition` first (to give a good `conflict` message), and `SetRunnerStatus` checks again
**inside the transaction** against the row it just read, so a concurrent change cannot slip
through. `disable` and `enable` also update the registry (`SetStatus`); the registry remembers the
status per runner id even while the runner is offline and applies it at its next registration.

### Operator

```mermaid
stateDiagram-v2
    [*] --> active: enroll.register with the current code
    active --> active: register again with the same key and name (idempotent)
    active --> revoked: operator revoke (also kicks live connections)
    revoked --> [*]
```

*Look at: there is no way back from `revoked`. A person returns by registering a new key (the name
is free again), never by "un-revoking".*

Re-registering with the same key and name returns success (and is still audited with message
`already registered`). Re-registering a revoked key is `ErrConflict`, and so is using the name of
a different active operator, or a key that belongs to a runner.

### Join token (derived state)

```mermaid
stateDiagram-v2
    [*] --> fresh: token create (uses = 0)
    fresh --> partly_used: a join (0 < uses < max_uses)
    fresh --> used_up: a join (uses = max_uses)
    partly_used --> used_up: last join
    fresh --> expired: now >= expires_at
    partly_used --> expired: now >= expires_at
    fresh --> revoked: token revoke (revoked_at set)
    partly_used --> revoked: token revoke
    used_up --> [*]
    expired --> [*]
    revoked --> [*]
```

*Look at: no column named `status` exists. Only `uses`, `max_uses`, `expires_at`, `revoked_at`
decide, and they are checked together in the one UPDATE.* `JoinToken.Usable(now)` is a read-only
pre-check helper; `Enroll` re-checks atomically.

## 6.9 Connection pools and pragmas

`sqlite.Open` creates **two** `database/sql` pools on the same file.

| Pool | Size | Extra settings | Used for |
|---|---|---|---|
| **writer** | exactly 1 connection (`SetMaxOpenConns(1)`) | `_txlock=immediate` | every INSERT/UPDATE (all mutating `Store` methods), and `RecordAudit` |
| **reader** | 4 connections (`readerConns`) | `query_only(1)` | every `Get*`/`List*` |

Why not one shared pool? In **WAL mode** (write-ahead log: changes are appended to a separate
log file first, so readers keep reading the old data while a write happens) readers never block
the writer, and the writer never blocks readers: that is the whole point. But a transaction that starts as a *read* and then tries
to *write* can fail instantly with `SQLITE_BUSY` if another writer got in between ("read to write
upgrade"). Two separate pools plus `BEGIN IMMEDIATE` (take the write lock *at the start*) avoid
that: the single writer connection serialises writers inside the process, and every write
transaction begins by acquiring the write lock, so callers never see `SQLITE_BUSY`. One
connection for everything would instead make a slow list query stall every write.
`query_only(1)` makes it impossible for a reader connection to write by mistake.

The writer pool's DSN option `_txlock=immediate` is what makes Go's `database/sql` issue `BEGIN
IMMEDIATE` instead of the default `BEGIN` (which takes the lock lazily, at the first write).

### Pragmas (set per connection through the DSN)

| Pragma | Value | Why |
|---|---|---|
| `busy_timeout` | `5000` ms | Wait for a lock instead of failing at once. Listed first so even the WAL switch waits. |
| `foreign_keys` | `1` | SQLite ignores foreign keys unless asked, per connection. |
| `journal_mode` | `WAL` | Write-ahead log: readers and the writer do not block each other. |
| `trusted_schema` | `0` | Do not let functions or views embedded in the database file run unvetted code. |
| `synchronous` | **`FULL`** | See below. |
| `query_only` | `1` (readers only) | Readers cannot write. |

**Why `synchronous=FULL`.** In WAL mode the cheaper setting `NORMAL` does not force the log to
disk at every commit; after a power cut the last few commits can vanish (the database stays
consistent, but recent transactions are lost). Imagine: Dana revokes `db-1`, the CLI prints
"revoked", then the machine loses power. With `NORMAL`, after reboot `db-1` could be `active`
again, and the attacker we just kicked is back. `FULL` flushes on every commit, so **a revoke
that was acknowledged survives power loss**. The price is slower writes, which is fine because
central's write rate is low (heartbeats are not written).

### Another safety measure: the `ping` retry

When two processes create a *fresh* database at the same instant, switching to WAL needs an
exclusive lock that the busy handler does not always wait for. `pingRetry` retries briefly on
`SQLITE_BUSY`/`LOCKED`. (Normally the `flock` on
`central.lock` already keeps two centrals apart; the retry is a second line of defence.)

## 6.10 Migrations

Schema changes are **embedded SQL files** `internal/store/sqlite/migrations/NNNN_name.sql`
(`0001_init.sql` today), applied by `migrate` in `migrate.go`.

- **Forward-only.** There are no "down" migrations. Rolling back means restoring a backup.
- **Versioned with `PRAGMA user_version`**, an integer stored in the database header. After
  applying migration N the code sets `user_version = N`.
- **All in one transaction.** `migrate` runs `BEGIN IMMEDIATE`, then reads `user_version`
  *inside* the transaction, applies every migration with a higher number, then `COMMIT`. Reading
  inside the lock means two processes starting together serialise: the second one sees the
  first one's result. Any error rolls everything back.
- **Fails closed on a newer database.** If `user_version` is greater than the number of migrations
  this binary has, `Open` returns `database schema version N is newer than this binary supports
  (M): refusing to open; upgrade mirafleet`. Running an old binary on a new schema could corrupt
  data it does not understand.
- **Numbering must be contiguous.** `loadMigrations` insists on `0001`, `0002`, ... with no gaps
  and exactly four digits; a test enforces it, so two developers adding `0002_*.sql` on separate
  branches get a loud failure at merge.
- **`PRAGMA` does not accept bound parameters**, so the version number is concatenated into the
  statement. That is safe because it is an `int` parsed from a file name that was just validated,
  never user input.

To add a column later: create `0002_add_something.sql`, write plain `ALTER TABLE ...`, and add a
test in `storetest`. Never edit `0001_init.sql` after it has shipped.

## 6.11 The audit table

Every change and every action leaves a row. The table is **append-only**.

```sql
CREATE TRIGGER audit_no_update BEFORE UPDATE ON audit
BEGIN SELECT RAISE(ABORT, 'audit is append-only'); END;
CREATE TRIGGER audit_no_delete BEFORE DELETE ON audit
BEGIN SELECT RAISE(ABORT, 'audit is append-only'); END;
```

A **trigger** is code stored in the database that runs on an event. `RAISE(ABORT, ...)` cancels the
statement and returns an error, so an `UPDATE audit ...` or `DELETE FROM audit ...` fails, even
from the `sqlite3` shell. (Whoever has file access can still `DROP TRIGGER`; this guards against
bugs and ordinary mistakes, see [chapter 5](05-security.md#59-what-mirafleet-does-not-protect-against).)

**Denormalised on purpose:** `fleet` and `runner` are *names as text*, not ids, and there are no
foreign keys, so rows survive when what they describe is renamed, revoked or deleted. A reader of the
log should not have to join five tables to learn that `alice` ran `restart-runner` on `prod/db-1`.

### Columns

| Column | Type | Meaning |
|---|---|---|
| `id` | AUTOINCREMENT PK | Monotonic. `audit.list` pages by `since_id`; a SIEM exporter can tail it. |
| `ts` | `INTEGER NOT NULL` | Unix ms when the event happened (injected clock `Service.now`). |
| `source` | `TEXT` | Which **front end** (the door the request came through): `cli` (admin socket), `api` (operator API), `enroll`, and `gui` (planned, M4). `system` is defined in `service.Source` but unused . |
| `actor_kind` | `TEXT` | The principal's kind. Values actually written today: `admin` (the local CLI), `operator` (the API) and `enroll` (a peer joining or registering). `runner` is a principal kind too but a runner has no API, so it never appears; `system` exists as a `source` value but nothing uses it yet. |
| `actor` | `TEXT` | Name of the actor. For the local admin it is `uid:<n>`; for an enrolling peer it is empty (the enrollee has no name yet). |
| `remote` | `TEXT` | Source address (`host:port`) of an SSH peer, or `local` for the admin socket. |
| `fleet` | `TEXT` | Fleet name, or empty. |
| `runner` | `TEXT` | Runner name, or empty. |
| `subject` | `TEXT` | What was acted on: the action name for `action.run`, `fleet/runner` for lifecycle, a token id, an operator name. |
| `action` | `TEXT` | What happened: `action.run`, `runner.revoke`, `enroll.join`, `fleet.create`, ... (method names). |
| `params` | `TEXT`, nullable, `json_valid` | Small string map. For `action.run` start rows: the action's parameters, **verbatim**. For lifecycle: `{"from":...,"to":...}`. |
| `outcome` | `TEXT` | `ok`, `denied`, `start`, `error`, `cancelled`, `timeout`. |
| `exit_code` | `INTEGER`, nullable | The action's exit status; `NULL` when there is none (start rows, errors). |
| `duration_ms` | `INTEGER NOT NULL DEFAULT 0` | How long the action took. |
| `message` | `TEXT` | Human text: `runner revoked`, `timed out`, `confirm missing or wrong`, `already registered`, ... |
| `request_id` | `TEXT` | 32 hex characters (16 random bytes) tying an exec's `start` row to its outcome row. |

Indexes: `audit_ts (ts)` for time range queries; `audit_fleet_runner (fleet, runner)` for "show me everything
on prod/db-1".

### Example rows

A guarded `action.run`, as two rows. alice runs `tail-logs` on `prod/db-1` with `confirm = "prod"`:

| id | ts | source | actor_kind | actor | remote | fleet | runner | subject | action | params | outcome | exit_code | duration_ms | message | request_id |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1041 | 1794562867512 | api | operator | alice | 203.0.113.7:51514 | prod | db-1 | tail-logs | action.run | `{"lines":"200"}` | **start** | NULL | 0 | | `9f1c…e07a` |
| 1042 | 1794562867561 | api | operator | alice | 203.0.113.7:51514 | prod | db-1 | tail-logs | action.run | NULL | **ok** | 0 | 41 | | `9f1c…e07a` |

(`request_id` is shortened with “…” here; both rows hold the same 32-character value.) What to notice:

- Row 1041 exists **before** the runner is asked; if it could not be written, row 1042 would never exist
  because nothing ran. This is the fail-closed rule.
- The outcome row copies the identity fields but sets `params` to NULL, so parameters are stored
  once. `duration_ms` comes from the runner's measurement when it reports one.
- The `confirm` string is not stored. A *missing* confirm instead produces one row, `outcome =
  denied`, `message = confirm missing or wrong`, `params = NULL`.

If Dana revokes `db-1` while that action is still running, the outcome row would read instead
`outcome = cancelled`, `message = runner revoked`, `exit_code = NULL`.

The revoke itself, run from the central CLI on the host as uid 1000:

| id | ts | source | actor_kind | actor | remote | fleet | runner | subject | action | params | outcome | exit_code | duration_ms | message | request_id |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| 1043 | 1794562901204 | cli | admin | uid:1000 | local | prod | db-1 | prod/db-1 | runner.revoke | `{"from":"active","to":"revoked"}` | ok | NULL | 0 | runner revoked | |

This row was written in the **same transaction** as `UPDATE runners SET status = 'revoked'`
(`SetRunnerStatus`), so you can never find a revoked runner without its audit row or the reverse.
`request_id` is empty because lifecycle changes are not requests that pair a start and an outcome.

### What is audited, and what is not

| Audited (a row exists) | Not audited (only logged on central) |
|---|---|
| fleet created; token created and revoked; runner approved, disabled, enabled, revoked | Failed joins, bad registration codes (a flood must not also flood the audit table) |
| operator registered, re-registered, revoked | SSH authentication failures |
| registration secret created and rotated; **a code being shown** (never its value) | Heartbeats and `last_seen_at` writes |
| every `action.run` (start plus outcome); denied calls and refused confirms | Forbidden methods attempted on `api@` (these go to the log) |
| successful joins and registrations | |

## 6.12 File safety

The database file lives in the central data directory (default `/var/lib/mirafleet/central`),
which is `0700`. `sqlite.Open` is stricter than "open a file":

1. Refuse `""`, `":memory:"` and `file:` paths. (`:memory:` gives *each pooled connection its own*
   empty database, which would silently break the two-pool design. Tests use files under
   `t.TempDir()`.)
2. `checkDBFile`: if the path exists it must be a regular file, not a symlink; open it with
   **`O_NOFOLLOW`** (unix) so a symlink in the last path component is refused atomically; `fstat`
   the open handle and compare with a fresh `Lstat` of the path using `os.SameFile` ("is the file I
   opened still the file at that path?"); `chmod 0600`.
3. `checkSidecars`: if `mirafleet.db-wal` or `-shm` already exist they must be regular files, not
   symlinks (a planted symlink would redirect SQLite's journal writes to another file).
4. After opening, `tightenModes` best-effort `chmod 0600`s the database and sidecars. The caller
   sets `umask 077` before `Open` so SQLite creates the sidecars `0600` from the start.
5. The data dir is `flock`ed (`central.lock`), so only one central process uses it.

There is a theoretical race left: SQLite re-opens the file by name after these checks. Closing
it needs the directory itself to be private (`0700`), which central enforces; only someone who
can write the directory could swap the file.

## 6.13 The `Store` interface

`internal/store/store.go`. All methods are safe for concurrent use and take a `context.Context`.

### Rules that hold for every method

- **Every mutator takes `now time.Time`.** Nothing below the service calls `time.Now()`; tests
  inject a fake clock.
- **Every state-changing method also takes an `AuditEvent` and writes it in the same transaction.**
  Either both commit or neither. A zero `ev.Time` is set to `now`. `RecordAudit` is the exception:
  it has no state to change and no `now`, so it requires `ev.Time` (zero gives `ErrInvalid`).
- **Secrets never appear in events.**
- **Errors** are four sentinels, matched with `errors.Is`:

| Error | Meaning | Wire code the service maps it to |
|---|---|---|
| `ErrNotFound` | Fleet, runner, token, operator or setting does not exist. From `Enroll` it *also* means "wrong secret" (indistinguishable on purpose). | `not_found` (enroll paths: `unauthenticated`) |
| `ErrInvalid` | Input failed validation (name shape, mode, malformed id or hash, `MaxUses < 1`, expiry not after now, empty key, oversize info, zero audit time). Describes the shape of input, never a token's existence. | `bad_request` |
| `ErrConflict` | A `UNIQUE` or state precondition failed (duplicate name or key, revoked key re-enrolling, status `from` set no longer matches). | `conflict` |
| `ErrStale` | Join token expired, revoked or used up. | `conflict` (enroll paths: `unauthenticated`) |

  Only `UNIQUE` and `PRIMARY KEY` violations become `ErrConflict` (`isUnique`, extended result codes 2067
  and 1555). `CHECK`, `NOT NULL`, foreign-key and trigger errors are returned as they are and end
  up as `internal` on the wire.

### Summary of the method contracts

The full per-method table is the [appendix](#appendix-the-store-method-contracts) at the end of
this chapter. In five lines:

1. **Fleets, tokens, operators, settings:** create, get, list; state-changing methods take `now`
   and an `AuditEvent` and commit both atomically.
2. **Runners:** `Enroll` is the one big atomic operation (token lookup, constant-time compare,
   consume a use, key checks, insert runner, audit row); `SetRunnerStatus` enforces the state
   machine inside the transaction.
3. **Lookups by key or id return any status**, including revoked (auth needs to see `revoked` to
   refuse it); lookups by name return only non-revoked rows.
4. **Heartbeat-type writes** (`TouchRunner`, `TouchOperator`) are not audited.
5. **Audit:** `RecordAudit` appends without a state change; `ListAudit` pages by id.

## 6.14 How a join touches the database

Putting the pieces together, the 6 steps of a successful `enroll.join` for `build-1` into `dev`:

1. `service.Join` parses the token, checks the host-key pin, and hashes the secret with SHA-256.
2. `store.Enroll` begins `BEGIN IMMEDIATE` on the writer connection.
3. `SELECT ... FROM join_tokens WHERE token_id = ?` then a constant-time compare.
4. `UPDATE join_tokens SET uses = uses + 1 WHERE ... uses < max_uses ...` affects 1 row.
5. `INSERT INTO runners (...)` with status `pending`, then `INSERT INTO audit (...)`
   (`action = enroll.join`, `source = enroll`).
6. `COMMIT` (flushed to disk because of `synchronous=FULL`). The new runner row is read back inside the
   transaction and returned.

If any step fails, `withTx` rolls back and the token still has its use. The deferred rollback also
runs on a panic, so the single writer connection is never left stuck inside an open transaction.

## Check your understanding

1. Why does `runners` have a *partial* unique index on `(fleet_id, name)` but a plain `UNIQUE`
   on `pubkey`?
2. A power cut happens one second after "revoked" was printed. What guarantees the runner is still
   revoked after reboot?
3. What does `RegisterOperator` return, and what audit row is written, if Alice registers twice
   with the same key and name?
4. Why are `fleet` and `runner` in the `audit` table text names and not foreign keys?
5. You open the database with an older mirafleet binary after upgrading. What happens, and why is
   that better than letting it try?

<details>
<summary>Answers</summary>

1. Names are meant to be reusable once their runner is revoked (so a replacement `db-1` can be
   enrolled), hence the index only covers `status <> 'revoked'`. Keys must never be reusable, so
   `pubkey` is unique over every row including revoked ones.
2. `synchronous=FULL`. The WAL is flushed to disk at each commit, so a committed revoke (status
   change plus audit row, one transaction) survives power loss. With `NORMAL` it could be lost.
3. The existing operator with `created = false`; the store writes no audit row in that case. The
   service then records one itself with message `already registered`
   (`service.Register`).
4. So history outlives what it describes: a deleted or renamed fleet or a revoked runner must not
   change or break old rows. Append-only triggers make them immutable.
5. `migrate` reads `PRAGMA user_version`, sees a number higher than the migrations it embeds, and
   returns an error ("newer than this binary supports"). Failing closed prevents an old binary
   from writing data in a layout it does not understand.

</details>

## Appendix: the Store method contracts

Reference table for `internal/store/store.go`. Read the rules and the error table in section 6.13 first.

**Fleets**

| Method | Contract |
|---|---|
| `CreateFleet(f, now, ev)` | Inserts and returns the fleet with `ID` and `CreatedAt = now`. `ErrInvalid` for a bad mode or name; `ErrConflict` if the name exists. Atomic with `ev`. |
| `GetFleet(name)` | `ErrNotFound` if absent. `RunnerCount` is **not** filled. |
| `ListFleets()` | Ordered by name; `RunnerCount` = non-revoked runners, computed in the same query. |

**Runners**

| Method | Contract |
|---|---|
| `ListRunners(fleet, includeRevoked)` | Ordered by name then id. `ErrNotFound` if the fleet does not exist. |
| `GetRunner(ref)` | The **non-revoked** runner `fleet/name`. |
| `GetRunnerByKey(pubKey)`, `GetRunnerByID(id)` | **Any status**, including revoked: auth needs to see `revoked` to refuse it, and audit needs old rows. |
| `TouchRunner(id, seen, info)` | Sets `last_seen_at` and `info`. **Not audited** (high frequency). Never changes status. `ErrInvalid` if the info JSON is over 16 KiB; `ErrNotFound` if the id is unknown. |
| `Enroll(tokenID, secretHash, now, newRunner, ev)` | **The most important atomic operation.** One transaction: (1) look up the token by id; (2) constant-time compare the hash, against a dummy hash if the id is unknown; (3) conditionally consume a use (revoked, expired or used up gives `ErrStale`); (4) refuse a key that is an operator's (`ErrConflict`; this runs after the token checks so a caller with a bad token learns nothing about operator keys); (5) insert the runner with the token's fleet, labels and status (`active` if `auto_approve`, else `pending`); (6) write `ev` with `ev.Fleet` set to the token's fleet. **Nothing is consumed on any error.** `ErrInvalid` (checked first) for a bad name, empty key/fingerprint, or a hash that is not 32 bytes. `ErrConflict` if the name is taken by a non-revoked runner in that fleet or the key is already known (revoked runner keys included). |
| `SetRunnerStatus(id, from, to, now, ev)` | Moves a runner to `to` only if its current status is in `from` **and** `CanTransition` allows it, atomically with `ev`; sets `updated_at = now`. `ErrInvalid` for an empty `from` set or an invalid status; `ErrNotFound` for an unknown id; `ErrConflict` if the status is not in `from` or the transition is illegal. This is how a concurrent change is detected. |

**Join tokens**

| Method | Contract |
|---|---|
| `CreateToken(t, now, ev)` | Inserts; returns the token with `SecretHash = nil`. `ErrInvalid` unless the hash is 32 bytes, the id is 16 lowercase hex, `MaxUses >= 1` and `ExpiresAt` is after now. `ErrNotFound` for an unknown fleet; `ErrConflict` for a duplicate `TokenID`. |
| `RevokeToken(tokenID, now, ev)` | Sets `revoked_at`. `ErrNotFound` if unknown. **Revoking twice is a no-op returning nil with no event.** |
| `ListTokens(fleet)` | Ordered by id; `fleet == ""` means all. `SecretHash` is never selected. |

**Operators**

| Method | Contract |
|---|---|
| `RegisterOperator(name, pubKey, fingerprint, now, ev)` | Inserts an `active` operator. **Idempotent per key:** if the key is already an *active* operator, that operator is returned with `created = false` and **no audit row is written**, so the service must record the re-registration itself (it does). `ErrConflict` if the key belongs to a revoked operator or to *any* runner (checked inside the transaction), or if the name is held by a different active operator. `ErrInvalid` for a bad name. |
| `GetOperatorByKey`, `GetOperatorByID` | Any status. |
| `GetOperatorByName(name)` | The non-revoked operator only. |
| `TouchOperator(id, seen)` | Sets `last_seen_at`; not audited. |
| `RevokeOperator(name, now, ev)` | Marks the non-revoked operator revoked, atomically with `ev`. `ErrNotFound` if there is none. The *caller* then kicks connections (the store does not know about SSH). |
| `ListOperators(includeRevoked)` | Ordered by id. |

**Settings**

| Method | Contract |
|---|---|
| `GetSetting(key)` | `ErrNotFound` if absent. The value may be secret: never log it. |
| `CreateSetting(key, value, now, ev)` | Insert only; `ErrConflict` if present. Atomic with `ev`. |
| `PutSetting(key, value, now, ev)` | Upsert, an explicit overwrite. Atomic with `ev`. |

**Audit**

| Method | Contract |
|---|---|
| `RecordAudit(ev)` | Appends one row (its own transaction), returns the new id. Used for events with no state change: exec start and outcome rows, denials, re-registrations, "code shown". |
| `ListAudit(sinceID, limit)` | Rows with `id > sinceID`, ascending; `limit <= 0` means 100 and the cap is 1000. Reads from the reader pool. |

**Housekeeping:** `Close()` closes both pools; later calls fail.
