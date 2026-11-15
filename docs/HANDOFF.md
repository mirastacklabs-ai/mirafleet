# Handoff — 15 Nov 2026 (release rehearsal `v0.6.0-rc.2`: all done except npm)

Read this first, then [TRACKER.md](TRACKER.md), [RELEASING.md](RELEASING.md) and the plans
([M4.md](M4.md), [M6.md](M6.md), [M7.md](M7.md)). Every decision is recorded in those plans.

## Where things are

| What | State |
|---|---|
| `main` | M1–M7 built and merged; version files at `0.6.0-rc.2`; all CI green. Protected: changes only by PR. |
| Tag `v0.6.0-rc.1` | dead: tagged before the version bump, failed `verify`, nothing published. Left in place (tags are never moved). |
| Tag `v0.6.0-rc.2` | release run https://github.com/mirastacklabs-ai/mirafleet/actions/runs/43718452505 |
| GitHub release `v0.6.0-rc.2` | public, marked pre-release. Checked: cosign signature on `checksums.txt`, all 24 checksums, provenance (`gh attestation verify`), `install.sh --verify-signature` in a fresh container, binary prints `0.6.0-rc.2`. |
| Images | `docker.io/mirastack/mirafleet:0.6.0-rc.2` and `ghcr.io/mirastacklabs-ai/mirafleet:0.6.0-rc.2`: same digest `sha256:4da49d3d…f872`, cosign verified on both, image runs. One image for central and runner (subcommand picks the role). |
| npm | **not published.** The `npm` job failed `ENEEDAUTH`: no npm auth yet. |
| Wiki | https://github.com/mirastacklabs-ai/mirafleet/wiki, generated from `docs/` at `3eb3c11` (`relgen wiki`; re-run and push after doc changes). |

## Settings in place (15 Nov 2026)
- **GitHub repo** `mirastacklabs-ai/mirafleet`: `release` environment (required reviewer aarvee11,
  `v*` tags only); ruleset `main` (PR, squash only, required checks `gate`, `licenses`, `lint`,
  `dco`, `windows-latest`, `macos-latest`); ruleset `release-tags` (only admins on `v*`);
  auto-merge and delete-branch on; private vulnerability reporting on.
- **`release` environment:** variable `DOCKERHUB_USERNAME=mirastack`, secret `DOCKERHUB_TOKEN`
  (Docker Hub organization access token, Image Push on `mirastack/mirafleet`).
- **Docker Hub:** org `mirastack`, public repo `mirafleet`.
- **Homebrew tap:** `mirastacklabs-ai/homebrew-tap` created with a README.
- **DCO:** every commit uses `git commit -s` as the user (aarvee11); no Claude co-author trailer.

## Next steps, in order
1. **User (in progress, in another Claude session):** npm org `mirastacklabs`; a granular token
   with "Read and write (stage only)" and no 2FA bypass, stored with
   `gh secret set NPM_TOKEN --env release --repo mirastacklabs-ai/mirafleet`.
   Trusted publishing cannot be set up before a package exists, so the first publish uses this token.
2. **Agent:** the rc.2 tag's `npm` job still has the old `npm publish`; it is not re-run. After the
   staged-publish PR (version 0.6.0-rc.3) merges, tag `v0.6.0-rc.3` (user's yes) and the user
   approves the `release` deployments. The `npm` job only stages: the **user** approves the 7 staged
   versions at https://www.npmjs.com/settings/mirastacklabs/staged-packages (2FA). Then check
   `npx -y @mirastacklabs/mirafleet@0.6.0-rc.3 version` in a container.
3. **User:** on npmjs.com add a trusted publisher to each of the 7 `@mirastacklabs/...` packages
   (repo `mirastacklabs-ai/mirafleet`, workflow `release.yml`, environment `release`, allowed action
   `npm stage publish` only); delete the npm token;
   `gh secret delete NPM_TOKEN --env release --repo mirastacklabs-ai/mirafleet`; set publishing
   access to "Require 2FA and disallow tokens".
4. **User:** `HOMEBREW_TAP_TOKEN` (fine-grained PAT, contents read/write on the tap only, with an
   expiry) on the `release` environment, before `v0.6.0`.
5. **Agent:** (bump PR done, version files at 0.6.0) PR `relgen bump 0.6.0` (branch, `git commit -s`, `gh pr merge --auto --squash
   --delete-branch`), then tag `v0.6.0` on the merge commit (P4). The user approves each job (P5):
   `draft`, `publish-release`, `image`, `npm`, `brew`, `mcp-registry`. Agent verifies after each
   (RELEASING.md §3 step 13).
6. Then P6 (Docker Hub description, ghcr package public), P9–P12, and the full-product test
   (M4 S19 browser check, M5 S17, M7 S21 smoke on the user's Mac + a VM with their GitLab).

## Rules learned this session
- Approving a `release` deployment is always the user's click; the agent never approves it.
- The version files must match the tag exactly, rc included (RELEASING.md §3 step 3).
- Verification downloads go under the repo's git-ignored `dist/`, not the scratchpad (colima only
  shares the home folder with containers). Delete them afterwards.
- Talk to the user in short, plain sentences; give exact commands and clicks.

## Open doc markers (resolve at the points named)
- `docs/guide/README.md`: wiki link (once published as part of the release).
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
