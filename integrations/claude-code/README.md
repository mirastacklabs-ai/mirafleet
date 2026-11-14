# mirafleet plugin for Claude Code

Gives Claude Code the mirafleet MCP tools (list fleets, workspaces, allowlisted actions), a
`/mirafleet` command and two aliases, and hooks that tie each Claude session to its workspaces.

## Install

```sh
claude --plugin-dir integrations/claude-code
```

A marketplace install arrives in M6. Requirements:

- `mirafleet` must be on `PATH` (release binary or `go install`). The plugin ships no binary;
  `.mcp.json` and the hooks run the bare command `mirafleet`.
- Set up a context once: `mirafleet mcp login ADDR -name NAME -code -` (see the
  [developer guide](../../docs/guide/README.md)).

Commands: `/mirafleet <task>`, `/openfleet <task>`, `/claudefleet <task>`. If another plugin owns the
bare name, use `/mirafleet:mirafleet`, `/mirafleet:openfleet`, `/mirafleet:claudefleet`.

## What is in it

| File | Purpose |
|---|---|
| `.claude-plugin/plugin.json` | Manifest. Version is kept in lockstep with `pluginVersion` in `cmd/mirafleet/mcp_hook.go` by a test. |
| `.mcp.json` | MCP server `mirafleet`: `mirafleet mcp -project-dir ${CLAUDE_PROJECT_DIR}`. |
| `hooks/hooks.json` | SessionStart, SessionEnd (timeout 15), PreToolUse, all exec form. |
| `skills/mirafleet/SKILL.md` | The command body; equals `integrations/commands/mirafleet.md` (tested). |
| `skills/openfleet`, `skills/claudefleet` | Aliases, `disable-model-invocation: true`, each a full copy of the same body (tested byte for byte). |
| `hook/` | Go package behind `mirafleet mcp hook claude-code <event>`. |

Hooks: SessionStart writes a handoff so the MCP server (same parent PID) reuses session
`cc:<session_id>` after `--resume`. SessionEnd removes it and, on reason `clear` or `logout`, closes
that session's workspaces (best effort, 10 s). PreToolUse answers `ask` when `confirm` is set; it
never allows. Central enforces `confirm` regardless of the hook.

## Troubleshooting

- Tools missing or `/mcp` shows `plugin:mirafleet:mirafleet` failed: `which mirafleet`; launch
  Claude Code from a shell where it is on PATH.
- Tools say "not set up": run `mirafleet mcp login` (see above).
- Hooks silent: `claude --debug`; run by hand, e.g.
  `echo '{"session_id":"x","cwd":"."}' | mirafleet mcp hook claude-code session-start`.
- `claude plugin validate integrations/claude-code --strict` should pass.
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
- Hooks run in exec form; if `args` is unsupported, every mirafleet tool is blocked (PreToolUse exits 2).
- `ask` still prompting in auto/bypass-permissions modes (not documented on the pages read).
- Parent PID topology (hook and MCP server both direct children of Claude Code).
- That `${CLAUDE_PROJECT_DIR}` is non-empty in a plugin `.mcp.json` when `--plugin-dir` is used.
- SessionEnd `reason` at the top level of the hook input, and the `tool_input` field names.
- `claude plugin validate --strict` (run on the host by the user).
