# mirafleet — Action tracker

Live status of every remaining step. Plans: [PLAN.md](PLAN.md), [M4.md](M4.md), [M7.md](M7.md),
[M6.md](M6.md). Updated whenever a step changes state. ETAs are rough agent wall-clock times.

Status: `done` · `running` · `next` · `queued` · `waiting on user` · `blocked`.

**15 Nov 2026.** Three lanes run in parallel, each in its own worktree; every branch push runs the
full gate on GitHub (`gate.yml`), main moves only on a green gate.

## Done

| Milestone | Result |
|---|---|
| M1 Transport and actions | done |
| M2 Central core | done 13 Nov 2026 |
| M3 Workspaces | done 13 Nov 2026 (`34dec01`) |
| M5 Orchestrator | done 14 Nov 2026 (`4e0649b`); its smoke test (S17) runs in the full-product test |
| M4 Admin GUI | done 15 Nov 2026 (`fce2a18`), pending the user's browser check (S19, in the full-product test). Waves A–E, final security review: 0 P0/P1; G-01…G-39 closed, G-40…G-43 (P3) open |

## M7 — Developer workspaces (`m7-wave-a`, [M7.md](M7.md))

| Step | Status |
|---|---|
| S0–S16 incl. glue (hostx → runner), S11 wiring, S14 GUI credentials page, session reattach, fsops/MCP control-tag fixes | done |
| Central dispatch for fs/jobs/output/credentials + `TestM7Done` (S17) | running |
| `TestM7DoneIntegration` + `integration-host` gate job (S18) | running |
| Combined review → fixes → merge; docs S19/S20 (M7.md §3.4 layout now `sessions/<slug>`, `work/<name>`) | next |

## M6 — Release and distribution (`m6-now`, [M6.md](M6.md))

| Step | Status |
|---|---|
| N1, N2 (reviewed; P1 cosign-bundle fix done), N3.1–N3.3 | done |
| N3 review P1s (release `verify` required checks, cosign bundle signing) + PR #1 CI fixes (brew style, Windows installtest) | running |
| Merge → late lane L1–L9 (GUI/host-mode packaging, version skew, README/QUICKSTART/THREAT-MODEL, guide, bump to 0.6.0, closing review) | next |
| Publish P1–P12 | **stop: each step needs the user's yes** |

Draft PR #1 exists only to run `ci.yml`/`orchestrator.yml` on Linux/macOS/Windows; it is not merged
from GitHub.

## User

| Step | Status |
|---|---|
| Accounts for publishing (repo rules, `release` environment, homebrew-tap repo + token, npm org, Docker Hub token for `mirastack`) — see RELEASING.md (lands with M6) | before publish |
| DCO on our own commits: the `dco` check fails on agent commits (no `Signed-off-by`); decide before the main ruleset requires PRs | before publish |
| Docker-Sponsored Open Source application (text in RELEASING.md) | after `v0.6.0` |
| Full-product test, incl. M5 S17 smoke and the M4 GUI browser check | after `v0.6.0` |
| A week of own use, then launch posts (drafts on request) | after the test |
