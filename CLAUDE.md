# mirafleet

Claude Code orchestrates fleets of servers through a central server. Open source (Apache-2.0)
under the `mirastacklabs-ai` GitHub org. Design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
Milestones: [docs/PLAN.md](docs/PLAN.md). Developer guide (for new engineers):
[docs/guide/](docs/guide/README.md) — every milestone updates it, following docs/guide/STYLE.md.

## Ground rules
- One static Go binary: `mirafleet central | runner | mcp`. `CGO_ENABLED=0` must keep building.
- Platforms: central and runner (fleet nodes) are **Linux only**; `mirafleet central|runner`
  refuse to start elsewhere. The orchestrator side (`mirafleet mcp`, harness integrations, npm
  wrapper) works on linux, darwin and windows (amd64 + arm64). The binary builds for all six, so
  `scripts/dev.sh cross` must pass (compile + vet only). OS-specific code goes behind build tags
  and fails closed elsewhere (no silent no-op on a security check).
- Central's OSS store is SQLite via `modernc.org/sqlite` (pure Go), behind the `Store` interface.
- Keep dependencies to those listed in docs/PLAN.md; ask before adding others.
- Licenses (all deps, transitive included, and any bundled binary or asset): permissive only —
  Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, 0BSD. Never GPL, LGPL, AGPL, MPL, SSPL, BUSL
  or other copyleft/source-available. `scripts/dev.sh licenses` enforces this with `go-licenses` (CI from M6).
  Exception: font files may be SIL OFL-1.1 (shipped with their licence text, never sold alone).
- Branding: follow [docs/BRAND.md](docs/BRAND.md) for the GUI, CLI colours, docs and store assets.
- Containers: the Docker ecosystem is supported (Docker CLI, Docker Engine/moby, Compose, docker.io
  images). Never a hard dependency on Docker's commercial products (Docker Desktop paid tiers, Docker
  Business/Enterprise). Runner workspaces (M3) use containerd via its Go client, which also works on
  Docker hosts (Docker Engine runs on containerd).
- The runner-side actions guard is the security boundary: argv only, no shell, regex-validated params.
- Harness-agnostic core: Claude Code first, then opencode, Kilo Code, etc. The MCP server must work
  with no hooks; harness-specific code lives only in `integrations/<harness>/`.
- OSS stays lean. Enterprise features (RBAC, SSO, HA, ...) are out of scope; leave interface seams only.

## Commands (containers only)
Nothing is installed or run on the host toolchain: no host `go`, brew, npm or pip for this repo.
Every step runs in a digest-pinned container through `scripts/dev.sh`, using the Docker CLI
(on macOS: colima's Docker runtime, `colima start`). Override the CLI with `MIRAFLEET_CTR`.
- Build: `scripts/dev.sh build` · Test: `scripts/dev.sh test` (`go test -race`)
- Lint: `scripts/dev.sh vet` and `scripts/dev.sh fmt` · Cross-OS: `scripts/dev.sh cross`
- Anything else: `scripts/dev.sh go <args>` (e.g. `go mod tidy`) or `scripts/dev.sh shell`.
- New tools (shellcheck, go-licenses, goreleaser, ...) run as pinned container images, never host installs.

## Testing
- No real hosts, ports or data: ephemeral ports, `t.TempDir()`, keys generated at test time.
- containerd tests use build tags `linux && integration` and run in a container (with containerd)
  on the Docker runtime.
