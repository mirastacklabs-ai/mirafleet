# mirafleet threat model

What mirafleet protects, from whom, how, and what it deliberately does not. Written for people
who decide whether to deploy it. The long version, with code locations and worked examples, is
[chapter 5 of the developer guide](guide/05-security.md). Report vulnerabilities privately: see
[SECURITY.md](../SECURITY.md).

mirafleet has no telemetry: no usage data, crash reports or update checks are sent anywhere. Its
only network connections are the ones you configure. Central's only possible outbound connection
is HTTPS to your GitLab, and only when you turn on token self-rotation for a credential.

## 1. What the parts are

- **Central**: one Linux server. SQLite database, SSH server (port 2222) for runners and
  operators, local admin socket, and the Admin GUI (HTTPS, port 8443). It also holds the stored
  credentials (section 4.1).
- **Runner**: a process on a managed machine. It dials out to central and never accepts inbound
  connections. Modes: **actions** (named allowlisted commands), **workspace** (private
  containers, runner is root-equivalent on its host), **host** (an ordinary user on a dedicated
  VM, no containers).
- **Operator**: a person's laptop running `mirafleet mcp` for an AI agent. Holds an Ed25519 key.
- **Admin**: whoever can run `mirafleet central ...` on the central host, or log in to the GUI.

## 2. Assets

| Asset | Why it matters | Where it lives |
|---|---|---|
| Production servers and what runs on them | what the agent could damage | runners |
| Stored credentials (deploy keys, tokens, registry logins) | access to your source code and registries | encrypted on central; in memory on the assigned host-mode runner |
| `MIRAFLEET_SECRETS_KEY` | decrypts every stored credential | central's env file, your backup |
| Central's data directory (database, host key, registration secret, GUI certificate) | identity of the whole deployment | `/var/lib/mirafleet/central`, mode 0700 |
| Operator and runner private keys | act as that operator or runner | operator laptop; runner data dir |
| Admin password and GUI session | full control of fleets and credentials | admin's head and browser; hash and session hashes in the database |
| Source code and data on runner VMs | confidentiality and integrity | the VM |
| Audit log | accountability | central (fleet-wide), each runner (local copy) |
| Release artifacts | what you install | GitHub releases, registries, npm, Homebrew tap |

## 3. Actors

