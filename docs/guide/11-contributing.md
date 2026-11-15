# 11. Contributing

This chapter explains how we work: the ground rules and the reasons behind them, the
plan/implement/review loop, how milestones are written down, the rule that the guide is updated
with the code, a checklist for reviewing a change, and some real first tasks.

## What you will learn

- How to add an MCP file tool and a credential kind (section 11.10).
- Each ground rule from [CLAUDE.md](../../CLAUDE.md), with the reason it exists.
- The workflow for a change: plan, implement, read-only review, with P0 to P3 severities.
- How milestone plans (`docs/M*.md`) work and how to read one.
- The documentation rule, and how to add a diagram.
- A review checklist and a list of small, real tasks to start with.
- How to add support for a new agent tool (a *harness integration*) without touching the core
  ([section 11.7](#117-adding-a-harness-integration)).
- How to add a screen to the admin GUI, and the template rules that keep it safe
  ([section 11.8](#118-adding-a-gui-screen)).

## 11.1 The ground rules, with reasons

These come from [CLAUDE.md](../../CLAUDE.md) **as of 13 Nov 2026**. If the two ever disagree,
CLAUDE.md wins and this table should be fixed. Read the file itself too; it is short and
binding. Status today: M1, M2, M3 (workspaces), M4 (the admin GUI) and M5 (the MCP server and
harness integrations) are done, and M7 (host mode) is built; next comes M6 (release). A rule without a reason gets broken the first time it is inconvenient, so here is the reason for
each.

| Rule | Why |
|---|---|
| **One static Go binary** (`mirafleet central \| runner \| mcp`); `CGO_ENABLED=0` must keep building. | A static binary copies to any Linux host and runs, with no libraries to match. It makes packaging, containers and the "download and run" install easy. A dependency that needs a C compiler (cgo) would break this, which is why SQLite is the pure-Go `modernc.org/sqlite` and not the C one. |
| **Platforms:** central and runner are **Linux only**; `mirafleet mcp` and the harness integrations run on Linux, macOS and Windows (amd64 and arm64; CI tests Windows and macOS since M5). The binary must still build for all six targets, and `scripts/dev.sh cross` checks it. OS-specific code goes behind build tags and **fails closed** elsewhere. | Servers are Linux; laptops are not. Features such as the admin socket peer check or process-group kill have no honest Windows equivalent. If a security check cannot run on a platform, silently skipping it would be a hole, so the code returns an error instead (see [chapter 8](08-code-tour.md#87-build-tags-and-why)). |
| **Central's store is SQLite** behind the `Store` interface. | SQLite needs no separate server, which keeps the open-source edition easy to run. The interface keeps the door open for other stores without touching `central`. |
| **Keep dependencies to the list in `docs/PLAN.md`; ask before adding others.** | Every dependency is code we did not write but must trust, patch and licence-check. A short list keeps the supply-chain risk small and the binary lean. |
| **Permissive licences only** (Apache-2.0, MIT, MIT-0, BSD-2-Clause, BSD-3-Clause, ISC, 0BSD) for all dependencies, including transitive ones and bundled assets. Never GPL, LGPL, AGPL, MPL, SSPL, BUSL or other copyleft/source-available. Fonts may be SIL OFL-1.1. | mirafleet is Apache-2.0 and meant to be used inside companies. Copyleft or source-available terms could force those users to publish their own code or forbid their use. The licence gate is `scripts/dev.sh licenses` ([chapter 9](09-dev-setup-and-testing.md#99-the-licence-gate)). |
| **Follow [BRAND.md](../BRAND.md)** for GUI, CLI colours, docs and assets. | Consistent look, and the colours carry meaning (teal is data, orange is emphasis only, status colours are runner states). |
| **Containers:** the Docker ecosystem is supported (Docker CLI, Docker Engine, Compose, `docker.io` images), never a hard dependency on Docker's commercial products. Runner workspaces (M3) use containerd through its Go client, never Docker Engine. | We want open tooling anyone can run for free. Docker Engine runs on containerd, so one client covers Docker hosts and plain containerd hosts; the workspace namespace is `mirafleet`, and `moby` (Docker's) is refused in code so the two never mix. |
| **The runner-side actions guard is the security boundary:** argv only, no shell, regex-validated params. | Central and the agent are not trusted to decide what is safe to run. The only thing that cannot be talked around is the allowlist on the machine that executes. Every change near `internal/actions` gets the strictest review. |
| **M3 design rule (from [M3.md](../M3.md) section 1.11, not a line of CLAUDE.md): the workspace boundary is the container, and the runner builds it.** `workspace.yaml` has no field for privileged containers, unconfined profiles or host mounts; the runner re-validates everything central sends; nothing in a workspace gets API keys. The actions guard rule above is unchanged. | A workspace runner is root-equivalent (it holds the containerd socket), so central must not be able to ask for more than the runner's own template allows. Review changes in `internal/workspace/ctrd/spec_linux.go`, `wsconfig`, `egress` and `runner/workspace.go` as strictly as `internal/actions`. |
| **M4 design rule (from [M4.md](../M4.md) section 2.1, not a line of CLAUDE.md): the GUI is a front end of `service`, nothing more.** `internal/gui` calls `*service.Service` through the `gui.Backend` interface and never imports the store, the registry, SSH or `internal/central` (`TestGUIDeps`); it is wired only from a Linux file. Every route is behind the session check, every POST behind CSRF, and the CSP forbids inline script and style. | The GUI parses untrusted input from a browser on a port that may be open to a whole network. If a handler could reach the database it could skip the policy and the audit that every other front end goes through. Review changes in `internal/gui` (middleware, session, CSRF, templates) as strictly as `internal/actions`. |
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
test can check. For M2 it is literally the test `TestM2Done`; for M3 it is `TestM3Done` (fake
executor, every run) and `TestM3DoneIntegration` (real containerd, `scripts/dev.sh integration`).

For M5 it is `TestM5Done` (the MCP layer against a real central) together with `TestM5NoHooks` (the
built binary with no hooks).

For M4 it is `TestM4Done` (the admin GUI against a real central) together with the GUI security
suite and the built-binary smoke test `TestGUIBinary`.

For a milestone with real design choices there is a detailed file, `docs/M2.md`, `docs/M3.md`,
`docs/M4.md`, `docs/M5.md`. Reading [M2.md](../M2.md) shows the shape ([M3.md](../M3.md) and
[M5.md](../M5.md) follow it, with an extra section for the developer-guide updates):

1. **Design decisions** (section 1): store, tokens, auth, protocol, admin socket, revoke, audit.
   Each with the reason.
2. **Layout** (section 2): which packages and files are new or changed.
3. **Steps** (section 3): a table of small steps in waves. Steps marked parallel touch disjoint
   files so they can be done at the same time. Each step lists what it depends on.
4. **Reviewer focus per step** (section 4): the exact things a reviewer must try to break.
5. **Binding rules from reviews** (section 4a) and **deferred findings** (section 4b).
6. **Dependencies and licences** (section 5) and **decisions with dates** (section 6).

Your job when starting a milestone is to write that file *first* and get it agreed. [M3.md](../M3.md)
shows the whole life of one: decisions first (executor interface, OpenSandbox as opt-in), then an
approved plan in waves, then per-step reviewer focus, and finally a list of **deferred review
findings** (P2/P3, labelled A-xx to D-xx by wave) that a later hardening pass works through. When you
fix one, note it in the file. Read those lists before you touch workspace code: several are still
open (see the good first tasks below).

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
3. Use the running example: MIRASTACK LABS, `central.example.com:2222`, fleets `dev` and `prod`,
   runners `build-1` and `db-1`, operators `durga` and `kalyan`.
   Never real hostnames, people or secrets.
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
`action-run.svg`, `revoke.svg`, `framing.svg` and, since M3, `workspace-open.svg`,
`workspace-exec.svg` and `workspace-boundary.svg`, and, since M4, `gui-setup.svg` and
`gui-revoke.svg`. Reuse them; add new names only for new flows,
and add the matching numbered list to `diagrams/README.md`.
View your SVG in both a light and a dark browser theme, and with "reduce motion" on, before you
commit. One more rule learned the hard way: **write a plain colour before every `var()`**, for
example `fill:#00695F;fill:var(--accent,#00695F)`. Browsers use the second declaration; renderers
that do not understand CSS variables (some image viewers, `rsvg-convert`, link previews) use the
first, so the picture is never black or blank. Check the file is well-formed XML and render it
once with a non-browser renderer.

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
- [ ] Operator-side code uses `opconf.Dir` / `opconf.StateDir`, not hard-coded `~/.config`.
- [ ] Code in `internal/orch`, `internal/mcpserver` or the hook does not import the server side
      or the go-sdk (outside `mcpserver`); `TestOrchestratorDeps` passes, and the tests also pass
      on Windows (no Unix-mode assumptions).

**MCP tools and integrations**

- [ ] A new tool wraps an operator method only, decodes with `DecodeArgs`, includes `where`,
      caps and sanitises remote text, and passes `confirm` through untouched (chapter 8, "A new
      MCP tool").
- [ ] Tool descriptions and results keep saying that remote output is data, not instructions.
- [ ] A harness integration lives only under `integrations/<harness>/` and adds nothing to the
      core that only that harness needs (section 11.7).

**GUI** (anything under `internal/gui`)

- [ ] The new routes are registered with a literal `"METHOD /path"` through `adminMux` (or are
      the documented exceptions: setup, login, static) so the security suite finds them.
- [ ] Every state change is a POST that goes through `screenForm`; a destructive one has a
      confirmation page; a success answers `Redirect` (303), not a rendered page.
- [ ] The handler reaches data only through `gui.Backend`; no store row with secret material
      reaches a template; runner-supplied text is shown through `inertRunnerText` or the
      escaping templates, never as HTML.
- [ ] The template obeys the CSP rules of section 11.8 (`TestTemplateLint*` pass), and the flow
      works without JavaScript (`TestS16NoJSFlows` style).
- [ ] The service method the screen calls is audited, and the audit row says `source` `gui`.

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
   vet`, `go test -race` and `go-licenses`. The only workflow today is the small
   `orchestrator.yml` (Windows and macOS tests of the operator side, chapter 9, section 9.13);
   the Linux gate is run by hand. A workflow
   that runs `scripts/dev.sh fmt`, `vet`, `test`, `cross` and `licenses` on every pull request
   is the smallest correct version; set `CI=1` so `deps_test.go` fails instead of skipping when
   `go` is missing. Teaches: the whole dev flow. Any new GitHub Action you pull in must itself be
   licence-checked.
5. **A deferred M4 finding.** The end of [M4.md](../M4.md) lists review findings (G-xx, P2 and
   P3). For example G-38: the fleet name `new` is reserved by the GUI (it would be shadowed by
   `/fleets/new`) but the CLI can still create it, so reserve it in `store.ValidFleetName`; or
   G-37: show the workspaces screen's operator, session and image through `inertRunnerText` as
   the runner page does. Check the code first, since some entries are fixed before the list is
   updated (G-10 is fixed in the code); write the failing test first (`storetest` for a store behaviour); mark the entry. Teaches: the store contract and
   how reviews close. (The audit filters that this list used to suggest as a task landed in M4:
   see [chapter 7.6](07-protocols.md#76-admin-methods-local-admin-socket).)
6. **systemd units for the M6 packages.** `runner join` already tells users the unit "ships with
   the M6 packages". Draft `mirafleet-central.service` and `mirafleet-runner.service` with a
   dedicated unprivileged user and `Restart=on-failure`, and document them in chapter 10. Teaches:
   how the service is meant to be deployed.
7. **A deferred M5 finding.** The end of [M5.md](../M5.md) lists review findings (F-xx, P2 and P3)
   that were not fixed in their wave. Pick one that is still open (for example a missing
   `GIT_SSH`-style name in `workspace.EnvDenied`, or one more case for
   `TestEveryToolRejectsMiscasedAndDuplicateKeys`). Check the code first, since the list is
   updated as items are fixed; write the failing test first; mark the entry. For anything under
   `internal/keys`, `internal/opconf` or `internal/handoff`, make sure the change also passes on
   Windows (the CI workflow runs it; chapter 9, section 9.13). Teaches: the orchestrator packages
   and how reviews close.
8. **Warn about tag-only image patterns.** [M3.md](../M3.md) section 1.10 says a tag-only pattern
   (`golang:*`) is allowed "but warned about" (digests are recommended), yet `fleet create` and
   `fleet workspace set` print nothing. Add a one-line warning on stderr in
   `cmd/mirafleet/admin_workspace.go` (`apply`) when an `-images` entry has no `@sha256:`, with a
   test in `admin_workspace_test.go`. Teaches: the admin CLI, `adminCLI.call`, why digests matter.
9. **`-json` for `runner workspace list`.** `central workspace list` has `-json`; the local
   `mirafleet runner workspace list` only prints a table (`printWorkspaces` in
   `cmd/mirafleet/runner_ws_linux.go`). Add the flag, using the fake executor seam (`newExecutor`)
   for the test. Teaches: the `newExecutor` test seam and `workspace.Info`.
10. **Filters for `central workspace list`.** Today only `-fleet`. Add `-operator NAME` and `-state S`
    (the store's `WorkspaceFilter` already has `OperatorID` and `States`): `api.WorkspaceAdminListParams`,
    `service.AdminListWorkspaces`, `handleWorkspaceAdminList`, the CLI, the conformance suite and
    chapter 10. Agree parameter names first (it is an additive protocol change, chapter 7.12).
11. **A deferred hardening finding.** Pick one from the "Deferred review findings" list at the end of
    [M3.md](../M3.md) that is still open (for example D-11: a runner started without `-workspace` in
    a workspace fleet should log a hint, or D-09: `mcp api -out` should check its output directory
    first). Check the code first, since many entries are already fixed; write the test before the
    fix; and update the list. Teaches: reading review findings and how the plan/review loop closes.
12. **Private registry credentials (design note first).** Workspace pulls are anonymous
    (question Q7 in M3.md). Write a short design note for a runner-side credentials file that never
    enters a container (mode 0600, owned by the runner user, per-registry). Do not code it before the
    note is agreed: it touches `wsconfig`, `ctrd` pulls and the security chapter.
13. **Guide gaps.** Read a chapter as if you were new, and fix whatever confused you: add the
   missing glossary entry, correct a command, add a "common mistake" box. This is always welcome
   and is the best way to learn the code.

## 11.7 Adding a harness integration

A **harness** is the program an agent runs in: Claude Code, opencode, Kilo Code, Cline, Cursor, and
so on. mirafleet's core, `mirafleet mcp`, is a plain stdio MCP server, so **any harness that can
launch one already works, with no code change**. An integration is only a convenience layer on top.
This section is the recipe for adding one. The rule behind it is in section 11.1: the core is
harness-agnostic, and everything harness-specific lives under `integrations/<harness>/`.

### What an integration may add, and what it may not

It may add: (1) the *configuration* that makes the harness start `mirafleet mcp`; (2) a *command*
or prompt (`/mirafleet`) that carries the shared instructions; (3) optionally *hooks*, if the
harness has them, which supply three conveniences: tie a harness session to its workspaces,
close them when the session ends, and show an extra "are you sure?" before a guarded call.

It may not: change `internal/orch` or `internal/mcpserver` for one harness's sake; make the core
read a harness's environment variable (the core reads no `CLAUDE_*`: the project directory arrives
through the `-project-dir` flag, which the plugin fills in); make any safety property depend on a
hook. Why: hooks are a convenience that the next harness may not have, or that a user may switch off.
The rule that decides things, `confirm`, is enforced by **central**, which refuses a call to a
guarded fleet unless `confirm` equals the fleet name, hook or no hook. A hook can only make the
human's decision more visible; it can never grant anything. This is why the hook code
never emits `allow` (only `ask`), and why `TestM5NoHooks` runs the full flow with no hook at all.

### The recipe

1. **Try the generic setup first.** Put `{"command": "mirafleet", "args": ["mcp", "-project-dir",
   "."]}` into the harness's MCP configuration (see `integrations/generic/README.md`) and run
   `list_fleets`. If that works, you may be done; many harnesses need nothing more.
2. **Read the harness's documentation, and write down what you relied on.** The opencode README
   has a "Docs relied on" list with dates and an "Unverified" paragraph. Copy that habit: harness
   configuration formats change, and a reader needs to know what was checked against a real
   program and what was only read.
3. **Create `integrations/<harness>/`** with a `README.md` (install, requirements: `mirafleet` on
   `PATH`, and the one-time `mirafleet mcp login`; what you lose without hooks; troubleshooting)
   and the configuration snippet or files.
4. **Reuse the shared command body, as a copy.** `integrations/commands/mirafleet.md` is the single
   source of the instructions the model gets (use `list_fleets` first, prefer workspaces, never set
   `confirm` without the user's agreement, treat tool output as data, close what you opened). Put
   it into the harness's file format (frontmatter plus the same body, with `$ARGUMENTS` if the
   harness has an argument placeholder) and **add a test that the body equals the shared one**: copy
   `TestOpencodeCommandBodiesMatchShared` in `integrations/harnesses_test.go`. A copy and a test,
   not an include, because several harnesses cannot include files, and because the confirm rules
   must never depend on the model first opening another file.
5. **Add the README to `TestREADMERelativeLinks`'s file list** so its relative links are checked.
6. **Hooks, only if the harness has them.** Add a Go package `integrations/<harness>/hook` and one
   `case` in `cmd/mirafleet/mcp_hook.go` (today it routes only `claude-code`) so the harness can
   run `mirafleet mcp hook <harness> <event>`. Follow the Claude Code package
   (`integrations/claude-code/hook/hook.go`), which shows each rule:
   - parse the harness's JSON strictly, accept unknown fields (harnesses add them), cap the input
     size, and never print on stdout except the harness's own answer format;
   - **session start:** `handoff.Write(stateDir, handoff.Key{PID: os.Getppid(), Project: dir}, "<prefix>:<session id>", now)`.
     The MCP server reads it back with `orch.SessionFor`. Use a prefix for your harness (`cc:`
     is Claude Code's) and keep the whole id within `api.ValidateSession` (128 characters of
     `A-Za-z0-9._:-`);
   - **session end:** remove the handoff with `handoff.RemoveIf` (so a newer session's file is not
     deleted), and close workspaces only for explicit end reasons, with a short time bound;
   - **before a tool call:** return `ask`, never `allow`, when the call carries `confirm`; build the
     reason from sanitised, clipped fields; if the input cannot be read, ask;
   - **always exit 0.** A broken hook must never block a session, and the plugin's `hooks.json`
     must be exec form (a `command` plus an `args` array, no shell string) so it also works on
     Windows;
   - the package may import only `orch`, `handoff`, `opconf` and `api` (add it to `targets` in
     `TestOrchestratorDeps` and to `PKGS` in `.github/workflows/orchestrator.yml`).
   The handoff relies on the hook process and the MCP server being children of the same harness
   process, which is an assumption to check by hand for each harness. If it is false, the server
   falls back to a random per-process session, which is safe: you lose only the automatic
   reattach after a restart.
7. **Tests.** Real hook JSON fixtures; the hook cases of chapter 9 (start writes, end closes only
   the matching session and only for the right reasons, ask on `confirm`, ask on malformed input);
   a test that the list of tool names the hook knows equals `mcpserver.ToolNames`
   (`TestKnownToolsMatchMCPServer`); tests that the manifest, the MCP file and the hook file are
   valid, that hooks are exec form, and that any matcher covers all nine tools and nothing else
   (`integrations_test.go`).
8. **Update the docs.** The matrix in `integrations/README.md`, [chapter 10](10-operations.md#10105-opencode-and-any-other-harness),
   and this section if you found a better way.
9. **Check by hand with the real harness, and write down the result.** What the automated tests
   cannot see is the harness itself: does the command name resolve, is the project directory
   substituted, do hooks run, is the parent process shared. Put those in the README's
   "Unverified" list until someone has checked them.

### Checklist for the reviewer

- [ ] Nothing outside `integrations/<harness>/`, the one router `case` in `mcp_hook.go` and the test
      target lists changed.
- [ ] `TestM5NoHooks` and `TestM5Done` still pass untouched.
- [ ] The hook (if any) never prints `allow`, always exits 0, imports nothing server-side.
- [ ] The shared body is copied unchanged and a test proves it.
- [ ] The README says what works with no hooks and what was not verified.

## 11.8 Adding a GUI screen

The file-by-file recipe (service method, `Backend`, handler, route, template, tests) is in
[chapter 8, "A new GUI screen"](08-code-tour.md#a-new-gui-screen-m4). This section is the part
only a contributor needs: the **template rules**, with the reason for each. They exist because the
GUI sends a strict Content-Security-Policy (`default-src 'none'; script-src 'self'; style-src
'self'; ...`, no `unsafe-inline`, no `unsafe-eval`). That policy is the second wall behind output
escaping: if some text from a runner (a host name, a label) ever reached the page unescaped, the
browser would still refuse to run it. Break a rule below and either the browser blocks your page
or a lint test fails. The tests that check them are `TestTemplateLintSources` and
`TestTemplateLintRenderedPages` in `internal/gui/security_test.go`.

**Checklist for a template**

- [ ] **No inline script and no inline style.** No `<script>` with a body, no `<style>`, no
      `style="..."` attribute. Put a class in `static/app.css` instead. *Why:* the CSP allows only
      files served from our own origin, so an inline one is blocked, and a way to inject one is the
      very hole the CSP closes.
- [ ] **No event attributes.** No `onclick=` or any `on...=`; no `javascript:` or `js:` values.
      *Why:* same rule, and these are the usual XSS carriers. Behaviour goes into `static/app.js`
      (under 150 lines, no dependencies) or into an htmx attribute.
- [ ] **Only the allowed htmx attributes:** `hx-get`, `hx-post`, `hx-target`, `hx-swap`,
      `hx-trigger`, `hx-select`, `hx-confirm`, `hx-indicator`, `hx-headers`, `hx-disabled-elt`,
      `hx-include`. No `hx-on...`, `hx-vars`, `data-hx-...`. *Why:* the others evaluate
      JavaScript (`allowEval` is off, and the lint says the same thing earlier).
- [ ] **`hx-trigger` only in the plain forms** `load`, `every Ns`, `change`, `submit`,
      `keyup changed delay:Ns`, joined with commas; no `[...]` filters. *Why:* a filter is a
      JavaScript expression.
- [ ] **Every form works without JavaScript.** A form has `method="post"` and a real `action`;
      `hx-post` is an extra on top of it, never instead. Include the hidden token with
      `{{template "ui/csrf" .}}`. *Why:* accessibility, the text-mode browser, and the proof that
      the server does the work (`TestS16NoJSFlows`).
- [ ] **A fragment that is polled is registered with `mux.Passive`.** *Why:* otherwise an open tab
      keeps the session alive forever.
- [ ] **Untrusted text is plain template output**, which `html/template` escapes. Never
      `template.HTML`, `template.JS` or `template.URL` outside the one vetted helper file
      (`TestTemplateNoTrustedTypesOutsideHelper`). Long values are clipped in Go with
      `inertRunnerText` and carry the full value in a `title`. *Why:* a runner's host name is
      chosen by whoever controls that host.
- [ ] **Build links with `PathEscape` / `QueryEscape`** in Go, not by pasting names into a path.
      *Why:* a runner called `a/../b` must not become a different URL.
- [ ] **Status is a word and a colour**, never a colour alone (use the `ui/badge` partial). The
      colours come from [BRAND.md](../BRAND.md). *Why:* colour-blind readers, and
      `TestS16ContrastAA` checks the contrast.
- [ ] **A secret is shown once.** If the page shows a join token or a code, the handler renders it
      directly with `Cache-Control: no-store` (no redirect, no history entry) and nothing later can
      show it again. A new such page needs a design note first.
- [ ] **A destructive action goes through a separate confirmation page** (`GET .../revoke`, then
      `POST`), in the red dialog style, saying what it will do. The biggest ones ask for the typed
      name too (`fleet/runner` for a runner revoke, the fleet name to turn a guard off). *Why:* a
      mis-click on a table row must not revoke anything.

If you add an asset (a font, a script), it must be vendored under `internal/gui/static`, listed in
`static/ASSETS.md` with its licence and hash, and pass the licence rule in
[section 11.1](#111-the-ground-rules-with-reasons); `CheckVendored` fails otherwise.

## 11.10 Extending developer workspaces (M7)

Two recipes for the M7 code. Read [8.13](08-code-tour.md#813-developer-workspaces-m7-the-new-packages) first. The
general rules of [11.1](#111-the-ground-rules-with-reasons) apply: container-only commands, no new
dependencies, and a secret value must never reach a log, audit row, error message, tool result or
file the agent can read.

### Add an MCP file tool (a new `workspace.fs` op)

Example: a `stat` tool. (The `stat` op already exists in the enum; the steps below are what a *new*
op needs, and for `stat` you would only do steps 5 to 8.) All file tools run in the runner process,
so the op travels the whole path from the model to `fsops`.

1. `internal/workspace/fsops`: implement the operation as a pure function on `FS` with a golden test
   under `testdata/`. Return `*fsops.Error` kinds (`ErrNotFound`, `ErrRefused`, ...), not raw `os`
   errors. The host-mode tools are not confined: absolute paths work anywhere the OS user can reach, so
   do not add a path check that pretends otherwise. (`os.Root` is for the container executor.)
2. `internal/transport/devws.go`: add the op constant to `FSOps`, list its allowed fields in
   `fsAllowed` (a field not listed for the op is `bad_request`, which is how strictness is kept), and
   add it to `FSMutating` if it changes files (that is what makes a guarded fleet demand `confirm`).
3. `internal/workspace/hostx/files.go`: add a case to `Files.Do`. Mirror it in
   `internal/workspace/fake/devops.go` so `TestM7Done` can run it.
4. `internal/service/fsdispatch.go`: add what the audit row records in `fsAuditParams`. Record
   sizes and hashes, **never content, patterns or edit strings**.
5. `internal/mcpserver/tools_fs.go`: the schema string (`additionalProperties:false`, `fsProps`,
   `confirmProp` if mutating), the handler (`DecodeArgs`, `checkRemotePath`, `checkFS`, `s.runFS`,
   `s.makeOut`), and the `Tool` entry with honest annotations: read-only tools are
   `ReadOnly+Idempotent`, tools that change files are `Destructive`.
6. `integrations/claude-code/hook/hook.go`: add the name to `knownTools` (a test compares it with the
   server) and decide whether `remote: true` projects deny a local twin.
7. Tests: the expected set and hints in `TestM5Done/02_tools` (it fixes the exact tool list on
   purpose, so you must change it knowingly), the miscased-key test, and a case in `TestM7Done`
   step 3. Run `scripts/dev.sh go test -race ./internal/workspace/... ./internal/mcpserver/...
   ./internal/service/...` then the full gate once.
8. Docs: the table in [7.16](07-protocols.md#716-developer-workspaces-m7-tools-jobs-credentials)
   and this guide's tool count.

A tool that is *not* a file operation (a new kind of background work, say) follows the job path
instead: `transport` job op, `hostx` job code, `service/jobs.go`, `orch/devws.go`, `mcpserver/tools_bash.go`.

### Add a credential kind

Example: `aws-key`. Think first: a kind exists to let the agent *use* something without *having*
it. For each kind you must answer "which process uses the secret, and why can the agent not
read it?" For `ssh-key` it is the agent socket; for `https-token` and `registry` it is the broker.
If your kind needs a third mechanism (here: signing AWS requests), you are adding a security
boundary, not a field: write the threat-model row in `docs/M7.md` and get a review first. Ask
before adding a dependency.

1. `internal/transport/devws.go`: the `Cred...` constant, `ValidCredKind`, and the `CredMeta.Validate`
   rules for its fields (reject fields that belong to other kinds, as the existing kinds do).
2. `internal/store/store.go`: the `CredentialKind` constant and its validity check. The kind is stored
   in the `credentials` row and the extra fields in the `meta` JSON, so usually no migration is
   needed; if you add a column, follow 8.8 "a new store method and a migration".
3. `internal/service/credentials.go`: `metaFromParams` and `credFingerprint` (the fingerprint is
   a hash, never part of the value) and the masked `credInfo`. Sealing needs no change: the whole
   payload is sealed with AAD of id and version.
4. `internal/runner/creds.go`: turn the delivered value into the runner's in-memory type. This is
   the only place the plaintext exists outside the sealing code.
5. The mechanism: a new `Provider` for `sshagent`, a new injection kind in `broker/cred.go`
   (add it to `Credential.Validate`, keep `String`/`GoString`/`LogValue` redacting), or a new
   package. Tests must prove **the value never appears in logs, errors or responses** (see
   `broker_test.go` for the pattern) and that **other hosts are not intercepted**.
6. `internal/runner/hostops.go`: wire it into the per-session set-up and tear-down.
7. `cmd/mirafleet/admin_credentials.go`: flags for `credential set` (the value still comes from
   standard input) and `internal/gui/handlers_credentials.go`: the form field (write-only; the
   page must render no value, and the decoy scan in the GUI tests must still pass).
8. Tests: `storetest` if the store changed; step 1 and step 9 of `TestM7Done` (add a decoy for the
   new kind: it must be absent from everything); the integration leak scan if the mechanism is real.
9. Docs: [7.16](07-protocols.md#716-developer-workspaces-m7-tools-jobs-credentials),
   [10.13](10-operations.md#1013-developer-workspaces-on-a-vm-m7-host-mode-and-credentials), the
   threat-model table in `docs/M7.md` section 4.6, and `credential-flow.svg` if the flow differs.

### Check your understanding (M7)

1. Why must a new `workspace.fs` op list its fields in `fsAllowed`?
2. Why does a new credential kind need a threat-model row?
3. What must a new tool's audit params never contain?

<details>
<summary>Answers</summary>

1. Unlisted fields for an op are `bad_request`; that keeps the protocol strict.
2. Each kind is a way to use a secret without having it; a new mechanism is a new security boundary.
3. Content, patterns, edit strings or any secret; only op, path, sizes and hashes.

</details>


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
6. You add an animated SVG that looks perfect in your browser, but a teammate's chat preview shows
   a black rectangle. What did you most likely forget, and how do you check for it?
7. You change a function in `internal/workspace/ctrd/spec_linux.go`. Why does the review treat
   this like a change to `internal/actions`, and what must the PR contain?
8. A teammate says "my harness has hooks, so let the server skip the guarded-fleet refusal when
   the hook has already asked the user." Why is that wrong?
9. What is the smallest set of changes that adds support for a new harness with no hooks?
10. You add a button with `onclick="copyToken()"` to a GUI template. What happens in the browser,
    and which test fails first?
11. Why may `internal/gui` not import `internal/store/sqlite` "just to read one column"?

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
6. A non-browser renderer ignores CSS variables, so every `fill:var(--x)` fell back to the default
   (black). Write a plain colour first (`fill:#00695F;fill:var(--accent,#00695F)`). Check with an
   XML well-formedness check and a render in a throwaway container with `rsvg-convert`; look at the
   PNG, and check its size is not suspiciously tiny.
7. The spec builder decides what a workspace container can do (capabilities, mounts, namespaces,
   read-only rootfs), and a workspace runner is root-equivalent, so a loosened field is a possible
   host compromise. The PR needs the matching assertion in `spec_linux_test.go`, an integration
   attack test if behaviour changes, a [chapter 5](05-security.md) update, and a read-only review
   (`qa-reviewer`) that tries to break it.
8. The hook is a convenience; a harness may lack hooks, or the user may disable them. If a
   guarded call were safe only when the hook ran, a plain MCP client would be unsafe. Central
   enforces `confirm` itself, so the hook can only make the human's decision more visible, and the
   hook never returns `allow`.
9. Almost nothing: a new directory `integrations/<harness>/` with a README and the configuration
   or command files, a copy of the shared command body plus a test that it is identical, and, if
   the harness has hooks, a hook package and one `case` in `cmd/mirafleet/mcp_hook.go`. No change to
   `internal/orch` or `internal/mcpserver`, and `TestM5NoHooks` must stay green untouched.
10. The browser refuses to run it: the Content-Security-Policy has no `unsafe-inline`, so the
    handler never fires (the button does nothing and the console shows a CSP violation).
    `TestTemplateLintSources` fails first, because it scans the template source for `on...=`
    attributes. The fix is a class on the button and a few lines in `static/app.js` (or no
    script at all, using a normal form).
11. The GUI handles untrusted browser input on a port that may face a network. Through `service`
    every action is authorised, validated and audited the same way as for the CLI; a direct
    database read would skip all three and put SQL next to HTML. `TestGUIDeps` fails the build.
    The right move is a new `service` method, then a line in `gui.Backend`.

</details>
