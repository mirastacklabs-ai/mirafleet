# mirafleet for Gemini CLI

MCP server entry plus a `/mirafleet` custom command. No hooks.

## Install

1. Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal.
2. Add the server to `~/.gemini/settings.json` (user) or `.gemini/settings.json` (project), see
   [settings.example.json](settings.example.json):

   ```json
   {
     "mcpServers": {
       "mirafleet": {
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
         "command": "npx",
         "args": ["-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "."]
       }
     }
   }
   ```

3. Copy the command: `mkdir -p .gemini/commands && cp integrations/gemini/commands/mirafleet.toml .gemini/commands/`
   (or `~/.gemini/commands/`). Run `/mirafleet list my fleets`.
   The prompt is the shared body with `$ARGUMENTS` written as Gemini's `{{args}}`.

## No hooks

No session handoff (pass `-session <name>`), no close on exit (idle TTL reaps), and Gemini's tool confirmation is the gate.
Central still enforces `confirm` for guarded fleets. See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

## Docs relied on (checked 2026-11-14)

- https://geminicli.com/docs/tools/mcp-server/: top-level `mcpServers`; stdio uses `command`, `args`, optional `env`, `cwd`, `timeout`, `trust`; files above. Use hyphens, not underscores, in server names.
- https://geminicli.com/docs/cli/custom-commands/: TOML files in `commands/`; `prompt` required, `description` optional; `{{args}}` is the typed text.
- That page notes Gemini CLI is being replaced by Antigravity CLI for some account types (June 2026). Not installed here; untested against a real Gemini CLI.
