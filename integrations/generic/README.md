# mirafleet with any MCP harness

`mirafleet mcp` is a stdio MCP server. Any harness that can launch a stdio server works, with no hooks.

## Install

Put `mirafleet` on `PATH` by any channel in the [main README](../../README.md#install):
Homebrew, `install.sh` (macOS, Linux), `install.ps1` (Windows), a release archive or `go install`.
If the harness cannot rely on `PATH`, launch it through npm instead: command `npx`, args
`["-y", "@mirastacklabs/mirafleet@0.6.0", "mcp", "-project-dir", "."]` (on Windows, where the
harness starts servers without a shell, command `cmd` with args `["/c", "npx", ...]`). The npm
package runs no install scripts and downloads nothing.

Tested snippets with the same shape for Codex CLI, Gemini CLI, Cursor, VS Code (Copilot agent),
Cline and Kilo Code are in the sibling directories listed in the [matrix](../README.md).
mirafleet sends no telemetry; the server talks only to the central you logged in to.

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

## Remote projects (host mode)

For a project with `remote: true` in `.mirafleet.yaml`, the command body tells the model to use
only the mirafleet tools against the runner VM. Without Claude Code's hook nothing blocks the
harness's own local tools, so use the harness's tool permissions to keep them off.
<!-- verify: tool names and `remote:` semantics match M7 as merged -->

## Works with no hooks. What you lose

- No session handoff: no automatic reattach after a restart; pass `-session` yourself.
- No SessionEnd close: workspaces are reaped by the idle TTL, not on exit.
- No PreToolUse ask on `confirm`: the harness's own tool-approval prompt is the gate. Central still
  enforces `confirm` on guarded fleets.

Compare: [../README.md](../README.md).
