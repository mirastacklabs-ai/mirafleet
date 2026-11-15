# Handoff — 15 Nov 2026 (session paused, Mac going offline)

Read this first, then [TRACKER.md](TRACKER.md) and the plans ([M4.md](M4.md), [M7.md](M7.md),
[M6.md](M6.md)). Every user decision is recorded in those plans; nothing is waiting on the user
except the publish stage (accounts + a yes per outward step).

## Where the code is

| Checkout | Branch | Commit | State |
|---|---|---|---|
| `mirafleet` | `main` (pushed) | `9191b9f` + this doc | M1–M3, M5 done; M4 waves A–B; M6 N1; CI gate workflow. Green on CI. |
| `../mirafleet-m4c` | `m4-wave-c` (pushed) | `808e267` | M4 wave C screens (S7–S12) + partial fix pass + S13/S14 tests. **Not gated.** |
| `../mirafleet-m7` | `m7-wave-a` (pushed) | `f657509` | M7 wave A done; wave B mostly done. **Not gated.** |
| `../mirafleet-m6` | `m6-now` (pushed) | `5a70f55` | M6 N2.1–N2.7 written, partly verified. **Not gated.** |

All agents were stopped mid-step (the user wanted them finished — resume them first). No
containers or waiters are running. Push of a branch triggers `.github/workflows/gate.yml` (full
gate in ~4 min on GitHub); main moves only after CI is green on the branch.

## How to work (standing rules, also in memory)
- Container-only (`scripts/dev.sh`, one target per call); never host go/python/sed -i.
- Opus plans/reviews, Sonnet implements; one combined review per wave; only P0/P1 block.
- Run lanes in parallel, each in its own worktree; agents run only their packages' tests; CI is
  the gate. Never serialise milestones that don't depend on each other.
- Keep it simple: no hardening layers the user didn't ask for; document gaps (enterprise closes them).
- Never force-push (merge main into a branch instead of rebasing).

## Next steps, in order (run the three lanes in parallel)

### M4 (`m4-wave-c`)
1. Re-run the wave C fix pass to completion: it was mid-way through the 12 items listed in the
   last brief (advertise/fingerprint into gui.Config; Shutdown→Close + BaseContext; export
   Sec-Fetch-Site check; setup re-issue + reset-admin hint; no username in failed-login log;
   60 s login/setup route timeouts + DeadlineExceeded→503; nonce set as Server field, no live
   eviction; all screens behind requireAdmin (then tighten TestGUIBinary to require /setup);
   password-change error sentinel; logout core; G-21 token check before Argon2; overview
   denominator + no audit writes from polling; TestGUIDeps transitive list; consolidated
   helpers_test.go + cheap Argon2 test hook). Check `git diff 9f4abae..` for what's done.
2. S14 found a real bug: `templates/audit.html:10` uses `hx-push-url="true"`, which §2.7 doesn't
   allow → remove it (the two lint tests then pass).
3. S13 `TestM4Done`: was running its test when stopped — run it, fix.
4. gofmt: server.go, fleets_test.go, runners_test.go, tokens_test.go, m4_done_test.go.
5. Push → CI green → merge main → S16 UX pass → wave E (S17 docs, S18a/b guide, S19 review).

### M7 (`m7-wave-a`)
Done: S0–S6, S7a, S7b, S8, S9a, S10, S12, S16. Stopped mid-step: S9b (service fs/jobs/placement
— its service tests passed, store+transport left), S13 (CLI + units), S15 (plugin; tests had
passed, fmt left). Not started: S11, S14, S17, S18, S19–S21.
1. Finish S9b, S13, S15.
2. **Glue step (new, before S17/S18):** hostx must implement the runner's optional interfaces in
   `internal/runner/hostops.go` — SessionHooks (SessionDir, SetSessionEnv merged into every
   child), FSExecutor.FS (convert hostx `Files.Do` raw fsops results → transport FSResult + image
   bytes; copy `internal/workspace/fake/devops.go`), JobExecutor (→ hostx Engine), DestroyDeleter,
   HostModer; hostx ReadFile/WriteFile on the same session dir/workdir; host-mode foreground exec
   → `hostx.Engine.Bash` (fills ref/stdout_bytes/stderr_bytes/omitted/truncated, cwd). Without it
   the ops return `unsupported`.
3. S11 central wiring: CredentialSender (SyncCredentials), RunnerConnected, ReportCredentialStatus
   (heartbeat), RunRotation goroutine, FilterRunnersByCredentials/ResolveCredentials in
   placement; set the `mirafleet.credentials` spec label (S8 used a label, not a WorkspaceSpec
   field); fill runner Config.CorporateCA; pass hostx as the executor.
4. Add `workdir` to WorkspaceOpenParams and have `mcp_serve.go` implement ProjectDirer on the Orch.
   S9b must fill the new `api.WorkspaceExecResult` fields (Ref/StdoutBytes/StderrBytes/Omitted).
5. S14 GUI credentials page after M4 wave C is on main (merge main into m7-wave-a).
6. S17 `TestM7Done`, S18 `TestM7DoneIntegration` (install ripgrep in the image; add a gate.yml
   job for `dev.sh integration-host`).
7. Fix list: fsops never emits `<system-reminder>`-style text (mcpserver also scrubs); fsops Glob
   includes hidden files like Claude Code; Latin-1 text shouldn't read as binary; broker rebuild
   on host-set change is done in S8; review whether S9a's GitLab rotation should persist before
   use (a failed store write after GitLab rotates = outage); watch the
   TestWorkspaceExecOutputFitsResultCap load flake.
8. Combined M7 review → fixes → CI → merge.

### M6 (`m6-now`)
N2 written: N2.2 (static pkgtest PASS), N2.6 (tests pass), N2.7 (harnesscheck passes) done.
N2.1 (goreleaser, tools image, release.sh targets), N2.3 (image + Compose; imagetest/labtest
never completed), N2.4 (npm), N2.5 (install scripts) were stopped mid-step.
1. Finish N2.1, N2.3, N2.4, N2.5; run `release.sh snapshot`, pkgtest on the snapshot, imagetest,
   labtest, npmtest, installtest, shellcheck.
2. Wire: nFPM maps the 4 maintainer scripts, central.env 0600 noreplace, runner.env 0644,
   examples → /usr/share/doc/mirafleet/examples/, apk without units; pkgtest matches
   `*<arch>*.deb|rpm|apk`; release.sh targets call the scripts; brew assumes
   `mirafleet_<ver>_<os>_<arch>.tar.gz` (binary at root) — match goreleaser; confirm
   `mirafleet --version` vs `version` for the formula test; compose image tag must be a relgen
   bump target; re-resolve distro images to multi-arch index digests; Go image digest duplicated
   in imagetest/labtest scripts.
3. Harness notes: VS Code ships the deprecated `.vscode/mcp.json` `servers` form (docs prefer
   `.mcp.json` mcpServers — reconsider); Codex uses a skill; none tried in a real harness.
4. Combined review → CI → merge → N3 (ci.yml release jobs, release.yml with Docker Hub + ghcr,
   m6check) → late lane L1–L9 after M4/M7 → publish P1–P12 (each needs the user's yes).

## After that
Full-product test by the user (incl. M5 S17 smoke and the GUI browser check), a week of their
own use, then launch posts (offer drafts).
