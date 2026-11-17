# Handoff — 17 Nov 2026 (`v0.6.2` released on every channel; directory listings submitted)

Read this first, then [RELEASING.md](RELEASING.md). Maintainers keep the milestone plans and the
tracker locally (git-ignored).

## Where things are

| What | State |
|---|---|
| `main` | M1–M7 built and merged; version files at `0.6.2`; all CI green. Protected: changes only by PR. |
| Tag `v0.6.2` (17 Nov, on `a43f824`) | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/44187252480: all 8 jobs green, first try. Ships PRs #32–#40: workspace-mode runner Compose setup (`deploy/compose/runner-workspace/`, CI job `compose-workspace`), Cursor plugin, AppArmor fail-closed in the ctrd executor (`apparmor: auto` refuses to start when the kernel has AppArmor on but `apparmor_parser` is missing), `mcp -project-dir` fail-closed. |
| Release `v0.6.2` channels | GitHub release (latest, 35 assets, notes by hand; checksums and attestations checked); `docker.io/mirastack/mirafleet:0.6.2` and GHCR, same digest `sha256:6e5844f6…`; tap formula 0.6.2; npm 7 packages 0.6.2 (approved by the user; `npx … version` prints `0.6.2`); MCP Registry 0.6.2. Wiki regenerated from `a43f824`. |
| Tag `v0.6.1` (16 Nov) | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43807258316: verify, build, draft, publish-release, image, npm, brew green; `mcp-registry` re-run after the npm approval. Ships PRs #14–#30 (MCP EOF drain, plugin node launcher at `integrations/claude-code/plugin/`, `mirafleet-setup`, Gemini archives). |
| GitHub release `v0.6.1` | https://github.com/mirastacklabs-ai/mirafleet/releases/tag/v0.6.1, latest, 35 assets (incl. `darwin|linux|win32.mirafleet.tar.gz` for Gemini). Notes written by hand (goreleaser's changelog is off). |
| Images, Homebrew (0.6.1) | `docker.io/mirastack/mirafleet:0.6.1` + `:latest` and GHCR, amd64 + arm64; tap formula at 0.6.1. |
| npm (0.6.1) | 7 packages staged through the trusted publishers (OIDC, provenance) — this validated them; approved by the user with 2FA. |
| Tag `v0.6.0-rc.1` | dead: tagged before the version bump, nothing published. Left in place. |
| Tag `v0.6.0-rc.2` | GitHub pre-release and images published; `npm` failed (no auth; old `npm publish`). Not re-run. |
| Tag `v0.6.0-rc.3` | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43730773461, all jobs green; npm `0.6.0-rc.3` live on dist-tag `next`. |
| Tag `v0.6.0` (on `cded12f`) | run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43732260792: all 8 jobs green (`brew` and `mcp-registry` re-run after the npm approval and the tap token). |
| GitHub release `v0.6.0` | https://github.com/mirastacklabs-ai/mirafleet/releases/tag/v0.6.0, latest, 29 assets. Checked: checksums, `linux_arm64` binary prints `0.6.0`. |
| Images | `docker.io/mirastack/mirafleet:0.6.0` and `:latest`, `ghcr.io/mirastacklabs-ai/mirafleet:0.6.0`: amd64 + arm64; image prints `0.6.0`; ghcr pulls anonymously (public). |
| npm | `@mirastacklabs/mirafleet` + 6 platform packages: `latest` = `0.6.0`, `next` = `0.6.0-rc.3`; `npx -y @mirastacklabs/mirafleet@0.6.0 version` prints `0.6.0`. |
| Homebrew | `mirastacklabs-ai/homebrew-tap` `Formula/mirafleet.rb` at 0.6.0; `brew install mirastacklabs-ai/tap/mirafleet` (homebrew/brew container) prints `0.6.0`. |
| MCP Registry | `io.github.mirastacklabs-ai/mirafleet` 0.6.0 listed. |
| Wiki | regenerated from `a43f824` (v0.6.2) on 17 Nov. |
| In v0.6.1 (16 Nov) | PRs #21/#26: `mirafleet mcp` answers pipelined requests even when stdin closes at once (EOF drain by id set, 2 s cap, `tools/call` still cancelled, SIGPIPE-safe, broken stdout exits 0). PR #22: flaky `TestM7Done` step 10 fixed (test bug). PRs #23/#25: directory setup plugin. PRs #29/#30: Gemini release archives, "Every release" checklist. |
| In v0.6.1 (15 Nov) | PRs #14–#19: plugin icon; plugin root moved to `integrations/claude-code/plugin/` with a node launcher (`bin/mirafleet.js`, no Go code in the plugin, Claude directory rules, M6 §2.18); front page, `docs/TESTING.md`, issue forms (`test-report` label), SUPPORT, CITATION, social preview; `gemini-extension.json`, `GEMINI.md`, `llms-install.md`, `glama.json`, `docs/assets/logo-400.png`; plugin listing links + `PRIVACY.md`. The marketplace now pins the plugin at `integrations/claude-code/plugin` with ref `v0.6.1`. |

