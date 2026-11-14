# Integrations

The core (`mirafleet mcp`) is harness-agnostic and needs no hooks. Harness-specific files live in
one directory each.

| Capability | Any MCP harness (no hooks) | opencode | Claude Code plugin |
|---|---|---|---|
| All MCP tools (fleets, actions, workspaces) | yes | yes | yes |
| Session handoff and `--resume` reattach | no (pass `-session`) | no (pass `-session`) | yes |
| Close workspaces at session end | no (idle TTL reaps) | no (idle TTL reaps) | yes (SessionEnd) |
| Confirm prompt before guarded calls | harness approval prompt | harness approval prompt | yes (PreToolUse ask) |
| Slash command and aliases | DIY from the shared body | `/mirafleet`, `/openfleet` | `/mirafleet`, `/openfleet`, `/claudefleet` |

Central enforces `confirm` on guarded fleets in every case.

- [generic/README.md](generic/README.md): any stdio MCP harness
- [opencode/README.md](opencode/README.md): opencode commands and MCP config
- [commands/mirafleet.md](commands/mirafleet.md): shared command body
- `claude-code/`: Claude Code plugin and hooks
