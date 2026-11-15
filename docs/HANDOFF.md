# Handoff — 15 Nov 2026 (build complete; release rehearsal in progress)

Read this first, then [TRACKER.md](TRACKER.md), [RELEASING.md](RELEASING.md) and the plans
([M4.md](M4.md), [M6.md](M6.md), [M7.md](M7.md)). Every decision is recorded in those plans.

## Where the code is

| Checkout | Branch | Commit | State |
|---|---|---|---|
| `mirafleet` | `main` (pushed) | squash of PR `m6/bump-rc2` | M1–M7 built and merged; version files at `0.6.0-rc.2`; all CI green. |

Wiki: https://github.com/mirastacklabs-ai/mirafleet/wiki, generated from `docs/` at `3eb3c11`
(`relgen wiki`; re-run and push after doc changes).

## Decisions taken 15 Nov 2026
- **DCO:** every commit (agents included) uses `git commit -s` as the user (aarvee11); no Claude
  co-author trailer.
- **P1, repo side, done:** `release` environment (required reviewer aarvee11, `v*` tags only);
  ruleset `main` (PR, squash only, required checks `gate`, `licenses`, `lint`, `dco`,
  `windows-latest`, `macos-latest`; no force-push or delete); ruleset `release-tags` (only admins
  on `v*`); auto-merge and delete-branch on; private vulnerability reporting on;
  `mirastacklabs-ai/homebrew-tap` created with a README.
- **Release:** the user said go ahead. `v0.6.0-rc.1` was tagged before the rc bump and failed
  `verify` (`relgen check`); nothing was built or published. The tag stays (tags are never moved);
  the rehearsal uses `v0.6.0-rc.2`.

## What is waiting on the user
1. **Account secrets on the `release` environment** (RELEASING.md §2): variable `DOCKERHUB_USERNAME`
   and secret `DOCKERHUB_TOKEN` before approving `image`; npm org `mirastacklabs` with trusted
   publishers (or a first-publish `NPM_TOKEN`) before approving `npm`; `HOMEBREW_TAP_TOKEN` before
   `v0.6.0`.
2. **Approvals** in the Actions page: P3 for the rc (`draft`, then `publish-release`, `image`, `npm`),
   P5 for `v0.6.0`.
3. **Full-product test** after `v0.6.0`: M4 GUI browser check (M4 S19), M5 S17 smoke, M7 S21 smoke
   (Mac + Claude Code + a VM with your GitLab), quickstart, harness spot checks.

## Next agent steps
- After the rc is verified (RELEASING.md §3 step 9): PR `relgen bump 0.6.0`, then tag `v0.6.0` (P4).

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
