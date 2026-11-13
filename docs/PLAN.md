# mirafleet — Build plan

Design: [ARCHITECTURE.md](ARCHITECTURE.md). Module: `github.com/mirastacklabs-ai/mirafleet`.
One static binary (`CGO_ENABLED=0`). Dependencies kept small:
`golang.org/x/crypto/ssh`, `golang.org/x/sys` (unix/windows syscalls), `modernc.org/sqlite`
(pure Go), `gopkg.in/yaml.v3`,
`github.com/containerd/containerd/v2` (M3), `github.com/modelcontextprotocol/go-sdk` (M5).
CLI uses the standard library (`flag` with subcommands).
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
- SQLite store behind a `Store` interface. Fleets, runners, join tokens, operators, audit.
- Enrollment (`token create`, `runner join`), runner lifecycle, operator login and approval.
- `api@mirafleet` channel for operators: list fleets/runners, run action.
- Interfaces: `Store`, `AuthProvider`, `PolicyEngine`, `AuditSink` (OSS implementations only).
- Lifecycle service on central with one code path behind the CLI, the GUI (M4) and the
  `runner_lifecycle` MCP tool (M5). Revoke immediately rejects the key, closes the live
  connection and cancels in-flight work (Q11). Every change is audited with its source.
- Carried over from the M1 review: size and time limits on admin requests (Q12).
- Deployment assumption: each fleet node talks to exactly one central, and each central owns
  its data dir (closes Q13). Handshake caps and connection-level DoS limits (Q19) are an
  enterprise feature; OSS docs recommend a firewall or rate limit in front of an
  internet-facing central.

## M3 — Workspaces
- containerd executor (namespace `mirafleet`), workspace dir, open/exec/file/close,
  resource limits, `concurrent: N`, scheduling with sticky sessions, TTL reaper.
- Integration tests build-tagged `linux && integration` (need a real containerd; on macOS run in
  a privileged containerd container on the Docker runtime).

## M4 — Admin GUI
- `html/template` + htmx (vendored, embedded). Screens per ARCHITECTURE.md. Admin bootstrap on
  first start. CSRF protection, secure session cookies, `127.0.0.1` default bind.

## M5 — Orchestrator
- `mirafleet mcp` stdio server with the tools in ARCHITECTURE.md, contexts, project default
  fleet, output capping, `confirm` argument for guarded fleets, idle-on-exit. Must be fully
  usable with no hooks at all.
- `integrations/claude-code/`: plugin with `.mcp.json`, `SessionStart`/`SessionEnd`/`PreToolUse`
  hooks, `/mirafleet` skill with aliases `/openfleet` and `/claudefleet`.
- Other harnesses get `/mirafleet` with alias `/openfleet`.
- Later, demand-driven: `integrations/opencode/`, `integrations/kilocode/`, others.

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
| Official MCP Registry (`server.json`, pointing at the npm and OCI packages) | discovery; many directories and harness galleries sync from it |
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
  for `mcp` need CI runners (`windows-latest`, `macos-latest`) — add in M5/M6.

## Status (13 Nov 2026)
- M1 built, reviewed and fixed (Q01–Q10, Q14–Q18); committed and pushed to `main`.
- Module path is `github.com/mirastacklabs-ai/mirafleet`.
- Dev toolchain is container-only (`scripts/dev.sh`, Docker CLI; colima Docker runtime on macOS).
- LICENSE (Apache-2.0), NOTICE and README disclaimer added. Branding approved in docs/BRAND.md.
- Next: M2 (central core, SQLite store). Central and runner are Linux only; `mcp` is cross-platform.
