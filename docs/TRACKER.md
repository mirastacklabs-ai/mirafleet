# mirafleet — Action tracker

Live status of every remaining step. Plans: [PLAN.md](PLAN.md), [M4.md](M4.md), [M7.md](M7.md),
[M6.md](M6.md). Updated whenever a step changes state. ETAs are rough agent wall-clock times.

Status: `done` · `running` · `next` · `queued` · `waiting on user` · `blocked`.

Every wave ends the same way: narrow read-only Opus reviews → fix P0/P1 (P2/P3 batched per
milestone) → full gate (`scripts/dev.sh fmt build vet test cross licenses integration`) → push →
Windows/macOS CI green → clean up.

## Done

| Milestone | Result |
|---|---|
| M1 Transport and actions | done |
| M2 Central core | done 13 Nov 2026 |
| M3 Workspaces | done 13 Nov 2026 (`34dec01`) |
| M5 Orchestrator | built, pushed through `4e0649b`; its smoke test (S17) runs in the full-product test |
| M4 / M6 plans | approved and pushed 14 Nov 2026 (`5a90875`) |

## M4 — Admin GUI ([M4.md](M4.md) §5)

| Wave | Step | What | Owner | Status | ETA |
|---|---|---|---|---|---|
| A | S0 | Vendored htmx + fonts + licences, `assets.go`, `TestVendoredAssets` | Sonnet | running | 15 min |
| A | S1 | Migration 0003, GUI admin/session/audit store methods + storetest | Sonnet | running | 45 min |
| A | S2 | Argon2id passwords + fuzz, `auth.GUIAdmin` | Sonnet | done | — |
| A | S3 | Design system: CSS, JS, layout, partials, styleguide | Sonnet | done | — |
| A | — | Review, fixes, gate, push | Opus + Sonnet | next | 45 min |
| B | S4 | Service and API: GUI methods, audit query, guard toggle, admin methods | Sonnet | queued | 105 min |
| B | S5 | GUI core: server, listener, TLS cert, Host allowlist, middleware, render, routes | Sonnet | queued | 105 min |
| B | S6 | Sessions (SQLite), login limiter, CSRF | Sonnet | queued | 60 min |
| C | S7–S11 | Screens: auth/account, overview/fleets, runners/tokens, operators/workspaces, audit/export | Sonnet ×5 | queued | 75–120 min |
| C | S12 | `central run` GUI flags, `central gui reset-admin`, `fleet set -confirm`, audit filters | Sonnet | queued | 75 min |
| D | S13 | `TestM4Done` | Sonnet | queued | 135 min |
| D | S14 | Security suite (every route, CSRF, DB-copy test, template lint) | Sonnet | queued | 90 min |
| D | S15 | Built-binary smoke test | Sonnet | queued | 75 min |
| D | S16 | UX/accessibility pass, `gui-shots` screenshots | Sonnet | queued | 90 min |
| E | S17 | ARCHITECTURE, PLAN, BRAND updates | Sonnet | queued | 30 min |
| E | S18a/b | Developer guide | Sonnet ×2 | queued | 60 min |
| E | S19 | Final security review, fixes; browser check moves to the full-product test | Opus + Sonnet | queued | 45 min |

## M7 — Developer workspaces ([M7.md](M7.md))

| Step | What | Owner | Status | ETA |
|---|---|---|---|---|
| Plan | `docs/M7.md`: credentials, SSH/TCP egress, guarded-fleet egress, persistent storage, background jobs, output paging, file tools, **Docker Engine executor** | Opus | running | 40 min |
| Decide | Open questions (incl. Docker Hub naming) | user | waiting on plan | — |
| Build | Waves from M7.md (filled in once approved) | Sonnet + reviewers | queued | from plan |

## M6 — Release and distribution ([M6.md](M6.md))

| Wave | What | Owner | Status | ETA |
|---|---|---|---|---|
| — | Update M6.md for M7: Docker Hub image, Compose install path, Docker-Sponsored OSS option, M7 packaging/quickstart/CI | Opus | queued (after M7 decisions) | 15 min |
| A | R0–R5 | Sonnet + review | queued | ~2 h |
| B | R6–R11 | Sonnet + review | queued | ~2.5 h |
| C | R12–R14 | Sonnet + review | queued | ~2 h |
| D | R15–R17a/b | Sonnet + review | queued | ~2 h |
| E | R18, closing review, test lab run | Opus + Sonnet | queued | ~2 h |
| F | P1–P9 publish (rc.1 rehearsal, then v0.6.0); accounts set up by the user; **each step needs the user's yes** | user + me | stop point | ~2 h |

## User

| Step | Status |
|---|---|
| Set up accounts for M6 wave F (repo rules, `release` environment, homebrew-tap repo + token, npm org, Docker Hub) | before M6 wave F |
| Full-product test, including M5 S17 and the GUI browser check | after `v0.6.0` |
| A week of own use cases, then LinkedIn/social posts (drafts on request) | after the test |
