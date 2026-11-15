# Glossary

Every term used in the mirafleet guide and code, alphabetical. Each entry has a short plain
definition and a link to the chapter that explains it properly. Examples use the MIRASTACK LABS running
example (central `central.example.com:2222`, fleets `dev` and `prod`, runners `build-1` and
`db-1`, operators `durga` and `kalyan`).

Jump to: [A](#a) [B](#b) [C](#c) [D](#d) [E](#e) [F](#f) [G](#g) [H](#h) [I](#i) [J](#j)
[K](#k) [L](#l) [M](#m) [N](#n) [O](#o) [P](#p) [R](#r) [S](#s) [T](#t) [U](#u) [W](#w) [Y](#y)
and [symbols](#symbols)

## Symbols

**/64.** An IPv6 address block: the first 64 bits are fixed, so one block holds about 2^64
addresses. One home or server network usually owns a whole /64, so central counts IPv6 clients
per /64, not per single address. See [chapter 2](02-background.md#28-rate-limiting-token-bucket).

## A

**AES-256-GCM.** The cipher central uses to store credential values: a 32-byte key, a fresh random
12-byte [nonce](#n) per value, and an authentication tag that makes any change to the stored bytes
detectable. In `internal/secrets`. See [chapter 2.12](02-background.md#212-host-mode-background-secrets-you-can-use-but-not-read-m7).

**Agent forwarding.** `ssh -A`: lets a remote machine use your local ssh-agent through the SSH
connection. Convenient, but the remote side can ask the agent to sign for as long as you are
connected (see [signing oracle](#s)). mirafleet does not forward; it starts a restricted agent per
session on the VM instead. See [chapter 2.12](02-background.md#212-host-mode-background-secrets-you-can-use-but-not-read-m7).

**Associated data (AAD).** Extra bytes that AES-GCM authenticates without encrypting. mirafleet uses
the credential id and version, so a ciphertext cannot be moved to another row. See
[chapter 6.17](06-data-model.md#617-host-mode-and-credentials-migration-0004-m7).

**Action.** A named, pre-approved command that a runner may run, defined in `actions.yaml`, for
example `tail-logs`. An action is an argv array plus optional validated parameters and a timeout.
See [chapter 3](03-architecture.md) and [chapter 5](05-security.md).

**actions mode.** A fleet mode in which only named actions from the runner's `actions.yaml` can
run (no arbitrary commands). Built today. See [chapter 1](01-what-is-mirafleet.md#14-two-fleet-modes).

**actions.yaml.** The runner-side file listing every action the runner will perform. It lives on
the machine being protected and is the security boundary. See [chapter 5](05-security.md).

**Actions guard.** See [guard](#g).

**Admin.** The person (in our examples, aarvee) or, later, the GUI that manages central: creates
fleets and tokens, approves and revokes runners, reads the registration code. Admin work happens
only on central. The admin is not an operator. See [chapter 10](10-operations.md).

**Admin socket.** A Unix socket `<data>/admin.sock` on the central host. The `mirafleet central ...`
commands talk to the running central through it. See [chapter 7](07-protocols.md) and
[chapter 2](02-background.md#211-unix-sockets-file-permissions-and-umask).

**api@mirafleet.** The SSH channel type operators use to send JSON requests to central
(`hello`, `fleets.list`, `runners.list`, `action.run`). Nothing else is reachable on it. See
[chapter 7](07-protocols.md).

**Append-only.** A table that allows inserts but never changes or deletes. The `audit` table
enforces this with database triggers. See [chapter 6](06-data-model.md).

**argv.** The list of strings a program is started with: the program name, then one string per
argument (`["journalctl", "-u", "gitlab-runner"]`). No shell is involved, so no string can be
re-read as extra commands. See [chapter 5](05-security.md).

**Atomic / atomicity.** A group of changes that either all happen or none do, typically inside one
database transaction. See [chapter 2](02-background.md#26-sqlite-transactions-and-wal).

**Audit / audit row.** A permanent record of who did what, when and with what outcome. Each
`action.run` writes a `start` row before dispatch and an outcome row after, sharing a
`request_id`. See [chapter 6](06-data-model.md) and [chapter 4](04-flows.md).

**AuditSink.** The Go interface for recording audit events (`Record(ctx, Event) error`). Central
uses the SQLite store; the runner writes JSON lines to a file. See [chapter 8](08-code-tour.md).

**AuthProvider.** The Go interface that turns an SSH public key into a principal. One of four
"enterprise seams". See [chapter 8](08-code-tour.md).

## B

**Background job / background shell.** A host-mode command started with `bash {run_in_background:
true}`. A hidden `mirafleet host-shim job` supervises it in its own process group, logging to files;
poll with `bash_output`, re-read with `read_output`, stop with `kill_shell`. Survives a runner
restart. See [flow 27](04-flows.md#27-a-background-job).

**Broker.** The runner-local HTTPS proxy of host mode (`internal/workspace/broker`). For credential
hosts only it terminates TLS with a per-session CA, injects the stored `Authorization` header and
does the registry token exchange; other hosts are plain tunnels. See
[chapter 2.12](02-background.md#212-host-mode-background-secrets-you-can-use-but-not-read-m7) and [5.12](05-security.md#512-host-mode-and-credentials-m7).

**Body frame.** A second length-prefixed frame sent right after a workspace request frame,
carrying a file or stdin (at most 1 MiB). Central reads it only after the caller has been
authorised. See [chapter 4](04-flows.md#14-reading-and-writing-files-body-frames) and
[chapter 7](07-protocols.md).

**Backoff.** Waiting longer after each failed reconnect attempt. The runner starts at 1 s and
doubles up to 60 s (with about 20% random jitter), resetting after a connection stays healthy for
30 s. See [chapter 3](03-architecture.md).

**base32 / base64url.** Ways to write binary bytes as text. A registration code body is base32;
secrets and hashes in tokens are base64url. See [chapter 7](07-protocols.md).

**Bearer secret.** A secret that works for whoever holds it, with no further proof (a join token
is one). Anyone who copies it can use it, so it is random, hashed at rest and short-lived. See
[chapter 5](05-security.md).

**Big-endian.** Writing the most significant byte first. The 4-byte frame length is big-endian.
See [chapter 7](07-protocols.md).

## C

**Corporate CA.** A company's own certificate authority, which signs the certificates of internal
servers such as `gitlab.example.com`. Host mode trusts it for upstream TLS (`corporate_ca_file` in
`workspace.yaml` on the runner; `MIRAFLEET_CORPORATE_CA_FILE` for central's GitLab rotation).

**Credential.** A secret stored on central by name (kind `ssh-key`, `https-token` or `registry`,
with the hosts it applies to), encrypted at rest, write-only, assigned to runners or fleets. The
agent can use it but never read it. See [chapter 6.17](06-data-model.md#617-host-mode-and-credentials-migration-0004-m7).

**Credential holder.** The runner's in-memory set of delivered credentials (`internal/runner/creds.go`),
replaced atomically by each `creds_sync`. Never written to disk.

**`creds_sync`.** The operation central sends on `workspace@mirafleet` to give a host runner its full
effective credential set, on connect and on every change. See [flow 28](04-flows.md#28-credential-assign-delivery-use-and-rotation).

**Cancel / cancellation.** Telling running work to stop, in Go through a `context`. Revoke
cancels in-flight actions; the audit outcome is `cancelled`. See
[chapter 2](02-background.md#27-go-context-cancellation).

**Central.** The one server per network that runners and operators connect to. It authenticates
everyone, applies policy, relays work and keeps the database and audit log. Linux only. See
[chapter 1](01-what-is-mirafleet.md#13-the-three-roles) and [chapter 3](03-architecture.md).

**Channel (SSH).** A numbered stream inside an SSH connection with a type string. mirafleet's
types: `api@mirafleet`, `exec@mirafleet`, `join@mirafleet`, `register@mirafleet`. See
[chapter 2](02-background.md#22-ssh) and [chapter 7](07-protocols.md).

**CLI.** Command-line interface: the `mirafleet` program and its subcommands. See
[chapter 9](09-dev-setup-and-testing.md).

**Confirm / confirm guard.** On a fleet created with `-confirm`, `action.run` must carry a
`confirm` string equal to the fleet's name (such as `"prod"`). It is a typed second check, not a
boolean. See [chapter 5](05-security.md).

**Constant-time comparison.** Comparing two secrets in time that does not depend on how many
bytes match, to avoid timing attacks. See
[chapter 2](02-background.md#24-hashes-hmac-and-constant-time-comparison).

**Capabilities (Linux).** About 40 small switches that split "root's powers" (change file owners,
mount, use raw sockets, load modules...). A process with none of them is a very weak "root".
Workspace containers drop all of them and set *no new privileges*. See
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox) and
[chapter 5](05-security.md#59-the-workspace-container-boundary).

**cgroup (control group).** A Linux feature that limits what a group of processes may *use*: CPU,
memory, number of processes. It stops a fork bomb or memory hog from harming the host. Contrast
with a namespace, which limits what a process can *see*. See
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox).

**Container.** A fenced-in Linux process with its own file system view and resource limits. Used
by workspace mode (built in M3). See [chapter 2](02-background.md#29-containers-containerd-and-opensandbox).

**containerd.** The Linux program that runs containers. The product's workspaces (M3) use it
through its Go client, in its own namespace `mirafleet`; our developer tooling runs in Docker
containers (colima on macOS). Access to its socket is equivalent to root on the host. See
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox) and
[chapter 3](03-architecture.md).

**context (Go).** A value carrying cancellation and deadlines through function calls. See
[chapter 2](02-background.md#27-go-context-cancellation).

**context (contexts.yaml).** A different thing with the same word: a named central the operator
can talk to (a name, an address and an optional `default_fleet`), listed in the operator's
`contexts.yaml`, like a `kubectl` context. The MCP server keeps one SSH connection per context
and every tool result names the context it used. `mcp context list|use|remove` edits the list.
The pinned host key lives in `known_hosts`. See
[chapter 6](06-data-model.md#616-local-files-on-the-operators-machine-and-heartbeat-actions-m5-no-schema-change)
and [chapter 3](03-architecture.md#operator-side).

**CVE.** "Common Vulnerabilities and Exposures": a public id for a known security bug, such as
CVE-2024-45337 in the Go SSH library. See [chapter 5](05-security.md).

## D

**Dumpable / non-dumpable.** A Linux process flag. A non-dumpable process (`prctl(PR_SET_DUMPABLE, 0)`)
cannot be `ptrace`d by other non-root processes, its `/proc/<pid>/{mem,environ,fd,...}` are closed to
them, and it leaves no core dump. The host runner sets it before loading any secret. See
[chapter 5.12](05-security.md#512-host-mode-and-credentials-m7).

**DACL (discretionary access-control list).** The Windows list of "which account may do what" on
a file or folder. Windows has no `0600`, so mirafleet creates the operator key and its directory
with a *protected* DACL (no inheritance) that allows only the current user, SYSTEM and
Administrators, and checks it on every load, failing closed otherwise. See
[chapter 5](05-security.md#511-prompt-injection-through-tool-output) and
[chapter 3](03-architecture.md#where-does-each-secret-and-key-live).

**Data dir.** The directory where central keeps its state (default `/var/lib/mirafleet/central`):
host key, SQLite database, admin socket, lock file. A runner has its own data dir with
`runner.yaml`, key and `known_hosts`. See [chapter 10](10-operations.md).

**Data-dir lock.** An exclusive `flock` on `<data>/central.lock` so a second central cannot use
the same directory. See [chapter 10](10-operations.md).

**Deny by default.** Anything not explicitly allowed is refused. The policy engine and the
runner's guard both work this way. See [chapter 5](05-security.md).

**Disabled.** A runner status: still connected and visible, finishes in-flight work, but takes no
new work. `enable` reverses it. See [chapter 6](06-data-model.md).

**DTO (data transfer object).** A plain struct that only carries data across a boundary (for
example the JSON shape of an API reply), with no logic. See [chapter 3](03-architecture.md).

**Domain separation.** Putting a fixed label (`mirafleet-register-v1`) into a hash or HMAC input so
the result cannot be reused for another purpose. See
[chapter 2](02-background.md#24-hashes-hmac-and-constant-time-comparison).

**Double gate (now three gates).** The method table and the policy are two
independent gates; admin-only calls add a direct check of the caller's kind as a third. Admin
methods are absent from the operator method table, and the policy denies them as well. See
[chapter 5](05-security.md).

## E

**Effective set.** The credentials a runner holds: its own assignments united with those of its
fleet. See [chapter 6.17](06-data-model.md#617-host-mode-and-credentials-migration-0004-m7).

**Enrolment key.** A runner's own private key, kept on its disk so it can reconnect. In host mode the
agent runs as the same user and can read it; this is an accepted open-source gap, with a
`runner.replaced` audit row as the only trace. See [chapter 5.12](05-security.md#512-host-mode-and-credentials-m7).

**Ed25519.** The public-key signature algorithm mirafleet uses (SSH name `ssh-ed25519`); the only
key type accepted. See [chapter 2](02-background.md#21-public-key-cryptography-and-ed25519).

**Enroll (SSH user).** The SSH user for brand-new, not-yet-known callers. It may open exactly one
channel (`join@mirafleet` or `register@mirafleet`), within 30 s, rate-limited. See
[chapter 7](07-protocols.md).

**Enrolment / enroll.** Becoming known to central: a runner by `runner join`, an operator by
`mcp login`. See [chapter 4](04-flows.md).

**Envelope.** The JSON wrapper around every API message: `{"v":1,"id","method","params"}` for
requests and `{"v":1,"id","result"}` or `{"error":{...}}` for responses. See
[chapter 7](07-protocols.md).

**Error code.** One of `bad_request unauthenticated forbidden not_found conflict unavailable
too_large timeout unsupported_version internal`. See [chapter 7](07-protocols.md).

**exec@mirafleet.** The channel type central opens toward a runner (over the runner's own
connection) to run an action. See [chapter 7](07-protocols.md).

**Egress proxy.** The small HTTP/CONNECT proxy the runner starts inside a workspace's network
namespace. It is the container's only way out when the fleet's network is `egress`: it checks
every request against the allowlist, resolves DNS itself and blocks loopback, metadata and private
addresses. See [chapter 2](02-background.md#29-containers-containerd-and-opensandbox) and
[chapter 5](05-security.md#59-the-workspace-container-boundary).

**Executor.** The runner-side component that actually runs work. Actions mode runs argv on the
host through the guard. In workspace mode, a `workspace.Executor` (Go interface in
`internal/workspace`) runs commands in containers: `ctrd` (containerd, the default) or `osb`
(OpenSandbox, opt-in per fleet). See [chapter 3](03-architecture.md).

## F

**fsops.** `internal/workspace/fsops`, the pure Go package behind the host-mode file tools (read,
write, edit, multi_edit, notebook_edit, ls, glob, grep). It runs in the runner process as its OS user.
See [chapter 3](03-architecture.md#host-mode-the-m7-runner).

**Fail closed.** When something goes wrong, refuse rather than allow. Example: if the `start`
audit row cannot be written, the action does not run. See [chapter 5](05-security.md).

**flock.** A Unix file lock. Central takes an exclusive `flock` on `<data>/central.lock`; the
kernel drops it if the process dies, so a crash never leaves a stale lock. See
[chapter 10](10-operations.md).

**Foreign key.** A database column that must point at an existing row in another table (a
runner's `fleet_id` must be a real fleet). The database refuses changes that would break it. See
[chapter 6](06-data-model.md).

**Front end.** A way into central's one `Service` layer: the operator API (`api@mirafleet`), the
admin socket, and the enroll channels. Each authenticates its own way, then calls the same
service code. See [chapter 3](03-architecture.md).

**Fingerprint.** A short hash of a public key (like `SHA256:...`) used to name and compare keys.
Stored beside each key. See [chapter 2](02-background.md#21-public-key-cryptography-and-ed25519).

**Fleet.** A named group of runners and the unit of isolation: `dev`, `prod`. A runner belongs to
exactly one fleet; each fleet has a mode (`actions` or `workspace`). See
[chapter 6](06-data-model.md).

**Frame.** One length-prefixed message: 4 bytes (big-endian) giving the JSON length, then the
JSON. See [chapter 7](07-protocols.md).

## G

**goroutine.** A lightweight concurrent function in Go, started with `go f()`. Thousands can run
at once; a context tells them when to stop. See [chapter 2](02-background.md#27-go-context-cancellation).

**Guard (actions guard).** The code in `internal/actions` on the runner that checks an action
name and its parameters against `actions.yaml`, then runs it without a shell. This is the
security boundary. See [chapter 5](05-security.md).

**Guarded fleet.** A fleet created with `-confirm`. Every state-changing call on it (`run_action`,
`workspace_open`, `exec`, `write_file`) must carry `confirm` equal to the fleet's name, or central
refuses with `bad_request` and writes a `denied` audit row. The MCP server never fills `confirm`
itself and tells the model to ask the human first; a guarded fleet is never used as a default.
See [chapter 5, 5.3.7](05-security.md#537-guarded-fleets-confirm-must-equal-the-fleet-name) and
[flow 23](04-flows.md#23-a-guarded-call-and-the-confirm-prompt).

## H

**Host binding.** The ssh-agent check using `session-bind@openssh.com` (OpenSSH 8.9 and later): the
agent signs only if the server's host key, reported by the client, is pinned in the credential's
`known_hosts`. Can be set `off` per credential. See [chapter 5.12](05-security.md#512-host-mode-and-credentials-m7).

**Host mode.** Workspace mode with `executor: host`: tools run directly as the runner's own OS user on
a VM, without a container; credentials come from central. See
[chapter 1.4a](01-what-is-mirafleet.md#14a-host-mode-remote-development-on-a-vm-m7).

**hostx.** `internal/workspace/hostx`, the host executor (hardening, session directories, bash,
jobs, cleanup). `host-shim` is the hidden `mirafleet host-shim job` that supervises one background shell.

**Harness.** The program that hosts an AI agent loop and connects it to tools: Claude Code,
opencode, Kilo Code. It launches MCP servers, shows tool results to the model, and asks the human
for permission. See [chapter 2](02-background.md#210-mcp-and-harnesses).

**Harness-agnostic.** Core behaviour and safety do not depend on any one harness's features;
harness-specific code lives only in `integrations/<harness>/`. See
[chapter 2](02-background.md#210-mcp-and-harnesses).

**Hash.** A fixed-size fingerprint of data (SHA-256 here); one-way. See
[chapter 2](02-background.md#24-hashes-hmac-and-constant-time-comparison).

**Heartbeat.** A `heartbeat@mirafleet` SSH global request the runner sends every 15 s with its
hostname, OS, architecture, version and labels (and, since M5, an optional bounded list of the
actions the runner offers, for discovery only). Open and heartbeating means online. Heartbeats
stay in memory. See [chapter 7](07-protocols.md).

**HMAC.** A hash mixed with a secret key, so only secret holders can create or verify it. See
[chapter 2](02-background.md#24-hashes-hmac-and-constant-time-comparison).

**`hello`.** The simplest API method: it takes at most a client name hint and returns the
operator's name, the protocol versions central speaks and the server name. It also fails if the
operator is no longer active, so it doubles as a "does my login still work?" test. See [chapter 7](07-protocols.md).

**Hook.** A program the *harness* runs at a fixed event, such as session start, session end or
just before a tool call. The mirafleet Claude Code plugin has three: SessionStart writes the
session handoff file, SessionEnd closes the workspace on `clear` or `logout`, and PreToolUse asks
the human when a call carries `confirm`. Hooks only add convenience: the MCP server and central
are safe without them. See [chapter 2](02-background.md#210-mcp-and-harnesses) and
[flows 20 to 23](04-flows.md#mcp-flows-m5).

**Host key.** An SSH server's own key pair. Central's host key is created by `central init`;
clients pin its hash. See [chapter 2](02-background.md#23-host-key-pinning-versus-trust-on-first-use).

## I

**Idempotent.** Doing it twice has the same effect as once. Re-registering an already active
operator key is idempotent (but still audited). See [chapter 4](04-flows.md).

**Image (container).** The packaged file system a container starts from, named by a reference
(a tag, or better a `@sha256:` digest). A fleet's image allowlist decides which images may run. See
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox).

**In-flight.** Currently running. "In-flight actions are cancelled on revoke." See
[chapter 4](04-flows.md).

## J

**JSON.** A text format for structured data: `{"v":1,"method":"hello"}`. Every API message is
JSON. See [chapter 7](07-protocols.md).

**JSON-RPC.** A small standard for remote calls in JSON: a request has an `id`, a `method` and
`params`; the reply has the same `id` and a `result` or an `error`; a message without an `id` is a
notification. MCP is built on JSON-RPC 2.0, one message per line over stdio. See
[chapter 2](02-background.md#json-rpc-over-stdio).

**Join token.** A credential `mfj1.<id>.<secret>.<host-key-hash>` that lets one runner enrol. Single
use by default, 24 h lifetime, maximum 30 days. Central stores only a SHA-256 of the secret. See
[chapter 4](04-flows.md) and [chapter 5](05-security.md).

**join@mirafleet.** The enroll-channel type a runner uses to present a join token. See
[chapter 7](07-protocols.md).

## K

**Key-check value.** An HMAC of a constant under the secrets key, stored in `secrets_meta`, so central
notices a missing or wrong `MIRAFLEET_SECRETS_KEY` and refuses to start.

**Kick.** Closing a live connection from central's side (as part of revoke or operator revoke).
See [chapter 4](04-flows.md).

**known_hosts.** A file recording the pinned host key. Written by `runner join` and `mcp login`
after the first, pinned connection. See [chapter 2](02-background.md#23-host-key-pinning-versus-trust-on-first-use).

## L

**Labels.** Key/value tags on a runner (such as `role=jump`). Labels set by a join token are
trusted; labels a runner reports in heartbeats are informational only. See
[chapter 6](06-data-model.md).

**Lifecycle states.** `pending -> active -> disabled -> revoked`. Only `active` runners run
actions. See [chapter 6](06-data-model.md).

**Lifecycle (operations).** `approve`, `disable`, `enable`, `revoke`; available only on central,
never over `api@mirafleet`. See [chapter 5](05-security.md).

## M

**Master key (secrets key).** The 32-byte key in `MIRAFLEET_SECRETS_KEY` (from `mirafleet central keygen`)
that encrypts credentials at rest. Never in the database; losing it loses every stored secret.

**MCP (Model Context Protocol).** A standard for an agent harness to discover and call tools,
spoken as JSON-RPC over stdio. mirafleet's MCP server is `mirafleet mcp` (built in M5). See
[chapter 2](02-background.md#210-mcp-and-harnesses).

**MCP tool.** One named capability the MCP server advertises, with a JSON Schema for its
arguments and result. mirafleet has nine: `list_fleets`, `list_runners`, `run_action`,
`workspace_open`, `exec`, `read_file`, `write_file`, `workspace_close` and `workspace_list`. Each
is a thin wrapper over operator API methods; there is none for admin work. See
[chapter 2](02-background.md#210-mcp-and-harnesses) and
[flow 19](04-flows.md#19-one-exec-tool-call-end-to-end).

**Migration.** A numbered SQL file that moves the database schema forward one version. Embedded
in the binary; forward-only; a newer database than the binary fails closed. See
[chapter 6](06-data-model.md).

**Milestone (M1-M6).** A stage of the build plan. See [chapter 1](01-what-is-mirafleet.md#18-milestones-and-current-status)
and [../PLAN.md](../PLAN.md).

**mTLS (mutual TLS).** TLS where *both* sides present certificates: the server proves who it is,
and the client does too. Used between the runner and the OpenSandbox TLS front, so only the runner
can talk to it. See [chapter 5](05-security.md#59-the-workspace-container-boundary).

**Mode (fleet).** `actions` or `workspace`. See [chapter 1](01-what-is-mirafleet.md#14-two-fleet-modes).

## N

**Name constraint.** A certificate-authority restriction listing the only hostnames it may issue
certificates for. The broker's per-session CA is limited to that session's credential hosts.

**Nonce.** A number used once. AES-GCM needs a different one per value under the same key; mirafleet
draws 12 random bytes each time.

**NAT.** Network address translation: a router hides many machines behind one address and blocks
inbound connections. Outbound-only runners work through it. See
[chapter 1](01-what-is-mirafleet.md#12-what-mirafleet-does).

**nerdctl.** A command-line tool for containerd with Docker-like commands. Alternative to the Docker CLI in the dev
scripts (`MIRAFLEET_CTR=nerdctl` on a Linux host with containerd). See [chapter 9](09-dev-setup-and-testing.md).

## O

**One operator per runner.** A host runner is held by the operator with live workspaces on it; another
operator gets a `conflict` error, `busy: runner NAME is in use by OP`, until the last one ends. See [flow 24](04-flows.md#24-open-a-host-workspace).

**Operator.** A registered person: in practice, one Ed25519 key on one machine (durga's laptop)
that central knows by a name such as `durga`. Operators use fleets; they do not manage central.
MIRASTACK LABS's operators: `durga`, `kalyan`. They connect with SSH user `operator`, which is a role label
shared by all of them; it is not the same thing as the operator named `durga`. See
[chapter 3](03-architecture.md).

**Orchestrator.** The agent side of the system as a whole: Claude Code plus its plugin plus
`mirafleet mcp`. Related words: the **harness** is the app that hosts the agent (Claude Code); the
**agent** is the model-driven loop inside it; the **operator** is the registered key the agent
acts through; the **admin** is a different person who runs central. See
[chapter 1](01-what-is-mirafleet.md#13-the-three-roles).

**outcome.** The second audit row of an action run (for example `ok`, `error`, `cancelled`, `timeout`, `denied`),
written after the work ends and sharing the `request_id` of the `start` row. See
[chapter 6](06-data-model.md).

**OpenSandbox.** An open-source sandbox service; mirafleet's opt-in second workspace executor,
reached over HTTPS only (TLS 1.3, mutual TLS, a private CA, an optional SPKI pin) through a TLS
front. Its API key lives on the runner host only. See [../M3.md](../M3.md) and
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox).

**os.Root.** A Go 1.24 standard-library type: a directory handle through which every file
operation stays inside that directory, even across symlinks and `..`. The containerd executor
uses one per workspace for `read_file` and `write_file`. The `host` executor does not (its file
tools are not confined). See
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox).

**Output cap.** The limit on stdout plus stderr that `exec`, `run_action` and text `read_file`
return to the model: 30 KiB by default (`output_cap_bytes` in `contexts.yaml`, 4 KiB to 256 KiB).
The text is sanitised first (ANSI sequences, control characters and invisible Unicode removed or
made visible), then trimmed to a third head and two thirds tail around an omission marker, and the
result says `truncated: true`. See [chapter 5](05-security.md#511-prompt-injection-through-tool-output).

**Outbound-only.** All connections start from the runner or operator toward central; nothing
dials in to a runner. See [chapter 1](01-what-is-mirafleet.md#12-what-mirafleet-does).

## P

**ptrace.** A Linux system call that lets one process inspect and control another (debuggers use it).
Blocked against a non-dumpable process.

**Partial index.** A database index that covers only rows matching a condition (for example only
non-revoked keys), so it can enforce "unique among live rows". See [chapter 6](06-data-model.md).

**Peer UID.** The user id of the process on the other end of a Unix socket, as reported by the
kernel. Central requires it to equal its own before reading from the admin socket. See
[chapter 2](02-background.md#211-unix-sockets-file-permissions-and-umask).

**Pending.** A runner status after join: connected and heartbeating, but may never run actions
until approved (unless the token had `-approve`). See [chapter 6](06-data-model.md).

**Pinning (host-key).** Checking the server's key against a hash known beforehand, refusing
anything else, with no prompt. See
[chapter 2](02-background.md#23-host-key-pinning-versus-trust-on-first-use).

**Plugin.** A folder a harness installs to add tools, commands, skills and hooks in one package.
mirafleet's Claude Code plugin is `integrations/claude-code/`: it starts `mirafleet mcp`, adds the
`/mirafleet` skill (and the `/openfleet` and `/claudefleet` aliases) and three hooks. See
[chapter 2](02-background.md#210-mcp-and-harnesses).

**Policy / PolicyEngine.** The Go interface `Authorize(ctx, Principal, Action, Resource)` that
decides whether a principal may do something. The OSS table: local admin may do everything; an
active operator may only call `hello`, list methods and `action.run`; all else denied. See
[chapter 5](05-security.md).

**Process group.** A set of Unix processes that can be signalled together. The runner starts each
action in its own group so a timeout or revoke can kill the process and any children it spawned.
See [chapter 5](05-security.md).

**Principal.** The authenticated identity behind a request: `admin` (local socket, `local:uid=N`),
an operator, or a runner. See [chapter 5](05-security.md).

**Project default.** The optional `.mirafleet.yaml` in a project directory with a default `fleet`
and `context` for that project. It holds nothing else (no addresses, keys or caps), and a guarded
fleet named only there is refused. See
[chapter 6](06-data-model.md#616-local-files-on-the-operators-machine-and-heartbeat-actions-m5-no-schema-change).

**Prompt injection.** An attack where text the agent reads (a log line, a web page, a README)
contains instructions that trick the agent. mirafleet limits the damage with the guard, confirm,
a fixed set of nine tools, the output cap and sanitising, and by keeping lifecycle off the
operator API. See [chapter 1](01-what-is-mirafleet.md#11-the-problem) and
[chapter 5, 5.11](05-security.md#511-prompt-injection-through-tool-output).

**Public key / private key.** The shareable and secret halves of a key pair. See
[chapter 2](02-background.md#21-public-key-cryptography-and-ed25519).

## R

**Re-adoption.** After a runner restart, the host executor picks its open sessions and running
background jobs back up (from session records and each job's `status.json`). See
[flow 29](04-flows.md#29-runner-restart-re-adoption).

**`ref`.** The handle of a saved foreground `bash` output; pass it to `read_output` to page the full text.
See [flow 26](04-flows.md#26-bash-with-full-output-and-paging).

**Rotation (credential).** Replacing a credential's value; central pushes it to holders at once. Opt-in
`rotate: gitlab_self` lets central rotate a GitLab project token itself. See
[flow 28](04-flows.md#28-credential-assign-delivery-use-and-rotation).

**Rate limit.** A cap on how often something may happen, using a token bucket. See
[chapter 2](02-background.md#28-rate-limiting-token-bucket).

**Reaper.** The goroutine in central (every 60 s) that closes idle, expired or revoked-owner
workspaces and destroys them. See [chapter 4](04-flows.md#16-the-ttl-reaper).

**Reconcile.** Comparing a runner's own list of workspaces with central's database when the runner
connects, and fixing differences: destroy orphans, mark vanished ones `lost`. See
[chapter 4](04-flows.md#17-reconcile-after-a-runner-or-central-restart).

**Registration code.** `mfr1.<code>.<host-key-hash>`: a rotating, TOTP-style code an admin reads
from central so operators can register. Valid for one 60 s step, not single-use. See
[chapter 2](02-background.md#25-totp-style-rotating-codes) and [chapter 4](04-flows.md).

**register@mirafleet.** The enroll-channel type an operator uses to present a registration code.
See [chapter 7](07-protocols.md).

**Registry.** In-memory table of live runner sessions in `internal/central/registry`, keyed by
database id. Holds the tombstones and the cancel functions. Not the same as the database. See
[chapter 3](03-architecture.md) and [chapter 4](04-flows.md).

**Request id.** An id shared by the `start` and outcome audit rows of one action run. See
[chapter 6](06-data-model.md).

**Revoke.** Permanently ending a runner's (or operator's) access: tombstone, kick, cancel
in-flight, commit, refuse reconnect. A revoked key never re-enrols. See [chapter 4](04-flows.md).

**Role.** What a connection may do, selected by its SSH user: runner, operator or enroll. See
[chapter 7](07-protocols.md).

**Rotate-secret.** `mirafleet central operator rotate-secret` replaces the registration secret,
invalidating every outstanding code. See [chapter 10](10-operations.md).

**Runner.** The program on each fleet server that dials out to central and runs approved
actions. See [chapter 1](01-what-is-mirafleet.md#13-the-three-roles).

**runner.yaml.** The runner's own config file written by `runner join` in its data dir (mode
0600). See [chapter 10](10-operations.md).

## S

**Session CA.** The throwaway certificate authority the broker makes for one workspace session; its key
exists only in runner memory and its certificate (`ca.pem`) is added to the tools' trust list.

**Signing oracle.** Something that cannot reveal a key but will sign whatever a caller asks. A forwarded
ssh-agent is one; mirafleet's restricted agent narrows it to user-auth requests for pinned hosts.

**ssh-agent.** A program holding SSH private keys in memory and signing on request over a Unix socket
(`SSH_AUTH_SOCK`). mirafleet runs a restricted one (`internal/workspace/sshagent`) per host session:
`List` and `Sign` only.

**Seam (interface seam).** A Go interface left in the OSS code so an enterprise feature can plug
in later: `Store`, `AuthProvider`, `PolicyEngine`, `AuditSink`. See
[chapter 8](08-code-tour.md).

**SIEM.** "Security information and event management": a product that collects logs from many
systems for security teams. Enterprise audit export to a SIEM is an interface seam, not built.
See [chapter 12](12-why-mirafleet.md).

**seccomp.** A Linux filter listing which system calls a process may make; others fail. Workspace
containers use containerd's default profile and never run unconfined. See
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox) and
[chapter 5](05-security.md#59-the-workspace-container-boundary).

**Session (workspace).** A name the client chooses (for example Claude Code's session id) for the
workspace it wants. At most one live workspace exists per operator, fleet and session. See
[chapter 4](04-flows.md#12-open-a-workspace).

**Session handoff.** A small file (`sessions/<hash>.json` in the state directory) in which a
harness hook leaves the harness's session id (`cc:<id>` for Claude Code), keyed by the parent
PID and the project directory, so the MCP server can reuse the same workspace after
`--resume`. It holds an id, not a secret. Without it the server uses a random per-process
session. See [flow 20](04-flows.md#20-session-resolution-and---resume-reattach).

**Skill.** A Markdown file of instructions a harness hands to the model on demand or when the user
types a slash command. mirafleet's `/mirafleet` skill explains how to use the tools safely. A
skill is text only and cannot enforce anything. See
[chapter 2](02-background.md#210-mcp-and-harnesses).

**Slot.** One place for a workspace on a runner. The runner's `concurrent: N` setting gives N slots;
a workspace in `opening`, `open` or `closing` holds one. See
[chapter 4](04-flows.md#12-open-a-workspace).

**SPKI pin.** A hash of a certificate's *public-key info* (SubjectPublicKeyInfo). The runner can pin
the OpenSandbox front's key (`pin_sha256`) as an extra check on top of the CA. See
[chapter 5](05-security.md#59-the-workspace-container-boundary).

**Sticky (session).** A repeated `workspace.open` for the same session returns the same workspace
(`reused: true`). If its runner is offline the answer is `unavailable`, never a fresh workspace.
See [chapter 4](04-flows.md#12-open-a-workspace) and [chapter 6](06-data-model.md#615-the-workspace-tables-migration-0002).

**Slow loris.** An attack that opens many connections and sends data very slowly to tie up the
server. Handshake and enroll time limits defend against it. See [chapter 5](05-security.md).

**source.** Which front end a call came from, recorded in the audit row: `cli` (admin socket),
`api` (operator), `enroll` (join or register), `system` (central itself: the workspace reaper,
reconcile and revoke cascades); `gui` is reserved for M4. See [chapter 6](06-data-model.md).

**Symlink.** A file that points to another path. Opening a key file through a symlink could be
steered to a different file, so key loading refuses symlinks (`O_NOFOLLOW`). See
[chapter 4](04-flows.md).

**Service (`internal/service`).** The single code path used by CLI, API and (later) GUI and MCP
for every operation. See [chapter 3](03-architecture.md).

**Session context.** The Go context tied to one runner connection; cancelling it kicks the
runner and stops its work. See [chapter 2](02-background.md#27-go-context-cancellation).

**SQLite.** An embedded database in a single file; used through the pure-Go `modernc.org/sqlite`.
See [chapter 2](02-background.md#26-sqlite-transactions-and-wal).

**SSH.** The Secure Shell protocol: encrypted, authenticated connections with channels. See
[chapter 2](02-background.md#22-ssh).

**SSH user.** In mirafleet, a role label (`runner`, `operator`, `enroll`), not an operating-system
account. See [chapter 7](07-protocols.md).

**Static binary.** One executable with no shared libraries to install (`CGO_ENABLED=0`). See
[chapter 1](01-what-is-mirafleet.md#17-platforms).

**stdio.** Standard input and output; how tokens are passed in (`-token -`) and how MCP servers
talk to harnesses. See [chapter 2](02-background.md#210-mcp-and-harnesses).

**stdio server.** An MCP server that the harness launches as a child process and talks to over
that process's standard input and output, with no network port. `mirafleet mcp` is one: stdout
carries only protocol messages, logs go to stderr, and the server exits when stdin closes. See
[chapter 2](02-background.md#210-mcp-and-harnesses).

**Step (time step).** `floor(unix_seconds / 60)`: the number of the current minute, input to the
registration code. See [chapter 2](02-background.md#25-totp-style-rotating-codes).

**Store.** The Go interface for persistence, with domain-level atomic operations; the SQLite
implementation lives in `internal/store/sqlite`. See [chapter 6](06-data-model.md).

## T

**TLS interception.** A proxy ending a TLS connection itself, with a certificate the client trusts, so it can
add or read headers. The broker does it for credential hosts only.

**TOFU (trust on first use).** Accepting a server's key the first time and warning only if it
changes. mirafleet does **not** use it. See
[chapter 2](02-background.md#23-host-key-pinning-versus-trust-on-first-use).

**Token bucket.** A rate-limiting scheme with a refilling bucket of permits. See
[chapter 2](02-background.md#28-rate-limiting-token-bucket).

**TLS front.** A small reverse proxy (`mirafleet runner tls-front`, or any TLS-terminating proxy)
that sits next to OpenSandbox, which has no native TLS, and serves only TLS 1.3 with a required
client certificate. See [chapter 5](05-security.md#59-the-workspace-container-boundary).

**Trigger (database).** SQL that the database runs automatically on insert, update or delete.
The `audit` table uses triggers that reject every update and delete. See
[chapter 6](06-data-model.md).

**Tombstone.** A mark in the registry that a runner id is revoked, added *before* the database
commit so a racing reconnect cannot sneak in; removed if the commit fails. See
[chapter 4](04-flows.md).

**TOTP (time-based one-time password).** A code computed from a shared secret and the current
time step. mirafleet's registration code is TOTP-style but long and with 60 s steps. See
[chapter 2](02-background.md#25-totp-style-rotating-codes).

**Transaction.** A group of database changes that commit or roll back together. See
[chapter 2](02-background.md#26-sqlite-transactions-and-wal).

## U

**umask.** A per-process mask removing permission bits from newly created files; central uses
`077`. The arithmetic: a file requested as `0666` becomes `0666 & ~077 = 0600` (owner only). See [chapter 2](02-background.md#211-unix-sockets-file-permissions-and-umask).

**User namespace.** A Linux namespace that maps user ids: "root" (uid 0) inside a container is an
ordinary unprivileged id on the host. mirafleet gives every slot its own range, so an escape
lands as a nobody and workspaces cannot touch each other's files. See
[chapter 2](02-background.md#29-containers-containerd-and-opensandbox) and
[chapter 5](05-security.md#59-the-workspace-container-boundary).

**Unix domain socket.** A local-only connection endpoint that is a file. See
[chapter 2](02-background.md#211-unix-sockets-file-permissions-and-umask).

## W

**workdir.** Optional `.mirafleet.yaml` field (host fleets only) naming a shared directory
`<root>/work/<name>` that several sessions use instead of a per-session directory.

**WAL (write-ahead log).** SQLite mode where changes go to a log file first so readers and the
writer do not block each other. See [chapter 2](02-background.md#26-sqlite-transactions-and-wal).

**Workspace.** A per-session container with its own directory (`/workspace`) where an agent may
run any command. It has an id (`ws_id`), an owner, a state (`opening`, `open`, `closing`,
`closed`, `failed`, `lost`) and a time to live. Built in M3. See
[chapter 1](01-what-is-mirafleet.md#the-workspace-running-example) and
[chapter 6](06-data-model.md#the-workspace-state-machine).

**workspace mode.** The fleet mode for workspaces (built in M3). Its runner takes `-workspace
FILE` (`workspace.yaml`) instead of an actions file. See [../M3.md](../M3.md).

## Y

**YAML.** A human-friendly text format for config files, using indentation (`actions.yaml`,
`runner.yaml`, `contexts.yaml`). See [chapter 1](01-what-is-mirafleet.md#14-two-fleet-modes).
