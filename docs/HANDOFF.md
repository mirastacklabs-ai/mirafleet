# Handoff — 15 Nov 2026 (build complete; stopped at the M6 publish stage)

Read this first, then [TRACKER.md](TRACKER.md), [RELEASING.md](RELEASING.md) and the plans
([M4.md](M4.md), [M6.md](M6.md), [M7.md](M7.md)). Every decision is recorded in those plans.

## Where the code is

| Checkout | Branch | Commit | State |
|---|---|---|---|
| `mirafleet` | `main` (pushed) | `3eb3c11` + this doc | M1–M7 built and merged; version files at 0.6.0; full gate, `ci.yml` (dist, e2e-systemd) and `orchestrator.yml` (macOS, Windows) green. No other branches or worktrees. |

Wiki: https://github.com/mirastacklabs-ai/mirafleet/wiki, generated from `docs/` at `3eb3c11`
(`relgen wiki`; re-run and push after doc changes).

## What is waiting on the user
1. **DCO sign-off for agent commits.** M6.md §2.15 says "Agents commit with `-s`" (your identity in
   `Signed-off-by:`); an automated check stopped the agent from doing that without your explicit
   confirmation, so no agent commit is signed off. Decide before the main ruleset requires PRs: yes
   (agents sign off as you) or no (change §2.15 and `dco.yml` to external contributors only).
2. **Publish P1–P12** (M6.md §8, RELEASING.md): each outward step needs your yes; P1 (accounts,
   `release` environment, tokens), P3/P5 (environment approvals) are yours in the GitHub UI.
3. **Full-product test** after `v0.6.0`: M4 GUI browser check (M4 S19), M5 S17 smoke, M7 S21 smoke
   (Mac + Claude Code + a VM with your GitLab), quickstart, harness spot checks.

## Open doc markers (resolve at the points named)
- `docs/guide/README.md`: wiki link (once published as part of the release).
- `docs/guide/12-why-mirafleet.md`: re-research the other tools' distribution rows; re-read after S21.
- `docs/guide/04-flows.md` flows 30–36 and `05-security.md` §5.14 rows 1, 2, 6, 8, 11, 12: confirm in
  the S21 smoke.

## Standing rules (also in memory)
- Container-only (`scripts/dev.sh`, `scripts/release.sh`); no host go/python/sed -i.
- Design is the contract: docs are written from the plans in parallel with code; any deviation is
  recorded in the plan in the same commit.
- Before main moves: the branch gate green **and** a draft PR so `ci.yml`/`orchestrator.yml` run on
  macOS/Windows. Fast-forward only; merge main into branches; never force-push.
- Local `release.sh snapshot` on colima needs `mod_timestamp` commented out temporarily (virtiofs
  rejects `chtimes`); never commit that. CI doesn't need it.
- Docs use the MIRASTACK LABS cast (STYLE.md): aarvee, raj, durga, kalyan, sid; they/them.