| Actor | Capability assumed | Section |
|---|---|---|
| Prompt-injected agent (an operator's agent that read hostile text) | everything the operator key can do | 4.2, 4.4 |
| Prompt-injected agent inside a workspace or on a host-mode VM | any command as its unprivileged user | 4.3, 4.4 |
| Stolen operator laptop | the operator key file | 4.2 |
| Leaked join token or registration code | the string | 4.2 |
| Malicious or compromised runner | a valid runner key | 4.2 |
| Network attacker: LAN, scanner, man in the middle | reach to ports 2222 and 8443, traffic | 4.2, 4.5 |
| Hostile web page in an admin's browser | requests from the admin's browser | 4.5 |
| Local unprivileged user on the central host or a runner VM | a shell as another user | 4.1, 4.4 |
| Another user's agent on a shared runner VM | normal Unix access as a different user | 4.4 |
| Supply-chain attacker | a poisoned dependency, release or package | 4.6 |
| Insider admin | full admin rights | out of scope (5) |

## 4. Trust boundaries and defences

Every boundary below is a place where data crosses from something less trusted to something more
trusted. Each line is "defence, where it holds".

### 4.1 Central

Central is the **single trust point** of a deployment (see non-goals). It is defended as an
exposed network service, but if it is compromised the attacker can request work from every
runner and read every stored credential. That is why the runner's own allowlist exists (4.2).

- **SSH to central.** Ed25519 keys only, host keys pinned from the join token and registration
  code, no trust on first use, three auth tries, handshake caps and deadlines, join and enrol rate
  limits. Roles are bound to SSH users; identity travels only in `Permissions.Extensions`.
- **Deny by default.** The operator key can call a small set of operator methods. Runner
  lifecycle (approve, disable, revoke), tokens and credentials exist only on central: the admin
  socket and the GUI. An agent cannot approve a runner or read a credential.
- **Local files.** Data directory 0700, files 0600, umask 077, a `flock` against two centrals on
  one directory, and a peer-UID check on the admin socket. Packaged units run as the `mirafleet`
  user with a strict sandbox (no new privileges, read-only system, private /tmp, empty capability
  set, syscall filter).
- **Audit.** The start row is written before an action runs and fails closed; the log is
  append-only; every GUI and CLI change is a row with its actor and source.
- **Stored credentials (M7).** AES-256-GCM with a random nonce per value and the credential's id
  and version as associated data. The key is 32 bytes in `MIRAFLEET_SECRETS_KEY`, read from the
  0600 root env file; only a key-check value is stored. A stolen database or backup is
  useless without the key; central refuses to start with credentials and a missing or wrong
  key. Credentials are write-only: masked in the GUI and CLI, never returned by any method,
  never on the operator API or MCP, never in an audit row. **Losing the key loses the secrets**,
  and a restore of a backup needs the same key.
- **Version skew.** Central at protocol level N accepts N and N-1 and refuses the rest with a
  message that names who must upgrade; upgrade central first.
- **Upgrade safety.** An online SQLite backup precedes every migration (3 kept, mode 0600 in a
  0700 directory); a failed backup stops the upgrade; a newer database is refused.
- **Outbound.** None, except HTTPS to GitLab for opt-in token self-rotation (system roots plus an
  optional corporate CA file). In the scratch-based image there is no CA bundle, so that case
  needs the CA file mounted read-only.

### 4.2 Runners and operators

- **Enrolment.** Join tokens are single use by default, last 24 hours, are stored as SHA-256 only,
  show once, carry central's host-key hash, and produce a `pending` runner that a human approves.
  The registration code rotates every 60 seconds and also carries the host-key hash.
- **Revoke.** A tombstone, a kick of live connections and cancellation of in-flight work.
- **The runner is the boundary for actions (actions mode).** The runner re-checks every request
  against its own `actions.yaml`: argv only, no shell, every parameter a full-match regular
  expression, values never re-expanded or split, leading `-` refused by default, minimal
  environment, bounded time and output, process group killed on timeout. A compromised central
  cannot make the runner run anything the file does not allow. The file is as strong as its
  author: an action that is a shell is a shell.
- **A malicious runner** can only heartbeat and answer requests; it has no API and opens no
  channels; central clips and bounds everything it sends back.
- **Guarded fleets.** `confirm` must equal the fleet name, compared exactly, and is enforced by
  central whatever the harness does.
- **A stolen operator laptop** is handled by `central operator revoke`, which ends its live
  connections. The key is a file: disk encryption is yours.

### 4.3 Workspace mode (containers)

An agent gets a private container on a runner: user namespace, no capabilities, seccomp,
read-only root, cgroup limits, no network unless the fleet allows named destinations, an image
allowlist on the runner, an environment allowlist, and no secrets inside. The runner itself is
**root-equivalent** on its host (it drives containerd); use a dedicated machine. A kernel or
runc escape is outside what mirafleet can promise; the opt-in OpenSandbox executor and secure
runtimes (gVisor, Kata) are the stronger option.

### 4.4 Host mode (M7): an agent as a user on a VM

Design: **one runner is one OS user on one VM.** The agent runs commands as that user, exactly
like Claude Code on a laptop, and sees what that user sees. The VM is the boundary.

What the design gives you:

- **The runner is not root.** It refuses to start as uid 0 or with capabilities, and refuses a
  group- or world-writable home or state directory.
- **Secrets stay out of reach of the agent user** (their path from central to a pushed commit):
  - delivered over the existing authenticated SSH connection and held **in runner memory only**;
    never written to disk, never in an environment variable or argv, never in a child's
    environment;
  - the runner is **non-dumpable** (`PR_SET_DUMPABLE 0`, no core dumps), so a process of the same
    user cannot `ptrace` it or read `/proc/<runner>/mem`, `environ`, `fd` or `maps`;
  - child processes get a clean environment, a new session and only file descriptors 0 to 2;
  - SSH keys are used through a **per-session restricted ssh-agent** (list and sign only; no add,
    remove, lock or export; signs user-auth for the credential's user; with host binding the
    server's host key must be in the credential's pinned `known_hosts`; the socket is 0600 in a
    0700 directory);
  - HTTPS tokens and registry logins are injected by a **TLS-terminating broker** in a local
    proxy, only for the credential's hosts, with a per-session certificate authority that
    is name-constrained to those hosts and kept in memory. The broker performs the registry
    token exchange, so the agent never sees a registry JWT. Redirects are never followed. Hosts
    without a credential pass through as plain tunnels: the VM firewall decides;
  - credentials are scoped by what GitLab allows the token or key to do; mirafleet adds no
    path or ref rules, so use least privilege (project deploy keys, `write_repository` only).
- **Use is audited.** Every use is logged runner-locally (JSONL) and sent to central as
  aggregated events (per session, credential, host, status); values are never logged. The GUI
  shows per-runner delivery status; operators see names, kinds, hosts and status only.
- **Tool output is data.** The MCP server neutralises harness-control tags in every tool result
  and error. File tools run in the runner as the same user and refuse FIFOs and devices; upload
  and download are confined to the project directory and refuse `.git/`, `.env*`, keys and
  similar files.
- **One operator at a time per runner**, enforced by central; busy runners are skipped by
  placement. Directories of a previous operator stay on disk until cleanup and are readable by
  the next operator.
- **Another runner user on the same VM** (`qa`) is separated by normal Unix permissions: homes
  and session directories are 0700, sockets are 0600, and each session's proxy has its own
  password because loopback ports are shared.

**The accepted gap (read this).** The runner's own enrolment key must stay on disk so the runner
can reconnect, and it is readable by the same user. A determined, prompt-injected agent can read
that key, connect to central **as that runner**, and receive the runner's assigned credentials.
mirafleet does not prevent this in the open-source edition. It is mitigated, not closed:
central lets a newer connection replace the old one and writes a `runner.replaced` audit row
and flags the runner in the GUI; the blast radius is that runner's credentials, which you
scoped narrowly. The enterprise edition may close it later. Until then, treat the credentials
assigned to a host-mode runner as available to the agent that works there, and scope them to
what you would hand that agent anyway.
<!-- verify: the replacement audit row name `runner.replaced` and the GUI flag exist after merge (M7.md 3.2); M7 notes say duplicate-login refusal is NOT implemented - if a refusal was added, update this paragraph -->

Other residual risks, all accepted and documented:

- **SSH:** while a session is open the agent can use the key against the pinned hosts with the
  key's GitLab permissions; the key's public half is visible; the user can ssh anywhere the VM
  firewall allows, with keys it brings itself.
- **Exfiltration** of code: the VM firewall limits it; the model provider also sees what the
  agent reads, by design.
- **Root on the VM** can read anything, runner memory included. Use a dedicated VM.
- **The same user can kill or reconfigure its own runner**: an availability issue, not a secrecy
  one. Yama `ptrace_scope` is not relied on.
- **Background shells and outputs** are readable by the same user (the agent). Their status files
  are informational only.
- **Central compromise** reveals all stored credentials: single trust point (4.1).
- **A leftover directory** of a previous operator is readable by the next one until cleanup.

### 4.5 The Admin GUI

The GUI is a web page on a network port, **on all interfaces (`0.0.0.0:8443`) by default**, with
a **self-signed certificate** unless you give `-gui-cert` and `-gui-key`. It is always TLS 1.3.

- **First visit, man in the middle.** The first visit to a self-signed certificate is the one real
  weakness: an attacker on the path could present their own certificate and read the setup token
  or password. Defence: central logs the certificate's SHA-256 fingerprint at every start and the
  setup page repeats it; **the admin must compare the browser's fingerprint with the log before
  typing anything**, or do the setup over `ssh -L 8443:127.0.0.1:8443`. The quickstart does both.
  After setup the token is deleted and `/setup` is a 404.
- **Setup token.** 256 bits, 24 hours, stored hashed, written to a 0600 file (never logged),
  compared in constant time, single use.
- **Passwords and sessions.** Argon2id; a 12 to 256 byte rule; sessions stored in SQLite as
  SHA-256 of a 256-bit token, in a `__Host-` cookie (`Secure`, `HttpOnly`, `SameSite=Strict`);
  idle timeout 30 minutes, absolute 12 hours, bound to the password generation, so a password
  change or `reset-admin` ends them. A copied database cannot hijack a session.
- **Brute force.** Per-IP and per-user login limits with lockout, and a cap on concurrent hashing.
- **Web attacks.** Host allowlist (anything else gets 421, against DNS rebinding), CSRF tokens
  plus origin checks, strict CSP with no inline script, escaped output (a runner's hostname
  cannot inject markup), and `X-Forwarded-*` never trusted.
- **Restrict it.** `-gui-listen 127.0.0.1:8443` with a tunnel, `-gui-listen off`, or a firewall
  rule; central logs a notice at every start while it listens on all interfaces. HSTS is sent
  only with your own certificate, because it would lock an admin out of a self-signed one.
- **Credentials in the GUI.** Values are write-only; there is no file upload; secrets are never
  rendered back. One admin account (a limit, section 5).
- **Admin socket vs GUI.** Both are admin rights. Anyone who can run `mirafleet central` as the
  `mirafleet` user is an admin.

### 4.6 Supply chain and distribution

- **Dependencies:** a short, pinned list; permissive licences only, enforced by `go-licenses`
  (no GPL, LGPL, AGPL, MPL, SSPL or BUSL); `govulncheck` in CI; Dependabot for modules, actions
  and base images. The list of third-party licences ships in every archive, package and image.
- **Builds:** static binaries (`CGO_ENABLED=0`, `-trimpath`), built by goreleaser in a pinned
  container from a signed-off tag; the tag ruleset allows only admins to create `v*` tags.
- **Checksums versus signatures.** `checksums.txt` lists the SHA-256 of every artifact, and
  `install.sh`/`install.ps1` always verify it. A checksum protects against corruption and a bad
  mirror; it does not protect against someone who replaced both the file and the checksum on the
  same server. The keyless **cosign** signature of `checksums.txt` and the GitHub **build
  provenance** attestation tie the artifacts to this repository's release workflow
  (`install.sh --verify-signature` requires the signature; images are signed by digest on both
  registries and carry provenance). Verify with `cosign verify-blob` and `gh attestation verify`.
- **SBOMs** (CycloneDX) are published for every archive and package.
- **No Apple notarization and no Microsoft code signing.** macOS and Windows may warn about a
  binary downloaded in a browser. Homebrew, npm, `install.sh` and `install.ps1` avoid the
  warning without it. The trust anchor is the checksum and the cosign/provenance chain, not a
  platform vendor's signature. Quarantine one-liners are in the [quickstart](QUICKSTART.md).
- **npm:** published with provenance via trusted publishing, no install scripts, no download at
  install time. **Homebrew:** a formula rendered from `checksums.txt`.
- **Image:** `FROM scratch`, non-root (65532), no shell, no bundled certificates or secrets.
- **Claude Code plugin:** the marketplace entry is pinned to the release tag. Hooks run
  `mirafleet` from `PATH`: whoever controls `PATH` controls the hook.

## 5. Non-goals: what mirafleet does not protect against

Stated plainly so nobody promises more than it delivers. Several are the enterprise gaps: the
open-source core leaves interface seams for them and does not implement them.

- **Root on a machine you ran it on.** Root on central, on a runner or on a host-mode VM can read
  everything there. A dedicated VM per sensitive job is the answer.
- **A compromised central.** Central is the single trust point: it can ask runners for any work
  their allowlists permit, and it holds every stored credential and the key to decrypt them. No
  high availability, no hardware key storage, no split keys.
- **The host-mode enrolment-key gap** (4.4): a determined agent on a runner can obtain that
  runner's own credentials. Closing it (for example with a hardware-bound or per-session runner
  secret) is enterprise work.
