# mirafleet for VS Code (Copilot agent mode)

MCP server entry plus a `/mirafleet` prompt file. No hooks.

## Install

1. Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal.
2. Create `.vscode/mcp.json` in the workspace (or run **MCP: Add Server** from the Command Palette), see
   [mcp.example.json](mcp.example.json):

   ```json
   {
     "servers": {
       "mirafleet": {
         "type": "stdio",
         "command": "mirafleet",
         "args": ["mcp", "-project-dir", "${workspaceFolder}"]
       }
     }
   }
   ```

   npx form:

   ```json
   {
     "servers": {
       "mirafleet": {
         "type": "stdio",
         "command": "npx",
         "args": ["-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "${workspaceFolder}"]
       }
     }
   }
   ```

3. Copy the prompt: `mkdir -p .github/prompts && cp integrations/vscode/prompts/mirafleet.prompt.md .github/prompts/`.
   In Copilot Chat (agent mode) run `/mirafleet`; VS Code asks for the task (`${input:task}`).
4. Start the server from the **MCP: List Servers** view if it does not start itself, and trust it when prompted.

## No hooks

No session handoff (pass `-session <name>`), no close on exit (idle TTL reaps), and VS Code's tool approval is the gate.
Central still enforces `confirm` for guarded fleets. See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

## Docs relied on (checked 2026-11-14)

- https://code.visualstudio.com/docs/agents/reference/mcp-configuration: `servers` map with `type: "stdio"`, `command`, `args`, optional `cwd`, `env`.
  The page marks this format deprecated for the Local harness and recommends `mcpServers` in a workspace-root `.mcp.json`;
  the same stdio entry shape as [../generic/README.md](../generic/README.md) works there. We ship `.vscode/mcp.json`, which still works.
- https://code.visualstudio.com/docs/copilot/customization/prompt-files: `.github/prompts/*.prompt.md`; frontmatter `description`, `agent`, `tools`
  (`<server>/*` selects all tools of an MCP server); `${input:name:placeholder}` asks for input. The page says prompt files run only in Local
  harness sessions, which are deprecated and removed in February 2027; agent skills are the recommended successor.
- Not run in a real VS Code here.
