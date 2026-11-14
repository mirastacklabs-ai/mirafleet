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
linux, darwin and windows (Windows key protection lands in M5; until then the key loader fails
closed).

## Fleet modes

| Mode | Use | What runs | Executor |
|---|---|---|---|
| `actions` | ops, typically prod | only named actions from the runner's `actions.yaml` | host shell, as the runner user |
| `workspace` | coding, typically dev/uat | any command, inside the session's workspace container | containerd (default) or OpenSandbox (opt-in) |

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
- **Env:** must be in the runner's `env_allow` and pass a hard denylist (`*_API_KEY`, `*_TOKEN`,
  `*_SECRET*`, `*_PASSWORD`, `*_KEY`, `ANTHROPIC_*`, `OPENAI_*`, `AWS_*`, `LD_*`, `*PROXY*`,
  `SSH_AUTH_SOCK`, `GIT_ASKPASS`, `GIT_SSH_COMMAND`, `BASH_ENV`, `ENV`, `NODE_OPTIONS`,
  `PYTHONSTARTUP`; source of truth `workspace.EnvDenied`), enforced on central and on the runner. Agent API keys never enter a workspace; the MCP server
  forwards nothing from its own environment.
- **Exec timeout:** per fleet on central, default 2 min, max 30 min, hard ceiling 24 h in code;
  the runner's `limits.exec_max` also caps it.
- **Other limits:** `concurrent: N` slots per runner, 8 workspaces per operator, files up to
  `limits.max_file` (required, e.g. 64 MiB) moved in chunks of at most 1 MiB; the runner's
  `max_age` caps the fleet max TTL and drives `runner workspace prune`.
- **Guarded fleets** (`confirm`): `confirm == fleet name` on `open`, `exec` and `write_file`,
  and a secure runtime (gVisor/Kata) is required on both executors.
- Repo cloning with runner-held credentials is not built; Claude clones via `exec`, so no git
  secrets live in workspaces.

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

## Orchestrator

`mirafleet mcp` is a stdio MCP server. Claude Code is the first supported harness. Because the
core is plain MCP, any MCP-capable harness (opencode, Kilo Code and others) can use it, and each
gets a thin integration under `integrations/<harness>/`.

**Harness-agnostic rule:** correctness and safety never depend on harness-specific features such
as hooks. Hooks only add convenience.

| Concern | Works in every harness (core) | Claude Code extra (`integrations/claude-code/`) |
|---|---|---|
| Workspace per session | one MCP process per session mints its workspace | `SessionStart` hook passes the session ID, so `--resume` reattaches |
| Cleanup | MCP process exit marks the workspace idle; TTL reaper removes it | `SessionEnd` hook closes it immediately |
| Prod guard | fleets with `confirm: true` require a `confirm: "<fleet>"` argument; the harness's own tool-approval prompt applies | `PreToolUse` hook adds an explicit confirmation |
| Install | `mirafleet mcp` added to the harness's MCP config | plugin with `.mcp.json`, hooks and slash commands |
| Commands | `/mirafleet`, alias `/openfleet` (harness custom-command files) | `/mirafleet`, aliases `/openfleet` and `/claudefleet` (plugin skills) |

`mirafleet mcp` exposes these tools:

M5 builds these tools on the API methods that already exist from M2/M3:

| Tool | Purpose | Backed by |
|---|---|---|
| `list_fleets` | fleets on the configured central(s), with mode | `fleets.list` |
| `list_runners` | runners in a fleet: status, labels, free slots | `runners.list` |
| `run_action` | actions mode: run a named action on a runner | `action.run` |
| `workspace_open` | workspace mode: get a workspace for this session | `workspace.open` (session = harness id or per-process random) |
| `exec` | run a command in the session's workspace | `workspace.exec` (`["/bin/sh","-c",cmd]`, then a 30 KB head+tail cap) |
| `read_file` / `write_file` | file access inside the workspace | `workspace.read_file` / `workspace.write_file`, chunked by offset |
| `workspace_close` | release the workspace | `workspace.close` |

- **Contexts:** `~/.config/mirafleet/config.yaml` lists one or more centrals (for example a
  private-network central and a cloud central), with an optional SSH jump host per central.
- **Project default:** `.mirafleet.yaml` (repo root, harness-neutral) with `fleet: uat` sets the default fleet for
  sessions opened in that repo. Prod is used only when named explicitly.
- **Output:** command output returned to Claude is capped (default 30 KB, head plus tail) so a
  noisy build cannot flood the context.

## Admin GUI

Served by central on a separate HTTPS listener bound to `127.0.0.1:8443` by default. Admins reach
it with `ssh -L` or place it behind their own reverse proxy. Built with Go `html/template` and
htmx, with all assets embedded, so it loads nothing from the internet.

Screens: **Fleets** (create, mode, default image, confirm flag), **Runners** (status, labels,
heartbeat, approve, disable, revoke), **Join tokens** (create, revoke), **Operators**
(current registration code, revoke), **Workspaces** (active sessions, age, kill), **Audit** (filter, export).
One admin account, created on first start.

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
mirafleet mcp login central.example.com:2222 -name alice -code -
```

Join tokens and registration codes are described under
[Join tokens and registration codes](#join-tokens-and-registration-codes).

## Security summary

- Runners and operators authenticate with Ed25519 keys. Central's host key is pinned on join and
  login, so there is no trust-on-every-connect.
- Actions are argv-only with regex-validated parameters. Workspaces are unprivileged containers
  (user namespace, no capabilities, read-only rootfs, no network by default) with image and env
  allowlists; a workspace runner itself is root-equivalent on its host.
- Agent API keys never enter a workspace.
- Runners never accept inbound network connections.
- The GUI is not exposed publicly by default.
- Audit is written on central (fleet-wide view) and on each runner (local copy).
- Known OSS limits: no per-fleet operator grants, central is a single trust point, one admin.

## OSS vs Enterprise

The OSS core ships the fundamentals above under Apache-2.0. Enterprise features will be built
based on demand, behind Go interfaces defined in the core (`Store`, `AuthProvider`,
`PolicyEngine`, `AuditSink`), so they plug in rather than fork. Candidates: per-fleet RBAC, SSO,
approval workflows, SSH CA with short-lived certs, HA with Postgres, SIEM export, federation of
centrals, session recording.
