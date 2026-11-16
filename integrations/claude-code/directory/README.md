# mirafleet setup plugin for Claude Code

A setup guide for [mirafleet](https://github.com/mirastacklabs-ai/mirafleet), packaged as a Claude
Code plugin. It holds one skill, `mirafleet-setup`, and one command, `/mirafleet-setup:mirafleet-setup`.
It contains only Markdown, JSON and an icon: no hooks, no MCP server, no scripts, no binary.

Ask Claude "set up mirafleet" and the skill walks you through, one confirmed step at a time:

1. installing the `mirafleet` binary (you choose the channel; nothing is piped to a shell without
   your explicit yes);
2. adding the `mirastacklabs` marketplace and installing the full `mirafleet` plugin;
3. logging in once to your central server;
4. restarting Claude Code and checking the connection with a read-only call.

## What the full plugin adds

The full `mirafleet` plugin gives Claude the mirafleet MCP tools (list fleets and runners, open
isolated workspaces, run allowlisted actions), the `/mirafleet` command, and hooks that tie each
Claude session to its workspaces, including the PreToolUse guard that blocks local file and shell
tools in a remote project. Those parts need a local MCP server, command hooks and a launcher
script, which the Claude plugin directory does not accept from organizations that are not
verified. So the full plugin is installed from our own marketplace
(`mirastacklabs-ai/mirafleet`, plugin `mirafleet@mirastacklabs`), and this plugin is the
directory-friendly way to get there. When the organization is verified, the directory entry will
move to the full plugin.

The two plugins have different names (`mirafleet-setup` and `mirafleet`), so you can have both
installed.

## Requirements

- Claude Code.
- For the full plugin: Node.js 18 or later (`node` on `PATH`) and the `mirafleet` binary on `PATH`.
- A mirafleet central server you can reach, as `HOST:PORT` (default port 2222), and a one-time
  registration code from it. No central yet? Start with the
  [quickstart](https://github.com/mirastacklabs-ai/mirafleet/blob/main/docs/QUICKSTART.md).

## Privacy

This plugin runs nothing and sends nothing. mirafleet has no telemetry; the full plugin and the
binary talk only to the central server you log in to. See
[PRIVACY.md](https://github.com/mirastacklabs-ai/mirafleet/blob/main/PRIVACY.md).

## Links

[mirafleet](https://github.com/mirastacklabs-ai/mirafleet#readme) ·
[full plugin](https://github.com/mirastacklabs-ai/mirafleet/blob/main/integrations/claude-code/plugin/README.md) ·
[support](https://github.com/mirastacklabs-ai/mirafleet/blob/main/SUPPORT.md) ·
[license (Apache-2.0)](https://github.com/mirastacklabs-ai/mirafleet/blob/main/LICENSE)
