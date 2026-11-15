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

## Remote projects (`remote: true`)

A project whose `.mirafleet.yaml` has `remote: true` does all its work on the remote machine through
central; the model still runs on your laptop. The server exposes tools that replace the harness's
local ones:

| mirafleet tool | Replaces | Notes |
|---|---|---|
| `bash`, `bash_output`, `kill_shell`, `job_list` | Bash and background shells | `run_in_background: true` for builds over about 2 minutes |
| `read`, `write`, `edit`, `multi_edit` | Read, Write, Edit, MultiEdit | `edit` needs a unique `old_string` |
| `grep`, `glob`, `ls`, `notebook_edit` | Grep, Glob, LS, NotebookEdit | run on the remote workspace |
| `read_output` | - | pages long or truncated output |
| `upload`, `download` | - | local project dir and remote, only when asked |

Workflow: `workspace_open`, clone with a plain `git@` or `https://` URL, edit with the remote tools,
long builds as background shells, commit and push from the workspace. Credentials are held by the
runner and injected; the agent never sees them.

Without hooks nothing blocks the harness's local tools in a remote project. The command body and the
server instructions carry the rule (instructions only); you can also disable the local tools in the
harness's own permission settings.

## Works with no hooks. What you lose

- No session handoff: no automatic reattach after a restart; pass `-session` yourself.
- No SessionEnd close: workspaces are reaped by the idle TTL, not on exit.
- No PreToolUse ask on `confirm`: the harness's own tool-approval prompt is the gate. Central still
  enforces `confirm` on guarded fleets.

Compare: [../README.md](../README.md).
