# mirafleet for Cursor

MCP server entry plus a `/mirafleet` command. No hooks.

## Install

1. Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal.
2. Add the server to `.cursor/mcp.json` (project) or `~/.cursor/mcp.json` (global), see
   [mcp.example.json](mcp.example.json):

   ```json
   {
     "mcpServers": {
       "mirafleet": {
         "type": "stdio",
         "command": "mirafleet",
         "args": ["mcp", "-project-dir", "."]
       }
     }
   }
   ```

   npx form:

   ```json
   {
     "mcpServers": {
       "mirafleet": {
         "type": "stdio",
         "command": "npx",
         "args": ["-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "."]
       }
     }
   }
   ```

3. Copy the command: `mkdir -p .cursor/commands && cp integrations/cursor/commands/mirafleet.md .cursor/commands/`.
   In chat type `/mirafleet` followed by the task. The file is the shared body verbatim; Cursor commands have no
   argument placeholder, so the text you type after the command reaches the model as extra context.

## No hooks

No session handoff (pass `-session <name>`), no close on exit (idle TTL reaps), and Cursor's tool approval is the gate.
Central still enforces `confirm` for guarded fleets. See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

## Docs relied on (checked 2026-11-14)

- https://cursor.com/docs/context/mcp: `mcpServers`, `type: "stdio"`, `command` (on PATH or full path), `args`, `env`, `envFile`; files above.
- Commands: https://docs.cursor.com/en/agent/chat/commands (only a search snippet was readable): plain Markdown files in `.cursor/commands/`,
  file name is the command. Global `~/.cursor/commands/` and "no frontmatter" come from community sources. Cursor 2.4+ can convert commands to skills (`/migrate-to-skills`).
- Not installed here; untested against a real Cursor.
