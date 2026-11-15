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
- The M5 additions: `TestM5Done` (the MCP layer against a real central), `TestM5NoHooks` (the built
  `mirafleet mcp` as a child process, with no harness), the hook and plugin tests, the CI workflow
  that tests the orchestrator side on Windows and macOS ([9.13](#913-the-orchestrator-workflow-windows-and-macos)), and how to try the plugin
  with `claude --plugin-dir integrations/claude-code` ([9.14](#914-try-it-locally-the-mcp-server-and-the-claude-code-plugin)).
- The M4 additions: `TestM4Done` (the admin GUI against a real central, over real TLS), the GUI
  security suite, the built-binary smoke test `TestGUIBinary`, `scripts/dev.sh gui-shots`, and how
  to look at the GUI yourself ([9.15](#915-try-it-locally-the-admin-gui)).
- The M6 additions: `scripts/release.sh` and `m6check`, what `pkgtest`, `installtest`, `image`,
  `lab`, `npmtest`, `harnesscheck` and `e2e-systemd` each prove, the CI workflows and their safety
  rules, and the worktree rules ([9.16](#916-testing-distribution-m6)).

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
| `scripts/dev.sh gui-shots [DIR]` | rw (+ `--network none`) | builds the GUI package's test binary, then runs it in a pinned headless Chromium image that photographs every screen, light and dark, at 1280 and 390 px wide, into `dist/gui-shots` (gitignored) or `DIR`; not part of the gate | 2 to 5 min (first run: pulls the Chromium image) |
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
| `TestM5Done` (`internal/central/m5_done_test.go`) | The whole M5 scenario: MCP servers over pipes against a real in-process central (below). |
| `TestM5NoHooks` (`cmd/mirafleet/mcp_stdio_linux_test.go`) | The **built** `mirafleet mcp` as a child process with no hooks and no harness variables (below). |
| `TestOrchestratorDeps` (`cmd/mirafleet/deps_test.go`) | `orch`, `mcpserver` and the hook link no server-side package, on six targets; only `mcpserver` imports the go-sdk ([chapter 8](08-code-tour.md#87-build-tags-and-why)). |
| `TestDecodeArgsExactKeys`, `TestEveryToolRejectsMiscasedAndDuplicateKeys` (`internal/mcpserver`) | A key like `Confirm` or a repeated `confirm` is refused by every tool before the backend is touched. |
| `TestRawInitializeVersions`, `TestRawUnknownMethodAndMalformed` (`raw_test.go`) | A hand-written JSON-RPC client gets the versions and error codes the guide documents. |
| `TestGuardRefusesNonOperatorMethods` (`internal/orch/calls_test.go`) | `orch.call` refuses every admin and enrol method (it panics in tests, so a slip cannot hide). |
| `TestNoEnvAccessInSource`, `TestDecoyEnvNeverForwarded` (`internal/orch`) | `orch` never reads the process environment; decoy secrets in the server's environment never reach a request. |
| `TestNoRetryForNonIdempotent`, `TestRetryIdempotentRedials` | After a dead connection `exec`, `write_file` and `action.run` are not repeated; lists and reads are. |
| `TestSanitize`, `FuzzSanitize`, `FuzzCapOutput`, `TestCapOutputWorstCaseEncoding` (`internal/orch/output_test.go`) | ANSI and invisible characters are removed, and the capped result still fits after JSON escaping. |
| `TestServeMalformedFrameExitsNonZero`, `TestServeSignalCancel` (`cmd/mirafleet/mcp_serve_test.go`) | A garbage line ends the server with an error (so exit 1) and is never echoed to stdout; a cancelled context (what a signal does) stops it cleanly. |

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
   durga and kalyan; two runners with `concurrent: 1` and *different trusted labels* (`zone=a`,
   `zone=b`; the runners also claim a lying label in their heartbeat, which must be ignored).
2. **Placement and sticky.** durga opens session `s1` with label `zone=a` and lands on the matching
   runner; opening `s1` again returns the same workspace with `reused: true` and creates **no** second
   container.
3. **Ownership.** kalyan's `exec`, `read_file`, `write_file` and `close` on durga's workspace all get
   `not_found`; kalyan's `workspace.list` is empty; durga's row is untouched.
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
8. **Revoke.** Revoking durga closes durga's workspaces (the next reaper sweep destroys the container;
   the row is `closed`, with a `workspace.close_all` audit row); kalyan's workspace on the other runner
   is untouched. Then revoking runner `rb` marks kalyan's workspace there **`lost`** (not `closed`:
   central can no longer reach that host, so it cannot prove the container is gone; a
   `workspace.lost` audit row says so). The containers of a revoked runner stay on its host until
   someone runs `mirafleet runner workspace prune` there (chapter 10).
9. **Audit.** Every call has a `start` row and an outcome row, in exactly the counts the test
   computes, with the `workspace` column set.

The clock trick is the same as in `TestM2Done`: the service takes `Now`, and the test advances an
atomic offset instead of sleeping. The `after` hook of the fake variant also asserts what the
isolation depends on at the executor boundary: every create carried network `none`, the fleet's
limits and the allowed image, and no exec ever carried a denied variable.

### `TestM5Done`, `TestM5NoHooks` and the plugin tests: the M5 acceptance tests

M5 added the part of mirafleet that a model talks to, so its tests check three different things:
that the **tools** behave against a real central, that the **built binary** works with nothing
around it, and that the **plugin files** are what Claude Code expects.

**`TestM5Done`** (`internal/central/m5_done_test.go`) is the M5 "done when". It starts a real
central in process (SQLite, registry, service, an SSH listener on an ephemeral port) and real
runners, with a **fake executor** for the workspace side, so it needs no containerd and runs in
every `scripts/dev.sh test`. The MCP servers are real: `mcpserver.Serve` over `io.Pipe`, with an
`orch.Orch` each, driven by the go-sdk's own *client*. (That is why the SDK is allowed in test
files, [chapter 8](08-code-tour.md#87-build-tags-and-why).) Time is an injected clock, as in
`TestM2Done`, so the idle reaper is tested without sleeping. The set-up is: fleet `dev` (workspace
mode, runners `build-1` and `build-2`) and a guarded actions fleet `prod` (runner `db-1` with
`tail-logs` and a `noisy` action that prints 200 KiB with colour codes); two operators with their
own config directories (durga and kalyan); a project directory whose `.mirafleet.yaml` says
`fleet: dev`; a second live context and a dead one; and *only decoy secrets*
(`ANTHROPIC_API_KEY`, `AWS_SECRET_ACCESS_KEY`) in the servers' environment. Then, as subtests:

1. `02_tools`: `tools/list` returns exactly the nine tools with schemas and annotations;
   `fleet_create` is not a tool; version negotiation is what the SDK answers; a bad argument is a
   `bad_request`.
2. `03_lists`: `list_fleets` shows `dev` as default and `prod` as guarded from both live contexts
   plus one error entry for the dead one; `list_runners prod` shows `db-1` with `tail-logs` and
   its pattern; `list_runners dev` shows `free_slots`.
3. `04_guarded_action`: `run_action` on `prod` without `confirm` is an `isError` with the "ask
   the user" text and a `denied` audit row; with `confirm: "prod"` it returns output; `noisy`
   comes back at most 30 KiB, `truncated`, with the marker and no escape character.
4. `05_implicit_open_exec`: `exec "echo hi"` opens a workspace on `dev` by itself under a random
   `p:...` session; the fake runner receives `["/bin/sh","-c","echo hi"]`; no decoy secret appears
   in any create spec or exec request; `env: {ANTHROPIC_API_KEY: ...}` is refused.
5. `06_files`: a 1.5 MiB `write_file` becomes two chunks; paged `read_file` calls reassemble to the
   same SHA-256; a default `read_file` is capped.
6. `07_ownership`: kalyan using durga's workspace id with `exec`, `read_file` and `workspace_close`
   gets `not_found` (the same answer as for an id that does not exist), and central writes
   exactly one `denied` audit row for each refusal.
7. `08_mode_mismatch`: `workspace_open` on the actions fleet and `run_action` on the workspace
   fleet are `conflict`.
8. `09_cancel`: `notifications/cancelled` for a long `exec` cancels the exec on the runner, and
   the audit outcome is `cancelled`.
9. `10_idle_on_exit`: closing durga's stdin makes `Serve` return within 5 s and leaves durga's row
   `open`; a new server gets the other runner; a third gets `unavailable` (both slots used);
   advancing the injected clock past the idle TTL makes the reaper close the first row.
10. `11_reattach`: two servers started with the same `-session s1`: the second `workspace_open`
    says `reused: true` (no hook involved).
11. `12_hygiene`: every call that reached central has a start row and an outcome row; no admin
    method appears in durga's audit; no result contains the config directory, a key fingerprint or
    a decoy value.

Because the servers speak the real protocol and use the real `orch`, a bug in any layer between the
model and the runner shows up here. What it does *not* cover is a real harness, which is why the
manual smoke test (below) exists.

**`TestM5NoHooks`** (`cmd/mirafleet/mcp_stdio_linux_test.go`, Linux only) proves the claim "the core
works with no hooks". It re-runs the test binary as a child process that executes `main`'s `mcp`
branch, so it is the real entry code: real flag parsing, real signal handling, real stdout. The
child's environment is only `PATH`, `HOME`, the config-dir variable and decoys: no `CLAUDE_*`, no
handoff directory, no hook. The test speaks hand-written newline-delimited JSON-RPC to it and
checks: version negotiation; `tools/list`; `list_fleets`, `workspace_open`, `exec`, `read_file`
and `workspace_close` against an in-process central; **every stdout line parses as JSON-RPC**;
closing stdin and `SIGTERM` each end the child with exit code 0 within 5 s; and a workspace left
open is still `open` afterwards (the server never closes workspaces on exit).

**The hook tests** (`integrations/claude-code/hook/hook_test.go`, `cmd/mirafleet/mcp_hook_test.go`)
use real Claude Code hook JSON as fixtures and a fake backend, so no network and no Claude Code are
needed:

- `session-start` writes `cc:<session_id>` into the handoff, prints nothing, and ignores an invalid
  id (`TestSessionStartWritesHandoff`, `TestSessionStartInvalidIgnored`);
- `session-end` closes workspaces only for the reasons `clear` and `logout`, only the ones whose
  session matches, within a time bound, and never removes a *newer* session's handoff
  (`TestSessionEndClearClosesOnlyMatching`, `TestSessionEndOtherReasonsKeepWorkspaces`,
  `TestSessionEndBounded`, `TestSessionEndKeepsNewerSessionHandoff`);
- `pre-tool-use` says `ask` when `confirm` is set and prints nothing otherwise; hostile or huge
  fields in the reason are sanitised and clipped; malformed input, a non-string `confirm`, an
  unknown tool carrying `confirm`, or ambiguous keys (`Confirm`) all give a generic `ask`
  (`TestPreToolUse...`);
- `TestKnownToolsMatchMCPServer` keeps the hook's hard-coded tool names equal to the server's;
- `TestMCPHookEndToEnd` runs the lifecycle through `mcpHook` with an injected parent PID: the
  handoff is written, `orch.SessionFor` reads `cc:abc-123` from it, `pre-tool-use` asks only with
  `confirm`, `session-end` with `clear` closes the fake backend's workspace and removes the
  handoff. It does *not* cover real stdio or the real parent-process topology (see "Try it" below).

**The integration tests** (`integrations/integrations_test.go`, `harnesses_test.go`) check the
files as data, because a typo in JSON would fail only inside someone else's Claude Code: the plugin
manifest keys and semver; `.mcp.json` runs the bare `mirafleet` with `-project-dir`; every hook is
**exec form** (a `command` and an `args` array, no shell string, so it works on Windows too) and
SessionEnd has `timeout: 15`; the PreToolUse **matcher** matches all nine tool names under
`mcp__plugin_mirafleet_mirafleet__` and no other tool; the skill body is byte-for-byte
`integrations/commands/mirafleet.md`, and the aliases are full copies with
`disable-model-invocation: true`; the opencode command bodies match the shared one, its example
JSON parses; and every relative link in the integration READMEs resolves.
`TestPluginVersionLockstep` (in `cmd/mirafleet`) keeps `plugin.json` equal to `pluginVersion` in
`mcp_hook.go`.

### `TestM4Done`, the security suite and `TestGUIBinary`: the M4 acceptance tests

M4 added a web page that can change a fleet, so its tests lean hard on one question: *what can a
person (or a web page) who is not the admin make it do?* There are three layers.

**`TestM4Done`** (`internal/central/m4_done_test.go`, helpers in `m4_helpers_test.go`) is the M4
"done when". It starts a real central in process (the `newEnv` machinery of [9.5](#95-tour-of-the-test-helpers):
SQLite in a temp dir, registry, service, SSH on an ephemeral port, a fake workspace executor) and
puts the real `gui.Server` in front of it: `httptest` with `StartTLS` and the GUI's own
self-signed certificate. The client is a browser stand-in: a cookie jar, TLS that **trusts only
that one certificate**, and `Origin` and `Sec-Fetch-Site` headers like a real browser. One
offset clock is injected into both the service and the GUI (session expiry lives in the service),
so "31 minutes later" costs no waiting. The steps are subtests, run in order because each builds on
the last:

1. `01_bootstrap`: every page redirects to `/setup`; the token file is mode 0600; a wrong token is
   refused; the right one creates the admin, deletes the file and logs in; `/setup` is then 404.
2. `02_cookies_headers`: the cookie is `__Host-mf_session`, `Secure`, `HttpOnly`,
   `SameSite=Strict`; the token changes at login; the CSP is exactly the documented one; a foreign
   `Host` gets 421.
3. `03_csrf`: a POST without `_csrf`, with a cross-site `Sec-Fetch-Site`, or with another session's
   token is 403, changes nothing and writes no audit row.
4. `04_login_limits`: ten wrong passwords from one address give 429; moving the clock 15 minutes
   unlocks; an unknown user and a wrong password look the same.
5. `05_fleets`: create `prod` (actions, guarded) and `dev` (workspace); an invalid name is 422 and
   creates nothing; the guard cannot be turned off without typing the fleet name.
6. `06_join_token_runner`: a join token is shown once (`no-store`) and never again; a runner joins
   with it over the real enrol path, shows as `pending`, is approved from the GUI, and its detail
   page lists its actions.
7. `07_revoke_cascade`: with a long `action.run` in flight on `db-1`, a revoke with the wrong typed
   name is refused; with the right one the action ends `cancelled` and the reconnect is refused:
   the same outcome as `TestM2Done`, now started from a browser.
8. `08_operators`: Show code writes an `operator.code` row without the code in it; durga registers
   with it; Rotate secret kills the old code; Revoke drops durga's connection.
9. `09_workspaces`: kalyan's workspace is listed and Kill makes the fake executor see the destroy.
10. `10_audit`: filters by source, fleet and action prefix; paging both ways; the CSV export has a
    header, neutralises a `=cmd` cell and is preceded by an `audit.export` row; JSONL parses.
11. `11_sessions`: a polled (passive) fragment does not extend the idle timer; 31 minutes later the
    next request redirects to `/login` (an htmx one gets `HX-Redirect`); 12 hours kills even an
    active session; logout makes the old cookie useless.
12. `12_restart_credential_binding`: the `gui.Server` and the service are thrown away and rebuilt
    over the same SQLite file, and both sessions still work (so no session state lived in
    memory). A password change keeps the changing session and kills the other on its very next
    request; `ResetGUIAdmin` kills everything at once.
13. `13_hygiene`: no response after the creation response holds the join token; none ever holds the
    registration secret, a password hash, the setup token or key material; `gui_sessions` holds
    only 32-byte hashes, never a cookie; every GUI change has its audit row with `source=gui` and
    none was written as `source=api`.

**The security suite** (`internal/gui/security_test.go`, in the normal gate) is the one that makes a
new screen safe by default. It finds the routes **by scanning the package's own source** for
route registrations, so nobody has to remember to add a route to a list. Over every route it
checks that anonymous requests are redirected to `/login` (`TestSecurityAuthRequiredOnEveryRoute`),
that every POST is refused without a valid CSRF token (`TestSecurityCSRFOnEveryPost`,
`TestSecurityCSRFOnSetup`), that all the security headers and `Cache-Control` are on every
response (`TestSecurityHeadersAndCacheControlOnEveryResponse`), that the polled fragments are
passive (`TestSecurityPolledFragmentsArePassive`), and that before setup everything leads to
`/setup` (`TestSecurityBeforeSetupEverythingGoesToSetup`). Two tests look at the database:
`TestSecurityDBCopyYieldsNoCookie` (a copy of the file gives an attacker no cookie) and
`TestSecurityTamperedSessionRowRefusedAndDeleted`. The **template lint** tests
(`TestTemplateLintSources`, `TestTemplateLintRenderedPages`, `TestTemplateHTMXConfigMetaExact`,
`TestTemplateNoTrustedTypesOutsideHelper`, with `TestTemplateLintSelfTest` as the positive
control) read the HTML and fail on an inline script, a `style=` attribute, an `on...=` handler, an
`hx-on` attribute or a `template.HTML` outside the one vetted helper, because each of those is a
hole in the Content-Security-Policy ([chapter 5](05-security.md)).

**Other GUI tests worth knowing.** `limiter_test.go` (the login rate limit, lockout and the
hash semaphore); `server_test.go` (`TestHostAllowlist`, `TestStartupNotice`, `TestSelfSignedSANs`,
`TestOperatorCertKeyPermsAndHSTS`: the bind, certificate and Host rules); `session_test.go` (cookie
attributes, the purge ticker); `nojs_test.go` (`TestS16NoJSFlows`: every flow also works without
JavaScript); `a11y_test.go` (`TestS16ContrastAA` computes the colour contrast of every theme
pair); `internal/store/storetest` (`GUISetup`, `GUISetupRace`, `GUIPassword`, `GUISessions`,
`GUIPurge`, `QueryAudit`, `SetFleetConfirm`, run against every store); `internal/service/gui_test.go`;
and `TestGUIDeps` and `TestGUILinkedOnlyOnLinux` in `cmd/mirafleet/deps_test.go`
([chapter 8](08-code-tour.md#87-build-tags-and-why), point 5).

**`TestGUIBinary`** (`cmd/mirafleet/gui_linux_test.go`, Linux only) is the "does the real program
work" test, as `TestM5NoHooks` is for MCP. It re-runs the test binary as a child process that
executes `main`, so flags, signals and log lines are real. It runs `central init` and `central
run` with the default-shaped bind (`-gui-listen 0.0.0.0:0`, self-signed) in a temp directory and
checks: the log has the GUI URL, the certificate fingerprint and the all-interfaces notice, and
**not** the setup token; the client pins that fingerprint (a wrong pin fails the handshake);
before setup every page redirects to `/setup`; `Host: rebind.evil.example` gets 421 while
`127.0.0.1` and the machine's own host name work; setup from the token file, then login; a fleet
made in the GUI shows in `central fleet list` and one made by the CLI shows in the GUI; after a
SIGTERM and a restart on the same data directory the old cookie still works and the fingerprint
is unchanged; `central gui reset-admin` prints a token and the session is dead at once;
`-gui-listen off` opens no GUI port; and SIGTERM exits 0 within 5 s.

**`scripts/dev.sh gui-shots`** is not a test but belongs here: it is how a reviewer sees what the
GUI looks like without running a central. `TestGUIShotsServe` (skipped unless
`MIRAFLEET_GUI_SHOTS_ADDR` is set, so it never runs in the gate) serves a seeded GUI on loopback
inside a container, and a pinned headless Chromium image photographs each screen. The Chromium
image is a dev tool: it runs with `--network none`, is never shipped, and so is outside the
licence rule for bundled code.

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
MIT, MIT-0 (MIT with no attribution clause; added in M5 for one transitive module), BSD-2-Clause,
BSD-3-Clause, ISC, 0BSD. Never GPL, LGPL, AGPL, MPL, SSPL, BUSL or other
copyleft or source-available licences. (Font files may be SIL OFL-1.1 with their licence text.)

Why: mirafleet is Apache-2.0 and meant to be embedded by companies. One copyleft dependency
could force those companies to open their own code, or make them unable to use mirafleet at all.

`scripts/dev.sh licenses`:

1. checks that `modernc.org/mathutil` is exactly `v1.7.1` and `github.com/segmentio/asm` is
   exactly `v1.2.1`, otherwise prints `re-review ... licence` and fails;
2. builds `go-licenses` v1.6.0 inside the container (`/tmp/bin`, never on your host);
3. for each of the six `GOOS/GOARCH` targets runs
   `go-licenses check ./... --ignore modernc.org/mathutil --ignore github.com/segmentio/asm
   --allowed_licenses=Apache-2.0,MIT,MIT-0,BSD-2-Clause,BSD-3-Clause,ISC,0BSD`.

It checks **linked** code only, i.e. what ends up in the binary, and it needs network access to
download the tool.

**The `mathutil` exception.** `go-licenses` recognises a licence by matching the module's top
LICENSE file against known texts. The `modernc.org/mathutil` file is word-for-word BSD-3-Clause
but says "The mathutil Authors" in the copyright line, which the classifier does not match. A
person read the file and confirmed it is BSD-3-Clause, so the script skips that one module. The
skip is tied to the exact version: bump the module and the gate fails until someone reviews the
new licence text again. A related note in the script: `modernc.org/libc` bundles musl-derived MIT
code that the tool cannot see; that was reviewed by hand (see `docs/M2.md` section 6.3).

**The `segmentio/asm` exception (M5).** The MCP go-sdk pulls in `github.com/segmentio/asm` (through
`segmentio/encoding`). Its LICENSE is verbatim MIT-0, which is on our allow-list, but go-licenses
v1.6.0 has no classifier for MIT-0 and reports it as unknown. As for `mathutil`, a person read the
file, and the skip is pinned to the reviewed version. The go-sdk itself needs no exception: its
combined Apache-2.0 and MIT file is classified correctly. The full list of what M5 linked is in
`docs/M5.md` section 10.

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

**6. Register an operator, `durga`.** `operator code` prints three labelled lines
(`code:`, `valid for:`, `host key:`); `sed` keeps only the value after `code:`. The code is only
valid for its 60-second window, so use it straight away. A separate config directory plays the
part of durga's laptop:

```bash
CODE=$(mf central operator code -data /tmp/c | sed -n 's/^code: *//p')
export MIRAFLEET_CONFIG_DIR=/tmp/op1
printf '%s\n' "$CODE" | mf mcp login 127.0.0.1:2222 -name durga -code -
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

**4. Register durga and use the workspace.**

```bash
CODE=$(mf central operator code -data /tmp/c | sed -n 's/^code: *//p')
export MIRAFLEET_CONFIG_DIR=/tmp/op1
printf '%s\n' "$CODE" | mf mcp login 127.0.0.1:2222 -name durga -code -

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

**6. Look from the other side.** aarvee lists and kills; the audit log shows start and outcome rows
with the workspace column; the runner host shows the container and its directory:

```bash
mf central workspace list -data /tmp/c
mf central audit -data /tmp/c -limit 20
mf runner workspace list -data /tmp/r -workspace /etc/mirafleet/workspace.yaml
mf central workspace kill -data /tmp/c "$WS"
```

Stop with `kill $(jobs -p)`. If anything is left over, `mf runner workspace prune -all -yes -data
/tmp/r -workspace /etc/mirafleet/workspace.yaml` removes this runner's containers and directories.

## 9.13 The orchestrator workflow: Windows and macOS

`scripts/dev.sh` runs everything in a **Linux** container. That is right for central and runner,
which are Linux-only, but the operator side (`mirafleet mcp`, the harness integrations) also runs
on macOS and Windows laptops. `scripts/dev.sh cross` proves those builds *compile and vet*; it
cannot run a single Windows test, and the Windows-only code (the owner-only DACL in
`internal/keys/acl_windows.go`, the `%LocalAppData%` directories) would otherwise never execute
before a user ran it.

So M5 added one GitHub Actions workflow, `.github/workflows/orchestrator.yml`:

- **When:** every push to `main` and every pull request against it.
- **Where:** a matrix of two GitHub-hosted runners, `windows-latest` and `macos-latest`. (This is
  CI, not your machine: the "install nothing on the host" rule is about developer laptops.)
- **What:** for the orchestrator-side packages listed in the `PKGS` variable at the top of the file
  (today `internal/keys`, `opconf`, `handoff`, `orch` and `mcpserver`; the file's comment says to
  append the hook and `integrations` packages as they are added): `go vet`; a build of
  `./cmd/mirafleet` with `CGO_ENABLED=0` (the shipped configuration); `go test` with
  `CGO_ENABLED=0` (as shipped); and `go test -race` with `CGO_ENABLED=1` (the race detector needs
  cgo).
- **Safety:** the workflow has `permissions: contents: read` only, the third-party actions are pinned
  to a commit hash, `persist-credentials` is off, and a newer run for the same branch cancels the
  older one.

What this means when you change code in those packages:

- A test must pass on Windows. Do not assume Unix permission bits (on Windows `os.Stat` reports
  made-up mode bits; the DACL is what protects a file), a `/tmp` path, or that a symlink can be
  created. Put Unix-only tests behind `//go:build unix` and give security checks
  an explicit Windows branch that **fails closed**, never a silent skip. (Wave C of M5 had a P1 of
  exactly this kind: handoff tests that assumed Unix modes.)
- Use `t.TempDir()` and `filepath.Join`, never string-built paths.
- A failing Windows job is real even if `dev.sh test` is green. Reproduce it by reading the log; you
  cannot run it locally (no Windows in the container), which is why the tests are written to be
  small and deterministic.

The workflow does not replace `dev.sh`: central, runner, containerd and the SQLite store are still
tested only in the Linux container.

## 9.14 Try it locally: the MCP server and the Claude Code plugin

Two ways, from cheapest to most complete. Both need a `mirafleet` binary for **your laptop's** OS,
built with the container toolchain (chapter 10, "Step 0"): for an Apple-silicon Mac,
`CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o dist/mirafleet ./cmd/mirafleet` inside
`scripts/dev.sh shell`. Put `dist/` on your `PATH` for the session.

**Way 1: speak MCP by hand.** Without a Claude Code you can still see the server answer. The server
needs no central to list its tools (with no contexts it starts and reports "not set up" only when a
tool needs central). Keep stdin open a moment so the answers can come back; closing stdin is
what stops the server:

```bash
{ printf '%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"hand","version":"0"}}}' \
  '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/list"}' \
  '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"list_fleets","arguments":{}}}'
  sleep 2; } | mirafleet mcp
```

You should get three JSON lines: the `initialize` result (protocol version, `capabilities.tools`,
the instructions text), the nine tools with their schemas, and a `list_fleets` result with
`"isError":true` and the text `not set up: run `mirafleet mcp login ...``. Run `mirafleet mcp
login central.example.com:2222 -name work -code -` first (against a central from section 9.11) and
the third line becomes a list of fleets. Try a bad argument (`"arguments":{"Context":"x"}`) to see
the exact-case rule, and a broken line (`{not json`) to see the server stop with exit code 1.

You can also run a hook by hand. A guarded call makes `pre-tool-use` print an `ask`:

```bash
echo '{"hook_event_name":"PreToolUse","tool_name":"mcp__plugin_mirafleet_mirafleet__exec","tool_input":{"command":"ls","fleet":"prod","confirm":"prod"}}' \
  | mirafleet mcp hook claude-code pre-tool-use
```

Without `confirm` it prints nothing. `session-start`, given
`{"session_id":"abc","cwd":"/path/to/project"}` on stdin, prints nothing either and writes a handoff
file under the state directory (chapter 10).

**Way 2: the plugin in a real Claude Code.** From the repository root, in a project directory of
your choice that has (or does not have) a `.mirafleet.yaml`:

```bash
claude --plugin-dir integrations/claude-code
```

`--plugin-dir` loads a plugin straight from a folder, with no install and no marketplace (those come
in M6). Then:

1. `/mcp` lists the server as `plugin:mirafleet:mirafleet`. If it is failed, `mirafleet` is not on
   the `PATH` Claude Code was started with (chapter 10, troubleshooting).
2. `/mirafleet list my fleets` runs the command from `integrations/commands/mirafleet.md`. If
   another plugin owns the bare name, use `/mirafleet:mirafleet`.
3. Ask it to run something on an unguarded fleet; check the result's `where` shows the context and
   fleet you expected.
4. Ask it to do something on a guarded fleet. You should be asked twice: Claude Code's own tool
   prompt, and the hook's `ask` with the reason `Guarded fleet "prod": ...`.
5. `claude --debug` shows the hook commands and their output. `claude plugin validate
   integrations/claude-code --strict` checks the manifest.

Some behaviours depend on Claude Code itself and are checked by hand rather than by a test
(M5 step S17): that bare `/mirafleet` resolves, that a hook `ask` still prompts in the
auto-approve permission modes, that `${CLAUDE_PROJECT_DIR}` is filled in the plugin's `.mcp.json`,
and that the hook and the MCP server share a parent process (so that `--resume` reattaches to the
old workspace). If one of them is false for your version, the core still works; you lose the
convenience, and the docs say which ([chapter 11](11-contributing.md#117-adding-a-harness-integration)).

## 9.15 Try it locally: the admin GUI

Three ways, from cheapest to most real.

**1. Look at screenshots (no central, no browser).** `scripts/dev.sh gui-shots` photographs every
screen, light and dark, wide and narrow, into `dist/gui-shots/`:

```bash
scripts/dev.sh gui-shots
ls dist/gui-shots | head
```

**2. Drive it from inside the container.** `scripts/dev.sh shell` publishes no ports, so nothing
outside can reach a server in it, but you can talk to it from the same shell. Build and start
central as in [9.11](#911-try-it-locally-central-runner-and-operator-in-one-container), then:

```bash
mf central run -data /tmp/c -listen 127.0.0.1:2222 &     # the GUI starts too, on 0.0.0.0:8443
cat /tmp/c/gui/setup-token                              # the one-time setup token
curl -sk -o /dev/null -w '%{http_code} %{redirect_url}\n' https://localhost:8443/    # 303 to /setup
```

`-k` skips the certificate check, which is fine for a throwaway container and wrong anywhere else
(see [chapter 10](10-operations.md#1011-the-admin-gui-m4) for the right way).

**3. Use a real browser on your Mac.** You need a port forward, so start the container yourself
with `-p` instead of through `dev.sh`. It is the same image and mounts that `dev.sh` uses, plus
one published port bound to your loopback address only:

```bash
IMG=$(sed -n 's/^GO_IMAGE="\${MIRAFLEET_GO_IMAGE:-\(.*\)}"$/\1/p' scripts/dev.sh)
docker run --rm -it --user "$(id -u):$(id -g)" -p 127.0.0.1:8443:8443 \
  -v "$PWD":/src:ro -v mirafleet-gomod:/go/pkg/mod -v mirafleet-gocache:/gocache -w /src \
  -e HOME=/tmp -e GOCACHE=/gocache -e GOPATH=/gocache/gopath -e GOMODCACHE=/go/pkg/mod \
  -e GOFLAGS="-buildvcs=false -modcacherw" "$IMG" bash
```

Inside, build, `central init` and `central run` as in 9.11. Read the log: it prints the GUI URLs,
the certificate's SHA-256 fingerprint and the path of the setup token. Then on the Mac open
`https://localhost:8443/`. The browser warns about the self-signed certificate; compare the
fingerprint it shows with the one in the log **before** you click through and paste the token
(that comparison is the whole security of the first visit, see [chapter 5](05-security.md)).
`localhost` is always on the Host allowlist, so the forwarded request is accepted.

Things worth trying once you are in:

- Create a fleet `dev` in the GUI, then `mf central fleet list -data /tmp/c` in a second shell
  (`docker exec`) to see the CLI sees it: one service, two front ends.
- Join a runner with a token created in the GUI (9.11, step 4), approve it, and revoke it from the
  Runners screen. Run `mf central audit -data /tmp/c -source gui` and read the rows your clicks left.
- Run `mf central gui reset-admin -data /tmp/c`: it prints a fresh setup token and your browser
  session is dead on its next click.
- Start central again with `-gui-listen 127.0.0.1:8443` and then with `-gui-listen off` and see the
  difference in the log and in what `docker run -p` can reach.

Stop with `kill $(jobs -p)` and `exit`; the container and `/tmp/c` vanish.

## 9.16 Testing distribution (M6)

M6 adds a second family of checks: not "does the code work" but "does what we ship install and
run". They follow the same container-only rule as the rest of this chapter, through
`scripts/release.sh` instead of `scripts/dev.sh` (code tour: [8.12](08-code-tour.md#812-the-release-tooling-m6)).
Scenario: sid has changed a packaging script and wants to know whether the `.deb` still installs.

### One command: `m6check`

`scripts/release.sh m6check` runs everything in order, stops at the first failure, prints one
summary line (`m6check: OK (N steps passed)` or `m6check: FAIL at <step>`) and always cleans up:

`fmt vet build test cross licenses vuln lint-sh lint-actions snapshot relgen pkgtest installtest
image lab npmtest harnesscheck`

The slow privileged `integration` run is not part of it; CI runs that. While iterating, run one
target at a time (`scripts/release.sh pkgtest`), and skip steps CI already covers with
`MIRAFLEET_M6CHECK_SKIP="cross licenses"` (skipped steps are listed in the summary). It is
expensive, so run it once, not after every edit.

### What each target proves

| Target | Script | Proves |
|---|---|---|
| `snapshot` | `.goreleaser.yaml` via the tools image | goreleaser builds every asset (archives, packages, checksums, SBOMs) with signing and publishing skipped. Later targets use its output in `dist/`. |
| `pkgtest` | `scripts/release/pkgtest.sh` | The `.deb`, `.rpm` and `.apk` install in `debian:12`, `ubuntu:24.04`, `rockylinux:9` and `alpine:3` containers: user `mirafleet`, directories, file modes (`central.env` is 0600 root), `mirafleet version`, `systemd-analyze verify` on the units, and that upgrade, remove and purge keep the data and the user. A missing format is reported as SKIP. |
| `installtest` | `installtest.sh`, `installtest.ps1` | `install.sh` against a fake release served over HTTPS inside a container. A tampered archive, an archive missing from `checksums.txt`, no SHA tool and a wrong architecture are all refused. The cosign cases use a stand-in `cosign` that accepts only the expected identity and issuer, so they prove the script passes and enforces those values; real keyless signing is covered by the release rehearsal. The PowerShell twin runs on Windows in CI. |
| `image` | `imagetest.sh` | The OCI image builds (no push); `central init` then `run` on random ports; `docker exec ... fleet list` works; there is no shell in the image; the user is 65532; the GUI answers over TLS with the logged fingerprint. |
| `lab` | `labtest.sh` | `deploy/compose/lab` end to end: join, approve, `mcp login`, an action, the GUI login page, then `compose down -v`. Ports bind to `127.0.0.1` only. |
| `npmtest` | `npmtest.sh` | `relgen npm` output packs; no lifecycle scripts; `os`/`cpu` fields; `mcpName`; the launcher test (`npm/mirafleet/test/launcher.test.js`, `node --test`); and `npx` of the local tarballs runs `version` and an MCP `initialize`. |
| `harnesscheck` | `harnesscheck.sh` | The six harness snippets parse, command copies equal the shared body, files are LF. |
| `e2e-systemd` | `e2e-systemd.sh` | Real systemd as PID 1 in a throwaway privileged container: install the `.deb`, run central and an actions runner as the shipped units, join, approve, `mcp login`, `action.run`, one MCP `tools/call`, the GUI checks, and removal that keeps the data. Run by CI; needs a snapshot first. |

Two details worth knowing. **Real signing cannot be tested locally**: keyless signing needs a
GitHub OIDC identity, which only a real workflow run has. That is why the first release is
rehearsed as an `-rc.1` tag ([10.12](10-operations.md#1012-install-upgrade-and-verify-by-channel-m6)).
**Everything is labelled** `org.mirastacklabs.mirafleet.release=1`, so `scripts/release.sh clean`
removes the images, containers, volumes and `dist/` that a run left behind.

<!-- verify: pkgtest/e2e-systemd host-mode parts (user unit with `systemd-analyze --user`, dev and qa runners, backup-on-upgrade) are late-lane L3 and not built on m6-now -->

### The CI workflows

| Workflow | When | Jobs |
|---|---|---|
| `gate.yml` | push, PR | `fmt-vet`, `build`, `test-race`, `test-nocgo`, `cross`, `licenses`, `integration`, and the roll-up job `gate` (the required check). |
| `ci.yml` | push to `main`, PR | `vuln`, `lint` (shellcheck, actionlint), `dist` (snapshot, then pkgtest, installtest, image, lab, npmtest, harnesscheck), `e2e-systemd`. |
| `orchestrator.yml` | push, PR | One job on `windows-latest` and `macos-latest`: the operator-side packages (including `./integrations/...`, so CRLF mistakes show up), `install.ps1`, the npm launcher, `brew style`. |
| `dco.yml` | PR | Every commit has a `Signed-off-by:` trailer matching its author ([11.9](11-contributing.md#119-releasing-the-dco-and-new-package-formats-m6)). |
| `release.yml` | tag `v*` | `verify`, `build`, `draft`, `publish-release`, `image`, `npm`, `brew`, `mcp-registry` ([11.9](11-contributing.md#119-releasing-the-dco-and-new-package-formats-m6)). |

Safety rules every workflow follows: `permissions: contents: read` unless a job needs more; no
secrets on pull requests; never `pull_request_target` (it would run PR code with secrets);
third-party actions pinned by commit SHA; untrusted text such as a branch name never pasted into
a `run:` script. The required checks on `main` are `gate`, `licenses`, `lint`, `dco` and both
`orchestrator` jobs; the slow ones (`integration`, `dist`, `e2e-systemd`) are checked by the
release `verify` job on the exact commit being tagged.

### The worktree rules

M6 was built in its own git **worktree** (a second checkout of the same repository in another
directory, here `mirafleet-m6` on branch `m6-now`) so that several people and agents could work
in parallel without stepping on one another. The rules that kept it safe apply to any parallel
work:

- Work only in your worktree, on your branch. Stage files **by name**, never `git add .` or
  `-A`, because another worker may have changed other files in the same tree.
- Every Go and tool step goes through `scripts/dev.sh` or `scripts/release.sh`, one target per
  call. Run only the tests of the packages you touched; the full gate runs in CI.
- Never touch a live app, real hosts or real data. Tests use ephemeral ports, `t.TempDir()` and
  keys made at test time.
- At most one background waiter per long run, and its loop must also end on failure or when the
  job dies, never only on success. Kill containers you started before you finish.
- Never push, rebase or force-push unless asked; do not add a `Signed-off-by:` trailer for
  someone else (section 11.9 explains why).
- **The design is the contract.** If code differs from `docs/M*.md` in any user-visible way
  (names, flags, layout, limits, errors), change the code or update the plan in the same commit
  and say so. The docs are written from the plan in parallel, so a silent change makes them lie.

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
9. `TestM5Done` uses the go-sdk's client, and `TestM5NoHooks` uses none. What does each one prove
   that the other cannot?
10. Why is the orchestrator-side CI run on Windows and macOS when `scripts/dev.sh cross` already
    builds for both?
11. A new screen's author forgets the CSRF check on one POST. Which test fails, and how does the
    test know about a route nobody told it about?
12. `TestM4Done` step 12 throws away the `gui.Server` and the service and builds new ones over the
    same SQLite file. What claim does that prove?
13. In `TestM4Done` the clock is an offset that the test advances. Why is it injected into the
    service as well as the GUI?
14. `installtest` uses a stand-in `cosign`. What does that prove, and what does it not?
15. Name three rules from the worktree list that protect other people's work, and say why `git
    add -A` breaks one of them.
16. Which target would catch a `.deb` that leaves `central.env` world-readable, and which one
    would catch an image that contains a shell?

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
9. `TestM5Done` proves the *behaviour* of the tools against a real central: guarded refusal,
   ownership, caps, chunking, cancel, idle reaping, audit rows. It runs the server in process over
   pipes. `TestM5NoHooks` proves the *shipped entry point*: real flags, signals and stdout, with
   a clean environment and hand-written JSON-RPC, so a stray `Println` or a dependency on a
   `CLAUDE_*` variable would fail it.
10. `cross` only compiles and vets. The Windows DACL code, the path handling and the tests of
    `orch`, `handoff` and `keys` have to *run* on Windows to prove anything, and there is no
    Windows in the Linux container.
11. `TestSecurityCSRFOnEveryPost` fails: it sends every discovered POST route without a token and
    wants a refusal. It knows the route because the suite scans the package's source for
    `Handle`, `HandleFunc` and `Passive` calls with a literal `"METHOD /path"`, and it fails if the
    number of registration calls differs from the number it parsed, so an unusual registration
    breaks the suite instead of hiding from it.
12. That no session state lives only in memory: sessions are rows in SQLite, so they survive a
    restart of central, and the credential-generation check (a password change or `reset-admin`)
    is made against the database on every request, not remembered by the process.
13. Session expiry is decided in the service (`GUIAuthenticate` compares with `s.now()`), so
    advancing only the GUI's clock would change nothing. One offset clock shared by both means
    "31 minutes later" is real for the check that matters, and the test never sleeps.
14. It proves `install.sh` passes the exact certificate identity and OIDC issuer to cosign and
    refuses when verification fails. It does not prove that Sigstore signs anything: real keyless
    signing needs a GitHub OIDC identity, so the `-rc.1` rehearsal covers it.
15. Any three of: work only in your worktree; stage files by name; do not rebase or force-push;
    no pushing unasked; one background waiter; do not touch live data. `git add -A` would stage
    files another worker changed in the same tree and commit them under your message.
16. `pkgtest` (it checks file modes) and `image` (it checks there is no shell and that the user
    is 65532).

</details>
