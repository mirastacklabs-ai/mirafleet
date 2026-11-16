# Help test mirafleet

Thank you for trying mirafleet early. This page tells you what to set up, what to try, and how
to report what you find. Plan for 30 to 45 minutes. You do not need to finish everything; a
report that says "I got stuck at step 3 because..." is as useful as a full pass.

mirafleet is pre-1.0 (v0.6.0). The protocol and CLI can still change between minor versions.
Read the [threat model](THREAT-MODEL.md) first, and do not point it at anything important.

## What you need

- A **laptop** (macOS, Linux or Windows) with **one** supported harness: Claude Code, opencode,
  Codex CLI, Gemini CLI, Cursor, VS Code Copilot, Cline or Kilo Code. Any stdio MCP client works too.
- One **Linux VM** you can throw away. It runs central and a runner together for this test.
  Debian, Ubuntu, Fedora and RHEL have packages; Docker Compose works anywhere Linux does.
  Use a VM with nothing valuable on it.
- No Linux VM? Do the [one-machine Docker lab](QUICKSTART.md#7-try-it-on-one-machine) instead.
  Say so in your report.

mirafleet sends no telemetry. The only connections it makes are the ones in the quickstart.

## The 30 to 45 minute path

Follow the [quickstart](QUICKSTART.md); it is the source of truth. Do not skip the
**Check it worked** step at the end of each part.

1. **Central** on the VM: [part 1](QUICKSTART.md#1-central-linux). Note which install channel you used.
2. **Admin GUI first run**: [part 2](QUICKSTART.md#2-the-admin-gui-first-run).
3. **An actions runner** on the same VM: [part 3](QUICKSTART.md#3-a-runner-linux), the *actions runner* section.
4. **Your laptop**: [part 5](QUICKSTART.md#5-operator-your-laptop-macos-linux-windows). Install the
   binary, log in, connect your harness.
5. Optional, if you have time: a workspace runner or host mode (below).

Write down the time each part took and every place you hesitated. That is the most valuable data.

## What to try

### Actions mode (everyone)

- [ ] `mirafleet mcp api fleets.list` shows your fleet.
- [ ] `mirafleet mcp api action.run` runs an action from `actions.yaml` and returns output.
- [ ] Ask the agent to run an action that is **not** in `actions.yaml`. It must be refused.
- [ ] Give an action a bad parameter (a space, a quote, `;`). It must be refused.
- [ ] The audit log (GUI **Audit**, or `mirafleet central audit`) shows each call with your operator name.
- [ ] Mark the fleet guarded (`central fleet set <fleet> -confirm=true`). A call without the fleet name as `confirm` must fail.
- [ ] Stop the runner and start it again. It reconnects without a new token.

### Workspace mode (optional; needs containerd on the runner)

See [part 3, container-workspace runner](QUICKSTART.md#container-workspace-runner-root-needs-containerd).

- [ ] The agent opens a workspace, runs a command in it, and closes it.
- [ ] A file written in the workspace is still there when you reconnect to the same session.
- [ ] The network limits you set in `workspace.yaml` hold (try to reach something you blocked).

### Host mode (optional; the most to review)

See [part 4](QUICKSTART.md#4-remote-development-on-a-vm-host-mode). Use a dedicated VM.

- [ ] The agent works as the ordinary `dev` user over the mirafleet file and shell tools.
- [ ] Ask the agent to print its environment and look for tokens or keys. None should appear.
- [ ] Revoke the runner in the GUI. The session ends and the runner cannot reconnect.

### One harness

Pick the harness you use every day and follow its guide from the
[integrations index](../integrations/README.md). Then:

- [ ] Ask it to list the fleets. It calls the mirafleet tools and shows the result.
- [ ] Ask it to run a command on the runner, and read what it asks you to approve.
- [ ] Claude Code only: the slash command `/mirafleet`, the confirm prompt on a guarded fleet, and workspace close at session end.
- [ ] Other harnesses: note which step needed the most guessing (the config file path, the restart, the approval prompt).

## What to watch for

- **Install friction.** A command that failed, a missing step, a version mismatch, a link that is dead.
- **Unclear errors.** An error that does not say what to do next, or names the wrong thing.
- **Security surprises.** Anything that lets the agent do more than the config allows; a secret
  in a log, in argv or in the GUI; a port open that the quickstart did not mention.
- **Wrong or stale docs.** Anything the quickstart says that did not match what you saw.

## How to report

- **Anything you find, including "it worked":** open a
  [test report](https://github.com/mirastacklabs-ai/mirafleet/issues/new?template=test_report.yml).
  One report per session is fine.
- **A clear bug:** use the [bug report](https://github.com/mirastacklabs-ai/mirafleet/issues/new?template=bug_report.yml) form.
- **A question:** ask in [Discussions](https://github.com/mirastacklabs-ai/mirafleet/discussions).
- **A security problem:** do **not** file a public issue. Report it privately, see
  [SECURITY.md](../SECURITY.md).

Before you paste logs, remove tokens, keys, join tokens, registration codes and real hostnames.

## Known limitations

- Pre-1.0: the protocol and CLI can change between minor versions, so match the version on
  every machine and upgrade in order: central, then runners, then operators.
- Central and runners are **Linux only**. They refuse to start on macOS or Windows. Only the
  orchestrator side (`mirafleet mcp`) runs on macOS, Linux and Windows.
- Release archives are not signed by Apple or Microsoft. Use Homebrew, npm or the install
  scripts, or clear the quarantine flag after checking the checksum
  (see [Install in the README](../README.md#install)).
- Without hooks, harnesses other than Claude Code have no session handoff, no automatic
  workspace close (an idle timeout reaps them) and no confirm prompt from the plugin. Central
  still enforces `confirm`. See [integrations](../integrations/README.md).
- Enterprise features (RBAC, SSO, HA) are out of scope for this tree.
- What is **not** protected is listed in the [threat model](THREAT-MODEL.md): for example root on
  a runner VM, a compromised central, and, in host mode, a determined agent that reads its own
  runner's enrolment key.
