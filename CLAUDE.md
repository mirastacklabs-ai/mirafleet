# mirafleet

Claude Code orchestrates fleets of servers through a central server. Open source (Apache-2.0)
under the `mirastacklabs-ai` GitHub org. Design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md).
Milestones: [docs/PLAN.md](docs/PLAN.md).

## Ground rules
- One static Go binary: `mirafleet central | runner | mcp`. `CGO_ENABLED=0` must keep building.
- Cross-platform: every binary and package we ship builds and works on linux, darwin and windows
  (amd64 + arm64). OS-specific code goes behind build tags with a real, secure fallback for every
  other OS (no silent no-op on a security check). `scripts/dev.sh cross` must pass (it proves
  compilation only). Windows stays unsupported until the gaps in docs/PLAN.md (Q20–Q24) are fixed.
- Central's OSS store is SQLite via `modernc.org/sqlite` (pure Go), behind the `Store` interface.
- Keep dependencies to those listed in docs/PLAN.md; ask before adding others.
- Licenses (all deps, transitive included, and any bundled binary or asset): permissive only —
  Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, 0BSD. Never GPL, LGPL, AGPL, MPL, SSPL, BUSL
  or other copyleft/source-available. CI enforces this with `go-licenses`.
  Exception: font files may be SIL OFL-1.1 (shipped with their licence text, never sold alone).
- Branding: follow [docs/BRAND.md](docs/BRAND.md) for the GUI, CLI colours, docs and store assets.
- Containers mean containerd via its Go client. Never Docker Engine or docker.com tooling.
- The runner-side actions guard is the security boundary: argv only, no shell, regex-validated params.
- Harness-agnostic core: Claude Code first, then opencode, Kilo Code, etc. The MCP server must work
  with no hooks; harness-specific code lives only in `integrations/<harness>/`.
- OSS stays lean. Enterprise features (RBAC, SSO, HA, ...) are out of scope; leave interface seams only.

## Commands (containers only)
Nothing is installed or run on the host toolchain: no host `go`, brew, npm or pip for this repo.
Every step runs in a pinned container through `scripts/dev.sh`, on the colima VM `mirafleet`
(containerd runtime, driven by nerdctl): `colima start -p mirafleet --runtime containerd`.
- Build: `scripts/dev.sh build` · Test: `scripts/dev.sh test` (`go test -race`)
- Lint: `scripts/dev.sh vet` and `scripts/dev.sh fmt` · Cross-OS: `scripts/dev.sh cross`
- Anything else: `scripts/dev.sh go <args>` (e.g. `go mod tidy`) or `scripts/dev.sh shell`.
- New tools (shellcheck, go-licenses, goreleaser, ...) run as pinned container images, never host installs.

## Testing
- No real hosts, ports or data: ephemeral ports, `t.TempDir()`, keys generated at test time.
- containerd tests use build tags `linux && integration` and run inside the `mirafleet` VM.
