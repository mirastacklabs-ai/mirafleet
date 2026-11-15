# mirafleet for opencode

Slash commands `/mirafleet` and `/openfleet` plus a local MCP server entry. No hooks.

## Install

1. Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal.
2. Copy the commands. Project scope: `.opencode/commands/`. Global: `~/.config/opencode/commands/`.

   ```sh
   mkdir -p .opencode/commands
   cp integrations/opencode/commands/*.md .opencode/commands/
   ```

3. Add the MCP entry from [opencode.example.json](opencode.example.json) to your `opencode.json`
   (or `opencode.jsonc`):

   ```json
   {
     "$schema": "https://opencode.ai/config.json",
     "mcp": {
       "servers": {
         "mirafleet": {
           "type": "local",
           "command": ["mirafleet", "mcp", "-project-dir", "."]
         }
       }
     }
   }
   ```

   Replace `.` with an absolute project directory if opencode does not start in your project.
   To pin a session or context, append `"-session", "<name>"` or `"-context", "<name>"` to `command`.
4. Restart opencode, run `/mirafleet list my fleets`.

## Remote projects (`remote: true`)

A project whose `.mirafleet.yaml` has `remote: true` does all its work on the remote machine through
central; the model still runs on your laptop. The MCP server exposes tools that replace opencode's
local ones: `bash`, `bash_output`, `kill_shell`, `job_list` (use `run_in_background: true` for builds
over about 2 minutes), `read`, `write`, `edit`, `multi_edit`, `grep`, `glob`, `ls`, `notebook_edit`,
`read_output` (pages long output) and `upload`/`download` (only when asked).

Workflow: `workspace_open`, clone with a plain `git@` or `https://` URL, edit with the remote tools,
run long builds as background shells, commit and push from the workspace. Credentials are held by the
runner and injected; the agent never sees them.

opencode has no hook that blocks its local tools, so nothing stops them in a remote project: the
`/mirafleet` command body and the MCP server instructions carry the rule (instructions only).

## No hooks

opencode has no mirafleet hook integration. Consequences:

- No session handoff: each opencode process gets a random session unless you pass `-session <name>`
  in `command`. A new process does not reattach to the old process's workspaces.
- No close on exit: workspaces are reaped by the idle TTL on central.
- No confirm prompt from the harness: opencode's own tool-approval prompt is the gate. Central still
  enforces `confirm` for guarded fleets.

See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

## Docs relied on (checked 2026-11-13)

- https://opencode.ai/docs/commands
  - Dirs: `.opencode/commands/` and `~/.config/opencode/commands/` (legacy singular `command/` also read).
  - Only `.md` files; file name without `.md` is the command name.
  - Frontmatter keys: `description`, `agent`, `model`, `subagent` (`subtask` deprecated). We use `description` only.
  - The trimmed body is the prompt. `$ARGUMENTS` is the whole argument string.
  - A template cannot include another file (`@path` stays plain text), so `openfleet.md` is a full copy
    of `mirafleet.md`; a test keeps the bodies identical to `integrations/commands/mirafleet.md`.
- https://opencode.ai/docs/mcp-servers
  - Top-level `mcp`; the page documents servers under `mcp.servers.<name>` ("V2 does not place server
    names directly under `mcp`").
  - Local server: `type: "local"`, `command` is an array (executable then arguments).
  - Optional `environment`, `cwd`, `timeout`. The page says to use `disabled`, not `enabled`.

Unverified: the docs we read do not show the older shape `mcp.<name>` with `enabled: true`. If your
opencode is an older release that rejects `mcp.servers`, try that shape instead. We could not test
against a real opencode (it is not installed here), and the relative-path meaning of `.` in
`-project-dir` depends on opencode's working directory for the server process (docs list a `cwd` option).

## Troubleshooting

- Server missing or failing: run `mirafleet mcp -project-dir .` in a terminal; it should wait on stdio.
  Check `mirafleet` is on the PATH opencode sees.
- "not set up" or missing context in tool output: run `mirafleet mcp login`.
- Config rejected: compare with the shape for your opencode version (see Unverified above).
- `/mirafleet` not listed: check the file is in `commands/` (not `command/` or a nested folder) and ends in `.md`.
- Workspaces left behind: they expire by idle TTL; use `workspace_list` and `workspace_close`.