## Listings (15 Nov 2026)

| Where | State |
|---|---|
| Claude plugin directory | Two submissions at claude.ai/directory/manage (contact rv@mirastacklabs.ai). (1) **`mirafleet-setup`** (`integrations/claude-code/directory`: skills + command only; PRs #23, #25): tracks tag `v0.6.1` (not `main`); scan passed, Publish requested for v0.6.1 on 16 Nov, waiting for an Anthropic reviewer; Claude Code only. Its skill installs the binary and the full plugin from our marketplace. (2) Full plugin **`mirafleet`** (`integrations/claude-code/plugin`): refused automatically on 16 Nov, because non-Verified orgs may not ship command hooks, local MCP servers, `bin/` or scripts. Verification asked of Anthropic: ticket **#156498411**. Once Verified, push a new version to submission (2). Data answers for both: no personal data, sends nowhere else, not retained, not for under-18s. |
| Kilo Code marketplace | PR https://github.com/Kilo-Org/kilo-marketplace/pull/734 (`mcps/mirafleet/MCP.yaml`); bot review addressed (least-privilege guidance, pinned installers); pinned to 0.6.2 on 17 Nov (`mcps/marketplace.yaml` regenerated, their tests pass); awaiting maintainers. |
| punkpeye/awesome-mcp-servers | PR https://github.com/punkpeye/awesome-mcp-servers/pull/19631 (Command Line). Glama score badge added 16 Nov; awaiting maintainers. |
| Glama | Listed (https://glama.ai/mcp/servers/mirastacklabs-ai/mirafleet, "Official", author verified). Dockerfile admin: build step `npm install --no-audit --no-fund @mirastacklabs/mirafleet@0.6.1`, CMD `["mirafleet","mcp"]`; build test passed (starts, answers tools/list, no central needed); Glama release `0.1.0` (Glama's own numbering). Auto-Release rebuilds on each GitHub release, but the npm pin must be bumped by hand. **Pending for v0.6.2:** the pin is still 0.6.1 (needs the user's Glama sign-in in the browser used). |
| opencode ecosystem page | PR https://github.com/anomalyco/opencode/pull/64147 (Projects), awaiting review. |
| mcp.so | Free route: issue https://github.com/chatmcp/mcpso/issues/7411 (the web form is a paid $39 listing; not used). |
| Gemini CLI extensions gallery | Auto-indexed: root `gemini-extension.json` + repo topic `gemini-cli-extension`. Check the gallery in about a week. |
| Docker-Sponsored Open Source | Application submitted by the user (commercialization: "No"). Reply by email within about 2 weeks. |
| GitHub MCP registry / VS Code, PulseMCP, LobeHub, Zed | Fed from the official MCP Registry; not visible yet on 15 Nov. Re-check in 1–2 weeks. |
| Cursor Marketplace | Plugin `integrations/cursor/plugin`, listed by `.cursor-plugin/marketplace.json` (PR #34). Publisher application sent 16 Nov at cursor.com/marketplace/publish as MIRASTACK LABS / `@mirastacklabs-ai` (contact rv@mirastacklabs.ai); manual review, no timeline. The form's Owner field is locked to the signed-in account (Individual), so the user emails marketplace-publishing@cursor.com to make the org the owner. Not yet tested in a real Cursor. |
| cursor.directory | Submitted 16 Nov from the user's GitHub login (aarvee11): https://cursor.directory/plugins/mirafleet (MCP server, `/mirafleet` command, brand logo). The first scan failed on their side; re-run asked in https://github.com/cursor/community-plugins/issues/538, after which it was published. On 17 Nov the MCP pin was edited to 0.6.2, which starts a new scan and hides the plugin until it passes. |
| Cline marketplace | Not filed: its form requires confirming Cline installs it from `llms-install.md` and that it is stable. File once a tester confirms (logo: `docs/assets/logo-400.png`). |
| Skipped | appcypher list (archived), modelcontextprotocol/servers (closed to listings), Smithery (publishing shut down), Codex directory (no self-serve). |
| Repo page | Description, homepage (wiki), 19 topics, Discussions on. Social preview `docs/assets/social-preview.png` is uploaded by hand in Settings. |

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
  `release`, allowed action `npm stage publish` only), validated by the v0.6.1 staging on
  16 Nov 2026. Publishing access "disallow bypass 2FA tokens" is set on `mirafleet` and
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
2. Follow the listings above: answer Claude directory review questions, re-check the auto-fed
   directories, file Cline after a tester confirms. P12 (mirastacklabs.ai one-pager, copy in
   RELEASING.md §8) and the announcement are the user's.
3. Full-product test by the user's friends (the first outside testers), using `docs/TESTING.md`
   and the `test-report` issue form; it covers M4 S19, M5 S17 and M7 S21. Also smoke-test the new
   node launcher in real Claude Code (MCP server cwd = project, `--resume` reattach). Then resolve
   the open doc markers below.
4. Next release: follow RELEASING.md "Every release"; renew `HOMEBREW_TAP_TOKEN` before
   15 Dec 2026. The Claude directory's `mirafleet-setup` was left tracking `v0.6.1` for v0.6.2,
   on purpose: changing the tag cancels the publish request waiting for a reviewer. One-off, after `mirafleet-setup` v0.6.1 is live (not before: a new tag cancels
   the waiting publish request): remove `documentationUrl`, `supportUrl`, `privacyPolicyUrl` and
   `termsOfServiceUrl` from `integrations/claude-code/directory/.claude-plugin/plugin.json` and
   update `integrations/directory_plugin_test.go`. They raise the directory's "Unrecognized field"
   warnings ("No action needed"); the listing already saved the links at submission. Keep them in
   the full plugin (`integrations/claude-code/plugin/`), whose future submission reads them.

## Rules learned
- Approving `release` deployments: the user's click by default. On 15 Nov 2026 the user allowed the
  agent to approve them for the rc.3 and v0.6.0 runs, and on 16 Nov for v0.6.1 (gh API, with a
  comment naming that). Ask again for any later run. For v0.6.2 the user approved every job.
- An approval click that shows no green "approved" note did not register; check with
  `gh api repos/mirastacklabs-ai/mirafleet/actions/runs/<id>/approvals` before waiting on it.
- cursor.directory's edit form: set the MCP content by typing, not by a form-fill tool (that wrote
  `[object Object]`); check it parses as JSON before Update Plugin.
- The final "Approve packages" click on npm staged packages is the user's (the agent may select the
  batch and open the dialog); then 2FA.
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
