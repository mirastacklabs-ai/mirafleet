# mirafleet for Codex CLI

MCP server entry plus a `mirafleet` skill. No hooks.

## Install

1. Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal.
2. Add the server to `~/.codex/config.toml` (or `.codex/config.toml` in a trusted project), see
   [config.example.toml](config.example.toml):

   ```toml
   [mcp_servers.mirafleet]
   command = "mirafleet"
   args = ["mcp", "-project-dir", "."]
   ```

   Or without installing the binary first (npx form):

   ```toml
   [mcp_servers.mirafleet]
   command = "npx"
   args = ["-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "."]
   ```

   Equivalent CLI: `codex mcp add mirafleet -- mirafleet mcp -project-dir .`
3. Copy the skill: `cp -r integrations/codex/skills/mirafleet ~/.agents/skills/` (user scope) or into
   `.agents/skills/` in your repo. Restart Codex and ask it to use the `mirafleet` skill, or just describe the task.
   The skill body is the shared command body; the task is whatever you write next to it.

## No hooks

No session handoff (pass `-session <name>` in `args`), no close on exit (idle TTL reaps), and Codex's own
approval prompt is the gate. Central still enforces `confirm` for guarded fleets.
See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

Remote projects (`remote: true` in `.mirafleet.yaml`): no hook blocks Codex's local tools, so the skill body
and the server instructions steer it to the mirafleet `bash`, `read`, `write`, `edit` and friends.

## Docs relied on (checked 2026-11-14)

- https://learn.chatgpt.com/docs/extend/mcp?surface=cli (redirect of https://developers.openai.com/codex/mcp):
  `[mcp_servers.<name>]` table with `command`, `args`, `env`, `cwd`; user file `~/.codex/config.toml`, project file `.codex/config.toml` (trusted projects only).
- https://learn.chatgpt.com/docs/custom-prompts.md: custom prompts (`~/.codex/prompts/`) are deprecated in favour of skills, so we ship a skill.
- Skill location `.agents/skills/<name>/SKILL.md` with `name` and `description` frontmatter comes from secondary sources
  (https://developers.openai.com/codex/skills, partly unreadable here). Unverified against a real Codex; not installed here.
