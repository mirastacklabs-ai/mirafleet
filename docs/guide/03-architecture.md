# 3. Architecture

## What you will learn

- The three programs in the system (central, runner, operator tools) and what each one is
  responsible for.
- Which side listens on a network port (only central) and why runners never accept inbound
  connections.
- Where the trust boundaries are, and which key or secret lives on which machine.
- How the code inside central is layered, so you know where a new feature belongs.
- Which platforms are supported, and why mirafleet ships as one static binary.

This chapter is the map. It shows which programs exist, what each part of them is responsible for,
who is allowed to talk to whom, and where every secret lives. The next chapter,
[Flows](04-flows.md), walks through what happens at run time. If a term is new, look it up in the
[glossary](glossary.md); the background ideas (SSH, keys, SQLite, contexts) are explained in
[Background](02-background.md).

### Words you will meet

| Word | Plain meaning |
|---|---|
| **principal** | The verified identity behind a connection: its kind (`admin`, `operator`, `runner` or `enroll`) plus its id. |
| **front end** | One of the doors into central's logic: the operator API (`api@mirafleet` channels), the admin socket, and the enroll channels. Each has its own method table. |
| **`hello`** | The smallest operator method. It does nothing but answer, so a client can check that its connection and key work. |
| **DTO** | "Data transfer object": a plain struct that only carries data between layers, such as `api.Request` or `store.Runner`. It has no behaviour. |
| **role** | What the SSH user name selects after login: `runner`, `operator` or `enroll`. |
| **dispatch** | Picking the handler for something that arrived: first the role (by SSH user), then the method (by name, from a table). |

## The running example

Everything in this guide uses the same fictional company, **Acme**:

