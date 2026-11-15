# mirafleet for Cline

MCP server entry plus a `mirafleet` workflow. No hooks.

## Install

1. Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal.
2. In the Cline panel open MCP Servers, **Configure**, **Configure MCP Servers**, and add the entry to
   `cline_mcp_settings.json` (CLI: `~/.cline/data/settings/cline_mcp_settings.json`), see
   [cline_mcp_settings.example.json](cline_mcp_settings.example.json):

   ```json
   {
     "mcpServers": {
       "mirafleet": {
         "command": "mirafleet",
         "args": ["mcp", "-project-dir", "."],
         "disabled": false,
         "autoApprove": []
       }
     }
   }
   ```

   npx form:

   ```json
   {
     "mcpServers": {
       "mirafleet": {
         "command": "npx",
         "args": ["-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "."],
         "disabled": false,
         "autoApprove": []
       }
     }
   }
   ```

   Keep `autoApprove` empty so Cline asks before each mirafleet tool call.
3. Copy the workflow: `mkdir -p .clinerules/workflows && cp integrations/cline/workflows/mirafleet.md .clinerules/workflows/`
   (global workflows live in Cline's Workflows folder under Documents). Type `/mirafleet` in chat and add the task after it.
   The file is the shared body verbatim; workflows have no argument placeholder.

## No hooks

No session handoff (pass `-session <name>`), no close on exit (idle TTL reaps), and Cline's approval prompt is the gate.
Central still enforces `confirm` for guarded fleets. See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

## Docs relied on (checked 2026-11-14)

- https://docs.cline.bot/mcp/configuring-mcp-servers: `mcpServers`, `command`, `args`, `env`, `disabled`, `autoApprove` (the page does not use `alwaysAllow`); CLI path above.
- https://docs.cline.bot/customization/workflows (search result only; page text unreadable here): workflows are Markdown in `.clinerules/workflows/`
  invoked by slash command. Sources disagree on `/name` versus `/name.md`; try the other if one is not listed.
- Not installed here; untested against a real Cline.
