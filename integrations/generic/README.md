# mirafleet with any MCP harness

`mirafleet mcp` is a stdio MCP server. Any harness that can launch a stdio server works, with no hooks.

## Configure

Command: `mirafleet mcp -project-dir .` (stdio transport, no environment needed). Run
`mirafleet mcp login` once in a terminal first. Use an absolute path for `-project-dir` if the
harness does not start the server in your project.

Typical JSON shape (key names vary by harness):

```json
{ "mcpServers": { "mirafleet": { "command": "mirafleet", "args": ["mcp", "-project-dir", "."] } } }
```

## Flags

- `-session <name>`: fixed session name. Restart the harness with the same name to reattach to your
  open workspaces. Without it, each process gets a random session.
- `-context <name>`: use a named context instead of the project file's default.
- `-fleet <name>`: default fleet for the project.

## Aliases and commands

If the harness has custom commands or prompts, reuse the body in
[../commands/mirafleet.md](../commands/mirafleet.md) (`$ARGUMENTS` is the task). Register it under
`mirafleet`, plus aliases such as `openfleet` if you like. See [../opencode](../opencode/README.md)
for an example.

## Works with no hooks. What you lose

- No session handoff: no automatic reattach after a restart; pass `-session` yourself.
- No SessionEnd close: workspaces are reaped by the idle TTL, not on exit.
- No PreToolUse ask on `confirm`: the harness's own tool-approval prompt is the gate. Central still
  enforces `confirm` on guarded fleets.

Compare: [../README.md](../README.md).
