# 9. Dev setup and testing

This chapter gets you from a fresh laptop to a green `scripts/dev.sh test`, explains why we build
inside containers, and teaches you how the tests are built so you can write your own.

## What you will learn

- Why every Go step runs in a container, and how to set that up on macOS (colima) and Linux.
- What each `scripts/dev.sh` command does, what it costs in time, and how the cache volumes and
  host user ID work.
- The testing philosophy: no real hosts, ports or data; fakes where it is safe, real SQLite
  where it matters.
- A tour of the test helpers and what each important test proves, including `TestM2Done`.
- How to write a new test step by step, and how to fix the failures you will most likely meet.
- How to run central, a runner and an operator by hand in one container ([9.11](#911-try-it-locally-central-runner-and-operator-in-one-container)).
- The M3 additions: `scripts/dev.sh integration` (real containerd 2.4.2 in a throwaway privileged
  container, with a hermetic test image), `TestM3Done` and its real-containerd twin, and how to try
  a workspace by hand ([9.12](#912-try-it-locally-a-workspace)).

## 9.1 The container-only rule

From [CLAUDE.md](../../CLAUDE.md): *nothing is installed or run on the host toolchain.* There is no
host `go`, no `pip`, no `npm` for this repository. Every build, test and lint step runs inside a
container through `scripts/dev.sh`.

Why do we do this?

1. **Everybody builds the same thing.** The container image is pinned by tag **and** by digest
   (`golang:1.27.3-bookworm@sha256:...` in `scripts/dev.sh`). A teammate on an M-series Mac, a
   teammate on Ubuntu and the future CI all use byte-identical compilers. "Works on my
   machine" cannot happen because the machine is the image.
2. **The host stays clean.** A project that needs a particular Go version, a C compiler for the
   race detector, `go-licenses` and later `shellcheck` and `goreleaser` would otherwise litter
   your laptop. Here, new tools run as pinned container images and are thrown away afterwards.
3. **It is a security habit.** mirafleet is software that runs commands on servers. Building it
   should not require running someone else's install script on your machine.
4. **The tests assume Linux.** Central and runner are Linux-only, and several tests execute
   `/bin/echo`, `/usr/bin/false` and `/bin/sleep` or use Linux-only socket features. The
   container gives macOS developers a real Linux to run them on.

The only things you install on the host are the **container runtime** and its **command-line
client**. They are infrastructure, not repository tooling.

A note on the word "container", because there are two uses. The product's workspaces (M3) use
containerd through its Go client; our developer tooling runs in Docker containers (colima on
macOS). The Docker ecosystem is supported; there is no hard dependency on Docker's commercial
products (such as paid Docker Desktop tiers). That is why the macOS setup below uses colima.

## 9.2 Setting up

### macOS: colima and the Docker CLI

[colima](https://github.com/abiosoft/colima) runs a small Linux virtual machine and starts the
open-source Docker engine inside it. You then use the ordinary `docker` command-line client
against it.

1. Install `colima` and the `docker` CLI with your usual package manager (many people use
   Homebrew). Install only the command-line client, not a paid desktop application.
2. Start the VM with the Docker runtime:

   ```bash
   colima start
   ```

3. Check that the client can reach it:

   ```bash
   docker info
   ```

4. From the repository root, run `scripts/dev.sh build`. The first run pulls the Go image
   (a few hundred megabytes) and downloads the Go modules into a cache volume. This is slow
   once; afterwards it is fast.

**Gotcha: colima only shares `$HOME` with the VM by default.** If your clone lives outside your
home directory (for example `/work/mirafleet`), the container sees an empty `/src` and
`scripts/dev.sh` stops with:

```text
source not visible in the VM: colima only mounts $HOME by default
```

Move the clone under `$HOME`, or configure an extra mount in colima.

### Linux: plain Docker

Install Docker Engine from your distribution (the `docker.io` package on Debian and Ubuntu) and
add yourself to the `docker` group so you do not need `sudo`. Then run `scripts/dev.sh build`.

If you prefer containerd, set `MIRAFLEET_CTR=nerdctl` (nerdctl is a Docker-compatible
command-line client for containerd). The script uses the command in
`MIRAFLEET_CTR` (default `docker`) and skips the "is Docker running" check when it is set. That
variable and `MIRAFLEET_GO_IMAGE` are *trusted* environment: the script splits and executes the
value, so never fill them from untrusted input.

### Check it worked

```bash
scripts/dev.sh build     # prints nothing on success
scripts/dev.sh test      # prints one "ok" line per package
```

If `docker` is installed but not running, `dev.sh` prints
`Docker runtime not reachable: start it with `colima start` (Docker runtime)` and exits 1.

## 9.3 Every `scripts/dev.sh` command

All commands run `sh -ec` inside the pinned image (`sh -c` runs a command string, and `-e`
makes the script stop at the first command that fails), with the repository mounted at `/src`.
"Mode" is how the source is mounted: `ro` read-only (the command cannot change your files) or
`rw` read-write. Times are rough figures for a recent laptop with a warm cache; they are
estimates, not benchmarks, and the first run after a fresh clone or a new image is much slower
because of downloads.

| Command | Mode | What it runs | Roughly |
|---|---|---|---|
| `scripts/dev.sh fmt` | ro | `gofmt -l .`; fails and lists the files if any are not formatted | seconds |
| `scripts/dev.sh build` | ro | `CGO_ENABLED=0 go build ./...` and `CGO_ENABLED=0 go build -o /dev/null ./cmd/mirafleet` | 10 to 60 s |
| `scripts/dev.sh vet` | ro | `go vet ./...` | 10 to 30 s |
| `scripts/dev.sh test` | ro | `go test -race ./...` then `CGO_ENABLED=0 go test ./...` | 1 to 4 min |
| `scripts/dev.sh cross` | ro | for each of 6 targets: `go build -o /dev/null ./cmd/mirafleet` and `go vet ./...` | 1 to 3 min |
| `scripts/dev.sh licenses` | ro | installs `go-licenses` v1.6.0, then `go-licenses check ./...` for each of 6 targets | 2 to 6 min |
| `scripts/dev.sh integration` | ro (+ **privileged**, see below) | builds `-tags integration` test binaries, the real `mirafleet` binary and containerd 2.4.2; runs them as root against a real containerd in a throwaway `docker:28-dind` container | 3 to 10 min (first run: downloads and builds containerd) |
| `scripts/dev.sh go <args>` | rw | `go <args>` (for example `go mod tidy`, or a single test) | depends |
| `scripts/dev.sh shell` | rw | an interactive `bash` in the container | n/a |

Details that matter:

- **`test` runs the suite twice on purpose.** The race detector needs cgo, so
  `go test -race` runs with cgo enabled. Shipped binaries are built with `CGO_ENABLED=0`
  (a static binary), so the second run repeats the tests the way production is built.
- **`cross` compiles and vets only.** It builds for `linux`, `darwin` and `windows`, each on
  `amd64` and `arm64`. It does not run tests (there is no darwin or Windows machine in the
  container). What it catches: a Unix-only function called from code that must also build on
  Windows, a missing build tag, a missing `_other.go` stub.
- **`licenses` loops over all six targets** because some modules are linked only on some
  platforms (for example `golang.org/x/sys` pulls in different files on Windows).
- **`go` is the escape hatch.** Because it mounts the source read-write it is the only
  command (besides `shell`) that can modify files, for example `scripts/dev.sh go mod tidy`
  or `scripts/dev.sh go test -run TestM2Done -count=1 ./internal/central/`.

### `scripts/dev.sh integration`: a real containerd, safely

Workspace mode puts commands into real containers, and containers need a real kernel, root and
a real `containerd`. A unit test cannot prove that "the root filesystem is read-only" or "a fork
bomb hits the pids limit"; only a real container can. But the project rule is "nothing runs on the
host", and containerd needs root. `integration` squares this:

1. **Build step** (the pinned golang image, as your user, source mounted read-only). It writes into
   a named volume, `mirafleet-itest-bin`, three kinds of static binaries:
   - test binaries built with `go test -c -tags integration` for the packages in `ITEST_PKGS`
     (default `./internal/workspace/ctrd ./cmd/mirafleet ./internal/central`);
   - the real `mirafleet` binary (the built-binary test drives it through its CLI);
   - **containerd 2.4.2 and its runc shim, built from the very module version pinned in `go.mod`**
     (it reads the version with `go list -m`). The repository's `go.mod` only lists the client,
     so the daemon is built in a scratch module under `/tmp` inside the container; `go.mod` and
     `go.sum` are never touched and none of it is linked into mirafleet. This step needs the Go
     module proxy for the daemon's extra modules (cached in `mirafleet-gomod` afterwards).
2. **Run step**: a throwaway `docker:28-dind` container (pinned by tag and digest) started with
   `--privileged`. dind supplies `runc` and the userland; its bundled containerd 1.7 is **not**
   used. The script sets up cgroup v2 nesting (bounded to 50 tries), starts our containerd 2.4.2
   directly (no `dockerd`) with its root and socket on `tmpfs`, then runs every test binary as
   root. Test binaries are mounted read-only. The exit status is the tests' exit status.
3. **Hermetic test image.** No test pulls anything from a network. The workspace image is built
   *at test time*: the test builds an OCI layout tarball in memory from the test binary itself
   (which doubles as the in-container PID 1 and as a tiny tool) plus a minimal `/etc/passwd`, and
   imports it with `client.Import`. The image is therefore the same bytes every run, and the
   digest-pinned images are only the two tool images (golang, dind).
4. **Everything dies with the container.** The containerd root, sockets, snapshots and workspace
   directories live on `tmpfs` or inside the `--rm` container.

Two cautions, both in comments in `dev.sh`:

- **`--privileged` is for dev and CI only, and only on a VM-backed runtime** (colima, Docker
  Desktop). There it is confined to the VM and never touches the host. On a bare Linux host with a
  native Docker daemon it would hand the container full host privileges, so there you use a
  throwaway VM instead.
- Only `^TestIntegration` (in `cmd/mirafleet`) and `^TestM3DoneIntegration` (in `internal/central`)
  run from the packages that also hold ordinary unit tests; those are already covered by
  `scripts/dev.sh test`. The ctrd package's integration tests all run.

The tests have two layers of "is this host capable": `userns: required` needs kernel 5.19 or
newer, user namespaces enabled and an overlayfs snapshotter that supports id remapping (containerd
2.x does). The built-binary test tries `required` first and falls back to `off` (and says so in
its log) on a host that cannot do it; the production runner does **not** fall back (chapter 10).

### Volumes and the host user ID

`dev.sh` creates two named Docker volumes, shared by every run:

- `mirafleet-gomod` mounted at `/go/pkg/mod` (downloaded modules);
- `mirafleet-gocache` mounted at `/gocache` (the Go build cache and `GOPATH`).

They are why the second `build` takes seconds instead of minutes.

Docker creates new volumes owned by root. If the build then ran as your user, it could not write
its cache. The script solves this in two steps:

1. A first tiny container run (as root) checks the owner of both volumes and `chown`s them to
   your user and group if they differ. This is idempotent, so it costs almost nothing once it
   has run.
2. The real container runs with `--user "$(id -u):$(id -g)"`, i.e. as **you**. Files it writes
   into your source mount (when you use `go` or `shell`) therefore belong to you, never to root.

Other environment settings inside the container: `HOME=/tmp` (your user has no home directory in
the image), `GOFLAGS="-buildvcs=false -modcacherw"` (no VCS stamping, and module cache files stay
writable so volumes can be cleaned), `GOCACHE`, `GOPATH` and `GOMODCACHE` pointed at the volumes.

To start from zero (for example after a corrupted cache):

```bash
docker volume rm mirafleet-gomod mirafleet-gocache
```

## 9.4 Testing philosophy

The rules come from CLAUDE.md and [docs/PLAN.md](../PLAN.md) ("Rules for every milestone"):

1. **No real hosts, ports or data.** Tests listen on port 0 (`127.0.0.1:0`, the OS picks a free
   port; the test reads the real address back), never on 2222 or 22.
2. **No real files.** Use `t.TempDir()` (the Go testing package creates a fresh directory and
   deletes it when the test ends). Never write to `/var/lib/mirafleet`, your home directory or
   the repository.
3. **Keys are generated at test time.** Never commit a test key, not even a throwaway one. Use
   `keys.Generate(dir, name, comment)`, or `ed25519.GenerateKey` for in-memory keys. A committed
   key would be a secret in the repo, and scanners cannot tell a test key from a real one.
4. **Real SQLite temp files, not mocks, where the database behaviour matters.** The sqlite store
   has subtle behaviour (transactions, unique indexes, triggers, migrations). A mock would only
   prove that our mock agrees with us. Tests use
   `sqlite.Open(ctx, filepath.Join(t.TempDir(), "mirafleet.db"))`. (`:memory:` is refused by
   `Open` on purpose.)
5. **Fakes where it is safe and where you need to force a failure.** The service tests use a
   `fakeLive` (a stand-in for the registry that records calls) and `fakeConns`, so they can say
   "the runner is gone" or "the audit write fails" on demand. Real objects can rarely fail at
   exactly the line you need.
6. **Inject the clock.** Nothing in `service` or `store` calls `time.Now()`; the caller passes
   `now`. Tests move time by returning a different value (`TestM2Done` jumps past the 60-second
   registration step this way, with no `time.Sleep`).
7. **Tests that wait must also give up.** Use `waitFor(t, cond)` (polls up to 10 s and then
   fails) instead of a bare `time.Sleep`. Anything that blocks needs a context or timeout.
8. **Test the unhappy path first.** A security feature without a test that attacks it is a
   hope. For every defence there should be a test that shows the attack failing; chapter 5 names
   them.

The platform-specific tests follow the code: files ending `_unix_test.go` or carrying
`//go:build unix` only compile where the feature exists (see [chapter 8](08-code-tour.md#87-build-tags-and-why)).

## 9.5 Tour of the test helpers

This section is a map of what exists. To see the helpers used in a real test, jump to
[9.7](#97-writing-a-new-test-step-by-step).

### `newEnv` in `internal/central` (the big one)

`internal/central/integration_test.go` defines `type env`, a complete miniature deployment inside
one test process. Nearly every file in that package uses it.

`newEnv(t, "r1", "r2")` builds, in this order:

1. a temp directory with a freshly generated **host key** and a `known_hosts` file pinning it;
2. a parsed `actions.Set` from the constant `testActions` (`hello`, `fail`, `slow`, `long`);
3. a **real SQLite store** in another temp directory, with one fleet, `f1`;
4. a real `registry.Registry`, `OperatorConns`, `policy.TablePolicy{}` and `service.Service`;
5. one runner row per name, enrolled in the store (`addRunner`) with a fresh key pair;
6. a started central server on `127.0.0.1:0` (`startCentral`).

Variants: `newEnvHB(t, hb, names...)` sets the heartbeat interval (the registry treats `3 x hb` of
silence as offline), and `newEnvOpts(t, hb, mod, names...)` also lets a test tweak the central
`Config` (for example to set tiny rate limits).

Helper methods you will use constantly:

| Helper | What it does |
|---|---|
| `e.startRunner(name, mods...)` | runs a real `runner.Runner` with fast heartbeat and backoff; stopped by `t.Cleanup` |
| `e.waitOnline(names...)` | waits until the registry has live sessions for those runners |
| `e.exec(runner, action, "k", "v"...)` | calls `Registry.Exec` directly (tests the runner's guard) |
| `e.run(runner, action, ...)` | calls `Service.RunAction` as admin (tests policy and audit rows) |
| `e.addRunner(name, status)` / `tryAddRunner` | enrol a runner through the store with a given status |
| `e.addOperator(name)` / `e.opClient(name)` | register an operator in the store; connect as it with `apiclient` |
| `e.dial(user, keyName)` | raw `ssh.Dial` to central as any SSH user with any key (for attack tests) |
| `e.rawRunner(name, onChan)` | a hand-made SSH "runner" that can misbehave (for protocol tests) |
| `e.auditRows()` / `e.auditRowsFor(action)` | read the audit table |
| `e.authRejected(user, key)` | true if central refuses that key for that SSH user |
| `e.logText()` | everything central logged (to assert "the real reason is in the log only") |
| `tctx(t)`, `must`, `wantCode(t, err, code)`, `waitErr` | context with timeout, fail-on-error, assert an `api` error code, wait for an error |

### Other fixtures

| Where | Helper | Purpose |
|---|---|---|
| `internal/service/service_test.go` | `newEnv`, `fakeLive`, `wrapStore`, `callLog`, `newKey` | a service over a real temp SQLite store, a fake registry that logs `kick 3 runner revoked`-style lines, a store wrapper that can fail one method, a fixed clock `t0` |
| `internal/service/enroll_test.go` | `fakeConns`, `initSecret`, `currentCode`, `token` | enrol tests: record kicked operators, create secrets and codes |
| `internal/store/storetest` | `Run(t, newStore)` | the conformance suite; any `Store` must pass |
| `internal/store/sqlite/sqlite_test.go` | `open(t)` | a temp-file database |
| `internal/apiclient/fake_test.go` | `startFake`, `fakeServer` | a tiny scripted SSH server that answers framed requests |
| `internal/runner/runner_test.go` | `startFake`, `fakeCentral` | a scripted central that closes or stalls connections to test backoff and heartbeat timeouts |
| `internal/auth/auth_test.go` | `fakeStore`, `meta`, `fixture` | key lookups without a database |
| `cmd/mirafleet/admin_test.go` | `fakeAdmin` | the admin CLI with `call` replaced, so no central runs |
| `cmd/mirafleet/central_linux_test.go` | `centralTestDir` | a **short** temp path, because unix socket paths are limited to roughly 100 characters and `t.TempDir()` can be too long |
| `cmd/mirafleet/enroll_linux_test.go` | `newCenv` | a central plus the real `runner join` and `mcp login` code paths |

## 9.6 What the important tests prove

You do not need to read all of them. Know these and where they are.

| Test (file) | What it proves |
|---|---|
| `TestM2Done` (`internal/central/m2_done_test.go`) | The whole M2 scenario end to end (below). |
| `TestTwoRunnersActions`, `TestCentralAuditOutcomes` (`integration_test.go`) | Two runners run actions, refusals and non-zero exits come back correctly, and audit rows get the right outcomes. |
| `TestRunnerPinsHostKey`, `TestRunnerReconnectsAfterCentralRestart` | A runner refuses a central with another key, and recovers after a restart. |
| `TestChannelAllowList`, `TestRoleChannelIsolation` | Each SSH role can open only its own channel types. |
| `TestEndToEndJoinRegisterAndRun` (`api_test.go`) | Join, register and run over real SSH with the real client code. |
| `TestAPIAdminAndEnrollMethodsForbidden` | An operator calling an admin or enrol method on `api@` gets `forbidden` (`TestForbiddenMethodAttemptIsLogged` checks the log line). |
| `TestRevokeRunnerDuringLongAction` (`revoke_test.go`) | Revoking a runner cancels its in-flight action, audits `cancelled`, and blocks reconnects. |
| `TestRevokeOperatorDuringAction` | Revoking an operator drops its connection and cancels its action. |
| `TestDisableLetsInFlightFinish` | `disable` is gentler than `revoke`: running work finishes. |
| `TestAdminForeignUIDRejectedBeforeAnyRead` (`hardening_unix_test.go`) | A different user on the machine cannot use the admin socket; nothing is read from them. |
| `TestPreHandshakeConnectionLimit`, `TestEnrollRateLimit`, `TestHandshakeDeadlineDropsSilentPeer` | The connection and rate limits actually bite. |
| `TestOnlyEd25519KeysAreAccepted` | RSA, ECDSA and certificates are refused for every user. |
| `TestRunActionStartAuditFailureNeverExecs` (`internal/service/exec_test.go`) | If the start audit row cannot be written, nothing runs. |
| `TestJoinFailuresAreIdentical` (`enroll_test.go`) | Every join failure returns byte-identical errors. |
| `TestRegisterSameCodeTwoOperators` | Two operators can use one code in its 60 s window. |
| `TestConformance` (`internal/store/sqlite`) | The sqlite store passes the whole `storetest` suite. |
| `TestAuditAppendOnly`, `TestAuditFailureRollsBackStateChange` | The database refuses updating audit rows, and a failed audit write undoes the state change. |
| `TestInjectionAttempts`, `TestFullMatchRegex`, `TestLeadingDash` (`internal/actions`) | Shell-metacharacter, regex-anchor and `-option` tricks fail. |
| `TestCodeKnownAnswer`, `FuzzParseJoinToken`, `FuzzParseCode` (`internal/tokens`) | The code format never changes by accident; parsers survive garbage. |
| `TestNoSQLiteOnNonLinux`, `TestLinuxLinksSQLite` (`cmd/mirafleet/deps_test.go`) | darwin and windows binaries do not link SQLite. |
| `TestNoContainerdOnNonLinux`, `TestLinuxLinksContainerd` (same file) | darwin and windows binaries link no containerd or gRPC, and the Linux binary does (positive control). |
| `TestM3Done` (`internal/central/m3_done_test.go`) | The whole M3 scenario with the fake executor, on every `dev.sh test` (below). |
| `TestM3DoneIntegration` (`m3_done_integration_linux_test.go`) | The same scenario against a real containerd, in `dev.sh integration`. |
| `TestIntegrationWorkspaceCLI` (`cmd/mirafleet/e2e_integration_linux_test.go`) | The **built binary**: `central`, `runner run -workspace` and `mcp api` driven through the CLI with real containerd: open, sticky, exec (incl. stdin), chunked write and read with `-in`/`-out`, `workspace list/kill`, `runner workspace list/prune`. |
| `TestExecIdentityAndHardening`, `TestFileAttacks`, `TestForkBombHitsPidsLimit`, `TestMemoryHogIsOOMKilled`, `TestDiskQuota` (`internal/workspace/ctrd`, integration) | A real container is non-root, has a read-only rootfs and no capabilities; symlink, FIFO, hard-link and `/proc/self/root` tricks fail; a fork bomb and a memory hog are stopped by cgroups; the soft disk quota refuses writes. |
| `TestEgressProxyInNetns`, `TestEgressNetworkNone`, `TestEgressRefusals`, `TestEgressReattachAfterRestart` (ctrd, integration) | The allow-list proxy listens inside the container's network namespace, `none` has no route, metadata and private ranges are refused, listeners come back after a runner restart. |
| `TestExecWorkspaceStartRowFailClosed`, `TestExecWorkspaceEnvValuesNeverAudited` (`internal/service`) | If the start row cannot be written nothing runs; env values never reach the audit log. |
| `TestReconcileTable`, `TestReapIdleAndMaxTTL`, `TestRevokeRunnerCascade`, `TestReapSkipsInFlightOp` (`internal/service`) | The reconcile table, idle and absolute TTL reaping, revoke cascades, and "a workspace with a command in flight is never idle-reaped". |
| `TestCleanPath`, `TestFilterEnv`, `TestMatchImage`, `TestCaptureBudgetRandom` (`internal/workspace`) | Path confinement, the env denylist, image-pattern matching and the output budget hold (including random input). |
| `TestCentralInitIsIdempotent` (`central_linux_test.go`) | Running `central init` twice never replaces the host key or secret. |

### `TestM2Done`: the acceptance test

Every milestone has a "done when" sentence in [PLAN.md](../PLAN.md). For M2 that sentence is
`TestM2Done`. If this test is green, the milestone's promise holds. In order, it:

1. builds an `env` and a service with an **injected clock** pinned inside one registration step;
2. creates a fleet and a join token;
3. **joins** a runner with `apiclient.Join`, pinning central's host key from the token alone,
   and checks it is `pending`;
4. **approves** it on the admin side and starts the runner;
5. gets a registration code and has **two operators** register with the same code
   (`apiclient.Register`);
6. lists fleets and runners and runs `hello` as both operators over `api@mirafleet`;
7. checks that lifecycle, operator-revoke, token-create and fleet-create are all `forbidden`
   on `api@mirafleet`;
8. starts the 30-second `long` action, waits for the runner's own audit file to show it started,
   **revokes** the runner on central, and checks the action fails with `unavailable`, the audit
   shows `cancelled` with message `runner revoked` and exactly one `runner.revoke` row from
   source `cli`, the runner stays offline and its key no longer authenticates;
9. shows that **reusing the single-use join token** is refused;
10. moves the clock 61 seconds ahead and shows an **expired registration code** is refused and
    creates no operator.

### `TestM3Done` and `TestM3DoneIntegration`: the M3 acceptance test

M3's "done when" sentence in [M3.md](../M3.md) is one scenario, written **once** and run twice:

- `TestM3Done` (`internal/central/m3_done_test.go`) runs it with the fake executor
  (`internal/workspace/fake`). It runs on every `scripts/dev.sh test`, in under a few seconds,
  needing nothing special.
- `TestM3DoneIntegration` (`m3_done_integration_linux_test.go`, tag `linux && integration`) runs
  the **same** `runM3Done` function against two runners that each own a real `ctrd` executor, with
  the hermetic image of section 9.3. A small `m3Backend` struct holds the differences (the argv that
  means "print my uid", how to count containers), so the two cannot drift apart.

In order, the scenario:

1. **Set-up.** An injected clock; fleet `dev` in workspace mode with an image allowlist; operators
   alice and bob; two runners with `concurrent: 1` and *different trusted labels* (`zone=a`,
   `zone=b`; the runners also claim a lying label in their heartbeat, which must be ignored).
2. **Placement and sticky.** alice opens session `s1` with label `zone=a` and lands on the matching
   runner; opening `s1` again returns the same workspace with `reused: true` and creates **no** second
   container.
3. **Ownership.** bob's `exec`, `read_file`, `write_file` and `close` on alice's workspace all get
   `not_found`; his `workspace.list` is empty; alice's row is untouched.
4. **Inside the workspace.** `id -u` is not 0; writing under `/etc` fails (read-only rootfs; in the
   real run the message says "read-only"); a 1.5 MiB file round-trips in 512 KiB chunks with a
   matching SHA-256; a symlink to a host path cannot be read through `read_file`; a network dial
   fails under `network: none` (and the tool itself ran, so it is not "the tool could not start").
5. **Refusals.** An image outside the allowlist; env `ANTHROPIC_API_KEY`; a third open while every
   slot is taken (`unavailable`); `workspace.*` on an actions fleet and `action.run` on `dev`.
6. **Restarts.** Stop the runner (exec is `unavailable` meanwhile), start a new one over the same
   executor state: reconcile keeps the workspace and its files, exec works. Then close central and
   start another over the same store: the row persists, runners reconnect, the session is still sticky.
7. **Reaper.** Move the injected clock past the idle TTL: the untouched `s2` is destroyed (container
   and directory gone, row `closed`); `s1`, used just now, stays.
8. **Revoke.** Revoking alice closes her workspaces (the next reaper sweep destroys the container;
   the row is `closed`, with a `workspace.close_all` audit row); bob's workspace on the other runner
   is untouched. Then revoking runner `rb` marks bob's workspace there **`lost`** (not `closed`:
   central can no longer reach that host, so it cannot prove the container is gone; a
   `workspace.lost` audit row says so). The containers of a revoked runner stay on its host until
   someone runs `mirafleet runner workspace prune` there (chapter 10).
9. **Audit.** Every call has a `start` row and an outcome row, in exactly the counts the test
   computes, with the `workspace` column set.

The clock trick is the same as in `TestM2Done`: the service takes `Now`, and the test advances an
atomic offset instead of sleeping. The `after` hook of the fake variant also asserts what the
isolation depends on at the executor boundary: every create carried network `none`, the fleet's
limits and the allowed image, and no exec ever carried a denied variable.

## 9.7 Writing a new test, step by step

Goal: prove that the runner's guard refuses an over-long `name` for the `hello` action, all the
way through central. (A similar check exists at the `actions` level; this one proves the refusal
survives the trip.)

**Step 1: pick the lowest layer that can show the behaviour.** The pattern check lives in
`actions.Set.Plan`, so a pure unit test in `internal/actions/actions_test.go` is the cheapest.
Only go up to the `central` env if you need to prove the path: runner, SSH channel, registry,
service. We will do the higher one to learn the helpers.

**Step 2: name the file and test.** `internal/central/integration_test.go` is big already; add a
new file or put it next to related tests. A test name says what is proven, for example
`TestHelloRefusesOverlongName`.

**Step 3: build the environment.**

```go
func TestHelloRefusesOverlongName(t *testing.T) {
	e := newEnv(t, "r1")   // temp SQLite, fresh keys, central on 127.0.0.1:0
	e.startRunner("r1")    // real runner, stopped automatically at test end
	e.waitOnline("r1")     // poll until registered (gives up after 10 s)
```

**Step 4: act.** `testActions` defines `hello` with `name: pattern "[a-z]{1,8}"`. Twelve letters
are too long.

```go
	res, err := e.exec("r1", "hello", "name", "abcdefghijkl")
	if err != nil {
		t.Fatal(err)          // a transport problem, not what we are testing
	}
```

**Step 5: assert the outcome and the reason.** A guard refusal is a *result*, not an error:

```go
	if !res.Refused || !strings.Contains(res.Message, "does not match its pattern") {
		t.Fatalf("want a pattern refusal, got %+v", res)
	}
	if res.Stdout != "" {
		t.Fatal("refused action produced output")
	}
}
```

**Step 6: also test the path above it.** To check what the operator sees and what is audited,
call `e.run("r1", "hello", "name", "abcdefghijkl")` and assert the error code is `bad_request`
(use `wantCode`) and that `e.auditRowsFor(api.MethodActionRun)` has a `start` row and an
`error` row whose message starts `refused by runner`.

**Step 7: run just that test, uncached, with the race detector.**

```bash
scripts/dev.sh go test -race -count=1 -run TestHelloRefusesOverlongName ./internal/central/
```

**Step 8: break the code on purpose.** Temporarily weaken the pattern (or remove the
check in `Plan`) and make sure your test now **fails**. A test you have never seen fail may not
test anything.

**Step 9: run the full gate** (`fmt`, `vet`, `test`, and `cross` if you touched platform code)
before you ask for review.

Some habits worth copying from the existing tests:

- Use `t.Helper()` in helper functions so failures point at the caller.
- Use `t.Cleanup` for teardown, not `defer`, when a helper creates the resource.
- Table-driven tests (a slice of cases, `t.Run(name, ...)`) for many inputs.
- Put attacker-style inputs in a test's table: `"x; rm -rf /"`, `"--help"`, `"$(id)"`, a
  newline, an empty string, a value one byte over the limit.

## 9.8 The race detector and `-count`

`go test -race` instruments memory accesses and fails the test when two goroutines touch the same
variable without synchronisation. mirafleet is full of goroutines (one per connection, channel,
heartbeat), so the race detector is not optional: `scripts/dev.sh test` always uses it.
Races are probabilistic, so a clean single run proves little. To hunt one:

```bash
scripts/dev.sh go test -race -count=20 -run TestSomething ./internal/central/
```

`-count=N` runs the test N times in a row; `-count=1` also disables Go's test result cache
(useful when you changed something Go cannot see, such as a file the test reads). Note that
`dev.sh test` already benefits from the cache: if you re-run without changes, the packages print
`(cached)`.

If a test is timing-sensitive (the plan lists `TestBackoffGrowsWhenConnectionNeverHealthy` as
such), run it with `-count=5` under load before you blame your change. If you find a flaky
test, do not retry until green: find the race or make the timing robust, and log it in the
milestone plan (`docs/M2.md` section 4b has the format).

## 9.9 The licence gate

CLAUDE.md allows only these licences for everything we link (and anything bundled): Apache-2.0,
MIT, BSD-2-Clause, BSD-3-Clause, ISC, 0BSD. Never GPL, LGPL, AGPL, MPL, SSPL, BUSL or other
copyleft or source-available licences. (Font files may be SIL OFL-1.1 with their licence text.)

Why: mirafleet is Apache-2.0 and meant to be embedded by companies. One copyleft dependency
could force those companies to open their own code, or make them unable to use mirafleet at all.

`scripts/dev.sh licenses`:

1. checks that `modernc.org/mathutil` is exactly `v1.7.1`, otherwise prints
   `re-review modernc.org/mathutil licence` and fails;
2. builds `go-licenses` v1.6.0 inside the container (`/tmp/bin`, never on your host);
3. for each of the six `GOOS/GOARCH` targets runs
   `go-licenses check ./... --ignore modernc.org/mathutil
   --allowed_licenses=Apache-2.0,MIT,BSD-2-Clause,BSD-3-Clause,ISC,0BSD`.

It checks **linked** code only, i.e. what ends up in the binary, and it needs network access to
download the tool.

**The `mathutil` exception.** `go-licenses` recognises a licence by matching the module's top
LICENSE file against known texts. The `modernc.org/mathutil` file is word-for-word BSD-3-Clause
but says "The mathutil Authors" in the copyright line, which the classifier does not match. A
person read the file and confirmed it is BSD-3-Clause, so the script skips that one module. The
skip is tied to the exact version: bump the module and the gate fails until someone reviews the
new licence text again. A related note in the script: `modernc.org/libc` bundles musl-derived MIT
code that the tool cannot see; that was reviewed by hand (see `docs/M2.md` section 6.3).

When you add a dependency: **ask first** (CLAUDE.md), read its licence and its transitive modules'
licences, run `scripts/dev.sh licenses`, and note the licence in the PR. A CI job for this
is planned (M6); until then you run it by hand.

## 9.10 Common failures and fixes

| Symptom | Cause | Fix |
|---|---|---|
| `Docker runtime not reachable: start it with `colima start`` | the VM is stopped | `colima start` (or start your Docker service on Linux) |
| `source not visible in the VM: colima only mounts $HOME by default` | clone is outside `$HOME` | move it under `$HOME` or add a colima mount |
| `Cannot connect to the Docker daemon` on Linux | not in the `docker` group or daemon down | `sudo systemctl start docker`, add yourself to `docker`, log in again |
| `gofmt needed:` followed by a list | files not formatted | `scripts/dev.sh go fmt ./...` (rw) then re-run `fmt` |
| `go: updates to go.mod needed` | imports changed | `scripts/dev.sh go mod tidy`, commit `go.mod` and `go.sum` |
| `go.sum` mismatch / `missing go.sum entry` | new import not tidied | same as above |
| `licenses` fails: `re-review modernc.org/mathutil licence` | the pinned module version changed | review the new LICENSE text by hand, then update the pinned version in the script and `docs/M2.md` |
| `licenses` fails on a new module | forbidden or unrecognised licence | do not add it; ask the maintainers |
| Test fails only under `-race` with `DATA RACE` | real concurrency bug | read both stack traces in the report; fix the missing lock or channel |
| `listen unix ...: bind: invalid argument` or a path-too-long error | socket path over the OS limit | use a short temp dir like `centralTestDir` (`os.MkdirTemp("", "mf")`), not `t.TempDir()` |
| `timed out waiting for condition` | a runner never came online or the condition is wrong | check `e.logText()`; confirm the runner key name matches the `newEnv` name; confirm status is `active` |
| `sqlite: invalid database path ":memory:"` | `Open` refuses in-memory databases | use `filepath.Join(t.TempDir(), "mirafleet.db")` |
| `private key ... has mode 0644` in a test | a test wrote a key with `os.WriteFile` | use `keys.Generate`, or `0o600` |
| Files in the repo owned by root | someone ran the container as root | `dev.sh` runs as your user; if you used raw `docker run`, `sudo chown -R` the files and use `dev.sh` |
| `TestNoSQLiteOnNonLinux` fails | a non-`_linux` file imports `store/sqlite` | move that import into a `_linux.go` file |
| `cross` fails with `undefined: syscall.X` on windows | unix-only call outside a `unix`-tagged file | move it into a `_unix.go` file and add an `_other.go` stub that fails closed |
| `integration` fails at once with `cgroup v2 nesting did not settle after 50 tries` | the container runtime's cgroup tree cannot be nested | use a VM-backed runtime (colima, Docker Desktop) or a Linux VM with cgroup v2 |
| `integration`: `containerd did not start:` and the daemon log | `--privileged` not honoured, or a kernel feature is missing | read the log tail it prints; on colima make sure the VM is not rootless |
| `integration`: runner log says `userns: required` unsupported, test logs `using off` | the VM kernel is older than 5.19 or lacks overlayfs id-mapping | harmless for the test (it falls back to `off`); update the VM to test `required` |
| `integration`: build step fails fetching modules | no network to the Go module proxy for containerd's daemon modules | the first run needs network once; later runs use the `mirafleet-gomod` volume |
| `go test ./internal/workspace/ctrd` on a Mac says there are no tests | the containerd package is `linux`-only | run `scripts/dev.sh test` (Linux container) or `integration` |
| `TestNoContainerdOnNonLinux` fails | a non-Linux file imports `ctrd`, `containerd` or `grpc` | the only import of `ctrd` belongs in `cmd/mirafleet/runner_ws_linux.go` |
| A workspace test hangs until the timeout | a fake `Exec` ignored its context | an `Executor` must return when `ctx` is done; copy the fake's `Exec` |
| Slow first run, then fast | cold caches | normal; do not delete the volumes without a reason |
| `deps_test` skipped / `go tool not available` | running tests outside the container | run via `dev.sh`; in CI (`CI` set) a missing `go` is a hard failure |

## 9.11 Try it locally: central, runner and operator in one container

Tests are one way to see the system. Here is the other: run all three roles by hand, in one
throwaway container, and watch the real commands from [chapter 10](10-operations.md) work. Nothing
touches your host. Everything lives under `/tmp` inside the container and disappears when you
exit. Ports are fine here because the container is isolated; the "no real ports" rule is about
tests.

Open a shell in the container (read-write mount, runs as you):

```bash
scripts/dev.sh shell
```

All commands below run inside that shell. First build the binary and define a short helper
(`mf`). The admin commands (`central ...`) find central through its data directory, so they
need `-data /tmp/c` here; without it they would look in `/var/lib/mirafleet/central`.

```bash
cd /src
CGO_ENABLED=0 go build -o /tmp/mf ./cmd/mirafleet
mf() { /tmp/mf "$@"; }
```

**1. Create and start central** (SSH on loopback port 2222, the admin socket inside `/tmp/c`):

```bash
mf central init -data /tmp/c -listen 127.0.0.1:2222
mf central run -data /tmp/c -listen 127.0.0.1:2222 &
sleep 1
```

**2. Create a fleet and a join token.** The token goes to stdout and a note to stderr, so
`$(...)` captures only the token:

```bash
mf central fleet create dev -data /tmp/c
TOKEN=$(mf central token create -fleet dev -data /tmp/c)
```

**3. Write a minimal `actions.yaml`** (the runner's allowlist, explained in
[10.5](10-operations.md#105-writing-actionsyaml); YAML is a plain-text format for structured
data):

```bash
cat > /tmp/actions.yaml <<'EOF'
actions:
  hello:
    run: ["/bin/echo", "hello", "{{name}}"]
    params:
      name: { pattern: "[a-z]{1,8}" }
EOF
```

**4. Join the runner and start it.** The token is read from stdin (`-token -`), never from the
command line:

```bash
printf '%s\n' "$TOKEN" | mf runner join 127.0.0.1:2222 -name build-1 -data /tmp/r -token -
mf runner run -data /tmp/r -actions /tmp/actions.yaml &
sleep 1
```

**5. Approve it.** A newly joined runner is `pending`; central will not send it work yet:

```bash
mf central runner approve dev/build-1 -data /tmp/c
mf central runner list -data /tmp/c      # dev/build-1  active  true
```

**6. Register an operator, `alice`.** `operator code` prints three labelled lines
(`code:`, `valid for:`, `host key:`); `sed` keeps only the value after `code:`. The code is only
valid for its 60-second window, so use it straight away. A separate config directory plays the
part of alice's laptop:

```bash
CODE=$(mf central operator code -data /tmp/c | sed -n 's/^code: *//p')
export MIRAFLEET_CONFIG_DIR=/tmp/op1
printf '%s\n' "$CODE" | mf mcp login 127.0.0.1:2222 -name alice -code -
```

**7. Use it as the operator.** These are the calls an agent would make:

```bash
mf mcp api runners.list '{"fleet":"dev"}'
mf mcp api action.run '{"fleet":"dev","runner":"build-1","action":"hello","params":{"name":"world"}}'
```

The second call prints JSON with `"stdout": "hello world\n"` and a `request_id`. Look at both
audit rows it left on central (the `start` and the outcome share that `request_id`):

```bash
mf central audit -data /tmp/c
```

**8. Break it on purpose.** Send a value the guard refuses and watch it fail with `bad_request`
(`refused by runner: ... does not match its pattern`):

```bash
mf mcp api action.run '{"fleet":"dev","runner":"build-1","action":"hello","params":{"name":"x; id"}}'
```

Stop everything with `kill $(jobs -p)` and `exit`. The next `scripts/dev.sh shell` starts clean.

If a step fails, [10.6](10-operations.md#106-troubleshooting) lists the messages and fixes.

## 9.12 Try it locally: a workspace

**The walkthrough in 9.11 cannot run workspaces.** `scripts/dev.sh shell` is an ordinary,
unprivileged container: no containerd, no root, no user namespaces. A workspace runner needs all
three (it is root-equivalent by design, [chapter 5](05-security.md)). So there are two honest
ways to see workspaces working, and the first needs nothing but Docker.

### Way 1: watch the built binary do it, in the privileged harness

```bash
scripts/dev.sh integration
```

`TestIntegrationWorkspaceCLI` is this walkthrough, automated. It starts the real binary as central
and as `runner run -workspace` over a real containerd, then runs exactly the commands you would
type (the next section), with a hermetic image and no network. Read its log with `-test.v` output
(the script already passes `-test.v`): every step prints. Its source,
`cmd/mirafleet/e2e_integration_linux_test.go`, is the best worked example of the CLI.

### Way 2: by hand, on a throwaway Linux machine

You need a **disposable Linux VM** (not your laptop, not a shared server) with root, kernel 5.19
or newer, cgroup v2, `runc`, and containerd **2.x** running (the repository does not install it for
you; `scripts/dev.sh integration` builds the matching version if you want one to copy). Everything
below is the same sequence as 9.11 plus the workspace pieces. Run as root. Never do this on a
machine you care about: a workspace runner holds the containerd socket.

```bash
mf() { /usr/local/bin/mirafleet "$@"; }       # your build of the binary
mf central init -data /tmp/c -listen 127.0.0.1:2222
mf central run -data /tmp/c -listen 127.0.0.1:2222 &
sleep 1
```

**1. A workspace fleet.** The image allowlist is exact or a pattern; a digest pattern is best.
`-network none` is the default and the safe choice:

```bash
IMG=docker.io/library/busybox@sha256:<a digest you looked up and trust>
mf central fleet create dev -data /tmp/c -mode workspace -executor containerd \
  -image "$IMG" -images "docker.io/library/busybox@sha256:*" -pull if-not-present \
  -network none -cpus 1 -memory 512MiB -pids 128 -tmpfs 64MiB -disk 1GiB \
  -idle-ttl 1h -max-ttl 4h -exec-timeout 30s -exec-max 5m
mf central fleet list -data /tmp/c -json | head -30     # shows the full config (admin socket only)
```

**2. The runner's own policy**, `/etc/mirafleet/workspace.yaml` (the boundary on the runner's
side; every field is explained in [10.9.2](10-operations.md#1092-workspaceyaml-reference)):

```bash
mkdir -p /etc/mirafleet /var/lib/mirafleet/ws && chmod 0700 /var/lib/mirafleet/ws
cat > /etc/mirafleet/workspace.yaml <<'EOF'
executor: containerd
concurrent: 2
max_age: 24h
images: ["docker.io/library/busybox@sha256:*"]
limits: {cpus: 2, memory: 1GiB, pids: 256, tmpfs: 128MiB, disk: 2GiB, max_file: 64MiB, exec_max: 10m}
network: [none]
env_allow: [LANG, CI]
containerd: {namespace: mirafleet, root: /var/lib/mirafleet/ws}
EOF
```

**3. Join and start the runner** with `-workspace` instead of `-actions` (they are exclusive):

```bash
TOKEN=$(mf central token create -fleet dev -approve -data /tmp/c)
printf '%s\n' "$TOKEN" | mf runner join 127.0.0.1:2222 -name build-1 -data /tmp/r -token -
mf runner run -data /tmp/r -workspace /etc/mirafleet/workspace.yaml &
sleep 3
mf central runner list -data /tmp/c -fleet dev -json    # "online": true, "slots": 2, "executor": "containerd"
```
If the runner exits at start, its message names the failed rule (user namespaces unsupported,
`root` not 0700 and root-owned, a forbidden namespace, ...); see
[10.6](10-operations.md#106-troubleshooting).

**4. Register alice and use the workspace.**

```bash
CODE=$(mf central operator code -data /tmp/c | sed -n 's/^code: *//p')
export MIRAFLEET_CONFIG_DIR=/tmp/op1
printf '%s\n' "$CODE" | mf mcp login 127.0.0.1:2222 -name alice -code -

mf mcp api workspace.open '{"fleet":"dev","session":"s1"}'            # note the id
WS=<the id from the reply>
mf mcp api workspace.open '{"fleet":"dev","session":"s1"}'            # "reused": true
mf mcp api workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"id\"]}" # uid=1000, not 0
printf 'hello\n' > /tmp/hello.txt
mf mcp api workspace.write_file "{\"workspace\":\"$WS\",\"path\":\"hello.txt\",\"truncate\":true}" -in /tmp/hello.txt
mf mcp api workspace.read_file "{\"workspace\":\"$WS\",\"path\":\"hello.txt\"}" -out /tmp/back.txt
cmp /tmp/hello.txt /tmp/back.txt && echo round trip ok
```
`mcp api` fills in `length` and `sha256` for `-in`, sets `stdin: true` for an `exec` with `-in`, and
writes the body of a read to `-out` (0600, atomically).

**5. Break it on purpose.**

```bash
mf mcp api workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"touch\",\"/etc/x\"]}"       # exit_code 1, read-only file system
mf mcp api workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"env\"],\"env\":{\"ANTHROPIC_API_KEY\":\"x\"}}"  # bad_request
mf mcp api workspace.write_file "{\"workspace\":\"$WS\",\"path\":\"../etc/evil\"}" -in /tmp/hello.txt        # refused
```

**6. Look from the other side.** Dana lists and kills; the audit log shows start and outcome rows
with the workspace column; the runner host shows the container and its directory:

```bash
mf central workspace list -data /tmp/c
mf central audit -data /tmp/c -limit 20
mf runner workspace list -data /tmp/r -workspace /etc/mirafleet/workspace.yaml
mf central workspace kill -data /tmp/c "$WS"
```

Stop with `kill $(jobs -p)`. If anything is left over, `mf runner workspace prune -all -yes -data
/tmp/r -workspace /etc/mirafleet/workspace.yaml` removes this runner's containers and directories.

## Check your understanding

1. Why does `scripts/dev.sh test` run the test suite twice?
2. A teammate says "I'll just `brew install go` and run `go test` on my Mac". Give two reasons
   the project says no.
3. Why do tests call `sqlite.Open` on a file in `t.TempDir()` instead of using `:memory:`
   or a mock store?
4. In `TestM2Done` the expired-code check does not call `time.Sleep(61 * time.Second)`. How
   does it move time forward?
5. You want to check a `slow` action is killed after its timeout. Which `env` helper do you call,
   and is a refusal an `error` or a result?
6. `scripts/dev.sh integration` uses `--privileged`. Why is that acceptable here but not for
   `scripts/dev.sh test`, and what must be true of your Docker runtime?
7. `TestM3Done` and `TestM3DoneIntegration` never drift apart. What is the mechanism, and what does
   each variant prove that the other cannot?
8. Why does the integration harness build containerd from the version in `go.mod` instead of using
   the daemon bundled in the `dind` image?

<details>
<summary>Answers</summary>

1. The first run uses `-race`, which needs cgo (a C compiler in the build). The second run uses
   `CGO_ENABLED=0`, which is how the shipped static binary is built, so we test what we ship.
2. Any two of: a different Go version than the pinned one gives different results; the host
   gets cluttered (the project forbids host `go`, `brew`, `npm`, `pip` for this repo); macOS is
   not Linux, so Linux-only code and tests (`/bin/echo`, socket peer credentials) would not run;
   it breaks the "same bytes everywhere" guarantee of the digest-pinned image.
3. `Open` refuses `:memory:` because every pooled connection would get its own empty database.
   A real file exercises the actual SQLite behaviour (transactions, triggers, unique indexes,
   migrations). A mock would only prove our assumptions about SQLite.
4. The service takes a clock function in `service.Config.Now`. The test passes a closure that
   returns `base + offset` and increases `offset` by `tokens.StepSeconds + 1`.
5. `e.exec("r1", "slow")` (or `e.run`). A guard refusal is a *result* with `Refused: true`; a
   timeout is a result too, with `TimedOut: true` and `ExitCode: -1`. An `error` means a
   transport problem (runner offline, channel failed).
6. Only the integration tests need real containers, so only they get extra privilege, and only in a
   throwaway `--rm` container whose state lives on `tmpfs`; the normal gate stays unprivileged. The
   runtime must be VM-backed (colima, Docker Desktop) so "privileged" means privileged *inside the
   VM*, not on your host. On a bare Linux host use a throwaway VM.
7. Both call the single function `runM3Done` with an `m3Backend` (the executor factory, the argv for
   "print my uid" and so on). The fake variant proves the *logic* (placement, ownership, reaper,
   audit counts) fast and everywhere, but cannot prove isolation. The containerd variant proves the
   isolation claims by behaviour (non-root, read-only rootfs, no host symlink, no network), but is
   slow and needs the privileged harness.
8. The `dind` image's daemon is containerd 1.7; mirafleet's client and the user-namespace and
   id-mapped snapshot features it relies on are 2.x. Building the daemon from the same module
   version as the client makes the test exercise exactly what the code was written against, and a
   `go.mod` bump automatically bumps the test daemon.

</details>
