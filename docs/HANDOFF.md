# Handoff — 15 Nov 2026 (`v0.6.0` released; npm staged, brew and MCP Registry waiting)

Read this first, then [TRACKER.md](TRACKER.md), [RELEASING.md](RELEASING.md) and the plans
([M4.md](M4.md), [M6.md](M6.md), [M7.md](M7.md)). Every decision is recorded in those plans.

## Where things are

| What | State |
|---|---|
| `main` | M1–M7 built and merged; version files at `0.6.0`; all CI green. Protected: changes only by PR. |
| Tag `v0.6.0-rc.1` | dead: tagged before the version bump, nothing published. Left in place. |
| Tag `v0.6.0-rc.2` | GitHub pre-release and images published; `npm` failed (no auth; old `npm publish`). Not re-run. |
| Tag `v0.6.0-rc.3` | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43730773461, all jobs green: pre-release (29 assets), images on both registries (amd64 + arm64), 7 npm packages **staged** (tag `next`), not approved. |
| Tag `v0.6.0` (on `cded12f`) | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43732260792: `verify`, `build`, `draft`, `publish-release`, `image`, `npm` green; `brew` failed (no `HOMEBREW_TAP_TOKEN`); `mcp-registry` failed by design (npm not live yet). |
| GitHub release `v0.6.0` | https://github.com/mirastacklabs-ai/mirafleet/releases/tag/v0.6.0, latest, 29 assets. Checked: checksums, `linux_arm64` binary prints `0.6.0`. |
| Images | `docker.io/mirastack/mirafleet:0.6.0` and `:latest`, `ghcr.io/mirastacklabs-ai/mirafleet:0.6.0`: amd64 + arm64; image prints `0.6.0`; ghcr pulls anonymously (public). |
| npm | 7 packages × 2 versions (`0.6.0-rc.3`, `0.6.0`) **staged**, waiting for the user's 2FA approval at https://www.npmjs.com/settings/mirastacklabs/staged-packages. |
| Wiki | regenerated from `cded12f` (v0.6.0) and pushed (`0480e3e`). |

## Settings in place (15 Nov 2026)
- **GitHub repo** `mirastacklabs-ai/mirafleet`: `release` environment (required reviewer aarvee11,
  `v*` tags only); ruleset `main` (PR, squash only, required checks `gate`, `licenses`, `lint`,
  `dco`, `windows-latest`, `macos-latest`); ruleset `release-tags` (only admins on `v*`);
  auto-merge and delete-branch on; private vulnerability reporting on.
- **`release` environment:** variable `DOCKERHUB_USERNAME=mirastack`; secrets `DOCKERHUB_TOKEN`
  and `NPM_TOKEN` (npm granular token `mirafleet-stage-only`: stage only, scope `@mirastacklabs`,
  no 2FA bypass, expires 22 Nov 2026).
- **npm:** user account `mirastacklabs` (no npm org; the scope is the user scope). Publishing is
  staged (`npm stage publish`, PR #7); a maintainer approves with 2FA.
- **Docker Hub:** org `mirastack`, public repo `mirafleet` (description still empty).
- **Homebrew tap:** `mirastacklabs-ai/homebrew-tap` with a README; no formula yet.
- **DCO:** every commit uses `git commit -s` as the user (aarvee11); no Claude co-author trailer.

## Next steps, in order
1. **User:** approve the 14 staged npm versions (both `0.6.0-rc.3` and `0.6.0`) in one batch with
   one 2FA at https://www.npmjs.com/settings/mirastacklabs/staged-packages.
2. **Agent:** check `npx -y @mirastacklabs/mirafleet@0.6.0 version` in a container; re-run the
   failed `mcp-registry` job of run 43732260792 and verify the MCP Registry entry.
3. **User:** create `HOMEBREW_TAP_TOKEN` (fine-grained PAT, Contents read/write on
   `mirastacklabs-ai/homebrew-tap` only, with an expiry) and run
   `gh secret set HOMEBREW_TAP_TOKEN --env release --repo mirastacklabs-ai/mirafleet`.
   **Agent:** re-run the failed `brew` job; verify `brew install mirastacklabs-ai/tap/mirafleet`
   in a container.
4. **Agent, with the user's 2FA:** add a trusted publisher to each of the 7 packages (repo
   `mirastacklabs-ai/mirafleet`, workflow `release.yml`, environment `release`, allowed action
   `npm stage publish` only); delete the npm token and
   `gh secret delete NPM_TOKEN --env release --repo mirastacklabs-ai/mirafleet`; set publishing
   access to "Require 2FA and disallow tokens".
5. P6: Docker Hub description and README (needs a Docker Hub sign-in). Then P9–P12 (RELEASING.md §3
   step 16) and the full-product test (M4 S19 browser check, M5 S17, M7 S21 smoke on the user's
   Mac + a VM with their GitLab), quickstart and harness spot checks.

## Rules learned
- Approving `release` deployments: the user's click by default. On 15 Nov 2026 the user allowed the
  agent to approve them for the rc.3 and v0.6.0 runs only (gh API, with a comment naming that).
  Ask again for any later run.
- npm: bypass-2FA tokens are deprecated (no direct publish from Jan 2027); never use one. First
  publish of a new package uses a stage-only token; trusted publishers need the package to exist.
- The version files must match the tag exactly, rc included (RELEASING.md §3 step 3).
- Verification downloads: colima only shares the home folder, so pipe files into containers
  (`docker run -i ... < file`) or use the repo's git-ignored `dist/`, not the scratchpad.
- The agent never types token values; the user pastes them into `gh secret set`.
- Talk to the user in short, plain sentences; give exact commands and clicks.

## Open doc markers (resolve at the points named)
- `docs/guide/12-why-mirafleet.md`: re-research the other tools' distribution rows; re-read after S21.
- `docs/guide/04-flows.md` flows 30–36 and `05-security.md` §5.14 rows 1, 2, 6, 8, 11, 12: confirm in
  the S21 smoke.

## Standing rules (also in memory)
- Container-only (`scripts/dev.sh`, `scripts/release.sh`); no host go/python/sed -i.
- Design is the contract: docs are written from the plans in parallel with code; any deviation is
  recorded in the plan in the same commit.
- main is ruleset-protected: branch → `git commit -s` → PR → `gh pr merge --auto --squash
  --delete-branch` (RELEASING.md §4). Merge main into branches; never force-push.
- Local `release.sh snapshot` on colima needs `mod_timestamp` commented out temporarily (virtiofs
  rejects `chtimes`); never commit that. CI doesn't need it.
- Docs use the MIRASTACK LABS cast (STYLE.md): aarvee, raj, durga, kalyan, sid; they/them.
