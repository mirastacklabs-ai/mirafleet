# mirafleet plugin for Claude Code

Gives Claude Code the mirafleet MCP tools (list fleets, workspaces, allowlisted actions), a
`/mirafleet` command and two aliases, and hooks that tie each Claude session to its workspaces.

## Install

From the marketplace (inside Claude Code):

```text
/plugin marketplace add mirastacklabs-ai/mirafleet
/plugin install mirafleet@mirastacklabs
```

The marketplace is `mirastacklabs`; its `mirafleet` entry is pinned to the release tag, so the
plugin version matches the binary release. From a checkout (development):

```sh
claude --plugin-dir integrations/claude-code/plugin
```

Requirements:

- Node.js 18 or later (`node` on `PATH`). It runs only the small launcher `bin/mirafleet.js`.
- `mirafleet` must be on `PATH`. The plugin ships no binary; `.mcp.json` and the hooks run
  `node ${CLAUDE_PLUGIN_ROOT}/bin/mirafleet.js ...`, and the launcher runs the installed
  `mirafleet` with the same arguments. Install it by any channel in the [README](../../../README.md#install):
  Homebrew (`brew install mirastacklabs-ai/tap/mirafleet`), `install.sh` (macOS, Linux),
  `install.ps1` (Windows), the release archive, or `go install`. On Windows use `install.ps1` or
  the zip, not npm: the hooks run in exec form and an npm `.cmd` shim cannot be executed that way.
- Set up a context once: `mirafleet mcp login ADDR -name NAME -code -` (see the
  [quickstart](../../../docs/QUICKSTART.md) and the [developer guide](../../../docs/guide/README.md)).
- Update the plugin and the binary together: the plugin version follows the release tag.

mirafleet sends no telemetry. The plugin and the binary talk only to the central you logged in to.

## Remote projects (host mode)

If the project's `.mirafleet.yaml` says `remote: true`, the skill tells Claude to use only the
mirafleet tools (`bash`, `read`, `write`, `edit`, `grep`, `glob`, ...) against the runner VM, and
a PreToolUse hook denies the local `Bash`, `Read`, `Write`, `Edit`, `MultiEdit`, `Grep`, `Glob`,
`LS` and `NotebookEdit` tools with a reason that names the remote tool. Without the hooks (other harnesses) the skill and the MCP
server instructions carry the same rule, but nothing blocks the local tools.

Do not use `mirafleet` and the local tools on the same remote project: the files live on the VM,
not on your laptop.

Commands: `/mirafleet <task>`, `/openfleet <task>`, `/claudefleet <task>`. If another plugin owns the
bare name, use `/mirafleet:mirafleet`, `/mirafleet:openfleet`, `/mirafleet:claudefleet`.

## What is in it

| File | Purpose |
|---|---|
| `.claude-plugin/plugin.json` | Manifest. Version is kept in lockstep with `pluginVersion` in `cmd/mirafleet/mcp_hook.go` by a test. |
| `.mcp.json` | MCP server `mirafleet`: `node ${CLAUDE_PLUGIN_ROOT}/bin/mirafleet.js mcp`. The server's working directory is the project, which `mirafleet mcp` uses as its project directory by default. |
| `hooks/hooks.json` | SessionStart, SessionEnd (timeout 15), PreToolUse, all exec form through the launcher. |
| `bin/mirafleet.js` | Launcher: finds `mirafleet` on `PATH` and runs it with the same arguments and exit code (no shell, no download). Sets `MIRAFLEET_HARNESS_PID` to Claude Code's PID so the hooks and the MCP server share one handoff key. |
| `skills/mirafleet/SKILL.md` | The command body; equals `integrations/commands/mirafleet.md` (tested). |
| `skills/openfleet`, `skills/claudefleet` | Aliases, `disable-model-invocation: true`, each a full copy of the same body (tested byte for byte). |

The Go package behind `mirafleet mcp hook claude-code <event>` is `integrations/claude-code/hook/`,
outside the plugin folder on purpose: the plugin directory accepts only plugin files, so the plugin
root (`integrations/claude-code/plugin/`) holds no Go source.

Hooks: SessionStart writes a handoff so the MCP server (same parent PID) reuses session
`cc:<session_id>` after `--resume`. SessionEnd removes it and, on reason `clear` or `logout`, closes
that session's workspaces (best effort, 10 s). PreToolUse answers `ask` when `confirm` is set; it
never allows. Central enforces `confirm` regardless of the hook.

## Remote projects (`remote: true`)

If `.mirafleet.yaml` has `remote: true`, a second PreToolUse group (matcher
`Bash|Read|Write|Edit|MultiEdit|Grep|Glob|LS|NotebookEdit`) runs the same `pre-tool-use` hook. It
reads the project file from `${CLAUDE_PROJECT_DIR}` and **denies** the local tool with a reason naming
the mirafleet tool to use (`bash`, `read`, `write`, `edit`, `multi_edit`, `grep`, `glob`, `ls`,
`notebook_edit`). It fails open when there is no project file or `remote` is false, and fails closed
(deny, with the parse error) when the file cannot be read or parsed. The MCP server works without
the hook; the skill carries the remote workflow.

## Troubleshooting

- Tools missing or `/mcp` shows `plugin:mirafleet:mirafleet` failed: `which mirafleet` and
  `node --version` (18 or later); launch Claude Code from a shell where both are on PATH. The
  launcher prints "binary not found on PATH" when `mirafleet` is missing.
- Tools say "not set up": run `mirafleet mcp login` (see above).
- Hooks silent: `claude --debug`; run by hand, e.g.
  `echo '{"session_id":"x","cwd":"."}' | mirafleet mcp hook claude-code session-start`.
- `claude plugin validate integrations/claude-code/plugin --strict` should pass.
- Workspaces left open after exit: only `clear`/`logout` close them; others are reaped by the idle TTL.

## Conventions relied on (docs read 13 Nov 2026)

URLs: https://code.claude.com/docs/en/plugins-reference (redirects to the plugins manifest
reference), https://code.claude.com/docs/en/plugins/components.md,
https://code.claude.com/docs/en/hooks.md, https://code.claude.com/docs/en/skills.md.

