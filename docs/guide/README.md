# The mirafleet developer guide

This guide explains how mirafleet works, from the idea down to the code. It is written for
engineers fresh out of college. You need to know basic programming and a little Go (or a similar
language). You do **not** need to know SSH keys, SQLite, containers or MCP. The guide teaches
those as they come up.

After reading it you should be able to explain the whole design, find any piece of code, run the
tests, and make your first change without asking the authors.

> **Status:** M1, M2 and M3 (workspaces) are done (13 Nov 2026), and M5 (the orchestrator: MCP
> server, Claude Code plugin and hooks, Windows support) is built; a smoke test with a real Claude
> Code is still to run. Next: M4 (GUI) and M6 (release). Where a feature is planned, the guide
> says so and links the milestone in [../PLAN.md](../PLAN.md).

## The story in one paragraph

Acme is a company with a `dev` fleet and a `prod` fleet of servers. Engineers use an AI coding
agent (Claude Code) and want it to help run those servers. Letting an AI log in to production
with a shell is dangerous. mirafleet puts one server, **central**, in the middle. Each fleet
server runs a small **runner** that dials out to central. The agent talks to central, and central
passes along only the work that the runner's own allowlist permits. Every request is audited.
This is the same shape as GitLab CI runners. For coding work the `dev` fleet runs in
**workspace mode**: the agent gets a private container on a runner, where it may run any command
(`go test`, edits) without being able to reach the host, the network or anyone else's files.

## Reading order and time estimates

Read in order the first time. Later, jump around with the map below.

| # | Chapter | What it gives you | Time |
|---|---|---|---|
| 0 | [Glossary](glossary.md) | Every term, alphabetical. Keep it open in a tab. | skim 10 min, then look things up |
| 1 | [What is mirafleet?](01-what-is-mirafleet.md) | The problem, the three roles, the big picture | 25 min |
| 2 | [Background](02-background.md) | Public-key crypto, SSH, HMAC, TOTP, SQLite, context, containers (namespaces, seccomp, cgroups, egress proxy, `os.Root`), MCP in depth (JSON-RPC, tools, harness, plugin, skill, hook), Unix sockets, Windows DACLs | 80 min |
| 3 | [Architecture](03-architecture.md) | Components, packages, the orchestrator, executors, trust boundaries (the model is untrusted input) and how they fit | 65 min |
| 4 | [Flows](04-flows.md) | Twenty-three step-by-step flows: join, register, run an action, revoke, the workspace flows (open, exec, files, close, reaper, reconcile) and the MCP flows (one `exec` tool call, session handoff, idle on exit, SessionEnd, guarded `confirm`) | 95 min |
| 5 | [Security](05-security.md) | Every defence, the workspace container boundary, prompt injection through tool output, and what would go wrong without it | 95 min |
| 6 | [Data model](06-data-model.md) | The SQLite tables (including the workspace tables), migrations, the audit log, and the local files M5 added | 45 min |
| 7 | [Protocols](07-protocols.md) | SSH users, channels, framing, the JSON API | 40 min |
| 8 | [Code tour](08-code-tour.md) | Package by package, with function names | 60 min |
| 9 | [Dev setup and testing](09-dev-setup-and-testing.md) | Build, run, test, debug | 30 min |
| 10 | [Operations](10-operations.md) | Install, run and look after a real deployment | 40 min |
| 11 | [Contributing](11-contributing.md) | Rules, review process, your first change | 20 min |
| 12 | [Why mirafleet?](12-why-mirafleet.md) | How it compares with CI runners, Salt, Teleport, ssh-mcp and sandboxes; where AI API keys live | 30 min |

Total: about 10.5 hours (the times above add up to about 635 minutes). If you are short on time,
read 1, 3, 4 and skim 5 (start with 5.11 if you work on the MCP side), 2.2, 2.3, 2.7 and 2.10;
that is enough to review most changes. If you only want the MCP and orchestrator story, read
2.10, the "Operator side" section of chapter 3, flows 19 to 23 in chapter 4 and section 5.11.

## Map of the chapters

1. **What is mirafleet?** Why an AI agent needs a safe way to work on many servers, and what the
   three roles do.
2. **Background.** The ideas from outside mirafleet that you need first, each with "why we need
   this".