- Central runs on `central.example.com:2222`.
- There are two fleets: `dev` and `prod`.
- Runners are `build-1` (in `dev`) and `db-1` (in `prod`).
- Operators (a person's registered key and machine, driven by a person or an AI agent) are `alice`
  and `bob`.
- **Dana** is the admin. She has a shell on the central host and runs `mirafleet central ...` there.
- For workspace mode: Dana creates `dev` as a **workspace** fleet (with `build-1` as a workspace
  runner), and alice's Claude Code opens a workspace on it, runs `go test` and reads and writes
  files ([chapter 1](01-what-is-mirafleet.md#the-workspace-running-example)). `prod` stays an
  actions fleet, with `db-1` and the `tail-logs` action.

## The big picture

![Big picture: operators and runners both connect outbound to central; keys and secrets are shown where they live](diagrams/big-picture.svg)

*Look at the direction of the arrows: every connection points at central. Nothing points at a
runner or an operator's laptop.*

1. Claude Code starts `mirafleet mcp`. The LLM API key and the SSH key stay on the laptop.
2. `mcp` dials central `:2222` as SSH user `operator`; each request is one `api@` channel.
3. Central checks the key and the policy against SQLite, then acts.
4. Runners dial OUT to central (user `runner`). Nothing listens on a runner.
5. Central sends work back down that same connection: an `exec@` channel (actions fleet) or a
   `workspace@` channel (workspace fleet, where the runner drives containerd locally).
6. Dana runs the admin CLI on central. It uses a local unix socket, never the network.

### In detail

In one sentence: **operators and runners both dial out to central; central is the only thing that
listens, and every request passes through it.**

- The operator's SSH key and the AI provider's API key both stay on the operator's machine. Central
  never sees the AI key, and a runner never sees either.
- The "channel" (step 2) is one request/response lane inside the single SSH connection, so one
  connection can carry several requests at once.
- Step 5 is the trick that lets a runner sit behind a firewall: the runner opened the connection, so
  central can open new channels inside it without any inbound rule.
- Step 6: the admin CLI never touches the network. It writes to a file-like socket that only the
  user running central can open (details below).

## One binary, three roles

mirafleet is one program, `mirafleet` (see `cmd/mirafleet/main.go`). The first argument picks the
role:

| Command | Role | Runs on | Needs |
|---|---|---|---|
| `mirafleet central init`, `central run` | **Central**: the server in the middle | one Linux host | its data directory (default `/var/lib/mirafleet/central`) |
| `mirafleet central fleet ...`, `token ...`, `runner ...`, `operator ...`, `exec`, `audit` | **Admin CLI**: talks to the *running* central over a local socket | the central host | a running central |
| `mirafleet runner join`, `runner run` | **Runner**: sits on a fleet server | each Linux server in a fleet | its data directory (default `/var/lib/mirafleet/runner`) and either an `actions.yaml` or (workspace mode) a `workspace.yaml` |
| `mirafleet runner tls-init`, `tls-front`, `workspace list`, `workspace prune` | **Runner helpers** for workspace mode | the workspace runner's host | the runner's `workspace.yaml` (list, prune) or a new directory (tls-init) |
| `mirafleet mcp login`, `mcp api` | **Operator tools**: what an operator's machine runs | laptop (Linux, macOS; builds on Windows) | a config directory with the operator key |

`mirafleet mcp` will also become the MCP stdio server that Claude Code launches. That server is
**planned** for milestone M5 (see [PLAN.md](../PLAN.md)); today `mcp api METHOD [JSON]` makes one
raw call, which is how the flows in this guide are exercised. (`mcp api` also takes `-in FILE`
and `-out FILE`, to send a file body to `workspace.write_file` or `workspace.exec` and to save
the body of `workspace.read_file`.) There is also a hidden command, `mirafleet workspace-init`,
that is not for people: it is the first process (PID 1) inside every workspace container.

### Why one static binary?

A *static* binary carries everything it needs inside the one file.

- **Easy to install.** Copy one file to a server and run it. There is no runtime (no Python, no
  JVM) to install, and no shared libraries to match to the operating system version.
- **Easy to build for many targets.** The rule `CGO_ENABLED=0 go build ./...` must keep working
  (see `CLAUDE.md`). Without C code, the Go compiler can produce binaries for Linux, macOS and
  Windows from one machine. This is why the database is `modernc.org/sqlite` (SQLite translated to
  Go) and not the usual C version.
- **One version everywhere.** Central, runners and operator tools come from the same build, so
  they always agree on the wire format. There is no "runner v1.2 cannot talk to central v1.4"
  matrix to manage.
- **Small attack surface.** No scripting layer, no plugin loader, no package manager on the
  runner that could be tricked into running something else.

The price is that the operator laptop also carries the (unused) central code. The project accepts
this, but keeps the dependency graph honest: only `cmd/mirafleet/central_linux.go` imports
`internal/store/sqlite`, so macOS and Windows builds of the binary never link SQLite at all.
`server_linux.go` holds the runner commands, and `server_other.go` is the stub for other systems.

## Components and their responsibilities

The animated picture above is the overview. The three diagrams below show the *components*, one
machine at a time. Package names are the real directories under `internal/`; the table further
down names the functions.

**The operator's laptop**

```mermaid
flowchart LR
  MCP["cmd/mirafleet/mcp.go<br/>mcp login, mcp api"]
  AC["internal/apiclient<br/>SSH client"]
  OC["internal/opconf<br/>key, known_hosts, contexts.yaml"]
  CEN(["central :2222"])
  MCP -- "makes calls with" --> AC
  MCP -- "reads config from" --> OC
  AC -- "outbound SSH, user operator or enroll" --> CEN
```

**The central host**

```mermaid
flowchart LR
  SSH["internal/central<br/>SSH server and roles"]
  ADM["internal/central<br/>admin socket"]
  CLI["cmd/mirafleet/admin.go<br/>central fleet, token, runner, ..."]
  SVC["internal/service<br/>business rules"]
  POL["internal/policy<br/>who may do what"]
  REG["internal/central/registry<br/>live runner sessions"]
  ST["internal/store<br/>Store interface"]
  DB["internal/store/sqlite<br/>SQLite implementation"]
  CLI -- "unix socket" --> ADM
  SSH -- "calls" --> SVC
  ADM -- "calls" --> SVC
  SVC -- "asks" --> POL
  SVC -- "finds sessions in" --> REG
  SVC -- "reads and writes via" --> ST
  ST -. "implemented by" .-> DB
```

**The runner host**

```mermaid
flowchart LR
  RN["internal/runner<br/>dial-out loop, heartbeat"]
  GD["internal/actions<br/>the guard"]
  AU["internal/audit<br/>local JSONL log"]
  CEN(["central :2222"])
  RN -- "outbound SSH, user runner" --> CEN
  CEN -. "opens exec@mirafleet back over the runner's own connection" .-> RN
  RN -- "every action goes through" --> GD
  RN -- "writes events to" --> AU
```

*Look at the arrows that reach central: all start on the laptop or the runner. The single dotted
arrow is central re-using a connection the runner opened.*

### Central

Central is `mirafleet central run`. It is a long-running Linux process that owns the database and
is the only component with a view of the whole fleet. Its parts:

| Part | Package / file | Responsibility |
|---|---|---|
| **SSH server** | `internal/central/central.go` (`Server.Start`, `Server.handle`) | Accepts TCP connections, runs the SSH handshake (the key exchange that sets up the encrypted connection) with a deadline, authenticates the key, limits how many handshakes can be in progress. |
| **Roles** | `internal/central/roles.go` (`Server.dispatch`, `serveRunner`), `api.go` (`serveOperator`), `enroll.go` (`serveEnroll`) | After authentication, the SSH **user** name picks the role: `runner`, `operator` or `enroll`. Each role has its own handler and its own allow-list of SSH channel types. |
| **Dispatch** | `internal/central/dispatch.go` (`methodTable`) | Turns a request such as `action.run` into a handler call. A method that is not in the table for this front end is refused, whoever asks. |
| **Admin socket** | `internal/central/admin_unix.go` (`ServeAdmin`, `adminConn`), `adminclient.go` (`AdminCall`) | A Unix socket (a file that processes on the same machine connect to, instead of a network port) at `<data>/admin.sock` (mode `0600`). Same framing and the same dispatcher as the operator API, but with the `admin` principal. It exists so admin commands can reach the live connections (revoke has to kick a connected runner). |
| **Service** | `internal/service` (`Service`) | The one place where business rules live: check policy, validate input, change state, write audit, map errors to wire errors. The SSH server, the admin socket and (later) the GUI all call it. |
| **Policy** | `internal/policy` (`TablePolicy`) | A small fixed table: who may do what. Denies by default. |
| **Registry** | `internal/central/registry` (`Registry`, `Session`) | In-memory list of runners that are connected *right now*: their SSH connection, last heartbeat (the runner's "I am alive" message), live status, tombstones (in-memory "this runner id is dead" marks) for revoked runners, and (from the heartbeat) the runner's slots, executor and secure runtime. It is also how central runs an action (`exec@`) or a workspace operation (`workspace@`) on a runner. |
| **Store** | `internal/store` (interface), `internal/store/sqlite` (`DB`) | Persistence. The interface offers domain-level operations (`Enroll`, `SetRunnerStatus`, ...), each atomic and each writing its audit row in the same transaction. SQL stays inside `sqlite`. |
| **Audit** | `audit` table in the store; `Store.RecordAudit` | Append-only record of every state change and every action. Triggers (database rules that run automatically) in the schema abort any `UPDATE` or `DELETE`. |
| **Workspace handlers** | `internal/central/workspace_handlers.go` | Decode the six `workspace.*` operator methods and three admin ones strictly, call the service, map to DTOs. They also read the *body frame* of `write_file` and `exec` stdin, but only after the service has authorised the call. |
| **Workspace service** | `internal/service/workspace*.go` | Placement (`workspace_sched.go`), open/close (`workspace.go`), exec/read/write/list (`workspace_ops.go`), admin (`workspace_admin.go`), and the housekeeping: the **reaper** and **reconcile** (`workspace_reaper.go`). |
| **Auth** | `internal/auth` (`KeyAuth`, `Principal`) | Looks a presented key up in the store, applies the user-to-role binding, and carries the identity in `ssh.Permissions.Extensions`. |

Two small helpers sit beside these:

- `internal/central/conns.go` (`OperatorConns`) tracks live operator connections by operator id so
  that revoking an operator can close them.
- `internal/central/hslimit.go` (`hsLimiter`) counts handshakes in progress per source address, and
  globally, so a flood of half-open connections is cut off before any cryptography is done.

#### Limits you can tune (`central.Config`)

Central has a handful of tunable limits (handshake timeout, handshakes in progress per IP and
overall, enroll rate limits and deadline, heartbeat-write interval). They are fields of
`central.Config` in `internal/central/central.go`; a zero value means "use the default". The one
table of all limits, with their current values and the reason for each, is
[Protocols, section 7.9](07-protocols.md#79-limits).

These limits are a backstop, not denial-of-service protection: a central exposed to the internet
still needs a firewall in front of the SSH port.

### Runner

The runner is `mirafleet runner run`. It is deliberately small and does four things
(`internal/runner/runner.go`):

1. **Dial-out loop** (`Runner.Run`): connects to central, and when the connection drops, waits and
   tries again with exponential backoff (1 s up to 60 s, with +/-20 % jitter, so many runners do not
   all retry at the same second).
2. **Heartbeat** (`Runner.heartbeat`): sends an SSH global request (a message about the connection
   as a whole, not about one channel) called `heartbeat@mirafleet` right after connecting and then
   every 15 s (`heartbeatEvery` in `cmd/mirafleet/server_linux.go`). The payload is hostname, OS,
   architecture, version and labels; a workspace runner adds its slots, executor, secure
   runtime and image patterns.
3. **Actions guard** (`internal/actions`, `Set.Plan` and `Set.Run`): in actions mode, the only
   code that ever starts a process (a workspace runner has no guard; see below). It reads the allowlist `actions.yaml` from the runner's own disk (default
   `/etc/mirafleet/actions.yaml`). **This guard, not central, is the security boundary.** Even a
   fully compromised central can only ask for actions that the runner's own file allows.
4. **Local JSONL audit** (`internal/audit`, `Writer`): appends one JSON line per event to
   `<data>/audit.jsonl` (mode `0600`), so the runner keeps its own record even if central's copy is
   lost or disputed.

**Channels on the runner connection.** The runner opens no channels toward central, and central
opens only `exec@mirafleet` (actions runners) or `workspace@mirafleet` (workspace runners) toward
the runner. A runner without an actions file refuses `exec@`; one without a workspace config
refuses `workspace@`. The SSH library on the runner rejects every other channel type. The runner
has no listening socket toward the network (the package comment of `internal/runner` says so).
The one exception is the egress proxy below, which listens only *inside a container's own
network namespace*.

#### The workspace runner (M3)

`mirafleet runner run` takes **exactly one** of `-actions FILE` or `-workspace FILE`. One runner is
one fleet and one mode. With `-workspace` the runner does not start the actions guard at all;
instead `runner.serveWorkspace` (`internal/runner/workspace.go`) serves `workspace@mirafleet`
channels through a **`workspace.Executor`**, the Go interface in `internal/workspace`:
`Create`, `Exec`, `ReadFile`, `WriteFile`, `Destroy`, `List`. Everything above the interface
(scheduling, slots, TTL, audit, policy) is the same whatever sits below it. Two executors exist:

| Executor | Package | What it does | Default? |
|---|---|---|---|
| `containerd` | `internal/workspace/ctrd` (Linux only) | Runs the container itself through the containerd Go client; builds the OCI spec; reads and writes files with `os.Root`; runs the egress proxy. | yes |
| `opensandbox` | `internal/workspace/osb` | Speaks HTTPS to an [OpenSandbox](02-background.md#29-containers-containerd-and-opensandbox) server on the same host, through a TLS front, with mutual TLS. No SDK. | opt-in per fleet |

Beside them: `internal/workspace/wsconfig` (loads and validates `workspace.yaml`),
`internal/workspace/egress` (the proxy), `internal/workspace/initproc` (the tiny PID 1 that
runs inside each container, started as `mirafleet workspace-init`) and
`internal/workspace/fake` (an in-memory executor for tests).

```mermaid
flowchart LR
  CEN(["central :2222"])
  SW["internal/runner<br/>serveWorkspace<br/>re-validates every request"]
  CFG["wsconfig<br/>workspace.yaml"]
  EX["workspace.Executor"]
  CT["ctrd<br/>containerd executor"]
  OS["osb<br/>OpenSandbox executor"]
  CTR["containerd<br/>socket = root on host"]
  C1["container<br/>mf-ws_id"]
  CEN -. "workspace@mirafleet channel" .-> SW
  SW -- "limits and allowlists from" --> CFG
  SW -- "calls" --> EX
  EX -- "default" --> CT
  EX -- "opt-in" --> OS
  CT -- "create, exec, delete" --> CTR
  CTR -- "runs" --> C1
  OS -. "https, TLS 1.3, mTLS" .-> TF["TLS front<br/>next to OpenSandbox"]
```

*Look at the containerd socket: whoever can talk to it controls the host. That is why the runner
re-validates everything and builds the container itself.*

Facts to remember about a workspace runner:

- **Slots.** `concurrent: N` in `workspace.yaml` is the number of workspaces the runner accepts.
  It is sent in each heartbeat (`slots`, plus `executor`, `secure_runtime` and image patterns), so
  central can place new workspaces. The runner also has its own limit of N as a second gate.
- **Central is not trusted.** The runner rebuilds the entire container spec from a fixed
  template. Central supplies only the image, limits within the runner's caps, the network mode
  and allowlisted environment names. There is no field for "privileged", "host network" or "host
  mount".
- **Local admin.** `mirafleet runner workspace list` and `prune` talk to the executor directly,
  with no central involved, so they still work after the runner was revoked.
- **Root-equivalent.** A workspace runner can reach the containerd socket. See trust boundary 7
  below.

### Operator side

On an operator's machine, three packages work together:

| Package | Responsibility |
|---|---|
| `cmd/mirafleet/mcp.go` | `mcp login ADDR -name NAME -code -` (register this machine with central) and `mcp api METHOD [JSON]` (make one call to central and print the JSON). |
| `internal/apiclient` | The SSH client: `Dial`, `Call` (one request per `api@mirafleet` channel), `Join` and `Register` (the enroll channels), and the host-key checks (`PinnedHash`, `KnownHosts`). Pure Go, so it builds on Windows and macOS. |
| `internal/opconf` | Finds and reads the operator's local configuration directory: `operator_ed25519` (private key), `known_hosts` (pinned central keys) and `contexts.yaml` (which central to talk to). The directory is `$MIRAFLEET_CONFIG_DIR` if set, otherwise `os.UserConfigDir()/mirafleet`. |

Operators never connect to a runner. They send a request to central, and central decides.

### Shared building blocks

| Package | Why it exists |
|---|---|
| `internal/api` | The JSON request/response types, error codes, size and time limits, and the length-prefixed framing. Pure Go. See [Protocols](07-protocols.md). |
| `internal/transport` | The SSH vocabulary shared by both ends: channel names (`exec@mirafleet`, `workspace@mirafleet`, `api@mirafleet`, `join@mirafleet`, `register@mirafleet`), the request name `heartbeat@mirafleet`, the SSH user names, and the `Heartbeat`, `ExecRequest`, `ExecResult`, `WorkspaceRequest` and `WorkspaceResult` messages. |
| `internal/workspace` | Pure Go, no containers: the `Executor` interface, and the **one** set of input checks (`CleanPath`, `ValidArgv`, `FilterEnv` with its denylist, `MatchImage`). Central, the runner and the executors all call these; there is no second copy. |
| `internal/tokens` | Formats of join tokens (`mfj1.`) and registration codes (`mfr1.`), the host-key hash and the registration-code HMAC. Used by both central (to make and check) and the clients (to parse). |
| `internal/keys` | Generating and loading Ed25519 keys, refusing private keys that others can read, and the `known_hosts`-style pinning callback. |

## Deployment at Acme

This is where each program runs and what is listening where.

```mermaid
flowchart TB
  subgraph LAPTOPS["Operator laptops (no listeners)"]
    A["alice's laptop<br/>mirafleet mcp"]
    B["bob's laptop<br/>mirafleet mcp"]
  end

  subgraph CH["Central host: central.example.com"]
    L1["TCP 2222<br/>SSH (the only network listener)"]
    L2["Unix socket<br/>/var/lib/mirafleet/central/admin.sock<br/>mode 0600, same-UID peers only"]
    P["mirafleet central run"]
    L1 --- P
    L2 --- P
    ADMIN["Dana's shell on the central host<br/>mirafleet central ..."]
    ADMIN --> L2
  end

  subgraph DEVNET["dev network (behind NAT)"]
    R1["build-1<br/>mirafleet runner run<br/>(no listeners)"]
  end

  subgraph PRODNET["prod network (private)"]
    R2["db-1<br/>mirafleet runner run<br/>(no listeners)"]
  end

  A -- "outbound to :2222" --> L1
  B -- "outbound to :2222" --> L1
  R1 -- "outbound to :2222, stays open" --> L1
  R2 -- "outbound to :2222, stays open" --> L1
```

*Look at who owns a listener box: only the central host has any. A runner can sit in a private
network with no inbound firewall rule at all.*

Facts to remember:

- **Only central listens.** It binds the SSH address given by `-listen` (default `0.0.0.0:2222`) and
  creates the admin socket in its data directory. A GUI listener on `127.0.0.1:8443` is **planned**
  for M4; it does not exist yet.
- **Runners never accept inbound connections.** A runner dials central and keeps that one TCP
  connection open. When central needs the runner to do something, it opens an SSH *channel* back
  over that same connection. The firewall rule for `db-1` therefore only has to allow *outgoing*
  traffic to `central.example.com:2222`. (NAT, "network address translation", is the router feature
  that lets many private machines share one public address; it lets traffic out but blocks
  unsolicited traffic in, which is exactly what a dial-out runner needs.)
- **Operators dial out too.** `alice` and `bob` run `mirafleet` on their laptops, which connect to
  central and make requests.
- **Admins act on the central host.** Lifecycle changes (approve, disable, enable, revoke), fleet
  and token creation, registration codes and audit reading are done by Dana running
  `mirafleet central ...` on the central host itself. They go through the admin socket, never over
  the network. See [Security](05-security.md) for why.
- **Central needs no internet.** Only the operator side may need it (for the AI service). Central and
  runners talk only to each other, so private and air-gapped networks work.

## Trust boundaries

A trust boundary is a line where the data on one side cannot be assumed honest. The most important
rule: **each side checks everything that comes from the other side.**

```mermaid
flowchart LR
  subgraph UNTRUSTED["Outside: anyone on the network"]
    ANY["unknown peer<br/>(user enroll, any ed25519 key)"]
  end

  subgraph OPDOM["Operator domain"]
    OPK["operator key<br/>(alice)"]
  end

  subgraph CENDOM["Central domain (one trust point)"]
    TRANS["SSH server + auth"]
    SVC["service + policy"]
    ADMS["admin socket<br/>(same-UID only)"]
    STORE["SQLite store + host key"]
    TRANS --> SVC
    ADMS --> SVC
    SVC --> STORE
  end

  subgraph RUNDOM["Runner domain (db-1)"]
    RK["runner key"]
    GUARD["actions guard<br/>+ actions.yaml"]
    PROC["process on the host"]
    GUARD --> PROC
  end

  ANY -- "join@ or register@ only, rate-limited" --> TRANS
  OPK -- "api@mirafleet, active operator key" --> TRANS
  RK -- "heartbeat, runner key not revoked" --> TRANS
  SVC -- "exec@mirafleet over the runner's own connection" --> GUARD
```

*Look at each arrow into the central domain: each one passes through authentication and a
per-role channel allow-list. The only arrow out of central into a runner ends at the guard, not at
a shell.*

The boundaries, one by one:

1. **Network to central.** Anyone who can reach port 2222 can start an SSH handshake. Only the
   user name `enroll` accepts an unknown key, and then only for one `join@mirafleet` or
   `register@mirafleet` request, within `EnrollDeadline`, rate-limited. Users `runner` and
   `operator` require a key that is already in the database and not revoked.
2. **Operator to central.** An operator is trusted to ask for the ten operator methods (`hello`,
   `fleets.list`, `runners.list`, `action.run` and the six `workspace.*` methods) and nothing
   else. Admin and enroll methods are refused on the operator channel even if the key is valid.
   Even for its own workspace, an operator may only touch workspaces it owns; anybody else's id
   gets `not_found`.
3. **Central to runner.** The runner trusts exactly one thing: central's host key, pinned in its
   `known_hosts` at join time. But it does **not** trust central's *requests*: every `exec`
   request still goes through the actions guard. A stolen central cannot run `rm -rf /` unless
   `actions.yaml` on that runner defines such an action. A workspace runner likewise re-checks
   every `workspace@mirafleet` request against its own `workspace.yaml` (image allowlist,
   limits, network modes, env allowlist) and builds the container spec itself, so a stolen
   central cannot ask for a privileged container or a host mount.
4. **Admin to central.** The admin socket has no password. Its protection is filesystem
   permission (`0600` in a `0700` directory) plus a check that the connecting process runs as the
   *same user id* as central (`checkPeer`). Anyone who is already that user on the central host
   is an admin.
5. **Central to its own database.** The database file and the host key are the crown jewels.
   They sit in a `0700` data directory, and the data directory is protected by an exclusive lock
   (`flock`, an operating-system lock on a file) so that two centrals never share it.
6. **Container boundary (workspace runners).** Inside a workspace the agent can run any command.
   The line that matters is between that container and the runner host: a user namespace,
   no capabilities, a read-only root, a seccomp filter, cgroup limits and an empty network
   (or an allowlisting proxy). Chapter 5, [section 5.9](05-security.md#59-the-workspace-container-boundary),
   covers each defence. Nothing the container does is trusted: the runner reads its files only
   through `os.Root` and re-checks every path.
7. **Runner host to containerd.** A runner that serves workspaces must reach the containerd
   socket, and **access to that socket is equivalent to root on the host**. A workspace runner
   is therefore a root-equivalent process: run it on a dedicated machine or VM, in its own
   containerd namespace, and never on a host that holds other secrets. This is why a runner
   takes exactly one of `-actions` or `-workspace`: the actions guard is never run as root just
   because containerd needs it.

Known limit, stated honestly in [ARCHITECTURE.md](../ARCHITECTURE.md): central is a single trust
point. If an attacker owns central, they can ask every runner to run any action that runner's
allowlist permits. The runner-side guard bounds the damage but cannot remove it.

## Where does each secret and key live?

| Secret or key | Created by | Lives on | Path or place | Who else knows it |
|---|---|---|---|---|
| **Central host key** (Ed25519 private) | `central init` (`keys.Generate`) | Central host | `<data>/host_ed25519`, mode `0600` | Nobody. Its *hash* is embedded in every join token and registration code so clients can pin central. |
| Central host public key | `central init` | Central host | `<data>/host_ed25519.pub` (public) | Runners and operators store it in their `known_hosts` after join/login. |
| **Runner private key** | `runner join` | The runner host | `<runner data>/runner_ed25519`, mode `0600` (default data dir `/var/lib/mirafleet/runner`) | Nobody. Central stores only the *public* key (`runners.pubkey`). |
| Runner `known_hosts` | `runner join` | The runner host | `<runner data>/known_hosts`, mode `0600` | Pins central's host key. Written only after the pinned join succeeded. |
| Runner `runner.yaml` | `runner join` (written last) | The runner host | `<runner data>/runner.yaml` | Central address, runner name, fleet. Its existence marks the directory as "joined". |
| Runner `actions.yaml` | The runner's admin | The runner host | `/etc/mirafleet/actions.yaml` (default for `-actions`) | Nobody else. This is the allowlist. |
| Runner local audit | `runner run` | The runner host | `<runner data>/audit.jsonl`, mode `0600` | Local copy only. |
| **Operator private key** | `mcp login` | The operator's machine | `operator_ed25519` in the opconf directory (`$MIRAFLEET_CONFIG_DIR` or `os.UserConfigDir()/mirafleet`), mode `0600` | Nobody. Central stores only the public key (`operators.pubkey`). |
| Operator `known_hosts`, `contexts.yaml` | `mcp login` | The operator's machine | same directory | Pins central; names the current central. |
| **Join token secret** | `token create` (`tokens.NewJoinToken`) | Printed **once** to Dana, then carried by hand to the runner | Not stored in clear anywhere | Central stores only `SHA-256(secret)` in `join_tokens.secret_hash`. The runner uses the token once and never writes it to disk. |
| **Registration secret** (32 bytes) | `central init` (`InitRegistrationSecret`) | Central | `settings` table, key `registration_secret`, inside `<data>/mirafleet.db` (mode `0600`) | Nobody. Registration *codes* are derived from it every 60 seconds. Never logged, never audited. |
| Registration code (60 s) | `central operator code` | Shown to Dana, passed to an operator | Not stored | Valid only in its own 60-second step. |
| Admin socket | `central run` | Central | `<data>/admin.sock`, mode `0600` | Same-UID processes only. |
| Data-dir lock | `central init` / `central run` | Central | `<data>/central.lock` (`flock`) | Keeps a second central off the same directory. |
| Runner `workspace.yaml` | The runner's admin | The runner host (workspace runners only) | `/etc/mirafleet/workspace.yaml` (`-workspace`) | Nobody else. This is the workspace-mode boundary: image allowlist, limits, network modes, env allowlist, executor settings. |
| **OpenSandbox API key** | The runner's admin (OpenSandbox's own key) | The runner host only | The file named by `opensandbox.api_key_file` in `workspace.yaml`; must be mode `0600` or `0400` and owned by the runner's user | Never in env or argv, never logged, never passed into a sandbox. The runner refuses to start if the file is missing or loose. |
| **Node CA and certificates** (OpenSandbox executor) | `mirafleet runner tls-init -dir DIR` | The OpenSandbox host (the same machine as the runner) | `ca.pem`, `front.pem`, `front-key.pem`, `client.pem`, `client-key.pem`, all mode `0600` in a `0700` directory | The CA's private key is discarded right after signing. The front holds its server key; the runner holds the client key (mTLS). The runner may also pin the front's public key (`pin_sha256`). |
| Workspace directories | `workspace.Create` | The workspace runner | `<containerd.root>/<ws_id>`, default `/var/lib/mirafleet/ws`; root-owned `0700` | The files an agent wrote. Not secret by design: **never put credentials in a workspace**. |
| **The agent's AI API key** | The operator | The operator's laptop only | Environment of Claude Code | Never sent to central, never to a runner, never into a workspace. The env denylist (`*_API_KEY`, `ANTHROPIC_*`, ...) refuses to forward such names. |

A good habit when reading the code: ask "which of these could this function see?". A function in
`internal/service` can reach the registration secret through the store but never logs it; the CLI
prints a join token alone on stdout and everything else on stderr so a script can capture only the
token; secrets are read from stdin (`-token -`, `-code -`) so they do not appear in `ps` output or
shell history.

## Layering inside central

Code in central is organised in layers. A layer may call the layer below it, never above. The
business layers do not skip the service: policy, store and registry are reached through it.

```mermaid
flowchart TB
  T["1. Transport<br/>central.go: Server.handle<br/>listener, handshake deadline, limits, auth callback"]
  R["2. Roles<br/>roles.go, api.go, enroll.go<br/>per-role channel allow-list"]
  D["3. Dispatch<br/>dispatch.go: methodTable.serve<br/>method allow predicate, strict decode, timeout"]
  S["4. Service<br/>internal/service<br/>authorize, validate, act, audit, map errors"]
  B["5. Policy, store, registry<br/>TablePolicy, sqlite.DB, Registry"]

  T --> R --> D --> S --> B
```

*Look at layer 5: it is only ever reached through layer 4.*

1. **Transport** (`central.go`). Accepts the TCP connection, takes a handshake slot from
   `hsLimiter`, runs `ssh.NewServerConn` with `HandshakeTimeout`, and calls `auth.KeyAuth` to
   authenticate the key. Only Ed25519 keys are accepted (`PublicKeyAuthAlgorithms`), with
   `MaxAuthTries` of 3. The result is a `Principal` packed into the connection's
   `ssh.Permissions.Extensions`.
2. **Roles** (`roles.go`, `api.go`, `enroll.go`). `Server.dispatch` reads the principal back out of
   the permissions (`auth.Decode`) and hands the connection to the handler for its kind
   (`serveRunner`, `serveOperator`, `serveEnroll`). Runners open no channels (`allowedChannels` is
   empty for them). Operators may open `api@mirafleet` channels, at most `api.MaxChannelsPerConn`
   (8) at a time. Enroll peers may open exactly one channel, `join@mirafleet` or `register@mirafleet`.
3. **Dispatch** (`dispatch.go`). Each front end has a `methodTable`: an `allowed` predicate and a
   map of handlers. The operator table serves `hello`, `fleets.list`, `runners.list`,
   `action.run` and the six workspace methods (`workspace.open`, `exec`, `read_file`,
   `write_file`, `close`, `list`). The admin table adds the admin methods (including
   `fleet.workspace_set`, `workspace.admin_list` and `workspace.kill`). An admin or enroll method sent on
   `api@mirafleet` is answered `forbidden` and never reaches a handler.
4. **Service** (`internal/service`). The single code path for every front end. A typical method
   calls `authorize` (policy plus a direct kind check for admin-only methods), validates the
   input, calls the store, and returns an error that `ToAPIError` turns into a fixed wire message
   so SQL text or file paths cannot leak.
5. **Policy / store / registry.** `TablePolicy.Authorize` is a pure function of principal kind and
   the operator's *current* status. The store does durable, atomic work. The registry holds the
   live connections. Nothing below the service calls `time.Now()`: the clock is injected
   (`service.Config.Now`), which makes the code testable.

**Two deliberate exceptions to "always through the service".** Authentication (`auth.KeyAuth`)
looks keys up in the store directly, because it runs during the handshake, before any principal
exists for the service to check. And `serveRunner` talks to the registry (and reloads the runner
from the store) directly, because registering a live session is connection plumbing, not a business
action a caller asked for. Everything a caller can *ask for* goes through the service.

Two design rules fall out of this layering, and you will see them again in
[Security](05-security.md):

- **Two independent gates, plus a third for admin-only calls.** The method table (layer 3) and the
  policy (layer 4) are two independent gates; admin-only calls add a direct check of the caller's
  kind as a third. If someone adds a handler and forgets one gate, the others still refuse.
- **Identity travels only in the connection's permissions** and is reloaded by id for each request.
  Handlers never trust a status captured at login: a revoked operator is found out on the next
  request.

## Platforms

| Component | Linux | macOS | Windows |
|---|---|---|---|
| `central` (including the admin CLI and `central init`) | yes | refuses to start | refuses to start |
| `runner` | yes | refuses to start | refuses to start |
| `mcp` (operator tools) | yes | yes | builds on Windows; runs there from M5 (until then the key loader fails closed, because owner-only file permissions cannot yet be verified) |

"Fails closed" means that when a safety check cannot be done, the program refuses instead of
carrying on unchecked.

`main.go` calls `requireServerOS` for `central` and `runner`, and `serverSupported` is a
compile-time constant per platform. The reasons are practical: the runner kills whole process
groups (a *process group* is a set of related processes that can be signalled together; the runner
uses `Setpgid` and `syscall.Kill(-pid, SIGKILL)`), central relies on `flock` and on Unix-socket
peer credentials (the kernel telling the server which user is on the other end), and containers
(M3) are a Linux feature (the containerd code is `//go:build linux`, and `server_linux.go` is the
only file that imports it). The admin socket's peer check has implementations for Linux and macOS;
on other systems it rejects every peer.

One detail: "client disconnect cancels an admin `exec`" (for example pressing Ctrl-C in
`mirafleet central exec`) works only on Linux, because it depends on a hang-up signal that macOS
does not report reliably. On other systems the request still ends at its own timeout.

## Planned parts (not in the code yet)

M1, M2 and M3 are done (13 Nov 2026). Next: M5 (MCP server), then M4 (GUI) and M6 (release).
So you do not look for them: the MCP stdio server and `integrations/` (M5), the admin GUI (M4),
and release packaging and systemd units (M6). Workspace mode, the containerd executor and the
OpenSandbox executor are **built** (M3, [M3.md](../M3.md)).

## Check your understanding

1. Which processes in Acme's deployment listen on a network port, and which do not?
2. Why can the firewall in front of `db-1` block all inbound traffic and mirafleet still work?
3. A developer wires the admin handler for `token.create` into the operator handler map by
   mistake. Which checks would still stop an operator from creating a join token?
4. Where is the registration secret stored, and what does `alice` actually receive when she
   registers?
5. If central is fully compromised, what stops the attacker from running any command on `db-1`?
6. Why can a compromised central not make `build-1` start a privileged container with the host's
   `/etc` mounted? And why must the `build-1` host still be treated as sensitive?
7. A runner is started with both `-actions` and `-workspace`. What happens, and why?

<details>
<summary>Answers</summary>

1. Only central listens: the SSH port (`0.0.0.0:2222` by default) and the local Unix admin socket.
   Runners and operator machines listen on nothing; they dial out.
2. The runner opens the connection to central. Central then opens `exec@mirafleet` channels
   *inside* that connection, so no new inbound connection is ever needed.
3. The method table and the policy are two independent gates; admin-only calls add a direct check
   of the caller's kind as a third. Here all three still refuse. (a) The operator table's
   `allowed` predicate is `api.IsOperatorMethod`, which is false for `token.create`, so
   `methodTable.serve` answers `forbidden` before the handler map is even looked at. (b) Even if
   it got through, `TablePolicy` permits an operator only `hello`, `fleets.list`, `runners.list`
   and `action.run`. (c) `Service.CreateToken` calls `authorize(..., adminOnly=true)`, which
   requires `p.Kind == auth.KindAdmin` directly, independent of the policy.
4. In the central database, in the `settings` table under `registration_secret`, mode `0600`.
   Alice never receives the secret. She types or pipes a 60-second *code* derived from it and
   gets back the status `active`; her own private key never leaves her machine.
5. The runner's actions guard. It only runs actions defined in the runner's own `actions.yaml`,
   as an argv list without a shell, with parameters that fully match their regex. Central cannot
   change that file.
6. The runner builds the whole container spec itself from a fixed template, and re-checks the
   image, limits, network mode and env against its own `workspace.yaml`. Central's request has
   no field for privileges or mounts. But the runner can use the containerd socket, which is
   root-equivalent on that host, so the host must be dedicated and trusted.
7. The runner refuses to start. One runner is one fleet and one mode, and the actions guard must
   never be run as root just because containerd needs root.

</details>

Next: [4. Flows](04-flows.md).
