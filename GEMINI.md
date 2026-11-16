# mirafleet (Gemini CLI extension)

This extension runs `mirafleet mcp`, an MCP server that lets the agent work on remote servers
("fleets") through a central mirafleet server.

- The `mirafleet` binary must be on `PATH` and you must have run `mirafleet mcp login` once.
  Install and login steps:
  https://github.com/mirastacklabs-ai/mirafleet/blob/main/llms-install.md
- No binary installed? Use the npx form instead, in `~/.gemini/settings.json`:
  `"command": "npx", "args": ["-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "."]`.
- Central enforces `confirm` for guarded fleets; Gemini's tool confirmation is the local gate.
- Docs: https://github.com/mirastacklabs-ai/mirafleet and
  https://github.com/mirastacklabs-ai/mirafleet/blob/main/integrations/gemini/README.md
