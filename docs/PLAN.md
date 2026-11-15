# mirafleet — Build plan

Design: [ARCHITECTURE.md](ARCHITECTURE.md). Module: `github.com/mirastacklabs-ai/mirafleet`.
One static binary (`CGO_ENABLED=0`). Dependencies kept small:
`golang.org/x/crypto/ssh`, `golang.org/x/sys` (unix/windows syscalls), `modernc.org/sqlite`
(pure Go), `gopkg.in/yaml.v3`,
`github.com/containerd/containerd/v2` (M3, linux only; darwin and windows link none of it),
`github.com/modelcontextprotocol/go-sdk` v1.8.0 (M5) with the modules it links
(`google/jsonschema-go`, `yosida95/uritemplate`, `segmentio/encoding`, `segmentio/asm`,
`golang.org/x/{oauth2,sync,time}`; MIT-0 `segmentio/asm` accepted, see [M5.md](M5.md#10-dependencies)).
CLI uses the standard library (`flag` with subcommands).

**Build order (13 Nov 2026): M1 → M2 → M3 → M5 → M4 → M6.** M5 (orchestrator) comes before M4
(GUI): it is what lets Claude Code drive fleets end to end, and the admin CLI already covers the
GUI's functions. Milestone numbers are kept as labels. Every milestone also updates the
developer guide in [guide/](guide/README.md).
All dependencies must meet the permissive license policy in [CLAUDE.md](../CLAUDE.md).

## M1 — Transport and actions
- `mirafleet central` (minimal): embedded SSH server, runner keys from a flat file, in-memory
  registry, `heartbeat@mirafleet` handling, opens `exec@mirafleet` channels to runners.
- `mirafleet runner`: dial out, pin host key, reconnect with backoff, heartbeat, accept only
  `exec@mirafleet`, actions guard (`actions.yaml`, argv-only, regex params, timeouts),
  local audit as JSON lines.
- `mirafleet central exec <runner> <action> [k=v...]` for manual testing.
- **Done when:** an in-process integration test starts central and two runners on ephemeral ports,
  runs an allowed action, gets output and exit code, gets a refusal for an unknown action and for
  a bad parameter, and survives a runner reconnect.

## M2 — Central core
**Status: done (13 Nov 2026).** Detailed plan, decisions and steps: [M2.md](M2.md).
The "M2 done when" scenario is `TestM2Done` in `internal/central/m2_done_test.go`.
- SQLite store behind a `Store` interface. Fleets, runners, join tokens, operators, audit.
- Enrollment (`token create`, `runner join`), runner lifecycle, operator registration with a
  rotating TOTP-style code from central (no approval step).
- `api@mirafleet` channel for operators: list fleets/runners, run action. Nothing else.
- Interfaces: `Store`, `AuthProvider`, `PolicyEngine`, `AuditSink` (OSS implementations only).
- Lifecycle service on central with one code path behind the CLI and the GUI (M4), both on
  central only; never exposed over `api@mirafleet` or MCP. Revoke immediately rejects the key, closes the live
  connection and cancels in-flight work (Q11). Every change is audited with its source.
- Carried over from the M1 review: size and time limits on admin requests (Q12).
- Deployment assumption: each fleet node talks to exactly one central, and each central owns
  its data dir (closes Q13). OSS ships basic limits (Q19): per-IP and global caps on
  in-progress handshakes, a short handshake deadline and a per-IP enroll rate. Advanced
  connection-level DoS defence stays an enterprise feature; an internet-facing central should
  still sit behind a firewall or rate limiter.

## M3 — Workspaces
**Status: done (13 Nov 2026).** Reviewed twice; P0/P1 fixed, P2/P3 batched for M5 (M3.md E-01…E-17). Decisions, steps and rules: [M3.md](M3.md);
design: [ARCHITECTURE.md](ARCHITECTURE.md#workspace-mode). The "M3 done when" scenario is `TestM3Done`
(fake executor on every run; real containerd under `scripts/dev.sh integration`).
- Runner: `workspace.Executor` interface; hardened containerd sandbox (user namespace, no caps,
  read-only rootfs, seccomp, cgroup limits, in-netns egress allowlist proxy) and an opt-in
  OpenSandbox executor (HTTPS/TLS 1.3/mTLS only, `runner tls-init` and `tls-front`).
- Central: `workspace.*` operator methods with body frames, `workspace@mirafleet` channel,
  sticky sessions, placement on trusted labels, ownership, audit, TTL reaper, reconcile, revoke
  cascades; migration 0002; per-fleet workspace config and exec timeouts.
- CLI: `runner run -workspace`, `runner workspace list|prune`, `central fleet ... / workspace
  list|kill`, `mcp api -in/-out`.
- Integration tests build-tagged `linux && integration` (need a real containerd; on macOS run in
  a privileged containerd container on the Docker runtime).

## M4 — Admin GUI
**Status: planned (14 Nov 2026).** Detailed plan and decisions: [M4.md](M4.md).
- `html/template` + htmx 2 (vendored, embedded). Screens per ARCHITECTURE.md. Admin bootstrap on
  first start (setup-token file, `central gui reset-admin`). CSRF protection, secure session
  cookies (sessions in SQLite, tokens stored hashed), TLS always on, default bind `0.0.0.0:8443`
  with a self-signed certificate unless the operator supplies one. View and manage only: the GUI
  cannot run actions.

## M5 — Orchestrator
**Status: built; closing hardening and docs in progress.** Detailed plan, decisions and steps:
[M5.md](M5.md) (approved 13 Nov 2026; Q1-Q6 resolved there).
- `mirafleet mcp` stdio server on go-sdk v1.8.0 with nine tools (`list_fleets`, `list_runners`,
  `run_action`, `workspace_open`, `exec`, `read_file`, `write_file`, `workspace_close`,
  `workspace_list`), thin wrappers over the existing methods; every result says which context and
  fleet it ran on. Contexts (`contexts.yaml`, `mcp context` CLI), project file `.mirafleet.yaml`
  (`fleet`, `context`), session resolution (flag, handoff, process), output cap and sanitising,
  `confirm` pass-through for guarded fleets, idle-on-exit, operator-method guard. SSH jump hosts
  are deferred (a `jump` key is refused). Fully usable with no hooks at all.
- Action discovery: runners advertise their actions in heartbeats, shown by `runners.list`.
- Windows support for the orchestrator: `%LocalAppData%` config dir and an owner-only DACL on the
  key. Minimal GitHub Actions CI runs the orchestrator packages on `windows-latest` and `macos-latest`.
- Hardening of the M3 backlog (E-01...E-17) and of the env denylist.
- `integrations/claude-code/`: plugin with `.mcp.json`, `SessionStart`/`SessionEnd`/`PreToolUse`
  hooks (SessionEnd closes the workspace on `clear`/logout only), `/mirafleet` skill with aliases
  `/openfleet` and `/claudefleet`.
- Other harnesses: shared command body, `integrations/opencode/` (`/mirafleet`, `/openfleet`) and
  `integrations/generic/`.
- Later, demand-driven: `integrations/kilocode/`, others. Marketplace, npm, brew in M6.

## M6 — Release and distribution
- README and quickstart, LICENSE (Apache-2.0), threat-model notes.
- CI (GitHub Actions): `go vet`, `go test -race`, `go-licenses check` failing on anything outside
  the permissive allowlist in CLAUDE.md, generated third-party licenses file per release.
- One git tag drives every channel below, so all versions stay in lockstep.

**Binary (`mirafleet central | runner | mcp`)**, built by goreleaser for linux/darwin/windows ×
amd64/arm64. Server packages (`.deb`/`.rpm`/`.apk`, OCI image) are Linux only; darwin/windows
archives (zip + `.exe` on Windows) serve the orchestrator (`mirafleet mcp`):
| Channel | Audience | Notes |
|---|---|---|
| GitHub Releases | everyone | tarballs, checksums, SBOM, cosign keyless signatures, build provenance |
| Homebrew tap `mirastacklabs-ai/homebrew-tap` | orchestrator users on macOS/Linux | homebrew-core later, once adoption qualifies |
| `.deb` / `.rpm` / `.apk` (goreleaser nFPM) | runners and central | include systemd units and a `mirafleet` system user |
| OCI image on `ghcr.io/mirastacklabs-ai/mirafleet` | central on containerd / Kubernetes | Helm chart later, on demand |
| `go install …/cmd/mirafleet@latest` | Go users | no extra work |
| `install.sh` | quick start | downloads the release and verifies the checksum before installing |

**Harness and MCP stores (orchestrator)**:
| Channel | How users install |
|---|---|
| Claude Code plugin marketplace (repo with `.claude-plugin/marketplace.json`) | `/plugin marketplace add mirastacklabs-ai/mirafleet`, then `/plugin install mirafleet` |
| Anthropic's official plugin directory | submit once stable |
| npm wrapper `@mirastacklabs/mirafleet` (per-platform binaries as optional dependencies, no postinstall download) | `npx -y @mirastacklabs/mirafleet mcp` in any harness's MCP config |
| Official MCP Registry (`server.json`, pointing at the npm package; see [M6.md](M6.md)) | discovery; many directories and harness galleries sync from it |
| Harness galleries and community directories (Kilo Code / Cline MCP marketplaces, VS Code, Cursor, Smithery, Glama, PulseMCP) | per their submission process; mostly fed by the registry |
| opencode and others without a store | copy-paste config snippet in `integrations/<harness>/README.md` |

All distribution tooling used must itself meet the license policy (goreleaser, nFPM, cosign and
syft are MIT or Apache-2.0).

## Rules for every milestone
- Tests never touch real hosts or real data: ephemeral ports, temp dirs, in-memory or temp SQLite.
- No secrets in the repo. Test keys are generated at test time.
- Any background waiter must also exit on failure or when the job dies.

## Cross-platform (all milestones)
- **Central and runner: Linux only** (fleet nodes and the central server are Linux; containerd
  workspaces in M3 are Linux anyway). `mirafleet central|runner` refuse to start on other OSes.
  Their Unix-only security code (mode-bit key checks, admin socket peer check, process-group kill,
  `/usr/...` fixed PATH) therefore needs no Windows port. The non-unix admin socket fails closed as
  defense in depth; no non-unix admin transport is planned.
- **Orchestrator (`mirafleet mcp`, M5): linux, darwin, windows.** It must not reuse server-side
  Unix assumptions: per-OS config/state dirs (`os.UserConfigDir`, `os.UserCacheDir`), `os.Interrupt`
  for shutdown on Windows, client keys protected per OS (DACL on Windows).
- `scripts/dev.sh cross` builds and vets all six targets; tests run on linux. Windows/darwin tests
  for `mcp` run in a minimal GitHub Actions workflow on `windows-latest` and `macos-latest`
  (M5, `.github/workflows/orchestrator.yml`); the full CI comes in M6.

## Status (13 Nov 2026)
- M1 built, reviewed and fixed (Q01–Q10, Q14–Q18); committed and pushed to `main`.
- Module path is `github.com/mirastacklabs-ai/mirafleet`.
- Dev toolchain is container-only (`scripts/dev.sh`, Docker CLI; colima Docker runtime on macOS).
- LICENSE (Apache-2.0), NOTICE and README disclaimer added. Branding approved in docs/BRAND.md.
- M2 built (store, join tokens, rotating operator registration codes, lifecycle on central only,
  `api@mirafleet`, admin socket, CLI, `runner join`, `mcp login|api`). **M2 done** 13 Nov 2026:
  reviewed, all findings fixed (docs/M2.md §4b), built-binary smoke test and `TestM2Done` pass.
- M3 built (workspaces: containerd default, OpenSandbox opt-in, egress proxy, sticky sessions,
  reaper and reconcile, migration 0002, CLI). **M3 done** 13 Nov 2026: final review P1s fixed
  (destroy racing an in-flight create), full gate and integration green, `TestM3Done`,
  `TestM3DoneIntegration` and `TestIntegrationWorkspaceCLI` pass.
- M5 (orchestrator) built: MCP server, contexts, sessions, Claude Code plugin, other harnesses,
  `TestM5Done` and `TestM5NoHooks`; closing hardening and docs in progress, not yet marked done.
- Next: finish M5, then M4 (GUI), then M6 (release).
  Central and runner are Linux only; `mcp` is cross-platform.