3. **Architecture.** The components, how they connect, which Go package owns what, the
   orchestrator on the operator's machine and where each key lives on each operating system.
4. **Flows.** Twenty-three flows (join, register, run an action, revoke, the workspace flows:
   open, exec, files, close, reaper, reconcile, revoke cascades, and the MCP flows: one `exec`
   tool call, session handoff and `--resume`, idle on exit, SessionEnd, guarded `confirm`), told
   one numbered step at a time.
5. **Security.** The threat model, every defence layer, the workspace container boundary, and
   prompt injection through MCP tool output (with a worked example).
6. **Data model.** Tables (including `fleet_workspace` and `workspaces`), statuses, the workspace
   state machine, migrations, append-only audit, and (no schema change in M5) the local files
   `contexts.yaml`, `.mirafleet.yaml` and the session handoff file.
7. **Protocols.** The wire format: SSH users, channels, length-prefixed JSON frames.
8. **Code tour.** Where to find things in the repository.
9. **Dev setup and testing.** How to build and test, and the testing rules.
10. **Operations.** Running central and runners for real.
11. **Contributing.** How to propose, write and review a change.
12. **Why mirafleet?** How it compares with CI runners, Salt, Teleport, ssh-mcp and sandboxes,
    and where AI API keys live.

**Want to see it run?** Chapter 9 has a "Try it locally" section: central, a runner and an
operator in one container, from `central init` to a real `action.run`.

## How to read the diagrams

The guide uses two kinds of diagram.

**Animated SVGs** show the key flows. They live in [diagrams/](diagrams/) and are embedded like
this:

```markdown
![big picture](diagrams/big-picture.svg)
```

How to read one:

- Each drawing loops every 6 to 12 seconds. A moving dot is a message travelling along a line.
- Numbered circles in the drawing match the **numbered list directly under the image**. If your
  viewer does not animate (or you set "reduce motion" in your operating system), the drawing
  shows its final state and the list tells the whole story.
- Colours follow the brand: teal is data or a request, orange is emphasis only (the thing to
  look at), and the status colours match runner states (active, pending, disabled, revoked).
- The drawings work in light and dark mode. They contain no scripts and load nothing from the
  internet.
- Every diagram has a one-line caption saying what to look at first.

**Mermaid** diagrams (flowcharts, sequence and state diagrams) are written as text inside the
Markdown. GitHub draws them for you. Every arrow has a label. If your viewer shows raw text
instead of a picture, the code itself is readable: `A -->|label| B` means "A sends label to B".

## Conventions used everywhere

- **Running example.** Company Acme. Central is `central.example.com:2222`. Fleets are `dev` and
  `prod`. Runners are `build-1` (in `dev`) and `db-1` (in `prod`). Operators (people who use fleets) are `alice`
  and `bob`. Dana is the admin who runs `mirafleet central ...` on the central host. All hostnames
  use `example.com`; nothing is real. For **actions** the story is alice running `tail-logs` on
  `prod/db-1`. For **workspaces** it is Dana creating `dev` in workspace mode, then alice's Claude
  Code opening a workspace on `build-1`, running `go test` and reading and writing files (told in
  [chapter 1](01-what-is-mirafleet.md#the-workspace-running-example)).
- **Names, not line numbers.** The guide points at files and functions such as
  `internal/service/exec.go` so it does not rot when lines move.
- **Planned vs built.** "Planned" means not written yet.
- **Glossary links.** The first use of a term in a chapter links to [glossary.md](glossary.md).
- **Each chapter** starts with "What you will learn" and ends with "Check your understanding"
  (answers hidden in a collapsed block).

## Where else to look

- [../ARCHITECTURE.md](../ARCHITECTURE.md): the formal design.
- [../PLAN.md](../PLAN.md): milestones. [../M2.md](../M2.md), [../M3.md](../M3.md), [../M5.md](../M5.md): detailed decisions.
- [../BRAND.md](../BRAND.md): colours and fonts.
- [../../CLAUDE.md](../../CLAUDE.md): ground rules for contributors and AI agents.
- [STYLE.md](STYLE.md): the rules this guide follows.

If the guide and the code disagree, the code is right. Please fix the guide.
