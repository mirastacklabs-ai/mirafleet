# mirafleet for Cursor (plugin)

Lets Cursor work on your fleets of servers through a self-hosted mirafleet central server.

- MCP server `mirafleet`: `npx -y @mirastacklabs/mirafleet@0.6.1 mcp -project-dir ${workspaceFolder}`.
- A `/mirafleet` command (the shared command body): the model lists fleets first, prefers
  workspaces for builds and tests, and never sets `confirm` without your clear yes.

No hooks. Cursor's tool approval is the local gate; central enforces `confirm` on guarded fleets.

## Prerequisites

- Node.js 18+ (for `npx`).
- A mirafleet central server and an operator login, done once in a terminal:
  `npx -y @mirastacklabs/mirafleet@0.6.1 mcp login` (or `mirafleet mcp login` with the binary).

## Install

- Cursor Marketplace: search for `mirafleet` once it is listed.
- Local test: copy this folder to `~/.cursor/plugins/local/mirafleet`, then run "Reload Window".

The package version in `mcp.json` is pinned to the plugin version. For a server that tracks npm
`latest`, use the "Add to Cursor" button in [../README.md](../README.md).

## Status

Not yet tested in a real Cursor. The layout follows the Cursor plugin reference (checked 2026-11-16).
