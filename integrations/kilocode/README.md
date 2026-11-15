# mirafleet for Kilo Code

MCP server entry plus a `/mirafleet` command. No hooks.

## Install

1. Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal.
2. Add the server under `mcp` in `kilo.jsonc` (project root or `.kilo/kilo.jsonc`) or `~/.config/kilo/kilo.jsonc`, see
   [kilo.example.json](kilo.example.json):

   ```json
   {
     "mcp": {
       "mirafleet": {
         "type": "local",
         "command": ["mirafleet", "mcp", "-project-dir", "."],
         "enabled": true
       }
     }
   }
   ```

   npx form:

   ```json
   {
     "mcp": {
       "mirafleet": {
         "type": "local",
         "command": ["npx", "-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "."],
         "enabled": true
       }
     }
   }
   ```

3. Copy the command: `mkdir -p .kilo/commands && cp integrations/kilocode/commands/mirafleet.md .kilo/commands/`
   (global: `~/.config/kilo/commands/`). Run `/mirafleet` and give the task. The body is the shared body verbatim
   (the docs show no argument placeholder).

## No hooks

No session handoff (pass `-session <name>`), no close on exit (idle TTL reaps), and Kilo's permission prompts are the gate
(do not allow `mirafleet_*` tools blanket). Central still enforces `confirm` for guarded fleets.
See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

## Docs relied on (checked 2026-11-14)

- https://kilo.ai/docs/automate/mcp/using-in-kilo-code: MCP config lives in the main Kilo config under `mcp`; local server is `type: "local"`, `command` array, optional `environment`, `enabled`, `timeout`; `mcpServers` is only the deprecated SSE example. Older Kilo Code (VS Code extension) used `mcp_settings.json` with `mcpServers`; if yours does, use the [generic](../generic/README.md) shape.
- https://kilo.ai/docs/customize/workflows: Markdown commands in `.kilo/commands/` or `~/.config/kilo/commands/`, optional frontmatter `description`, `agent`, `model`, `variant`, `subtask`.
- Not installed here; untested against a real Kilo.
