# 11. Contributing

This chapter explains how we work: the ground rules and the reasons behind them, the
plan/implement/review loop, how milestones are written down, the rule that the guide is updated
with the code, a checklist for reviewing a change, and some real first tasks.

## What you will learn

- Each ground rule from [CLAUDE.md](../../CLAUDE.md), with the reason it exists.
- The workflow for a change: plan, implement, read-only review, with P0 to P3 severities.
- How milestone plans (`docs/M*.md`) work and how to read one.
- The documentation rule, and how to add a diagram.
- A review checklist and a list of small, real tasks to start with.

## 11.1 The ground rules, with reasons

These come from [CLAUDE.md](../../CLAUDE.md) **as of 13 Nov 2026**. If the two ever disagree,
CLAUDE.md wins and this table should be fixed. Read the file itself too; it is short and
binding. Status today: M1 and M2 are done; next come M3 (workspaces), then M5 (MCP server),
M4 (GUI) and M6 (release). A rule without a reason gets broken the first time it is inconvenient, so here is the reason for
each.

| Rule | Why |
|---|---|
| **One static Go binary** (`mirafleet central \| runner \| mcp`); `CGO_ENABLED=0` must keep building. | A static binary copies to any Linux host and runs, with no libraries to match. It makes packaging, containers and the "download and run" install easy. A dependency that needs a C compiler (cgo) would break this, which is why SQLite is the pure-Go `modernc.org/sqlite` and not the C one. |
| **Platforms:** central and runner are **Linux only**; `mirafleet mcp` runs on Linux and macOS (it builds on Windows and runs there from M5). The binary must still build for all six targets. OS-specific code goes behind build tags and **fails closed** elsewhere. | Servers are Linux; laptops are not. Features such as the admin socket peer check or process-group kill have no honest Windows equivalent. If a security check cannot run on a platform, silently skipping it would be a hole, so the code returns an error instead (see [chapter 8](08-code-tour.md#87-build-tags-and-why)). |
| **Central's store is SQLite** behind the `Store` interface. | SQLite needs no separate server, which keeps the open-source edition easy to run. The interface keeps the door open for other stores without touching `central`. |
| **Keep dependencies to the list in `docs/PLAN.md`; ask before adding others.** | Every dependency is code we did not write but must trust, patch and licence-check. A short list keeps the supply-chain risk small and the binary lean. |
| **Permissive licences only** (Apache-2.0, MIT, BSD-2-Clause, BSD-3-Clause, ISC, 0BSD) for all dependencies, including transitive ones and bundled assets. Never GPL, LGPL, AGPL, MPL, SSPL, BUSL or other copyleft/source-available. Fonts may be SIL OFL-1.1. | mirafleet is Apache-2.0 and meant to be used inside companies. Copyleft or source-available terms could force those users to publish their own code or forbid their use. The licence gate is `scripts/dev.sh licenses` ([chapter 9](09-dev-setup-and-testing.md#99-the-licence-gate)). |
| **Follow [BRAND.md](../BRAND.md)** for GUI, CLI colours, docs and assets. | Consistent look, and the colours carry meaning (teal is data, orange is emphasis only, status colours are runner states). |
| **Containers:** the Docker ecosystem is supported (Docker CLI, Docker Engine, Compose, `docker.io` images), never a hard dependency on Docker's commercial products. Runner workspaces (M3) use containerd through its Go client. | We want open tooling anyone can run for free. Docker Engine runs on containerd, so one client covers Docker hosts and plain containerd hosts. |
| **The runner-side actions guard is the security boundary:** argv only, no shell, regex-validated params. | Central and the agent are not trusted to decide what is safe to run. The only thing that cannot be talked around is the allowlist on the machine that executes. Every change near `internal/actions` gets the strictest review. |
| **Harness-agnostic core.** The MCP server must work with no hooks. Harness-specific code lives only in `integrations/<harness>/`. | Claude Code is first, but opencode, Kilo Code and others must be able to use mirafleet. Hooks are a convenience, never a requirement, so a plain MCP client keeps all safety guarantees. |
| **OSS stays lean.** Enterprise features (RBAC, SSO, HA, ...) are out of scope; leave interface seams only. | The open-source edition has a clear job. Seams such as `PolicyEngine`, `AuthProvider` and `Store` let enterprise builds replace pieces without forking. Do not build the enterprise piece "just in case". |
| **Containers only for tooling.** No host `go`, brew, npm or pip for this repo; everything via `scripts/dev.sh`. | Same compiler for everyone, a clean laptop, and a habit of not running install scripts. See [chapter 9](09-dev-setup-and-testing.md#91-the-container-only-rule). |
| **Testing:** no real hosts, ports or data; ephemeral ports, `t.TempDir()`, keys generated at test time. containerd tests use the tags `linux && integration`. | Tests must be safe to run anywhere, at any time, in parallel, and must never leak a secret or touch a production-like thing. |

Working habits we also expect: **stage files by name** (`git add
path/to/file`, never `git add .` or `-A`, so you cannot commit a stray key or database), **never
put secrets in committed files** (keep them in a git-ignored `.env.local`; `.gitignore` also
excludes `*.db`, `*.db-*` and `/dist/`), and **confirm before destructive or outward-facing
actions** such as a force-push or deleting data.

## 11.2 The workflow: plan, implement, review

A change goes through three stages. For a big change they are separate; for a one-line fix they
are quick but still happen.

### 1. Plan

Write down *what* and *why* before code. For anything beyond a small fix, the plan says:

- the goal, and what "done" looks like (a test you can run);
- the design decisions and what you rejected;
- the files you will touch;
- the checks a reviewer should focus on (see the "qa-reviewer focus per step" list in
  [M2.md](../M2.md) section 4 for the style).

Find the **root cause** before fixing a bug. If your first fix does not fix it, say so and
record the dead end in the plan, so nobody tries it a third time.

### 2. Implement

Small steps, each leaving `scripts/dev.sh fmt`, `build`, `vet` and `test` green (and `cross` if
you touched platform code, and `licenses` if you touched `go.mod`). Write the test with the
code. Read a file before you edit it. When working with an AI coding agent, give it one
well-defined step at a time and repeat the safety constraints (for example "do not touch the
real database") in every request; instructions do not carry across tasks.

### 3. Review (read-only)

A second pair of eyes that **cannot change the code** reads the diff and reports findings. This
is a read-only reviewer: a person or an AI agent that only comments. The maintainers use a
read-only `qa-reviewer` agent for this; a human reviewer does exactly the same job. The point of "read-only" is independence: a reviewer who can also fix things starts
arguing with the code instead of reporting on it.

Findings carry a severity. The project keeps no separate definition file; this table is the working convention used in the M2 reviews:

| Severity | Meaning | Rule |
|---|---|---|
| **P0** | A security hole, data loss or corruption, or a break of a documented guarantee (for example an admin method reachable from `api@`). | Stop. Fix before anything else. |
| **P1** | A real bug or missing test of an important behaviour; a defence that does not work as documented. | Fix before the change is pushed. |
| **P2** | Weakness or robustness gap that is unlikely or low impact (a race on shutdown, a log that could flood). | Fix in the milestone's hardening pass; list it in the plan. |
| **P3** | Polish: naming, small clarity, a hardening idea. | Fix opportunistically; list it. |

In the M2 plan this was the rule from wave D onwards: "only P0/P1 are fixed before a push; P2/P3
are listed (section 4b) and fixed together at the end". Section 4b of [M2.md](../M2.md) is a
good example of how to record them (id, severity, one-sentence description, status).

After the review: fix, then run the full gate again, then commit.

### Commits

- Stage files **by name**.
- Look at `git log --oneline` for the style: a prefix with the milestone and step, then what
  changed (for example `M2 S10a + S10b: central admin CLI, runner join, mcp login/api`).
- One logical change per commit; do not mix a refactor with a feature.
- Never put a secret, a key or a real hostname in a commit, including in tests and docs
  (`example.com` only). Never commit database files.
- Do not force-push shared branches. Ask first.

## 11.3 Milestones and the `docs/M*.md` plans

[PLAN.md](../PLAN.md) lists milestones M1 to M6 and the **build order** (M1, M2, M3, M5, M4,
M6; the numbers are labels, not the sequence). Each milestone has a "done when" sentence that a
test can check. For M2 it is literally the test `TestM2Done`.

For a milestone with real design choices there is a detailed file, `docs/M2.md`, `docs/M3.md`.
Reading [M2.md](../M2.md) shows the shape:

1. **Design decisions** (section 1): store, tokens, auth, protocol, admin socket, revoke, audit.
   Each with the reason.
2. **Layout** (section 2): which packages and files are new or changed.
3. **Steps** (section 3): a table of small steps in waves. Steps marked parallel touch disjoint
   files so they can be done at the same time. Each step lists what it depends on.
4. **Reviewer focus per step** (section 4): the exact things a reviewer must try to break.
5. **Binding rules from reviews** (section 4a) and **deferred findings** (section 4b).
6. **Dependencies and licences** (section 5) and **decisions with dates** (section 6).

Your job when starting a milestone is to write that file *first* and get it agreed. [M3.md](../M3.md)
is an example of an early-stage plan: only decisions so far, "detailed plan to follow".

Rules of the road:

- `PLAN.md` says what the milestone delivers; the `M*.md` file says how. If they disagree, fix
  the plan, do not leave both.
- Mark status honestly, with a date ("M1 and M2 are done (13 Nov 2026)"), and change it when the
  facts change.
- A decision that changes the design goes into the file with a date and a reason. Do not edit
  history to look as if you always knew.

## 11.4 The documentation rule

**Every milestone updates this guide.** This is a line in PLAN.md and it is part of "done". A
change that alters behaviour is not finished until the matching chapter says the same thing.

Practical rules (the full list is in [STYLE.md](STYLE.md)):

1. Facts come from the code. Name files and functions (`internal/service/exec.go`,
   `service.RunAction`), never line numbers. If the guide and the code disagree, the code is
   right: fix the guide.
2. Say "planned" for anything not built yet, and link the milestone.
3. Use the running example: Acme, `central.example.com:2222`, fleets `dev` and `prod`, runners
   `build-1` and `db-1`, operators `alice` and `bob`. Never real hostnames, people or secrets.
4. Explain a term the first time it appears and link [glossary.md](glossary.md). Add new terms
   there.
5. Each chapter starts with "What you will learn" and ends with "Check your understanding"
   (3 to 5 questions, answers in a collapsed `<details>` block).
6. Teach the *why*: for every defence, say what would go wrong without it, with a concrete
   example.

**Which chapters does a change touch?** Roughly:

| You changed | Update |
|---|---|
| a wire method, message or limit | [7 Protocols](07-protocols.md), [10 Operations](10-operations.md) (API table) |
| a table or migration | [6 Data model](06-data-model.md) |
| a defence or the threat picture | [5 Security](05-security.md) |
| a flow (join, register, run, revoke) | [4 Flows](04-flows.md), plus the matching SVG |
| a package, file or build tag | [8 Code tour](08-code-tour.md) |
| a CLI command or flag | [10 Operations](10-operations.md) (reference), the usage text in `cmd/mirafleet/main.go`, README quickstart |
| the dev setup or tests | [9 Dev setup and testing](09-dev-setup-and-testing.md) |
| the rules | CLAUDE.md and this chapter |

### Diagrams

Two kinds, both described in STYLE.md:

- **Mermaid** in the Markdown for flowcharts, sequences, state and ER diagrams. GitHub renders
  it. Under about 25 nodes per diagram, **every arrow labelled**.
- **Animated SVG** in `docs/guide/diagrams/<name>.svg` for the key flows, embedded as
  `![alt text](diagrams/<name>.svg)`. The rules: self-contained (no scripts, no external fonts,
  images or links), CSS animation only; light and dark colours through CSS variables with a
  `prefers-color-scheme` block, using the tokens in [BRAND.md](../BRAND.md); a
  `prefers-reduced-motion: reduce` block that stops motion and shows the final state; a loop of
  6 to 12 seconds with numbered steps in the drawing that match a numbered list directly under
  the image; JetBrains Mono for labels and Oswald for titles; readable at 800 px wide; a
  one-line caption.

The shared diagram names are `big-picture.svg`, `runner-join.svg`, `operator-register.svg`,
`action-run.svg`, `revoke.svg` and `framing.svg`. Reuse them; add new names only for new flows.
View your SVG in both a light and a dark browser theme, and with "reduce motion" on, before you
commit.

## 11.5 Review checklist for a pull request

Copy this into your PR description and tick it honestly. Reviewers use the same list.

**Security**

- [ ] Does any new input reach a command line, a file path, SQL or a log line? It is validated
      (regex, length, type) *before* use, and SQL is parameterised.
- [ ] Is any admin or lifecycle capability reachable from `api@mirafleet`, `join@`, `register@`
      or MCP? It must not be. Method sets stay disjoint and tests prove it.
- [ ] Deny by default: a new method needs an explicit entry in the policy and in the allow
      predicate of the right `methodTable`.
- [ ] Secrets (tokens, codes, keys, the registration secret) are never logged, audited, returned
      in errors or put in argv or environment variables. Comparisons of secrets are constant time.
- [ ] Error messages sent to a peer are fixed per code; the real reason goes to the server log.
      Failures that could be probed (join, register) return identical errors.
- [ ] New goroutines and reads have a deadline or a context and cannot leak or block forever;
      no lock is held across network I/O.
- [ ] State changes are audited **in the same transaction**; a failure to audit an action's start
      refuses the action (fail closed).
- [ ] Nothing weakens the actions guard (no shell, argv only, full-match regex, fixed PATH,
      empty environment).

**Tests**

- [ ] The happy path *and* the attack/failure path each have a test; the new test fails if you
      break the code (you checked).
- [ ] No real hosts, ports or data (`127.0.0.1:0`, `t.TempDir()`, keys generated in the test).
- [ ] `scripts/dev.sh test` passes with the race detector; timing-sensitive tests do not use
      bare `time.Sleep` for synchronisation.
- [ ] Store changes extend `storetest`; protocol changes extend the `api` / `central` tests.

**Licences and dependencies**

- [ ] No new dependency, or it was agreed first, is on the PLAN.md list, and
      `scripts/dev.sh licenses` passes.
- [ ] No bundled binary or asset with a non-permissive licence (fonts: OFL-1.1 with its text).

**Cross-platform**

- [ ] `CGO_ENABLED=0` still builds; `scripts/dev.sh cross` passes for all six targets.
- [ ] Unix-only code is behind a build tag with a fail-closed fallback; `mirafleet mcp` code
      does not import unix-only or SQLite packages (`TestNoSQLiteOnNonLinux`).
- [ ] Operator-side code uses `os.UserConfigDir` (via `opconf`), not hard-coded `~/.config`.

**Docs**

- [ ] The matching guide chapters, README quickstart and `docs/ARCHITECTURE.md` /
      `docs/PLAN.md` are updated; new terms are in the glossary; diagrams follow STYLE.md.
- [ ] CLI changes updated the usage text and the reference in chapter 10.

**Hygiene**

- [ ] `scripts/dev.sh fmt` and `vet` are clean.
- [ ] Files staged by name; no keys, `.db` files, `.env.local` or real hostnames in the diff.
- [ ] The commit message names the milestone and step and says why, not only what.

## 11.6 Good first tasks

Each of these is small, real and useful. They were found by reading the code and plans; check the
current state of the repo before you start, because someone may have done it. For anything that
changes behaviour, open a short plan or issue first (section 11.2).

1. **Direct tests for the CLI helpers in `cmd/mirafleet/mcp.go`.** `splitAddr`, `readSecret`
   (empty input, over 4096 bytes, trailing newline) and `pinHost` (replaces the line for the
   same address, keeps the others, file mode 0600) have no direct unit tests today.
   Table-driven tests with `t.TempDir()`. Teaches: Go table tests, the operator config files.
2. **A direct test for `keys.PinnedHostKeys`.** No unit test in `internal/keys` calls it; it is exercised only through
   other packages' tests (the runner and apiclient use it). Write cases for: exact host match, `*` wildcard, comment and blank lines,
   a wrong key, a missing file, a malformed line. Teaches: SSH key formats, known_hosts.
3. **A test that `audit.Open` creates the file with mode 0600 and appends across reopen.** The
   audit package tests cover formatting but not the file behaviour. Teaches: file modes,
   `t.TempDir()`.
4. **GitHub Actions workflow for the gate (M6).** [PLAN.md](../PLAN.md) says CI should run `go
   vet`, `go test -race` and `go-licenses`, but there is no `.github/workflows` yet. A workflow
   that runs `scripts/dev.sh fmt`, `vet`, `test`, `cross` and `licenses` on every pull request
   is the smallest correct version; set `CI=1` so `deps_test.go` fails instead of skipping when
   `go` is missing. Teaches: the whole dev flow. Any new GitHub Action you pull in must itself be
   licence-checked.
5. **Filters for `central audit`.** `audit.list` only supports `since_id` and `limit`; add
   optional `fleet`, `runner` and `action` filters. This touches every layer, so it is a good
   tour: `api.AuditListParams`, `store.Store.ListAudit` (and the conformance suite),
   `sqlite/audit.go` (an index on `(fleet, runner)` already exists), `handleAuditList`,
   `adminCLI.audit`, tests and chapter 10. Agree the parameter names first.
6. **systemd units for the M6 packages.** `runner join` already tells users the unit "ships with
   the M6 packages". Draft `mirafleet-central.service` and `mirafleet-runner.service` with a
   dedicated unprivileged user and `Restart=on-failure`, and document them in chapter 10. Teaches:
   how the service is meant to be deployed.
7. **Windows owner-only key check (M5).** `internal/keys/perm_windows.go` fails closed because
   it cannot verify permissions. Implementing an owner-only DACL check (using `golang.org/x/sys`,
   already a dependency) unblocks `mirafleet mcp` on Windows. This is larger; do it only after
   a short design note, and test on a real Windows runner in CI.
8. **Guide gaps.** Read a chapter as if you were new, and fix whatever confused you: add the
   missing glossary entry, correct a command, add a "common mistake" box. This is always welcome
   and is the best way to learn the code.

## Check your understanding

1. Why does the guide say "name files and functions, not line numbers"?
2. A reviewer reports: "`handleRunnersGet` is registered but the policy table has no entry, so
   every call is forbidden." Which severity is that most likely, and when must it be fixed?
3. Why must the project keep `CGO_ENABLED=0` building, and which rule is the reason SQLite is
   `modernc.org/sqlite`?
4. You want to add the third-party library `github.com/example/fancy-yaml`. List the steps
   before the PR can be merged.
5. Your change adds a new SSH channel type. Name three chapters or files that must change with
   it.

<details>
<summary>Answers</summary>

1. Line numbers change with every edit, so the guide would be wrong the day after it is
   written. File and function names survive refactors, and anyone can search for them.
2. P1: a real bug in a feature that does not work as intended. It must be fixed before the
   change is pushed. (It is not P0: failing closed is the safe direction; nothing insecure
   happens.)
3. A static binary is easy to copy and run anywhere and keeps packaging simple. cgo would need a
   C toolchain and shared libraries. That is the reason for the "one static Go binary" rule and
   for using the pure-Go SQLite driver.
4. Ask first (dependencies are limited to the PLAN.md list); read its licence and those of its
   transitive modules; make sure all are on the permissive allowlist; run `scripts/dev.sh
   licenses`; update PLAN.md's dependency list and `go.mod`/`go.sum` with `scripts/dev.sh go mod
   tidy`; confirm `CGO_ENABLED=0` and `cross` still pass.
5. Any three of: `internal/transport` (the channel name constant), the allow-list
   `allowedChannels` in `internal/central/roles.go` plus a test, [chapter 7](07-protocols.md)
   (protocols), [chapter 5](05-security.md) (if it changes what a role can do),
   [chapter 8](08-code-tour.md) (code tour), and the matching diagram.

</details>
