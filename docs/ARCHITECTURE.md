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
  (channel types `exec@mirafleet`, `file@mirafleet`, `workspace@mirafleet`). The runner
  accepts only these types; there is no shell, port forwarding or other channel.
- **Orchestrator → central:** the orchestrator connects with an operator key and makes
  JSON requests on an `api@mirafleet` channel (list fleets, list runners, open workspace,
  exec, read/write file, close workspace).

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
      └── workspace   session id, orchestrator, runner, image, created, last used
operator              name, public key, status (pending | active | revoked)
audit                 time, operator, fleet, runner, workspace, command/action, exit, duration
```

**Fleets** (`prod`, `uat`, `dev`, …) are the unit of isolation. A runner belongs to exactly one
fleet. Join tokens are scoped to a fleet. In OSS v1, any active operator can use any fleet.
Per-fleet grants are an enterprise feature.

**Runner lifecycle:** `pending → active → disabled → revoked`. An admin approves a runner after
it joins. A disabled runner stays connected (visible) but refuses work. A revoked runner's key is
rejected.

Lifecycle changes (approve, disable, enable, revoke) are operations on central. They are
exposed through three equivalent front ends that share one code path:

| Front end | Example |
|---|---|
| GUI | Runners screen → Revoke |
| CLI on central | `mirafleet central runner revoke <fleet>/<runner>` |
| Orchestrator (MCP) | `runner_lifecycle(fleet, runner, action="revoke", confirm="<fleet>/<runner>")` |

Revoke takes effect immediately. Central marks the key revoked, closes the runner's live
connection, cancels in-flight work, and rejects every later login. Disable keeps the connection
but refuses new work. Every change is audited with who made it and from which front end.

Lifecycle tools are off by default in `mirafleet mcp` (`lifecycle_tools: true` in the
orchestrator config turns them on) and always require the `confirm` argument. Without these
guards, a prompt injection in command output could talk an agent into revoking runners.

## Fleet modes

| Mode | Use | What runs | Executor |
|---|---|---|---|
| `actions` | ops, typically prod | only named actions from the runner's `actions.yaml` | host shell, as the runner user |
| `workspace` | coding, typically dev/uat | any command, inside the session's workspace container | containerd |

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
several orchestrators can work on different projects on the same runner at once.

- **Executor:** containerd, through its Go client. There is no Docker Engine dependency. It works
  on Kubernetes nodes and on hosts with the `docker.io` package (which runs on containerd).
  Workspaces live in the containerd namespace `mirafleet`, away from the kubelet's `k8s.io`.
- **Layout:** host dir `/var/lib/mirafleet/ws/<workspace-id>/` is bind-mounted at `/workspace`
  in a long-lived container (`sleep infinity`) created from the fleet's default image or the
  image requested. Commands run via containerd exec with `cwd=/workspace`.
- **Isolation:** one container per workspace, with only its own directory mounted, CPU and memory
  limits, no host network namespace sharing beyond default, and no privileged mode.
- **Lifecycle:** `workspace_open` → central schedules onto a runner in the fleet with matching
  labels and a free slot (sticky for the session) → `exec` / `read_file` / `write_file` →
  `workspace_close` deletes the container and directory. A TTL reaper (default 24h idle) catches
  sessions that died without closing.
- **Concurrency:** each runner sets `concurrent: N` in its config. Central never schedules more
  than N open workspaces on that runner.
- **Repos:** `workspace_open` may take a repo URL and ref, cloned inside the container using
  git credentials configured on the runner. Otherwise Claude clones via `exec`.

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

| Tool | Purpose |
|---|---|
| `list_fleets` | fleets on the configured central(s), with mode |
| `list_runners` | runners in a fleet: status, labels, free slots |
| `run_action` | actions mode: run a named action on a runner |
| `workspace_open` | workspace mode: get a workspace for this session |
| `exec` | run a command in the session's workspace |
| `read_file` / `write_file` | file access inside the workspace |
| `workspace_close` | release the workspace |
| `runner_lifecycle` | approve, disable, enable or revoke a runner (opt-in, needs `confirm`) |

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
(approve, revoke), **Workspaces** (active sessions, age, kill), **Audit** (filter, export).
One admin account, created on first start.

## Enrollment

```bash
# on central, or in the GUI
mirafleet central token create --fleet uat --labels role=jump,region=eu
# on the runner
mirafleet runner join central.corp:2222 <token>   # generates key, pins host key, installs systemd unit
# operator, on their machine
mirafleet mcp login central.corp:2222             # registers operator key → approve in GUI
```

## Security summary

- Runners and operators authenticate with Ed25519 keys. Central's host key is pinned on join and
  login, so there is no trust-on-every-connect.
- Actions are argv-only with regex-validated parameters. Workspaces are unprivileged containers.
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