- **Per-operator or per-fleet access control.** Any registered operator may use any registered
  runner and fleet. There are no roles, no RBAC, no per-runner grants. One operator at a time on a
  host-mode runner is a hold, not an authorisation.
- **More than one admin, or SSO.** One GUI admin account; no SSO, no MFA, no approval workflows.
- **Egress control in host mode.** Use the VM firewall. Workspace mode has a destination allowlist;
  host mode does not.
- **Perfect separation between runner users on one VM.** Normal Unix permissions only; kernel
  bugs and root are out of scope.
- **Container escapes** from workspace mode beyond the hardening in 4.3.
- **A badly written `actions.yaml`.** An allowlist that defines a shell defines a shell.
- **An agent's own judgement.** A prompt-injected agent can do anything the credentials and
  allowlists of its runner allow, including damaging that runner's workspace. The design limits
  the blast radius to one runner, one fleet's credentials and the audited actions; it does not
  make the agent trustworthy. Keep production fleets in `actions` mode and guarded.
- **Stolen operator keys before revocation, or a stolen laptop with an unencrypted disk.**
- **A lost secrets key, or a lost database without a backup.**
- **Denial of service** by someone who can reach port 2222 or 8443 beyond the built-in limits; a
  firewall is recommended.
- **Audit tampering by an admin.** The log is append-only through the API; someone with the
  database file can edit it. There is no SIEM export or tamper-evident chain.
- **Session recording, SSH certificate authority, federation.** Not in the open-source edition.
- **Telemetry or phone-home.** There is none to protect against; a review item checks that no
  code adds an unconfigured outbound connection.

## 6. Deployment checklist

1. Put 2222 behind a network rule; allow 8443 only from admins or bind the GUI to loopback.
2. Generate `MIRAFLEET_SECRETS_KEY`, keep the env file 0600 root, back the key up apart from the data.
3. Compare the GUI certificate fingerprint before first login; use a real certificate if you can.
4. Keep production fleets in `actions` mode, guarded; keep `actions.yaml` short and reviewed.
5. Use a dedicated VM per host-mode runner group; scope every credential to one project and the
   minimum permission; set expiry dates.
6. Give each developer their own host-mode runner.
7. Verify downloads (checksum at least; cosign and provenance when you can).
8. Upgrade central first; keep the backups the upgrade makes.
