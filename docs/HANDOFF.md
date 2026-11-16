# Handoff — 15 Nov 2026 (`v0.6.0` released on every channel)

Read this first, then [TRACKER.md](TRACKER.md), [RELEASING.md](RELEASING.md) and the plans
([M4.md](M4.md), [M6.md](M6.md), [M7.md](M7.md)). Every decision is recorded in those plans.

## Where things are

| What | State |
|---|---|
| `main` | M1–M7 built and merged; version files at `0.6.0`; all CI green. Protected: changes only by PR. |
| Tag `v0.6.0-rc.1` | dead: tagged before the version bump, nothing published. Left in place. |
| Tag `v0.6.0-rc.2` | GitHub pre-release and images published; `npm` failed (no auth; old `npm publish`). Not re-run. |
| Tag `v0.6.0-rc.3` | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43730773461, all jobs green; npm `0.6.0-rc.3` live on dist-tag `next`. |
| Tag `v0.6.0` (on `cded12f`) | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43732260792: all 8 jobs green (`brew` and `mcp-registry` re-run after the npm approval and the tap token). |
| GitHub release `v0.6.0` | https://github.com/mirastacklabs-ai/mirafleet/releases/tag/v0.6.0, latest, 29 assets. Checked: checksums, `linux_arm64` binary prints `0.6.0`. |
| Images | `docker.io/mirastack/mirafleet:0.6.0` and `:latest`, `ghcr.io/mirastacklabs-ai/mirafleet:0.6.0`: amd64 + arm64; image prints `0.6.0`; ghcr pulls anonymously (public). |
| npm | `@mirastacklabs/mirafleet` + 6 platform packages: `latest` = `0.6.0`, `next` = `0.6.0-rc.3`; `npx -y @mirastacklabs/mirafleet@0.6.0 version` prints `0.6.0`. |
| Homebrew | `mirastacklabs-ai/homebrew-tap` `Formula/mirafleet.rb` at 0.6.0; `brew install mirastacklabs-ai/tap/mirafleet` (homebrew/brew container) prints `0.6.0`. |
| MCP Registry | `io.github.mirastacklabs-ai/mirafleet` 0.6.0 listed. |
| Wiki | regenerated from `cded12f` (v0.6.0) and pushed (`0480e3e`). |

## Settings in place (15 Nov 2026)
- **GitHub repo** `mirastacklabs-ai/mirafleet`: `release` environment (required reviewer aarvee11,
  `v*` tags only); ruleset `main` (PR, squash only, required checks `gate`, `licenses`, `lint`,
  `dco`, `windows-latest`, `macos-latest`); ruleset `release-tags` (only admins on `v*`);
  auto-merge and delete-branch on; private vulnerability reporting on.
- **`release` environment:** variable `DOCKERHUB_USERNAME=mirastack`; secrets `DOCKERHUB_TOKEN`
  and `HOMEBREW_TAP_TOKEN` (fine-grained PAT `mirafleet-homebrew-tap`, owner `mirastacklabs-ai`,
  Contents read/write on `homebrew-tap` only, expires 15 Dec 2026: renew before the next release).
  `NPM_TOKEN` is deleted, and npm has no access tokens at all.
- **npm:** user account `mirastacklabs` (no npm org; the scope is the user scope). Publishing is
  staged (`npm stage publish`, PR #7); a maintainer approves with 2FA. All 7 packages have a
  trusted publisher (repo `mirastacklabs-ai/mirafleet`, workflow `release.yml`, environment
  `release`, allowed action `npm stage publish` only), **pending validation: it expires on
  18 Nov 2026 ~00:35 UTC unless a release stages through it before then.** If it expires, delete
  and recreate it on each package (one 2FA each) right before the next release, then tag within
  2 days. Publishing access "disallow bypass 2FA tokens" is set on `mirafleet` and
  `mirafleet-linux-x64` only; for the other 5 the npm website returned a Cloudflare bot check
  (HTTP 429) on save, for the agent and the user alike.
- **Docker Hub:** org `mirastack`, public repo `mirafleet`; short description and overview set
  (15 Nov 2026, P6 done).
- **GitHub org `mirastacklabs`** (not `mirastacklabs-ai`) shows "flagged, hidden from the public";
  the user may ask GitHub support for a review. It does not affect the release.
- **DCO:** every commit uses `git commit -s` as the user (aarvee11); no Claude co-author trailer.

## Next steps, in order
1. Optional: the other 5 packages' publishing access ("disallow bypass 2fa tokens"). Retry the
   website later, or use `npm access set mfa=publish @mirastacklabs/<pkg>` from a throwaway
   `node:24-alpine` container after the user's `npm login` (2FA per call).
2. P9–P12 (RELEASING.md §3 step 16): Anthropic plugin directory, galleries, Docker-Sponsored OSS,
   mirastacklabs.ai one-pager and announcement.
3. Full-product test: M4 S19 browser check, M5 S17, M7 S21 smoke on the user's Mac + a VM with
   their GitLab, quickstart and harness spot checks. Then resolve the open doc markers below.

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
