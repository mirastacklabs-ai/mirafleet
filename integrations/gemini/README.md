# mirafleet for Gemini CLI

MCP server entry plus a `/mirafleet` custom command. No hooks.

## Install

Put `mirafleet` on your PATH and run `mirafleet mcp login` once in a terminal. Then either:

**Extension (recommended).** One command adds the MCP server, `GEMINI.md` and the `/mirafleet` command:

```sh
gemini extensions install https://github.com/mirastacklabs-ai/mirafleet
```

It installs the latest GitHub Release (each release carries a small `<platform>.mirafleet.tar.gz`
extension archive: `gemini-extension.json`, `GEMINI.md`, `LICENSE`, `commands/`) and offers updates
when a newer release appears. To pin a release, add `--ref=<release tag>` (for example `--ref=v0.6.1`).
Gemini CLI also lists public repositories with the `gemini-cli-extension` topic in its extension gallery.

**Manual.**

1. Add the server to `~/.gemini/settings.json` (user) or `.gemini/settings.json` (project), see
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

2. Copy the command: `mkdir -p .gemini/commands && cp integrations/gemini/commands/mirafleet.toml .gemini/commands/`
   (or `~/.gemini/commands/`). Run `/mirafleet list my fleets`.
   The prompt is the shared body with `$ARGUMENTS` written as Gemini's `{{args}}`.

## No hooks

No session handoff (pass `-session <name>`), no close on exit (idle TTL reaps), and Gemini's tool confirmation is the gate.
Central still enforces `confirm` for guarded fleets. See [../generic/README.md](../generic/README.md) and the [matrix](../README.md).

Remote projects (`remote: true` in `.mirafleet.yaml`): no hook blocks Gemini's local tools, so the command body
and the server instructions steer it to the mirafleet `bash`, `read`, `write`, `edit` and friends.

## Docs relied on (checked 2026-11-14)

- https://geminicli.com/docs/tools/mcp-server/: top-level `mcpServers`; stdio uses `command`, `args`, optional `env`, `cwd`, `timeout`, `trust`; files above. Use hyphens, not underscores, in server names.
- https://geminicli.com/docs/cli/custom-commands/: TOML files in `commands/`; `prompt` required, `description` optional; `{{args}}` is the typed text.
- https://geminicli.com/docs/extensions/releasing/: GitHub Releases are used when an asset is named `<platform>.<arch>.<name>` or `<platform>.<name>` (`darwin`, `linux`, `win32`) with `gemini-extension.json` at the archive root; a generically named asset is used only if it is the release's only asset, hence one archive per platform. Install takes the Latest release, `--ref` picks a tag.
- That page notes Gemini CLI is being replaced by Antigravity CLI for some account types (June 2026). Not installed here; untested against a real Gemini CLI.