- Manifest at `.claude-plugin/plugin.json`; only `name` is required; `author` is an object with
  `name`; `license` is an SPDX id; `homepage` must parse as a URL; `displayName` is allowed.
- Other components sit at the plugin root: `hooks/hooks.json`, `.mcp.json`, `skills/<name>/SKILL.md`.
- `hooks/hooks.json` wraps the event map in a top-level `"hooks"` key.
- Exec-form hooks: with `args` set, `command` is spawned directly with no shell; this applies to plugin
  hooks (the docs show `command` plus `args` in a plugin `hooks.json`). On Windows `command` must
  resolve to a real executable (`mirafleet.exe` qualifies; `.cmd`/`.bat` shims do not).
- `timeout` is in seconds. SessionEnd hooks share a 1.5 s budget, raised to match a longer per-hook
  `timeout` (up to 60 s), hence `timeout: 15`.
- Matcher: only letters, digits, `_ - space , |` means exact strings; anything else is an unanchored
  JavaScript regexp. `^mcp__plugin_mirafleet_mirafleet__.*` contains `^`, `.` and `*`, so it is a regexp; the `^` anchor stops other
  tool names that merely contain the prefix from matching.
- Plugin MCP tool names: `mcp__plugin_<plugin>_<server>__<tool>`; `/mcp` shows `plugin:<plugin>:<server>`.
- `.mcp.json` takes `{"mcpServers": {...}}`; `${CLAUDE_PROJECT_DIR}` and `${CLAUDE_PLUGIN_ROOT}` are
  listed as substituted in MCP stdio `command`, `args` and `env`.
- Skill frontmatter: `description`, `argument-hint`, `disable-model-invocation`. `$ARGUMENTS` is
  substituted; `${CLAUDE_PLUGIN_ROOT}` is substituted in plugin skill bodies. Skill command name is
  `/<plugin>:<directory>`.

## Not verified (for the S17 smoke test)

- Bare `/mirafleet` (without the `mirafleet:` prefix) resolving. The docs only show the
  `/<plugin>:<skill>` form; bare-name resolution is not documented on the pages read.
- Aliases carry the full body (no file indirection), so the confirm and data-not-instructions rules
  never depend on Claude opening another file.
- Minimum Claude Code version for exec-form hooks (`args`): the docs read do not state one. Unverified;
  check against your version.
- Hooks run in exec form. If `args` is unsupported, or `node` or `mirafleet` is missing, the hook
  exits with a code other than 2, which does not block the tool call: the PreToolUse remote guard
  fails open in that case (unchanged from before), and the mirafleet MCP tools are unavailable
  anyway, because the server cannot start.
- `node` shims that start a child process (Volta, scoop or chocolatey on Windows) give the launcher
  a different parent PID than Claude Code, so the hook and the server key the handoff differently
  and `--resume` reattach is lost (fail-safe: a fresh session).
- That Claude Code starts the MCP server with the project root as its working directory (the
  plugin no longer passes `-project-dir`).
- `ask` still prompting in auto/bypass-permissions modes (not documented on the pages read).
- Parent PID topology (hook and MCP server both direct children of Claude Code).
- That `${CLAUDE_PROJECT_DIR}` is non-empty in the hook's environment when `--plugin-dir` is used.
- SessionEnd `reason` at the top level of the hook input, and the `tool_input` field names.
- `claude plugin validate --strict` (run on the host by the user).
