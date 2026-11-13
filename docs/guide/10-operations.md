# 10. Operations

This chapter is for the person who installs and looks after a mirafleet deployment. It gives a
complete walk-through for Acme, a reference for every command, the files each machine keeps, how
to write `actions.yaml`, and a table of error messages with fixes.

## What you will learn

- How to install and run central, a runner and an operator, step by step, with the output you
  should see.
- Every `mirafleet` command and flag, and which machine each one runs on.
- What files live where, with their permissions, and what each one is for.
- How to write `actions.yaml` safely.
- How to read an error message and know what to do about it.
- How to back up, restore and upgrade central, and what to do if its host key is lost.

> **Status:** M1 and M2 are done (13 Nov 2026). Next: M3 (workspaces), then M5 (MCP server),
> M4 (GUI), M6 (release). There are no packages or systemd units yet (planned for M6, see
> [PLAN.md](../PLAN.md)); you run the binary yourself. The MCP server that Claude Code talks to
> (`mirafleet mcp` with no arguments) arrives in M5; today `mirafleet mcp login` and
> `mirafleet mcp api` are the operator tools. Where the guide shows something planned, it says
> so.
>
> Want to see it all work before reading on? [Section 9.11](09-dev-setup-and-testing.md#911-try-it-locally-central-runner-and-operator-in-one-container)
> runs central, a runner and an operator in one throwaway container.

## 10.1 The cast

Acme has one central server and two fleets.

| Thing | Name |
|---|---|
| Central | `central.example.com`, SSH port `2222` |
| Fleets | `dev`, `prod` |
| Runners | `build-1` (in `dev`), `db-1` (in `prod`) |
| Admin | Dana, who runs `mirafleet central ...` on the central host |
| Operators | `alice`, `bob` (people or agents on laptops; registered keys, not Linux accounts) |

Who runs what:

| Machine | OS | Binary role | Network need |
|---|---|---|---|
| `central.example.com` | Linux | `mirafleet central` | accepts TCP 2222 from runners and operators |
| `build-1`, `db-1` | Linux | `mirafleet runner` | **outbound only**: connect to central:2222; no open port |
| alice's and bob's laptops | Linux, macOS (builds on Windows; runs there from M5) | `mirafleet mcp` | outbound to central:2222 |

Central and runner refuse to start on other operating systems:
`mirafleet: mirafleet central is supported on linux only (current: darwin)`.

**Firewall note.** Central has built-in limits (connection caps, enrol rate limits, handshake
deadlines) but they are a backstop, not DoS protection. If central is reachable from the
internet, put a firewall or rate limiter in front of port 2222. Better still, allow only your
runner and laptop address ranges.

## 10.2 Install and run, step by step

### Step 0: get the binary

Build it with the container toolchain (see [chapter 9](09-dev-setup-and-testing.md)):

```bash
scripts/dev.sh shell
# inside the container (the target must match the machine you copy it to):
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o dist/mirafleet ./cmd/mirafleet
```

(`/dist/` is git-ignored.) Change `GOOS`/`GOARCH` for other machines, for example
`darwin`/`arm64` for an Apple-silicon laptop that only needs `mirafleet mcp`.
Release archives, packages and `install.sh` are planned (M6). Copy the binary to
`/usr/local/bin/mirafleet` on every machine, and check:

```console
$ mirafleet version
dev
```

### Step 1: central: `init`

On `central.example.com`, as the user that will run central (here `root`, or a dedicated
`mirafleet` user that owns `/var/lib/mirafleet`):

```console
$ mirafleet central init -listen 0.0.0.0:2222
data dir:  /var/lib/mirafleet/central
host key:  /var/lib/mirafleet/central/host_ed25519
fingerprint: SHA256:Qm9ndXNFeGFtcGxlRmluZ2VycHJpbnRPbmx5Rm9yRG9jcw
listen:    0.0.0.0:2222
created the registration secret
next: mirafleet central run -data /var/lib/mirafleet/central -listen 0.0.0.0:2222
```

(The fingerprint above is a made-up example.) `init` is offline and **idempotent**: it
creates the data directory (mode 0700), generates central's SSH **host key** once, creates the
database and its tables, and generates the **registration secret** once. Running it again prints
`already initialised; registration secret kept` and changes nothing. Note down the fingerprint;
you can compare it later with what operators see. `-listen` here is only used for the hint line;
`init` does not open a port.

### Step 2: central: `run`

```console
$ mirafleet central run -listen 0.0.0.0:2222 &
central listening on 0.0.0.0:2222, admin socket /var/lib/mirafleet/central/admin.sock
```

Central logs to stderr; run it under a service manager so it restarts and its log is kept. Only
**one** central may use a data directory: a second one fails with
`another central is already using /var/lib/mirafleet/central (lock ... is held)`. Stop it with
Ctrl-C or SIGTERM; it shuts down cleanly.

An example systemd unit (illustrative only; the real unit ships with the M6 packages):

```ini
[Unit]
Description=mirafleet central
After=network-online.target

[Service]
User=mirafleet
ExecStart=/usr/local/bin/mirafleet central run -data /var/lib/mirafleet/central -listen 0.0.0.0:2222
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

**Who may run the admin commands?** Every other `mirafleet central ...` command talks to
the local **admin socket** `admin.sock` in the data directory. Central accepts only connections
from the **same Unix user** that runs central (it checks the connecting process's user ID before
reading anything), and the data directory is mode 0700. So run admin commands as that same user,
for example `sudo -u mirafleet mirafleet central fleet list`.

### Step 3: create the fleets

```console
$ mirafleet central fleet create dev
fleet dev created (mode actions, confirm false)
$ mirafleet central fleet create prod -confirm
fleet prod created (mode actions, confirm true)
$ mirafleet central fleet list
FLEET  MODE     CONFIRM  RUNNERS
dev    actions  false    0
prod   actions  true     0
```

`-confirm` makes `prod` a **guarded** fleet: every `action.run` against it must carry
`"confirm": "prod"` (the fleet name). It stops an agent from running something on prod by
accident, because the caller has to say the name on purpose. The check is enforced on central.
`-mode workspace` can be created, but workspaces arrive in M3; today only `actions` fleets can
run actions.

### Step 4: make a join token

```console
$ mirafleet central token create -fleet dev
mfj1.3f9a1c07b2d84e65.<43 characters>.<43 characters>
token 3f9a1c07b2d84e65 for fleet dev, expires 2026-11-14T10:15:00Z; shown once, store it now
```

The token itself goes to **stdout** (so you can pipe it) and the note goes to **stderr**. It is
shown exactly once; central keeps only a hash. By default a token is single-use and lives 24
hours. Useful options: `-uses 5` (up to 1000 runners), `-ttl 2h` (up to 30 days),
`-approve` (runners join already active, skipping step 7), `-labels role=build,region=eu`.

Treat the token like a password until it is used: anyone holding it can enrol a runner into the
fleet. Send it over a channel you trust.

### Step 5: join the runner

On `build-1`, as the user that will run the runner (needs write access to its data directory):

```console
$ echo "$JOIN_TOKEN" | mirafleet runner join central.example.com:2222 -name build-1 -token -
joined fleet "dev" as runner "build-1": status pending
an admin must approve this runner on central before it can run actions
next: start it with `mirafleet runner run -data /var/lib/mirafleet/runner` (under systemd; the unit ships with the M6 packages)
```

Notes:

- The token is read from **stdin** (`-token -`), not as an argument to mirafleet itself,
  because the arguments of a running program are visible to every user on the machine (`ps`)
  and are saved in shell history. (`echo` is a shell built-in, so the token never appears in
  another program's arguments either.) It is not read from the environment for the same reason.
- The token contains a hash of central's host key. The runner uses it to check it is really
  talking to *your* central before sending anything. There is no "trust on first use" prompt.
- The runner generates its own key pair; the private key never leaves the machine.
- A data directory can join only once. A second try fails with
  `this data directory has already joined a central (runner.yaml exists); use a fresh -data directory`.

### Step 6: write `actions.yaml` and start the runner

Write `/etc/mirafleet/actions.yaml` (see section 10.5 for the format). A minimal one:

```yaml
actions:
  hello:
    run: ["/bin/echo", "hello", "{{name}}"]
    params:
      name: { pattern: "[a-z]{1,8}" }
  disk-usage:
    run: ["df", "-h", "{{path}}"]
    params:
      path: { pattern: "/|/var|/home", default: "/" }
```

Then start the runner:

```console
$ mirafleet runner run -actions /etc/mirafleet/actions.yaml
runner "build-1" (fleet dev) -> central.example.com:2222, actions: disk-usage,hello
```

It dials out, keeps one SSH connection, heartbeats every 15 seconds and reconnects with
exponential backoff (1 s up to 60 s) if the connection drops. The actions file is read **once at
start**: restart the runner after editing it. A bad file stops the runner at start with an
error naming the action, so mistakes show up immediately.

### Step 7: approve the runner

Back on central:

```console
$ mirafleet central runner list
RUNNER         STATUS   ONLINE  LAST SEEN             LABELS
dev/build-1    pending  true    2026-11-13T10:16:02Z  -
$ mirafleet central runner approve dev/build-1
dev/build-1: active
```

A `pending` runner can connect and heartbeat but central will not send it work. Approving takes
effect on the live connection immediately; the runner does not need a restart. Other
lifecycle commands: `disable` (stop new work, let running work finish; reversible with
`enable`) and `revoke` (permanent: cancels running work, kicks the runner, its key can never
rejoin). Use `-approve` on `token create` if you trust the token holder enough to skip this step.

### Step 8: test an action from central

```console
$ mirafleet central exec dev/build-1 hello name=world
hello world
```

`central exec` is the admin's way to run an action with no operator involved (it is audited with
source `cli`). Its exit code is the action's exit code. For a guarded fleet, add
`-confirm prod`.

### Step 9: get an operator code

```console
$ mirafleet central operator code
code:      mfr1.<26 characters>.<43 characters>
valid for: 42s
host key:  SHA256:Qm9ndXNFeGFtcGxlRmluZ2VycHJpbnRPbmx5Rm9yRG9jcw
```

The code changes every 60 seconds, so it is only useful for a short time; it is derived from the
registration secret and the clock. Any number of operators may use one code within its window.
Dana hands it to alice over a channel she trusts (a call, a chat to the person). The `host key` line is
the fingerprint of central's key, so the person can double-check out of band.

### Step 10: register the operator (`mcp login`)

On alice's laptop:

```console
$ echo "$REGISTRATION_CODE" | mirafleet mcp login central.example.com:2222 -name alice -code -
registered operator "alice" (active); context "alice" -> central.example.com:2222 is now current
config: /home/alice/.config/mirafleet
```

This generates alice's key pair (first time only), connects with the host key pinned from the
code, registers her public key under the name `alice`, saves central's key into `known_hosts`
and records a **context** (name plus address) in `contexts.yaml`. bob does the same with
`-name bob`. There is no approval step for operators: a valid code is the approval.

### Step 11: use it (`mcp api`)

```console
$ mirafleet mcp api fleets.list
{
  "fleets": [
    {
      "name": "dev",
      "mode": "actions",
      "confirm": false,
      "runners": 1
    },
    {
      "name": "prod",
      "mode": "actions",
      "confirm": true,
      "runners": 0
    }
  ]
}
$ mirafleet mcp api runners.list '{"fleet":"dev"}'
{
  "runners": [
    {
      "name": "build-1",
      "status": "active",
      "online": true,
      "last_seen": "2026-11-13T10:17:31Z"
    }
  ]
}
$ mirafleet mcp api action.run '{"fleet":"dev","runner":"build-1","action":"hello","params":{"name":"world"}}'
{
  "exit_code": 0,
  "stdout": "hello world\n",
  "stderr": "",
  "duration_ms": 4,
  "request_id": "9c1f0e5b7a3d4f2e8b6a1c0d9e8f7a6b"
}
```

Now the guarded `prod` fleet. Suppose `db-1` joined `prod` the same way as `build-1` joined
`dev` (steps 4 to 7), its `actions.yaml` has a `tail-logs` action (see 10.5), and bob has
registered like alice. `prod` was created with `-confirm`, so every call must carry
`"confirm":"prod"`:

```console
$ mirafleet mcp api action.run '{"fleet":"prod","runner":"db-1","action":"tail-logs","params":{"lines":"200"},"confirm":"prod"}'
{
  "exit_code": 0,
  "stdout": "Nov 13 10:20:01 db-1 postgres[812]: checkpoint complete\n...",
  "stderr": "",
  "duration_ms": 6,
  "request_id": "9f1c4b7a2d0e8635a1c9b4d7f3e2e07a"
}
```

Without the `confirm` field the call fails with `bad_request` before anything runs (section
10.6). Methods available to operators today: `hello`, `fleets.list`, `runners.list`,
`action.run`. Nothing else is possible from a laptop, by design: approving runners, making
tokens and revoking things only work on central. When M5 lands, Claude Code will call the same
methods as MCP tools.

### Step 12: look at the audit log

Dana looks at what bob's `tail-logs` call left behind:

```console
$ mirafleet central audit -since 1040 -limit 2
ID    TIME                  SOURCE  ACTOR         ACTION      TARGET     OUTCOME  MESSAGE
1041  2026-11-13T10:20:01Z  api     operator:bob  action.run  prod/db-1  start
1042  2026-11-13T10:20:01Z  api     operator:bob  action.run  prod/db-1  ok
```

(The message column is empty for these rows. Both rows carry the same `request_id`,
`9f1c...e07a`; the table does not print it, `-json` does, and the start row also records the
parameters, `lines` 200. Events come oldest first, so `-limit 2` alone shows the *first* two
ever recorded; use `-since ID` to page forward: `-since 1042` shows only later events, handy
for polling. `-json` gives machine-readable output.) Each runner also keeps its own
`audit.jsonl`; see section 10.4.

## 10.3 CLI reference

`mirafleet` has one binary and four top-level commands: `central`, `runner`, `mcp`, `version`.
Usage errors print `mirafleet: usage error: ...`. Commands that take a secret read it from stdin
and require the flag value `-`.

### `mirafleet version`

Prints the version string (`dev` for a source build).

### Central (runs on the central host)

All admin commands accept `-data DIR` (central's data directory, default
`/var/lib/mirafleet/central`) and find `admin.sock` inside it. Flags may come before or after
positional arguments. `FLEET/RUNNER` is a fleet name, a slash and a runner name. Names must match
`^[a-z0-9][a-z0-9._-]{0,62}$` (lowercase letters, digits, `.`, `_`, `-`; at most 63 characters).

| Command | Flags | What it does |
|---|---|---|
| `central init` | `-data DIR`, `-listen ADDR` (default `0.0.0.0:2222`) | Create the data dir, host key, database and registration secret. Offline, idempotent. `-listen` only appears in the hint. |
| `central run` | `-data DIR`, `-listen ADDR` (default `0.0.0.0:2222`) | Run central: SSH server plus admin socket, until SIGINT/SIGTERM. Takes the data-dir lock. |
| `central fleet create NAME` | `-mode actions\|workspace` (default `actions`), `-confirm` | Create a fleet. `-confirm` makes it guarded. |
| `central fleet list` | `-json` | List fleets with mode, confirm and non-revoked runner count. |
| `central token create` | `-fleet F` (required), `-labels k=v,k=v`, `-approve`, `-uses N` (0 = 1, max 1000), `-ttl DURATION` (0 = 24h, max 720h, min 1s) | Create a join token; prints it once on stdout. |
| `central token list` | `-fleet F`, `-json` | List tokens (id, fleet, approve, uses/max, expiry, revoked, labels). Never secrets. |
| `central token revoke ID` | | Revoke a token by its 16-hex id. Already-enrolled runners are unaffected. |
| `central runner list` | `-fleet F`, `-json` | List non-revoked runners with status, online flag, last seen and labels. |
| `central runner approve FLEET/RUNNER` | | `pending` to `active`. |
| `central runner disable FLEET/RUNNER` | | `active` to `disabled`: no new work; running work finishes. |
| `central runner enable FLEET/RUNNER` | | `disabled` to `active`. |
| `central runner revoke FLEET/RUNNER` | | Permanent. Cancels running work, closes the connection, refuses reconnects. |
| `central operator code` | | Show the current registration code, seconds left and central's host-key fingerprint. Audited (the code is not). |
| `central operator rotate-secret` | | Replace the registration secret; every outstanding code stops working. Existing operators are not affected. |
| `central operator list` | `-json` | List operators (name, status, key fingerprint, last seen). |
| `central operator revoke NAME` | | Revoke an operator and drop its live connections; in-flight actions are cancelled. |
| `central exec FLEET/RUNNER ACTION [k=v...]` | `-confirm FLEET` | Run an action as admin and print its stdout/stderr; exits with the action's exit code. Waits up to 600 s. |
| `central audit` | `-since ID`, `-limit N` (default 100, max 1000), `-json` | Print audit events with id greater than `-since`, oldest first. |

Valid runner transitions: `pending` to `active`; `active` to and from `disabled`; any
non-revoked state to `revoked`; `revoked` is final. Anything else answers
`conflict: runner is <status>: cannot <op>`.

### Runner (runs on each fleet node)

| Command | Flags | What it does |
|---|---|---|
| `runner join ADDR` | `-name NAME` (required), `-data DIR` (default `/var/lib/mirafleet/runner`), `-token -` (required, reads stdin) | Enrol with central. `ADDR` is `host:port`, and comes first (flags may follow). Writes key, `known_hosts`, `runner.yaml`. |
| `runner run` | `-data DIR` (same default), `-actions FILE` (default `/etc/mirafleet/actions.yaml`) | Connect and serve actions until SIGINT/SIGTERM. |

### Operator (runs on a laptop; cross-platform)

| Command | Flags | What it does |
|---|---|---|
| `mcp login ADDR` | `-name NAME` (required), `-code -` (required, reads stdin) | Generate a key (first time), register with central, pin its host key, save a context. |
| `mcp api METHOD [JSON]` | | Call one operator method on the current context and print the JSON result. `JSON` is the params object. |
| `mcp` (no arguments) | | Prints a note that the MCP stdio server arrives in M5. |

Environment variable: `MIRAFLEET_CONFIG_DIR` overrides the operator config directory (must be
an absolute path).

**Operator API methods** (for `mcp api`):

| Method | Params | Result |
|---|---|---|
| `hello` | `{"client":"..."}` (optional hint) | operator name, supported protocol versions |
| `fleets.list` | none | fleets with mode, confirm, runner count |
| `runners.list` | `{"fleet":"dev"}` | runners with status, online, labels, last_seen |
| `action.run` | `{"fleet","runner","action","params":{},"timeout_s":N,"confirm":"FLEET"}` | exit_code, stdout, stderr, truncated, duration_ms, request_id |

`timeout_s`: 0 or absent means 120 s; it is capped at 600 s. Note that `mirafleet mcp api` itself
gives up after 2 minutes for the whole call, so long actions need the MCP server (M5) or
`central exec`. Limits: request at most 64 KiB, at most 32 params, param keys up to 64 bytes and
values up to 4 KiB, output up to 1 MiB (stdout and stderr together; `truncated: true` if cut).

## 10.4 Files and directories

### On central (`/var/lib/mirafleet/central`, mode 0700)

| File | Mode | What it is |
|---|---|---|
| `host_ed25519` | 0600 | central's SSH private host key. Its hash is baked into every token and code. **Back this up; losing it means every runner and operator must re-enrol.** Central refuses to start if the mode is looser than 0600. |
| `host_ed25519.pub` | 0644 | the matching public key |
| `mirafleet.db` | 0600 | the SQLite database: fleets, runners, tokens, operators, settings (including the registration secret) and the audit log |
| `mirafleet.db-wal`, `mirafleet.db-shm` | 0600 | SQLite's write-ahead log and shared memory; they appear while central runs. Back up with `.db` together, or stop central first |
| `central.lock` | 0600 | lock file held while central runs, so two centrals cannot share one directory |
| `admin.sock` | 0600 | the local admin socket; removed on clean shutdown (a leftover one is cleaned up on next start) |

### On a runner (`/var/lib/mirafleet/runner`, mode 0700)

| File | Mode | What it is |
|---|---|---|
| `runner.yaml` | 0600 | `central`, `name` and `fleet`; its presence means "joined" |
| `runner_ed25519` | 0600 | the runner's private key |
| `runner_ed25519.pub` | 0644 | its public key |
| `known_hosts` | 0600 | central's pinned host key, e.g. `central.example.com:2222 ssh-ed25519 AAAA...`. The runner connects only to this key |
| `audit.jsonl` | 0600 | the runner's own append-only audit log, one JSON object per line (`start` and the outcome of every action) |

Outside the data directory: `/etc/mirafleet/actions.yaml` (default location), the allowlist. It
should be owned by root and not writable by the user the runner runs as, so a compromised
runner process cannot widen its own permissions.

### On an operator's machine (`~/.config/mirafleet` on Linux, `~/Library/Application Support/mirafleet` on macOS)

| File | Mode | What it is |
|---|---|---|
| `operator_ed25519` | 0600 | the operator's private key. **This is the operator's identity.** Anyone who copies it can act as that operator until it is revoked |
| `operator_ed25519.pub` | 0644 | the public key |
| `known_hosts` | 0600 | pinned central host keys, one line per central address |
| `contexts.yaml` | 0600 | named centrals and the current one |

Example `contexts.yaml`:

```yaml
current: alice
contexts:
    - name: alice
      addr: central.example.com:2222
```

`mirafleet mcp` builds on Windows but runs there from M5: the key loader refuses to work today
(it cannot yet verify owner-only permissions), so it fails closed.

## 10.5 Writing `actions.yaml`

This file is the runner's **allowlist**. It is the security boundary of actions mode: central and
the agent can only request names from this file, and each parameter must match a pattern you
wrote. Anything not listed is refused.

### Format

```yaml
actions:
  <action-name>:
    run: ["<command>", "<arg>", "<arg with {{param}}>"]   # required: an argv list
    timeout: 30s                                           # optional: default 60s
    params:                                                # optional
      <param-name>:
        pattern: "<regular expression>"                    # required
        default: "<value>"                                 # optional
        allow_leading_dash: false                          # optional
```

Rules, all enforced when the runner starts (a mistake stops the runner with a clear error):

- **`run` is an argv array, never a shell string.** There is no `sh -c`, no pipes, no `&&`, no
  `$(...)`, no globbing. Each array element is one argument, passed straight to the program.
- **`argv[0]`** is either an absolute path (`/usr/bin/systemctl`) or a bare command name
  (`df`). A bare name is looked up **once at start** in a fixed path
  (`/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`), never in the runner's own
  `PATH`. A name with a slash that is not absolute (`./foo`) is refused. `argv[0]` may not
  contain `{{...}}`.
- **Placeholders** are `{{name}}` (letters, digits, underscore; not starting with a digit),
  allowed inside any argument after the first, even mid-string (`--tag=x{{lines}}y`). Every
  placeholder must have a matching entry in `params`; every parameter a caller sends must exist.
- **Patterns** are Go (RE2) regular expressions. The runner wraps them so they must match the
  **whole** value, no matter how you anchored them: `[a-z]+` does not accept `abc; rm -rf /`.
  The wrapper covers the *entire* alternation: `/|/var|/home` is read as `\A(?:/|/var|/home)\z`,
  so the `|` choices cannot be used to match only a piece of the value. Make patterns as narrow
  as you can (`[a-z0-9-]{1,40}`, not `.*`).
- **`default`** makes a parameter optional. It must match its own pattern. Without a default,
  the parameter is required.
- **Leading dash:** a value starting with `-` is refused unless `allow_leading_dash: true`,
  because a value like `--delete` would be read by the program as an option (option injection).
  A default that starts with `-` also needs the flag.
- **`timeout`** is a Go duration (`500ms`, `30s`, `5m`). It must not be negative; omitted means
  60 s. When it expires the whole process group is killed and the result says `timed_out`.
  Remember that the caller has its own limit: `action.run` waits up to `timeout_s` (default 120 s,
  max 600 s), so an action that needs more than two minutes also needs `timeout_s` in the call.
- **Unknown YAML keys are errors** (a typo like `timout:` is caught, not ignored).
- **The environment is empty:** the process gets only `PATH=<fixed path>` and `LANG=C`, runs in
  `/`, and its stdout and stderr together are capped at 1 MiB. Anything your command needs (a
  config path, a credential) must be written in the command itself or in files it reads.
- **The process runs as the user running the runner.** Run the runner as an unprivileged user
  and give it exactly the access it needs (for example one `sudoers` rule for one command).
  Never run a runner as root unless every action in the file really needs it.

### Examples

```yaml
actions:
  # No parameters: the simplest and safest kind.
  uptime:
    run: ["uptime"]

  # A parameter limited to three exact choices.
  disk-usage:
    run: ["df", "-h", "{{path}}"]
    params:
      path: { pattern: "/|/var|/home", default: "/" }

  # A bounded number, with a default. This is the tail-logs action from the prod/db-1 example.
  tail-logs:
    run: ["tail", "-n", "{{lines}}", "/var/log/syslog"]
    params:
      lines: { pattern: "[0-9]{1,4}", default: "200" }

  # A service name from a strict pattern; a longer timeout.
  restart-service:
    run: ["/usr/bin/sudo", "-n", "/usr/bin/systemctl", "restart", "{{unit}}.service"]
    params:
      unit: { pattern: "(nginx|postgresql|redis)" }
    timeout: 2m

  # Dangerous example, kept in its place: never take a free-form path to a destructive command.
  # BAD: run: ["rm", "-rf", "{{path}}"] with pattern ".*"
```

Try an action locally before you rely on it (`central exec` goes through the real runner). To
see what the guard refuses:

```console
$ mirafleet central exec dev/build-1 disk-usage path='/; rm -rf /'
mirafleet: bad_request: refused by runner: refused: parameter "path" does not match its pattern
```

(The exact `mirafleet:` prefix comes from the CLI's error printer.) Every refusal is audited.

## 10.6 Troubleshooting

Find your message, read what it means, do the fix. "Central log" means central's stderr. Messages
that start `forbidden`, `unauthenticated`, `conflict` etc. are API error codes followed by a fixed
message; they never contain internal detail, so the real reason for some refusals is only in the
central log. The tables are grouped by where you are when it goes wrong.

### Starting central and running admin commands

| Message you see | What it means | What to do |
|---|---|---|
| `mirafleet: mirafleet central is supported on linux only (current: darwin)` | You ran central or runner on macOS or Windows. | Use a Linux host. Only `mcp` runs elsewhere. |
| `dial unix /var/lib/mirafleet/central/admin.sock: connect: no such file or directory` | Central is not running, or `-data` points to the wrong directory. | Start `central run`; pass the same `-data` you used there. |
| `dial unix ...admin.sock: connect: permission denied` | You are not the user that runs central (the directory is 0700). | Run the command as central's user, e.g. `sudo -u mirafleet`. |
| `another central is already using /var/lib/mirafleet/central (lock ... is held)` | Another central process has that data directory. | Stop it, or use a different `-data`. Never delete `central.lock` by hand while it runs. |
| `admin socket ... is in use by a running central` | Same cause, found at socket start. | As above. |
| `load host key (run `mirafleet central init` first): ...` | No host key in the data directory. | Run `central init` with the same `-data`. |
| `private key /...: has mode 0644; it must not be accessible to group or others (chmod 600)` | A key file has loose permissions; mirafleet refuses to use it. | `chmod 600` the file (and check who else can read the directory). |
| `database schema version N is newer than this binary supports (M): refusing to open; upgrade mirafleet` | Database was written by a newer mirafleet. | Run the newer binary; do not downgrade. |

### Joining a runner and registering an operator

| Message you see | What it means | What to do |
|---|---|---|
| `-token must be - (the token is read from stdin, never from argv or env)` | You passed the token on the command line. | `echo "$TOKEN" \| mirafleet runner join ... -token -` |
| `-code must be - (the code is read from stdin, never from argv)` | Same for the registration code. | Pipe it: `echo "$CODE" \| mirafleet mcp login ... -code -` |
| `no join token on stdin` / `no registration code on stdin` | Stdin was empty. | Check the variable is set; mind that the pipe goes into the right command. |
| `invalid join token` / `invalid registration code` | The string is malformed (truncated, extra spaces, or wrong kind). | Copy it again in full. Tokens start with `mfj1.`, codes with `mfr1.`. |
| `unauthenticated: join refused` | **One message for many causes** (deliberately): token unknown, wrong secret, expired, revoked, used up, or made for a different central. | Check the central log for the true reason. Create a new token with `central token create`. |
| `unauthenticated: registration refused` | The code is wrong, was for an earlier 60 s window, was made before `rotate-secret`, or is for another central. | Get a fresh `central operator code` and use it within its window. |
| `conflict: conflict` (on join) | That runner name already exists in the fleet, or the key is already in use (revoked runner keys can never rejoin; a key can't be both runner and operator). | Choose a different `-name`, or use a fresh `-data` directory (new key). |
| `conflict: conflict` (on login) | The name is held by another operator, or this key already belongs to a runner or a revoked operator. | Choose another name; revoked operators must use a new key (delete the config dir). |
| `this data directory has already joined a central (runner.yaml exists); use a fresh -data directory` | `runner join` ran twice. | Use a new `-data`, or delete the old directory if you really want to start over. |
| `host key does not match the pinned key (server presented SHA256:...)` | During join/login: the server's key is not the one in your token/code. Could be the wrong address, a rebuilt central, or an attack. | Do not continue. Verify the address and the fingerprint (`central operator code` prints it). |
| `invalid name "Alice" (lowercase letters, digits, . _ -; max 63)` | Name rules. | Use a lowercase name. |
| `central address "central.example.com": ... missing port in address` | The address needs a port. | `central.example.com:2222` |
| `operator key storage is not supported on windows yet ...` | `mirafleet mcp` builds on Windows but cannot verify private-key permissions yet, so key loading fails closed. | Use Linux or macOS; Windows runs from M5. |

### Starting and running a runner

| Message you see | What it means | What to do |
|---|---|---|
| `runner is not joined: run `mirafleet runner join` first` | `runner run` found no `runner.yaml`. | Run `runner join`, and use the same `-data` for both. |
| `host key for central.example.com:2222 (SHA256:...) not pinned in .../known_hosts` | After join/login: central's key changed (for example `init` ran on a fresh directory) or the file lacks the line. | If the change is expected, re-join / re-login. If not, treat as an attack. |
| `ssh: handshake failed: ssh: unable to authenticate, attempted methods [none publickey], no supported methods remain` | Central does not accept your key: the runner or operator was **revoked**, never registered, or central's database was replaced. | Check `central runner list` / `central operator list`; re-enrol if needed. |
| `actions: "x": command "foo" not found in /usr/local/sbin:...` (runner start) | A bare `argv[0]` is not in the fixed path. | Use an absolute path or install the command. |
| `actions: "x": placeholder {{p}} has no param definition` | `run` uses `{{p}}` but `params` lacks `p`. | Define the parameter. |
| `actions: "x": default of "p" does not match its pattern` | Default must match its own pattern. | Fix one of them. |
| `actions: yaml: unmarshal errors: ... field timout not found ...` | Unknown or misspelled key. | Fix the key name. |
| Runner log: `disconnected: ...; retrying in 1.2s` | Connection lost; backoff in progress. | Read the reason after `disconnected:`; it is usually network, a changed host key or revocation. |

### Running an action (`mcp api` and `central exec`)

| Message you see | What it means | What to do |
|---|---|---|
| `no current context: run `mirafleet mcp login ADDR -name NAME -code -` first` | `mcp api` has no context. | Run `mcp login`. Check `MIRAFLEET_CONFIG_DIR` is the same as at login. |
| `forbidden: forbidden` | The policy said no. As an operator: your operator is revoked, or you called a method you may not (admin methods are never available over `api@`). | Check `central operator list`. Use the CLI on central for admin tasks. |
| `bad_request: invalid fleet or runner name` / `invalid fleet name` | Name does not match the name rules or the field is empty (`runners.list` needs `{"fleet":"..."}`). | Fix the JSON. |
| `bad_request: invalid params` | The JSON had an unknown field or trailing junk (params are decoded strictly). | Check field names against the table in 10.3. |
| `bad_request: fleet requires confirm to equal the fleet name` | A guarded fleet needs `"confirm":"<fleet name>"`. | Add it (or `-confirm FLEET` for `central exec`). It is audited as `denied`. |
| `conflict: fleet is not in actions mode` | `action.run` on a workspace fleet. | Workspaces arrive in M3. |
| `conflict: runner is pending` (or `disabled`) | The runner is not `active`. | `central runner approve` / `enable`. |
| `not_found: not found` | The fleet or runner does not exist, or the runner is revoked. | `central fleet list`, `central runner list`. |
| `unavailable: runner unavailable` | Central could not reach the runner: it is offline, busy, or the connection broke. | `central runner list` (ONLINE column); check the runner's log and network. If busy, wait: a runner runs 4 actions at a time. |
| `unavailable: runner revoked` / `unavailable: request cancelled` | The runner or operator was revoked mid-run, or the caller disconnected. | Expected after a revoke. The audit shows `cancelled`. |
| `unavailable: audit unavailable` | Central could not write the "start" audit row, so it refused to run the action (fail closed). | Check the central log, disk space and the database file. |
| `timeout: action timed out` | The call's `timeout_s` expired. | Raise `timeout_s` (max 600), or fix the action. |
| `bad_request: refused by runner: refused: unknown action "x"` | The action is not in that runner's `actions.yaml`. | Add it and restart the runner, or fix the name. Each runner has its own file. |
| `bad_request: refused by runner: refused: parameter "p" does not match its pattern` | Value failed the regex (or a required parameter is missing: `missing required parameter`). | Send a valid value, or widen the pattern carefully. |
| `bad_request: refused by runner: refused: parameter "p" must not start with '-'` | Leading-dash guard. | Use `allow_leading_dash: true` only if the program really needs it. |
| result has `exit_code: -1` and `timed_out: true` / stderr mentions the binary | The action timed out and was killed, or the program could not start (not executable, missing). | Check `timeout:` and the command path. |
| `timeout: request not received within 10s` | A client opened a channel and sent nothing. | Usually a broken client; retry. |
| `too_large: request exceeds 65536 bytes` | Request over 64 KiB. | Send less data. |
### What to look for in central's log

| Message you see | What it means | What to do |
|---|---|---|
| Central log: `connection from ... refused before handshake: per-IP handshake limit` (or `global handshake limit`) | Too many half-open connections from one source (or in total). | Wait; look for a misbehaving client; add a firewall rule. |
| Central log: `enroll connection from ... refused: rate limit exceeded` | Too many join/login attempts from one IP (10 per minute) or overall (60 per minute). | Wait a minute. |
| Central log: `runner "x" silent for >45s; closing connection` | A registered runner sent no heartbeat for 3 intervals (3 x 15 s); the connection is cut and the runner reconnects. | Check the runner host and network. |
| Central log: `runner "x" (id N) silent for >45s; closing connection` | The `(id N)` form is only for a connection that authenticated but never registered, i.e. never sent a first heartbeat. | Same: check the runner's version, host and network. |
| Central log: `join from ... refused: token ... pins another host key` | A token from a different central was used. | Make the token on the right central. |

## 10.7 FAQ

**Can an agent approve a runner, create tokens or revoke things?** No. Those methods exist only
on central's local admin socket. Over `api@mirafleet` they return `forbidden`, and a test
(`TestM2Done`) proves it.

**Where is the runner's SSH port? Do I open one?** There is none. Runners dial out to central;
only central listens. That is why a runner can sit behind NAT or a strict firewall.

**What happens if central restarts?** Runners lose their connection and reconnect by themselves
(backoff 1 s up to 60 s, with random jitter). Operators just reconnect on their next call. In-flight
actions are cut off (the runner kills them when the connection drops); whether their outcome row
is written depends on central shutting down cleanly. Revoked runners stay revoked, because the status is in the
database.

**What happens if a runner's host is lost?** `central runner revoke FLEET/RUNNER`. The key can
never be used again; a replacement joins under the same name with a new token (revoked names are
reusable, revoked keys are not).

**How do I rotate the operator registration secret?** `mirafleet central operator rotate-secret`.
Every outstanding code dies. Operators that already registered keep working (they authenticate
with their keys, not the code). To remove an operator, `central operator revoke NAME`.

**An operator's laptop was stolen.** Dana runs `central operator revoke NAME` straight away. It closes the
live connection and cancels any running action. The thief cannot use the key afterwards.

**How do I back up central?** See section 10.8.

**Can I run two centrals for high availability?** Not in the open-source edition. One central
owns one data directory (that is enforced with a lock), and each node talks to exactly one
central. HA is an enterprise topic; the code only leaves seams.

**Can I change a fleet's name or move a runner between fleets?** Not today. Revoke and re-join
with a token from the other fleet.

**Why does my action work in my shell but fail through mirafleet?** The guard runs it with an
empty environment (only `PATH` and `LANG=C`), in `/`, as the runner's user, with no shell.
Use absolute paths, avoid shell syntax, and check the user's permissions.

**How do I see what an agent did?** `mirafleet central audit`. Every `action.run` has a `start`
row and an outcome row with the same `request_id`, plus the operator's name. Parameters appear
in the start row, so never design an action that takes a secret as a parameter.

**Is there a GUI?** Planned for M4 ([PLAN.md](../PLAN.md)). Everything it will do is already
available on the CLI.

## 10.8 Day-2 operations

### Where logs and audit data live, and how they grow

- **Central's log** goes to stderr. Under systemd that is the journal; otherwise redirect it to
  a file and rotate it with your usual tool. Central never writes its own log file.
- **Central's audit log** is the `audit` table in `mirafleet.db`. It is append-only, enforced by
  database triggers, so there is no built-in way to delete old rows. It grows by two rows per
  `action.run` (start and outcome) plus one per admin action. Plan disk space for it. To keep an
  external copy, page through it with `central audit -json -since ID` and ship the output to
  your log store (a SIEM, a tool that collects and searches security logs).
- **A runner's audit** is `audit.jsonl` in its data directory: one JSON line per event, appended
  forever. The runner does not rotate it; rotate or ship it yourself (copy-and-truncate with
  your usual log tool).

### Backup and restore of the data directory

Back up `/var/lib/mirafleet/central` as one unit: `host_ed25519`, `mirafleet.db` and its `-wal`
and `-shm` files. The safest way is to stop central, copy the directory, start central. Copy
the files together; the database alone, without its write-ahead log, can miss recent writes.
Keep the copy private: it holds the registration secret and, with the host key, lets someone
impersonate central.

To restore, stop central, put the directory back with the same owner and modes (the directory
0700, the key 0600), and run `central run`. Runners and operators reconnect by themselves.
Anything that happened after the backup (new runners, operators, audit rows) is lost, so a
restored central may refuse a runner it never heard of: re-join that runner.

### Lost or replaced central host key

The host key is what every token, code, runner and operator pins. If `host_ed25519` is lost and
you run `central init` on a fresh directory, central gets a **new** key. Then every runner and
operator fails with `host key ... not pinned` or `host key does not match`, on purpose: from
their side this looks exactly like an impostor. The fix is to re-pin: every runner needs a
fresh `runner join` with a new token (new `-data` directory), and every operator a new
`mcp login` with a new code. This is why the host key is the file to back up.

### Upgrading central, including a migration

1. Read the release notes (planned for M6; for now `git log` and `docs/PLAN.md`).
2. Stop central and back up the data directory (above).
3. Replace the binary and start central. On start, central compares the database's schema
   version (SQLite `PRAGMA user_version`) with the migrations built into the binary and applies
   the missing ones in order, each inside one transaction. A database from a *newer* binary is
   refused (`database schema version N is newer than this binary supports`).
4. There is no downgrade. To go back, restore the backup from step 2 and run the old binary.
5. Upgrade runners and operators at your own pace; the wire protocol is versioned
   ([chapter 7](07-protocols.md)). A runner that rejects a new `actions.yaml` field is an older
   binary: upgrade it first.

## Check your understanding

1. Dana runs `mirafleet central fleet list` from her laptop and gets
   `command not found` or a socket error. Why can she not administer central from her laptop,
   and what is the right way?
2. You edit `/etc/mirafleet/actions.yaml` on `build-1` and add an action. alice still gets
   `refused: unknown action`. What did you forget?
3. A join fails with `unauthenticated: join refused`. Name three possible causes, and say why
   the message does not tell you which one it is.
4. `central operator code` shows `valid for: 7s`. alice pastes it, but her `mcp login` takes
   10 seconds to start. Will it work? Does bob need his own code?
5. Why is a pattern like `.*` on a path parameter dangerous even though there is no shell?

<details>
<summary>Answers</summary>

1. Administration is only possible through the local admin socket on the central host, by the
   same Unix user that runs central; laptops are operators and can only call the four operator
   methods. Dana must log in to the central host (for example over her normal SSH access) and
   run the command as central's user.
2. Restart the runner. `actions.yaml` is read once at start (`runner run`). Also check that the
   action is in the file of *that* runner: each runner has its own allowlist.
3. Any of: the token was already used, expired, revoked, unknown, mistyped, or made by a different
   central. The message is identical on purpose so that someone probing with guessed tokens cannot
   learn which ids exist or how close a guess was. The real reason is in central's log.
4. No: get a fresh code. The code is checked against the **current** 60 s window at the moment
   central receives the request, so by then it has expired. bob does not need his own: any
   number of operators can use one code in its window.
5. There is no shell, but the program still interprets the value. With `.*` a caller can pass any
   path (`/etc/shadow`, `../..`) or, if the program takes options, a value that changes its
   behaviour. The guard stops injection into the command line structure; it cannot know what is
   safe *for the program*, which is why patterns must be as narrow as the use case allows.

</details>
