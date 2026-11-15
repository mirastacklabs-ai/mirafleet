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
| M6 Release and distribution | now lane N1–N3 and late lane L1–L9 done; version files at 0.6.0-rc.2; `m6check` OK; closing review P1s fixed |
| Wiki | live, generated from main (guide + quickstart, MIRASTACK LABS cast) |

## Waiting on the user

| Step | Status |
|---|---|
| DCO sign-off for agent commits (M6.md §2.15) | decided 15 Nov 2026: `-s` as the user, no Claude trailer |
| P1 repo side: rulesets, `release` environment, auto-merge, vulnerability reporting, homebrew-tap repo | done 15 Nov 2026 |
| P1 accounts: Docker Hub token, npm org + trusted publishers, `HOMEBREW_TAP_TOKEN` (RELEASING.md §2) | waiting on user |
| P2 rc tag (`v0.6.0-rc.1` failed `verify`, unbumped; rehearsal is `v0.6.0-rc.2`) → P3 approve rc jobs → P4 tag `v0.6.0` → P5 approve → P6 package visibility → P9–P12 directory, galleries, Docker OSS, one-pager | go-ahead given; approvals are the user's |
| Full-product test: M4 GUI browser check, M5 S17, M7 S21 (Mac + Claude Code + VM with GitLab), quickstart, harness spot checks | after `v0.6.0` |
| A week of own use, then launch posts (drafts on request) | after the test |
