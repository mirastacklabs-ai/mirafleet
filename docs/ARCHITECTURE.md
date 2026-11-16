# mirafleet — Architecture

mirafleet lets a Claude Code session (the **orchestrator**) drive fleets of remote servers
(**runners**) through a **central** server, in the spirit of GitLab runners. Every connection is
outbound toward central, so runners can sit behind NAT or inside private networks.

This document covers the open-source core. Enterprise features are out of scope here
(see [OSS vs Enterprise](#oss-vs-enterprise)).

## Components

```
┌────────────────────┐        ┌──────────────────────────────┐        ┌──────────────────────┐
│ Orchestrator       │  SSH   │ Central                      │  SSH   │ Runner               │
│ Claude Code        │───────▶│ embedded SSH server          │◀───────│ dials out, keeps the │
│ + plugin           │        │ registry, fleets, lifecycle  │        │ connection open      │
│ + mirafleet mcp  │        │ scheduler, audit, admin GUI  │        │ actions guard        │
└────────────────────┘        │ SQLite                       │        │ containerd workspaces│
                              └──────────────────────────────┘        └──────────────────────┘
```

All three are subcommands of one static Go binary: `mirafleet central | runner | mcp`.

| Component | Runs on | Connects |
|---|---|---|
| Orchestrator (`mirafleet mcp` + Claude Code plugin) | user's machine | outbound SSH to central |
| Central (`mirafleet central`) | one host per network (public or private) | listens only |
| Runner (`mirafleet runner`) | each fleet server | outbound SSH to central |

Only the orchestrator needs internet access (for the Claude API). Central and runners never call
out beyond their own network, which makes private and air-gapped deployments straightforward.

## Transport

SSH is implemented in-process with `golang.org/x/crypto/ssh`. No OpenSSH sshd, autossh or
`authorized_keys` editing is required.

- **Runner → central:** the runner dials central (default port `2222`, commonly `443` in
  internet mode), authenticates with its runner key, and pins central's host key at join time.
  It reconnects with exponential backoff. One long-lived connection per runner.
- **Heartbeat:** the runner sends an SSH global request `heartbeat@mirafleet` every 15s with a
  JSON payload (hostname, OS, arch, version, labels, load, free workspace slots). A connection
  that is open and heartbeating means the runner is online.
- **Central → runner:** central opens channels *back over the runner's own connection*
  (channel types `exec@mirafleet` for actions and `workspace@mirafleet` for workspaces; a
  runner serves one, by mode). The runner accepts only these types; there is no shell, port
  forwarding or other channel.
- **Orchestrator → central:** the orchestrator connects with an operator key and makes
  JSON requests on `api@mirafleet` channels (see [Channels and the API](#channels-and-the-api)):
  list fleets and runners, run action, and the `workspace.*` methods.

### SSH users and channels

The SSH user selects the role; the role selects the channels the connection may open. Keys are
`ssh-ed25519` only. Anything not in the table is refused.

| SSH user | Authenticates as | Channels / requests |
|---|---|---|
| `runner` | an enrolled runner key (not revoked) | sends `heartbeat@mirafleet`; central opens `exec@mirafleet` or `workspace@mirafleet` toward it |
| `operator` | an active operator key | opens `api@mirafleet` |
| `enroll` | any ed25519 key (30 s deadline, rate-limited per source address) | exactly one of `join@mirafleet` (token) or `register@mirafleet` (registration code), then closed |

Identity travels only in the SSH `Permissions` of the authenticated connection and is reloaded by
id per request. Unknown or revoked keys are rejected for every user except `enroll`.

**Central executes; operators never reach runners directly.** Runners trust exactly one key,
central's. Operators only need access to central. Every command passes through central, so
central holds the complete audit trail. Trade-off: a compromised central can run anything a
fleet's policy allows on that fleet's runners. The runner-side guard bounds that.

## Data model

```
central
 └── fleet            name, mode (actions | workspace), policy
      ├── runner      name, labels, status, last heartbeat, concurrency
      ├── join token  one-time or multi-use, expiry, labels to apply
      ├── workspace config   executor, images, network, limits, TTLs, exec timeouts
      └── workspace   session, operator, runner, image, state, created, last used, expires
operator              name, public key, status (active | revoked)
audit                 time, operator, fleet, runner, workspace, command/action, exit, duration
```

**Fleets** (`prod`, `uat`, `dev`, …) are the unit of isolation. A runner belongs to exactly one
fleet. Join tokens are scoped to a fleet. In OSS v1, any active operator can use any fleet.
Per-fleet grants are an enterprise feature.

**Runner lifecycle:** `pending → active → disabled → revoked`. An admin approves a runner after
it joins. A disabled runner stays connected (visible) but refuses work. A revoked runner's key is
rejected.

Lifecycle changes (approve, disable, enable, revoke) are operations on central, and **only on
central**: they are never exposed to operators over `api@mirafleet` or MCP. Two front ends share
one code path:

| Front end | Example |
|---|---|
| GUI (served by central) | Runners screen → Revoke |
| CLI on central | `mirafleet central runner revoke <fleet>/<runner>` |

- **Revoke is immediate.** Central adds a tombstone for the runner id, cancels the session
  context, closes the live connection, then commits `status=revoked` (and the audit row) in one
  transaction. In-flight actions are cancelled (the runner kills the process group; audit outcome
  `cancelled`, message `runner revoked`) and every later login is rejected at auth. Revoked
  keys never re-enrol; revoked names can be reused.
- **Disable lets in-flight work finish.** The connection stays up and visible, but new work is
  refused. `enable` reverses it.
- **Pending** runners may heartbeat but never exec. Only `active` runners run actions.
- Revoking an operator (also central only) closes their connections and cancels their execs.

Every change is audited with who made it and from which front end (`cli`, `api`, `gui`, `enroll`,
`system`). Keeping lifecycle off the operator API means a prompt injection in command output
cannot talk an agent into approving or revoking runners, whatever the orchestrator config says.

## Central core

Detail and rationale: [M2.md](M2.md).

### Store

SQLite through `modernc.org/sqlite` (pure Go, so `CGO_ENABLED=0` holds) behind the `Store`
interface, which exposes domain-level atomic operations rather than SQL. WAL mode, one writer
connection plus a small read-only pool, `synchronous=FULL` so a committed revoke survives power
loss, files `0600`. Migrations are embedded and forward-only; a database newer than the binary
fails closed. The `audit` table is append-only (triggers abort updates and deletes). State changes
write their audit row in the same transaction; an action writes a `start` row before dispatch
(no row, no exec) and an outcome row after. Heartbeats stay in memory; `last_seen_at` is written
on connect, disconnect and at most every 5 minutes. A data-dir lock keeps a second central off the
same directory.

### Join tokens and registration codes

| | Format | Used by | Lifetime |
|---|---|---|---|
| Join token | `mfj1.<id>.<secret>.<host-key-hash>` | a runner, once, to enrol | single use by default, TTL 24h (max 30d, `-uses N` capped); never unlimited |
| Registration code | `mfr1.<code>.<host-key-hash>` | any number of operators, to register | valid only within its 60 s time step |

- Both end in the SHA-256 hash of central's host key, so the client pins central from the
  credential itself. There is no trust-on-first-use.
- Join tokens: only a SHA-256 of the 32-byte secret is stored; consumption is a conditional
  update in the same transaction as the runner insert. Joining yields `pending` (or `active` with
  `token create -approve`). Token labels are trusted; heartbeat labels are informational.
- Registration codes are TOTP-style: HMAC-SHA256 over the 60 s time step with a 32-byte secret in
  the store, 128 bits long, so they cannot be brute-forced inside their window. Central both
  issues and checks the code, so there is no clock skew. The code is not single use: any number of
  operators can register with the current code, and a registration needs no further approval
  because only an admin on central can read the code. `operator rotate-secret` invalidates all
  outstanding codes. Showing a code and every registration are audited; the code itself never is.
- Secrets and codes go on stdin (`-token -`, `-code -`), never in argv or shell history, and are
  never logged or audited.

### Channels and the API

`api@mirafleet` (operators) carries one request per channel: cancel is closing the channel,
concurrency is more channels. Frames are a 4-byte big-endian length plus JSON, with the length
checked before allocation. Requests are `{"v":1,"id","method","params"}` (unknown fields
rejected); responses are `{"v":1,"id","result"}` or `{"error":{"code","message"}}`. Error codes:
`bad_request unauthenticated forbidden not_found conflict unavailable too_large timeout
unsupported_version internal`.

Limits: request 64 KiB and read within 10 s; `action.run` timeout min(`timeout_s`, 10 min),
default 2 min; at most 8 concurrent channels per operator connection; at most 32 params
(key 64 B, value 4 KiB).

| Method set | Methods | Reachable on |
|---|---|---|
| Operator | `hello`, `fleets.list`, `runners.list`, `action.run`, `workspace.open`, `workspace.exec`, `workspace.read_file`, `workspace.write_file`, `workspace.close`, `workspace.list` | `api@mirafleet`, admin socket |
| Enrol | `enroll.join`, `enroll.register` | `join@mirafleet`, `register@mirafleet` only |
| Admin | `fleet.create`, `fleet.workspace_set`, `token.*`, `runner.lifecycle`, `operator.*`, `workspace.admin_list`, `workspace.kill`, `audit.list` | local admin socket only |

Workspace limits differ from `action.run`: `exec` timeout comes from the fleet (see
[Workspace mode](#workspace-mode)), and `write_file`/`exec{stdin}` add a body frame of at most
1 MiB after the 64 KiB request. `runners.list` also reports `slots`, `free_slots` and `executor`.
`mirafleet mcp api METHOD [JSON] [-in FILE|-] [-out FILE]` supplies and receives those bodies.

Admin and enrol methods are never dispatched on `api@mirafleet`, whatever the principal (the
method table and the policy are independent gates). `PolicyEngine` denies by default; an active
operator may only call the operator set.

### Local admin socket

`<data>/admin.sock` (`0600`) speaks the same framing, limits and dispatcher as `api@mirafleet`,
as principal `admin`. The peer UID must equal central's before anything is read. It exists so
that lifecycle changes reach live connections: the `mirafleet central ...` admin commands are
clients of the running central. Only `central init` runs offline.

### Platforms

`central` and `runner` are Linux only and refuse to start elsewhere; their wiring lives in
`server_linux.go`, so darwin and windows binaries never link SQLite. `mirafleet mcp` builds for
linux, darwin and windows, amd64 and arm64 (see [Orchestrator](#orchestrator) for per-OS key
protection).

## Fleet modes

| Mode | Use | What runs | Executor |
|---|---|---|---|
| `actions` | ops, typically prod | only named actions from the runner's `actions.yaml` | host shell, as the runner user |
| `workspace` | coding, typically dev/uat | any command, inside the session's workspace container | containerd (default) or OpenSandbox (opt-in) |
| `workspace`, executor `host` (M7) | remote development on a VM | any command and the full file-tool set, as the runner's own OS user in a per-session directory | `host`: see [Host mode](#host-mode-m7) |

### Actions mode

The runner, not central, holds the allowlist (`/etc/mirafleet/actions.yaml`):

```yaml
actions:
  restart-runner:
    run: ["systemctl", "restart", "gitlab-runner"]
    timeout: 60s
  tail-logs:
    run: ["journalctl", "-u", "gitlab-runner", "-n", "{{lines}}"]
    params: { lines: { pattern: "^[0-9]{1,4}$", default: "200" } }
    timeout: 15s
```

Commands are argv arrays, never run through a shell. Parameters are validated against their
regex before substitution. Anything not defined is refused. This file is the security boundary,
and it lives on the box being protected.

### Workspace mode

Each Claude Code session gets its own workspace, as GitLab gives each job a build directory, so
several orchestrators can work on different projects on the same runner at once. Detail and
decisions: [M3.md](M3.md); operator how-to: [guide/](guide/README.md).

**Runner config.** `mirafleet runner run -workspace /etc/mirafleet/workspace.yaml` (exactly one of
`-actions` or `-workspace`; one runner = one fleet = one mode). `workspace.yaml` is the
workspace-mode security boundary, like `actions.yaml`: strict parsing, fail-closed checks at
load, no "privileged" or "unconfined" option. Effective policy is **fleet ∩ runner**; the runner
builds the whole OCI spec itself from a fixed template, so a compromised central cannot obtain a
privileged container, host network or host mount.

**A workspace runner is root-equivalent on its host.** Access to the containerd socket amounts to
host root. Run workspace runners on dedicated, disposable hosts; the container is the boundary
for agent commands, the runner is not.

#### Executors

One runner-side interface (`workspace.Executor`: `Create`, `Exec`, `ReadFile`, `WriteFile`,
`Destroy`, `List`). Scheduling, slots, reaper, audit and policy live above it.

| Executor | Status | Notes |
|---|---|---|
| `containerd` | default | Go client, no Docker Engine. Its own namespace (`containerd.namespace`, required; use `mirafleet`; `k8s.io` and `moby` are refused), so it coexists with kubelet and `docker.io` hosts |
| `opensandbox` | opt-in per fleet | node-local [OpenSandbox](https://github.com/opensandbox-group/OpenSandbox), spoken to with stdlib HTTPS only (no SDK) |
| `host` | M7, per fleet | no container: the runner's own OS user on a VM, in a per-session directory ([Host mode](#host-mode-m7)) |

#### containerd sandbox

- **Identity:** uid/gid 1000 inside the container. With `userns: required` (the default; hosts
  without support get 0 slots and a clear error) each slot gets its own host uid range
  (`uid_base + slot*65536`) on an idmapped snapshot, so workspaces are isolated from each other
  too. The workspace directory is mode 0700 and owned by the mapped uid.
- **Hardening:** no capabilities, no-new-privileges, containerd's default seccomp profile,
  AppArmor when available, masked and read-only kernel paths, read-only rootfs, new
  pid/ipc/uts/mount/cgroup/net/user namespaces.
- **Mounts:** only `/workspace` (rw bind of `<containerd.root>/<id>/`, e.g. `/var/lib/mirafleet/ws/<id>/`; `root` is required), a size-limited tmpfs
  `/tmp`, generated read-only `/etc/hosts` and `resolv.conf`, and the mirafleet binary read-only
  as PID 1 (`/.mirafleet/init`, a zombie reaper that works in any image, distroless included).
  No other host mounts, no devices.
- **Limits:** cgroup CPU, memory (swap = memory) and pids; `RLIMIT_FSIZE` set to the disk quota;
  all containers sit under one `mirafleet` parent cgroup. Disk is a soft quota: measured every
  60 s and before writes, over quota refuses `write_file`, over 2× kills the workspace.
- **Files:** `read_file`/`write_file` run on the host through `os.Root` on the workspace
  directory (symlinks cannot escape; FIFOs and devices refused). Only `/workspace` is reachable.
- **Exec:** argv only (no shell string on the wire), cwd under `/workspace`, at most 4
  concurrent per workspace. Output is buffered, head plus tail, capped at 1 MiB JSON-encoded.
  Timeout kills the exec; daemonised children live until the workspace is destroyed, bounded by
  the pids and memory limits.

#### Network

Per fleet: `none` (no route out; default) or `egress`. Egress is an in-netns allowlisting proxy:
the runner opens a listener on `127.0.0.1:3128` inside the container's network namespace, feeding
a stdlib CONNECT/HTTP proxy that enforces the fleet's `host[:port]` allowlist (wildcards allowed)
and does its own DNS, checking the resolved address. Execs get `HTTP(S)_PROXY`; tools that ignore
proxies fail closed. No CNI, no iptables.

- **Never-unlockable ranges:** loopback, link-local, cloud metadata and unspecified addresses
  are refused whatever the rules say, because the proxy dials from the host's network namespace.
- **Private and reserved ranges** (RFC 1918, CGNAT, multicast, ...) are blocked unless the runner
  sets `egress_cidr_rules: true` (opt-in, default off) and a CIDR/IP rule covers them.
- Egress with a secure runtime (gVisor/Kata) is refused: the listener does not reach the guest
  netstack. Guarded fleets therefore run with network `none` for now.

#### Policy and limits

- **Images:** fleet allowlist ∩ runner allowlist (exact refs or `repo@sha256:*` / `repo:*`
  patterns, normalised references). The allowlist is checked on the resolved reference before
  any pull. Pull policy `if-not-present|always|never`; no registry credentials yet.
- **Env:** must be in the runner's `env_allow` and pass a hard denylist (credential-looking names such as
  `*_API_KEY`, `*_TOKEN`, `*_SECRET*`; `ANTHROPIC_*`, `OPENAI_*`, `AWS_*`; proxy variables; and
  variables that change what code runs: `LD_*`, `BASH_ENV`, interpreter option and path hooks, git
  config injection; source of truth: `workspace.EnvDenied`), enforced on central and on the
  runner. Agent API keys never enter a workspace; the MCP server forwards nothing from its own
  environment.
- **Exec timeout:** per fleet on central, default 2 min, max 30 min, hard ceiling 24 h in code;
  the runner's `limits.exec_max` also caps it.
- **Other limits:** `concurrent: N` slots per runner, 8 workspaces per operator, files up to
  `limits.max_file` (required, e.g. 64 MiB) moved in chunks of at most 1 MiB; the runner's
  `max_age` caps the fleet max TTL and drives `runner workspace prune`.
- **Guarded fleets** (`confirm`): `confirm == fleet name` on `open`, `exec` and `write_file`,
  and a secure runtime (gVisor/Kata) is required on both executors.
- Repo cloning with runner-held credentials is not built for container workspaces; Claude clones
  via `exec`, so no git secrets live in them. Host mode (M7) has central-held credentials.

#### Lifecycle, ownership and placement

- **Open:** `workspace.open{fleet, session}`. Sessions are client-chosen
  (`^[A-Za-z0-9._:-]{1,128}$`) and **sticky**: one live workspace per (operator, fleet, session);
  reopening returns it (`reused`). If its runner is offline, `open` returns `unavailable` rather
  than creating a fresh one, since the files live on that runner.
- **Placement:** active, online runners whose executor and secure runtime fit the fleet, whose
  **trusted token labels** are a superset of the requested labels (heartbeat labels are
  informational), and whose advertised images match; most free slots wins (random tie-break),
  retrying up to 3 candidates.
- **Ownership:** every call requires the workspace's operator to be the caller; anything else is
  `not_found`, so existence does not leak. `workspace.list` shows only your own. Admins act
  through admin methods on the local socket.
- **Runner status:** a `disabled` runner refuses new `open`/`exec`/`write` but still serves
  `read`, `close` and the reaper; `pending` runners never get workspaces.
- **Cleanup:** a TTL reaper on central (every 60 s) destroys idle workspaces (default 24 h) and
  those past their hard max (default 7 d), plus those of revoked operators or runners.
  **Reconcile** runs when a runner heartbeats: orphans on the runner are destroyed, `open` rows
  whose container vanished become `lost`. Runner revoke closes its workspaces in the store (the
  containers stay on that host; clean with `mirafleet runner workspace prune`); operator revoke
  cancels their execs and the reaper destroys their workspaces. At central start, `opening` rows
  become `failed`. On runner restart, egress listeners are re-attached.
- **Audit:** every workspace call, `list` included, writes a `start` row before dispatch (no row,
  no call) and an outcome row, with the `workspace` column set. Rows hold argv, cwd, env names
  (never values), paths and SHA-256 of written data (never content).

#### Channel and body frames

Central opens one channel type toward runners for all workspace operations: `workspace@mirafleet`
(create, exec, read, write, destroy, list). The request is JSON in the channel-open data (at most
64 KiB); write and stdin bytes follow raw, then `CloseWrite`; the runner answers with one
length-prefixed JSON result, then raw bytes for a read. There is no separate `file@` channel
(`exec@mirafleet` stays for the actions guard only). A runner with no workspace config refuses
`workspace@`.

On `api@mirafleet`, `workspace.write_file` and `workspace.exec{stdin:true}` carry a **body
frame** after the request: one more length-prefixed frame of at most 1 MiB, which central reads
only after policy, ownership and confirm checks pass; `workspace.read_file` returns its bytes in
a frame after the JSON result. Workspace methods on an actions fleet (and `action.run` on a
workspace fleet) return `conflict`.

#### OpenSandbox executor (opt-in)

OpenSandbox supplies the sandbox; mirafleet keeps the agent and its API keys outside it and adds
identity, policy, central audit, instant revoke and outbound-only runners.

- **HTTPS only**, even on loopback; plain `http://` is refused at config load. TLS 1.3, trust
  anchored on a configured `ca_file` (never the system pool), optional `pin_sha256` SPKI pin on
  the leaf, client certificate required (mTLS), no redirects. Endpoint hosts returned by
  OpenSandbox are ignored; every URL is built from the configured base.
- **TLS front:** OpenSandbox has no native TLS, so put a front next to it, in its network
  namespace. Either the built-in `mirafleet runner tls-front` (stdlib, TLS 1.3, mTLS required,
  loopback only, routes limited to the sandbox API and execd) with `mirafleet runner tls-init`
  to mint the node CA, front and client certificates, or an existing TLS proxy following the same
  rules (`opensandbox.front: external`).
- **Secrets:** the OpenSandbox API key is a 0600/0400 file on the runner host, required, never
  in env, argv, logs or a sandbox.
- **Ownership:** sandboxes are tagged with `mirafleet.workspace` and the runner's name, and
  listings are re-filtered client-side so foreign sandboxes are never touched.
- **Network:** deny-default policy on every create. OpenSandbox policies are host-only, so egress
  rules are limited to ports 80 and 443. No egress with gVisor/Kata yet.
- **Gaps:** hardening (read-only rootfs, seccomp, pids) is server-wide in OpenSandbox, not
  per-request, and must be configured by the operator; `exec` with stdin is unsupported; the
  server is 0.1.x, so its API may change. The linked-code licence scope excludes it (operator
  installed, not bundled).

#### Admin and local commands

On central: `fleet create|workspace set` (executor, image, allowlist, pull, network, egress,
limits, TTLs, exec timeouts, `-confirm`), `workspace list`, `workspace kill ID`. On the runner
host (no central needed, works after a revoke): `runner workspace list` and
`runner workspace prune [-workspace FILE] [-all] [-yes]` (a dry run without `-yes`). Give each runner on a host its own `containerd.namespace`.

### Host mode (M7)

Design and decisions: [M7.md](M7.md). North star: AI inference stays on the operator's laptop; all
tool calls run on a remote VM through central; credentials live on central. Host mode is a
`workspace`-mode fleet with executor `host`; sessions, sticky open, placement, ownership, audit,
reaper and the `workspace.*` methods are the ones above, unchanged.

**The runner is one OS user on one VM.** `mirafleet runner join|run` runs as an ordinary user (XDG
dirs: config, state, data; no root, no capabilities, refuses uid 0), and every tool call runs as
that same user. `dev` and `qa` are separate users (each with its own runner) or separate VMs. The
runner is hardened at start (`PR_SET_DUMPABLE 0`, no core dumps) and children get a clean
environment (`HOME USER LOGNAME PATH LANG`, `env_allow`, and the per-session tool variables). The
VM and its firewall are the boundary; there is no container. One operator holds a host runner at a
time (store reserve transaction): another operator's open is refused with code `conflict` and
message `busy: runner NAME is in use by OP`, and placement skips held runners. A fleet's guard is
only the typed fleet-name `confirm`. `workspace.yaml`: `executor: host` and an optional `host:`
block (`root`, `cleanup.delete_after`, `job_max`, `corporate_ca_file`).

**Directories.** A session's checkout is `<root>/sessions/<slug>` (0700); a shared project
directory (`workdir:` in `.mirafleet.yaml`, host fleets only, name `^[a-z0-9][a-z0-9._-]{0,62}$`)
is `<root>/work/<name>`. `<root>` defaults to `$XDG_DATA_HOME/mirafleet/work`. Session state
(sockets, CA, `ssh_config`, `out/`, `jobs/`, the persisted `creds_scope`) lives in
`$XDG_STATE_HOME/mirafleet/sessions/<slug>/`. Close kills the session's process groups and
closes its agent socket and broker session but keeps the directory; opening the same session
again reattaches directory, current directory and jobs, also after a runner restart (host
sessions are re-adopted). A second concurrent open of one session is refused. A daily sweep
deletes directories unused for `delete_after` (default 30 days); deletion is otherwise explicit
(`workspace_close {delete: true}`, `runner host prune`, admin `workspace kill -delete`).

**Credentials.** Central stores credentials (kinds `ssh-key`, `https-token`, `registry`) in
SQLite as AES-256-GCM ciphertext (random nonce; AAD = credential id and version). The key is
`MIRAFLEET_SECRETS_KEY` (32 bytes, base64, from `central keygen`, read from a 0600 env file); the
database holds only a key-check value, and central refuses to start with secrets but no or a wrong
key. `central secrets rekey` re-encrypts everything in one transaction. Credentials are managed by
admins only (local admin socket and the GUI Credentials page; never `api@` or MCP), values are
write-only and never echoed or audited. Each is assigned to a runner or a whole fleet (a runner's
set is the union). Central sends the effective set to each connected host runner on connect and
on every change over the `creds_sync` op inside the authenticated SSH connection; the runner
keeps values in memory only and replaces the set atomically, so an unassigned credential stops
working at once. A session may narrow to a subset (`workspace_open credentials`, recorded as
`creds_scope`; it never widens). Credentials on a non-host fleet are refused, and placement only
considers runners that hold every requested credential. Rotation: a replaced value is pushed and
used immediately; expiry is flagged 14 days ahead; opt-in `rotate: gitlab_self` has central rotate
a GitLab token (below 7 days left) and push the new value, or audit the failure and keep the old
value. Every use is logged on the runner and sent to central as aggregated `events@` rows.

**Agent never sees a secret.** SSH keys are used through a per-session restricted agent
(`internal/workspace/sshagent`): `List` and `Sign` only, signs only user-auth requests, with host
binding to the credential's `known_hosts` when the client supports it; the socket is
`<state>/sessions/<slug>/agent.sock` (0600) and `SSH_AUTH_SOCK` plus `GIT_SSH_COMMAND` point to
it. HTTPS tokens and registry logins go through a runner-local broker (`internal/workspace/broker`):
a loopback proxy set as `HTTPS_PROXY` with a per-session password; hosts that have no credential
are plain CONNECT tunnels, credential hosts are terminated with a per-session CA (name-constrained
to those hosts, key in memory) and the header is injected; registry 401 challenges are answered by
a token exchange. The CA bundle env variables are set for common clients. Accepted OSS gaps
(same-uid agent can use the key while a session is open, or re-enrol as the runner) are listed in
M7.md section 4.6 and the [threat model](THREAT-MODEL.md).

**Tools.** The MCP server exposes, for a host session: `bash` (default 120 s, max 10 min; the
current directory persists, env does not), `bash_output`, `kill_shell`, `job_list`, `read_output`,
`read`, `write`, `edit`, `multi_edit`, `grep`, `glob`, `ls`, `notebook_edit`, `upload`,
`download`, plus `workspace_open|close|list`. File tools are the pure `internal/workspace/fsops`
package run in the runner process as the OS user, so (as with local Claude Code) they reach
everything that user can see: absolute paths anywhere, relative paths from the session directory
or workdir. Results show a path relative when it is inside the start directory, absolute
otherwise. `grep` uses `rg` when it is on the runner's `PATH`, else a Go implementation. FIFOs and
devices are refused. `upload`/`download` stay inside the project directory on the operator side
and refuse secret-looking names. On guarded fleets mutating tools need `confirm`.
The operator-side protocol is `workspace.fs{op}`, `workspace.job_*` and `workspace.output_read`.

**Background jobs.** `bash {run_in_background}` starts a job supervised by the hidden
`mirafleet host-shim job` in its own process group, writing `stdout`, `stderr` and `status.json`
under `jobs/<id>/`. At most 8 per session, a maximum duration per fleet (default 12 h, at most
24 h), 256 MiB per stream. Jobs survive a runner restart (the unit uses `KillMode=process`) and
the runner re-adopts them from `status.json`, pid and start time. A running job marks the
workspace busy in the heartbeat, so the idle reaper skips it.

**Full output, never a silent cut.** A foreground `bash` returns at most about 32 KiB per stream
inline (head and tail with an omitted count) but the runner tees the complete output to
`out/<request_id>` (last 32 files, at most 64 MiB each). Results carry `ref`, `stdout_bytes`,
`stderr_bytes` and `omitted`; `read_output {ref | shell_id, offset, limit, tail_lines, grep}`
pages it, and `next_offset` resumes exactly where a page stopped.

**Session reattach.** The orchestrator's session id (Claude Code's) is sent as the
`mirafleet.session` label, so a new Claude Code process, a reconnect or a runner restart lands on
the same runner, directory, current directory and jobs. Central never decides this from state in
the harness, only from the session id and its own workspace rows.

## Orchestrator

`mirafleet mcp` is a stdio MCP server. Claude Code is the first supported harness. Because the
core is plain MCP, any MCP-capable harness (opencode, Kilo Code and others) can use it, and each
gets a thin integration under `integrations/<harness>/`.

**Harness-agnostic rule:** correctness and safety never depend on harness-specific features such
as hooks. Hooks only add convenience.

| Concern | Works in every harness (core) | Claude Code extra (`integrations/claude-code/`) |
|---|---|---|
| Workspace per session | session ID from `-session`, else a per-process random one; the workspace is sticky per session | `SessionStart` hook writes a handoff file, so `--resume` reattaches |
| Cleanup | server exit never closes workspaces; the idle-TTL reaper (default 24 h) removes them; `workspace_close` closes one | `SessionEnd` hook closes the workspace on `/clear` and logout only; other reasons keep it for the reaper |
| Prod guard | fleets with `confirm: true` require a `confirm: "<fleet>"` argument; the harness's own tool-approval prompt applies | `PreToolUse` hook adds an explicit confirmation |
| Install | `mirafleet mcp` added to the harness's MCP config | plugin with `.mcp.json`, hooks and slash commands |
| Commands | `/mirafleet`, alias `/openfleet` (harness custom-command files) | `/mirafleet`, aliases `/openfleet` and `/claudefleet` (plugin skills) |

`mirafleet mcp` (no subcommand) serves MCP over stdio, tools capability only, built on
`github.com/modelcontextprotocol/go-sdk`. Stdout carries protocol only; logs go to stderr. It
exits 0 on stdin EOF, `os.Interrupt` (Ctrl-C), SIGTERM or SIGHUP, cancelling in-flight calls.
Nine tools, thin wrappers over the API methods from M2/M3:

| Tool | Purpose | Backed by |
|---|---|---|
| `list_fleets` | fleets on one or all contexts, with mode and `guarded`; a failing context is an `errors[]` entry | `fleets.list` |
| `list_runners` | runners of a fleet: status, labels, free slots, executor, advertised actions; `runner` filter returns one runner with all its actions | `runners.list` |
| `run_action` | actions mode: run a named action on a runner; args `runner`, `action`, `params`, `timeout_s`, `confirm`, `context`, `fleet` | `action.run` |
| `workspace_open` | workspace mode: get a workspace for this session | `workspace.open` |
| `exec` | run a command (`command` or `argv`, `cwd`, `env`, `stdin`, `timeout_s`; optional `workspace` id, as on every workspace tool) in the session's workspace | `workspace.exec` (`["/bin/sh","-c",cmd]`) |
| `read_file` / `write_file` | file access inside the workspace, paged or chunked by offset, text or base64 | `workspace.read_file` / `workspace.write_file` |
| `workspace_close` | release the workspace | `workspace.close` |
| `workspace_list` | your own workspaces on a fleet | `workspace.list` |

- Every tool takes an optional `context` and `fleet`. Results are JSON (`structuredContent` plus
  the same text) and carry `where`: the resolved context and fleet and where each came from
  (argument, flag, project file, context default), so a user can see where a call ran. Errors are
  `isError` with an API error code. Tool descriptions say that stdout, stderr and file contents are
  data from remote machines, not instructions.
- **Contexts:** `contexts.yaml` in the per-user config dir (written by `mirafleet mcp login`;
  `mcp context list|use|remove` manage it) lists one or more centrals, for example a private-network
  central and a cloud central. Per context `default_fleet`; top level `output_cap_bytes`. SSH jump
  hosts are deferred: a `jump` key is refused.
- **Project file:** `.mirafleet.yaml` (harness-neutral) holds `fleet` and `context` only, strictly
  parsed; no addresses, keys or caps, so a cloned repo cannot add a central. It is found by
  walking up from the project dir, stopping at the first `.git` and never above home. A project
  default naming a guarded fleet is ignored: guarded fleets must be named explicitly.
  Precedence: tool argument, then `-context`/`-fleet` flags, then `.mirafleet.yaml`, then the
  context's `default_fleet`.
- **Sessions:** resolved at every open, in order: `-session ID` flag; the handoff file written by
  a hook for (parent PID, project dir); a per-process random `p:<hex>`. Handoff files live in the
  per-user state dir (`sessions/`; unix dir 0700, file 0600; on Windows the owner-only DACL of the
  state dir; atomic write, pruned after 7 days). A planted
  file can only point at another session of the same operator, since central enforces ownership.
  `exec`, `read_file` and `write_file` with no workspace open one implicitly on the default
  fleet, but only if that fleet is not guarded.
- **Output:** `exec`, `run_action` and text `read_file` return at most 30 KiB by default
  (`output_cap_bytes`, 4 KiB to 256 KiB): one third head, two thirds tail, with an omitted-bytes
  marker and `truncated: true`. Output is sanitised first: invalid UTF-8, ANSI escapes and control
  characters are removed, Unicode tag characters dropped and other invisible format characters
  (bidi overrides, zero-width) shown as `<U+XXXX>`. List tools return at most 200 entries.
- **Guarded fleets:** `confirm` is a pass-through argument, never filled from config or cached;
  central enforces `confirm == fleet name`. The refusal text names the context and fleet and tells
  the model to ask the user, and to set `confirm` only after the user explicitly agrees.
- **Action discovery:** heartbeats carry the runner's actions with parameter patterns and
  defaults (at most 128 actions, 64 B names, 32 params, 256 B patterns, 16 KiB encoded), shown in
  `runners.list`. Informational only; the runner's `actions.yaml` stays the boundary.
- **No admin reach:** the only path to central is one operator-method guard that refuses any API
  method that is not an operator method, and central refuses admin and enrol methods on `api@`
  anyway. The tool table is fixed; there is no generic call tool. Revoking runners or operators is
  not reachable from MCP.
- **Nothing forwarded from the MCP server's environment:** it reads env only to locate its config and
  state dirs (`MIRAFLEET_CONFIG_DIR`, `MIRAFLEET_STATE_DIR`, and the OS defaults: home, XDG dirs,
  `%LocalAppData%`), plus its flags, the cwd and the parent PID, all in `cmd/mirafleet/mcp_serve.go`;
  `internal/orch` reads no env. Workspace `env` comes only from the tool's `env` argument,
  checked against the denylist and the runner's `env_allow`. It never reads or uploads local files.
- **Credentials per OS:** the operator key, `known_hosts` and `contexts.yaml` live in the config
  dir: `$XDG_CONFIG_HOME/mirafleet` or `~/.config/mirafleet` (linux), `~/Library/Application
  Support/mirafleet` (darwin), `%LocalAppData%\mirafleet` (windows, not Roaming). Unix: the dir is
  created 0700; the key is checked on load and refused if it has any group/other bits (0600 or
  0400 pass). Windows: a protected DACL (inheritance off, owner is the current user,
  allow entries only for that user, SYSTEM and Administrators), set on create and checked on load;
  anything else fails closed. Handoff state lives in the cache dir (`%LocalAppData%\mirafleet\state`
  on Windows). No OS keychain or passphrase support yet. The server never registers; `mcp login` is
  a human step. Results never contain key material, local paths, env values or registration codes.
- **Connections:** one pinned SSH connection per context, dialled lazily with keepalive; only
  idempotent calls are retried after a redial (never `exec`, `write_file`, `run_action`, `workspace_close`).

### Claude Code plugin

```
integrations/claude-code/
├── plugin/            the plugin root: plugin files only (no Go source)
│   ├── .claude-plugin/plugin.json
│   ├── .mcp.json          node ${CLAUDE_PLUGIN_ROOT}/bin/mirafleet.js mcp
│   ├── hooks/hooks.json   exec-form hooks through the same launcher (no shell, works on Windows)
│   ├── bin/mirafleet.js   Node launcher: runs the installed `mirafleet` found on PATH
│   └── skills/{mirafleet,openfleet,claudefleet}/SKILL.md   the aliases point at the main skill
└── hook/              Go handlers behind `mirafleet mcp hook claude-code <event>`
```

The Anthropic plugin directory accepts only plugin files and flags plugins that start an installed
program directly, so the Go package lives next to the plugin root and the plugin starts the binary
through the launcher (Node 18+). The launcher sets `MIRAFLEET_HARNESS_PID` to Claude Code's PID,
which keys the session handoff for both the hook and the MCP server. The server starts in the
project directory, which `mirafleet mcp` uses by default. See docs/M6.md section 2.18.

- `SessionStart` writes the handoff (`cc:<session_id>`); offline, no output.
- `SessionEnd` removes the handoff if it still names this session and, only for reason `clear` or `logout`, closes the session's
  workspace (best effort, always exits 0). Other reasons leave it for `--resume` and the reaper.
- `PreToolUse` asks the user whenever a mirafleet tool call carries `confirm`; it never allows,
  and malformed input also asks. Safety does not depend on it.

What works without hooks, by harness (`integrations/README.md`):

| Capability | Any MCP harness | opencode | Claude Code plugin |
|---|---|---|---|
| All nine tools | yes | yes | yes |
| Session handoff, `--resume` reattach | no (pass `-session`) | no (pass `-session`) | yes |
| Close workspace at session end | no (reaper) | no (reaper) | yes (`clear`, logout) |
| Confirm prompt on guarded calls | harness approval | harness approval | yes (`PreToolUse`) |
| Slash commands | DIY from the shared body | `/mirafleet`, `/openfleet` | all three |

The shared command body is `integrations/commands/mirafleet.md`. Central enforces `confirm` in
every case. Other harnesses (Kilo Code, Cline, Cursor, ...) are demand-driven. M6 adds the
marketplace, npm and Homebrew; until then `mirafleet` must be on PATH.

## Admin GUI

Served by central on a separate HTTPS listener (`mirafleet central run -gui-listen ADDR`), by
default `0.0.0.0:8443`; `-gui-listen off` disables it. Built with Go `html/template` and htmx 2,
with every asset (fonts included) embedded, so it loads nothing from the internet. It is a second
front end on the same `*service.Service` methods as the admin socket, in the same process, with
source `gui`; it never touches the store or the registry. Linux only, like central. Detailed
design: [M4.md](M4.md).

- **TLS always on** (1.3 minimum). With `-gui-cert`/`-gui-key` (both or neither; the key file must
  be 0600 and owned by central's user) it serves the operator certificate and sends HSTS. Without
  them central generates a self-signed ECDSA P-256 certificate (2 years, renewed under 30 days
  left) in `<data>/gui/tls.crt|tls.key` and never sends HSTS (it would remove the browser's
  "proceed anyway" button and lock the admin out).
- **Startup notice.** Every start logs the GUI URL, the certificate SHA-256 fingerprint and, for a
  non-loopback bind, a warning that the GUI is reachable on all interfaces, with the ways to
  restrict it (`-gui-listen 127.0.0.1:8443` plus `ssh -L`, `off`, or a firewall rule). The same
  text and fingerprint are on the Account screen: compare the fingerprint in the browser before
  typing the setup token or password.
- **Host allowlist** (DNS-rebinding defence): the `Host` header must be `localhost`, one of this
  machine's own interface IPs (re-read every 60 s), its host name, or a name given with
  `-gui-host NAME,...`; anything else gets 421.
- **Bootstrap.** With no admin account, central writes a one-time setup token (valid 24 h, only
  its SHA-256 stored) to `<data>/gui/setup-token` (0600) and logs the path, never the token.
  `/setup` takes the token, a username and a password (Argon2id) and is gone once an admin exists.
  Recovery: `mirafleet central gui reset-admin` (admin socket) retires the admin and every
  session and prints a fresh setup token once. One admin account.
- **Sessions** live in SQLite (migration `0003_gui.sql`: `gui_admins`, `gui_setup`, `gui_sessions`,
  plus audit indexes on `actor` and `request_id`) as SHA-256 hashes of the cookie token, so they
  survive a restart. A session ends after 30 min idle or 12 h absolute and is bound to the admin's
  credential generation, so a password change or reset kills the others. Cookies are `__Host-`,
  `Secure`, `HttpOnly`, `SameSite=Strict`; state-changing routes have CSRF protection. Login is
  rate limited per client address, with a 15 minute lockout after 10 consecutive failures.
- **CSP.** `default-src 'none'; script-src 'self'; style-src 'self'; ... frame-ancestors 'none';
  base-uri 'none'`: no inline script or style. Screens work without JavaScript; htmx only adds
  live refresh.
- **Not in the GUI:** running actions or workspace commands (view and manage only), more than one
  admin, fleet delete.

Screens: **Setup**, **Login**, **Overview**, **Fleets** (create, workspace settings, confirm flag),
**Runners** (status, labels, heartbeat, approve, disable, enable, revoke; revoked runners can be
listed), **Join tokens** (create, revoke), **Operators** (current registration code, rotate
secret, revoke), **Workspaces** (active sessions, age, kill), **Audit** (filter by actor, source,
action or prefix, fleet, runner, workspace, request ID and time range; export), **Account**
(change password, list and revoke sessions, theme).

Audit actions the GUI flows write: `gui.setup_token`, `gui.setup`, `gui.login`, `gui.logout`,
`gui.lockout`, `gui.password_change`, `gui.session_revoke`, `gui.reset_admin` (source `cli`) and
`audit.export` (written before the export streams). Lifecycle changes (`fleet.create`,
`fleet.set_confirm`, `token.*`, `runner.*`, `operator.*`, `workspace.kill`) reuse the CLI actions
with source `gui`.

New admin methods, on the admin socket and the GUI service only (never `api@mirafleet` or MCP):
`audit.list` (filtered reads), `fleet.set_confirm` and `gui.reset_admin`. CLI parity:
`central gui reset-admin`, `central fleet set NAME -confirm=BOOL`, and filter flags on
`central audit` (`-fleet -runner -actor -source -outcome -action [-action-prefix] -workspace
-request-id -from -to -before`).

## Enrollment

```bash
# on central, or in the GUI
mirafleet central fleet create uat -mode actions
mirafleet central token create -fleet uat -labels role=jump,region=eu     # prints the token once
# on the runner (token on stdin; generates a key, pins central's host key from the token)
mirafleet runner join central.example.com:2222 -name jump1 -token -
mirafleet central runner approve uat/jump1                                # unless token create -approve
# on central, or in the GUI: current operator registration code (rotates every 60 s)
mirafleet central operator code
# operator, on their machine (code on stdin; pins central's host key from the code)
mirafleet mcp login central.example.com:2222 -name durga -code -
```

Join tokens and registration codes are described under
[Join tokens and registration codes](#join-tokens-and-registration-codes).

## Distribution

One git tag `vX.Y.Z` (pre-releases `vX.Y.Z-rc.N`) produces every channel from one build. The
design is in [M6.md](M6.md); the maintainer's checklist is [RELEASING.md](RELEASING.md).

### Channels

| Channel | For | What it carries |
|---|---|---|
| GitHub release | everyone | archives for six targets (`tar.gz`, `zip` on Windows), `.deb` `.rpm` `.apk`, `checksums.txt` with its signature bundle, SBOMs, `install.sh`, `install.ps1`, `THIRD_PARTY_LICENSES.txt` |
| OCI image | central, actions runners | `docker.io/mirastack/mirafleet` and `ghcr.io/mirastacklabs-ai/mirafleet` |
| Docker Compose | central, runners, one-machine lab | uses the image |
| Homebrew tap, `install.sh`, `install.ps1` | operators (laptops) | the binary |
| npm `@mirastacklabs/mirafleet` | MCP configs on any OS | launcher plus six platform packages |
| MCP Registry, Claude Code marketplace | discovery | point at npm and at this repository |

Central and runners are Linux only, so they come from packages, the image or Compose. Operators
(`mirafleet mcp`) use any channel. mirafleet sends no telemetry.

### Units and packages

The Linux packages install the binary, `/etc/mirafleet/{central,runner}.env`, example configs,
the licence files and three systemd units:

- `mirafleet-central.service`: user `mirafleet`, `central init` before `central run`, a strict
  sandbox, writes only to `/var/lib/mirafleet/central` (database, backups, GUI certificate,
  setup-token file, admin socket).
- `mirafleet-runner.service`: actions mode, user `mirafleet`, starts only when `runner.yaml` exists.
- `mirafleet-runner-workspace.service`: container workspaces, root, after `containerd`, no sandbox
  directives (the runner creates namespaces and cgroups).

Host mode (M7) adds a per-user runner unit (`/usr/lib/systemd/user/mirafleet-runner.service`) and
a template unit (`mirafleet-runner@.service`, `User=%i`); `pkgtest` runs `systemd-analyze verify`
on all five units, and on the user unit with `--user`.

### The OCI image

`FROM scratch`, the static binary at `/usr/bin/mirafleet`, licence files under
`/usr/share/doc/mirafleet/`, user `65532`, volume `/var/lib/mirafleet`, ports 2222 and 8443. Only
`COPY` steps, so one buildx run builds `linux/amd64` and `linux/arm64`. It is not distroless
(that would bundle MPL-licensed `ca-certificates`). The image runs central and an actions-mode
runner; container workspaces and host mode need host containerd or a real OS user, so they are
not supported in it. First start needs `central init` (a one-shot service in Compose). No
certificate, key or secrets key is baked in.

One build pushes one manifest list to both registries, so the digest is the same. Tags are
`X.Y.Z`, `X.Y` and `latest`; a pre-release gets only `X.Y.Z-rc.N`.

### Signing and provenance

- `checksums.txt` is signed keyless with cosign (GitHub OIDC; no key to keep). The signature is a
  single sigstore bundle, `checksums.txt.sigstore.json`. Verify with `cosign verify-blob --bundle`
  and the exact identity: the `release.yml` workflow of this repository at `refs/tags/v*`, issuer
  `https://token.actions.githubusercontent.com`. `install.sh` and `install.ps1` always check the
  SHA-256; `install.sh --verify-signature` also requires the cosign check.
- Every archive and package has a build-provenance attestation (`gh attestation verify FILE --repo
  mirastacklabs-ai/mirafleet`). The image is cosign-signed and carries provenance in both registries.
- SBOMs (CycloneDX) are published per artifact. npm packages are staged by CI with provenance and approved by the maintainer with 2FA.
- No Apple or Windows code signing; the docs give quarantine one-liners for browser downloads.
- `release.yml` runs `verify` and `build` without credentials; every publishing job waits for
  approval in the protected `release` environment, and each credential belongs to one job.

### Protocol levels and version skew

The protocol level is one integer: the release's minor version in 0.x (v0.6.x is 6), the major
version from 1.0. Central at level N accepts runners and operators at N and N-1; anything else is
refused with a message that says who must upgrade ("upgrade central first" when the peer is
newer). A missing level counts as 0 and is refused. Upgrade order: central, then runners, then
operators. The rule lives in `internal/protover` (a pure package). At 1.0 the level restarts at
1, so a 1.x central does not accept 0.x peers; this is called out in the 1.0 notes.

The wiring adds `proto` to the runner heartbeat and to the operator `hello`. Central refuses a
mismatch on the first heartbeat, audits `runner.refused_version` and closes; the runner logs the
message and backs off to its maximum interval. The orchestrator maps a refusal to a tool error.
The wire protocol itself has its own version list (`api.SupportedVersions`), separate from the level.

### Upgrades and backups

Before central migrates its SQLite database, it takes an online, consistent copy with `VACUUM INTO`
to `<data>/backups/mirafleet-schema<N>-<UTC time>.db` (directory 0700, file 0600) and keeps the 3
newest. A fresh database makes no backup. If the backup fails, central does not migrate and does
not start. A database newer than the binary is refused. To roll back: stop central, reinstall the
previous version, move the database and its `-wal` and `-shm` files aside, copy the newest backup
back, start. Writes after the upgrade are lost. Backups hold the encrypted credentials store, so a
restore needs the same `MIRAFLEET_SECRETS_KEY`.

## Security summary

- Runners and operators authenticate with Ed25519 keys. Central's host key is pinned on join and
  login, so there is no trust-on-every-connect.
- Actions are argv-only with regex-validated parameters. Workspaces are unprivileged containers
  (user namespace, no capabilities, read-only rootfs, no network by default) with image and env
  allowlists; a workspace runner itself is root-equivalent on its host.
- Agent API keys never enter a workspace.
- Runners never accept inbound network connections.
- The GUI listens on all interfaces by default (`0.0.0.0:8443`), always over TLS, behind a setup
  token, a Host allowlist, CSRF and a strict CSP. Restrict it with `-gui-listen 127.0.0.1:8443`,
  `-gui-listen off` or a firewall rule; central logs a notice at every start.
- Audit is written on central (fleet-wide view) and on each runner (local copy).
- Known OSS limits: no per-fleet operator grants, central is a single trust point, one admin.

## OSS vs Enterprise

The OSS core ships the fundamentals above under Apache-2.0. Enterprise features will be built
based on demand, behind Go interfaces defined in the core (`Store`, `AuthProvider`,
`PolicyEngine`, `AuditSink`), so they plug in rather than fork. Candidates: per-fleet RBAC, SSO,
approval workflows, SSH CA with short-lived certs, HA with Postgres, SIEM export, federation of
centrals, session recording.
