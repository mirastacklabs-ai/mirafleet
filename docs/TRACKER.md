# mirafleet — Action tracker

Live status of every remaining step. Plans: [PLAN.md](PLAN.md), [M4.md](M4.md), [M7.md](M7.md),
[M6.md](M6.md). Release runbook: [RELEASING.md](RELEASING.md).

Status: `done` · `running` · `next` · `waiting on user`.

**15 Nov 2026: build complete, stopped at the publish stage.** main `3eb3c11`, all CI green
(Linux gate, dist, e2e-systemd, macOS, Windows). Wiki generated from main.

## Done

| Milestone | Result |
|---|---|
| M1 Transport and actions | done |
| M2 Central core | done 13 Nov 2026 |
| M3 Workspaces | done 13 Nov 2026 (`34dec01`) |
| M5 Orchestrator | done 14 Nov 2026 (`4e0649b`); S17 smoke in the full-product test |
| M4 Admin GUI | built 15 Nov 2026; final review 0 P0/P1; G-01…G-43 closed or documented. Browser check (S19) in the full-product test |
| M7 Developer workspaces | built 15 Nov 2026; combined review (3 reviewers) P1s fixed; hardening H-01…H-21 done. User smoke (S21) in the full-product test |
| M6 Release and distribution | now lane N1–N3 and late lane L1–L9 done; version files at 0.6.0; `m6check` OK; closing review P1s fixed |
| Wiki | live, generated from main (guide + quickstart, MIRASTACK LABS cast) |

## Waiting on the user

| Step | Status |
|---|---|
| DCO sign-off for agent commits (M6.md §2.15): yes (agents sign off as you) or no (limit `dco` to external PRs) | waiting on user |
| P1 Accounts, repo rules, `release` environment, homebrew-tap repo + token, npm org, Docker Hub token (RELEASING.md §9) | waiting on user |
| P2 tag `v0.6.0-rc.1` → P3 approve rc jobs → P4 tag `v0.6.0` → P5 approve → P6 package visibility → P9–P12 directory, galleries, Docker OSS, one-pager | each needs the user's yes |
| Full-product test: M4 GUI browser check, M5 S17, M7 S21 (Mac + Claude Code + VM with GitLab), quickstart, harness spot checks | after `v0.6.0` |
| A week of own use, then launch posts (drafts on request) | after the test |
