# mirafleet — Action tracker

Live status of every remaining step. Plans: [PLAN.md](PLAN.md), [M4.md](M4.md), [M7.md](M7.md),
[M6.md](M6.md). Updated whenever a step changes state. ETAs are rough agent wall-clock times.

Status: `done` · `running` · `next` · `queued` · `waiting on user` · `blocked`.

**Paused 14 Nov 2026 (offline).** Local commits, not pushed: main `ce68f76` (M4 wave B) +
`299f27e` (test flake fix), gate to re-run; branch `m4-wave-c` (`../mirafleet-m4c`) WIP S7/S8/S12;
branch `m6-now` (`../mirafleet-m6`) N1 done + partial fix pass. Resume: gate main → push → finish
wave C and the M6 fix pass in their worktrees.

Every wave ends the same way: narrow read-only Opus reviews → fix P0/P1 (P2/P3 batched per
milestone) → full gate (`scripts/dev.sh fmt build vet test cross licenses integration`) → push →
Windows/macOS CI green → clean up.

## Done

| Milestone | Result |
|---|---|
| M1 Transport and actions | done |
| M2 Central core | done 13 Nov 2026 |
| M3 Workspaces | done 13 Nov 2026 (`34dec01`) |
| M5 Orchestrator | done 14 Nov 2026 (`4e0649b`); its smoke test (S17) runs in the full-product test |
| M4 wave A (S0–S3) | done 14 Nov 2026: reviewed, 2 P1s fixed, full gate green |
| M4 / M6 plans | approved and pushed 14 Nov 2026 (`5a90875`) |

## M4 — Admin GUI ([M4.md](M4.md) §5)

| Wave | Step | What | Owner | Status | ETA |
|---|---|---|---|---|---|
| A | S0–S3 | Assets, store + migration 0003, Argon2id, design system; review + fixes | Sonnet + Opus | done | — |
| B | S4 | Service and API: GUI methods, audit query, guard toggle, admin methods | Sonnet | built, in review | — |
| B | S5 | GUI core: server, listener, TLS cert, Host allowlist, middleware, render, routes | Sonnet | built, in review | — |
| B | S6 | Sessions (SQLite), login limiter, CSRF | Sonnet | built, in review | — |
| B | — | Review, fixes, gate, push | Opus + Sonnet | running | 1 h |
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
| Plan | `docs/M7.md` (lean): host mode (runner = one OS user on a VM), credentials store on central assigned to runners/fleets, full Claude Code tool parity on the VM, background jobs, full output | Opus | approved 14 Nov 2026 | — |
| A–E | 5 waves, 25 step slots (M7.md §steps) | Sonnet + reviewers | queued (after M4) | ~11–12 h incl. reviews |

## M6 — Release and distribution ([M6.md](M6.md))

| Wave | What | Owner | Status | ETA |
|---|---|---|---|---|
| — | Update M6.md with all decisions (Docker Hub `mirastack`, Compose, no telemetry, DCO, version skew, backup before migrate, PR + auto-merge, wiki docs, 6 harness configs, M7 changes) and split into a "now" lane (parallel with M4, own worktree) and a "late" lane | Opus | running | 20 min |
| Now N1 | N1.1 relgen, N1.2 LF/.gitattributes (F-45), N1.3 marketplace.json + server.json, N1.6 protover, N1.7 buildinfo version, N1.9 SECURITY/CONTRIBUTING/CoC/templates/dependabot | Sonnet | built, in review | — |
| Now N1 | N1.4 F-47/F-48 audit fixes, N1.5 backup before migrate | Sonnet | running | 45 min |
| Now N1 | N1.8 release.sh + extra licences | Sonnet | next | 45 min |
| Now N2 | goreleaser, packages + systemd, OCI image + Compose, npm, install.sh/ps1, relgen brew/serverjson/wiki/harness, 6 harness configs | Sonnet + review | queued | ~3 h |
| Now N3 | ci.yml, release.yml (Docker Hub + ghcr), m6check | Sonnet + review | queued | ~1.5 h |
| Late L1–L9 | GUI/host-mode packaging, version-skew wiring, README/QUICKSTART/THREAT-MODEL, guide, bump to 0.6.0, closing review | Sonnet + review | after M4/M7 | ~4.5 h |
| Publish P1–P12 | accounts, rc.1 rehearsal, v0.6.0, Docker Hub/ghcr, wiki, plugin directory, galleries, Docker OSS, one-pager; **each step needs the user's yes** | user + me | stop point | ~2.5 h |

## User

| Step | Status |
|---|---|
| Set up accounts for M6 wave F (repo rules, `release` environment, homebrew-tap repo + token, npm org, Docker Hub access token for the existing `mirastack` org) | before M6 wave F |
| Apply to the Docker-Sponsored Open Source programme (application text prepared by me) | after `v0.6.0` |
| Full-product test, including M5 S17 and the GUI browser check | after `v0.6.0` |
| A week of own use cases, then LinkedIn/social posts (drafts on request) | after the test |
