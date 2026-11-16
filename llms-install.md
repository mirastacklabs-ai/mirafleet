# Install mirafleet for Cline

mirafleet lets an agent work on a fleet of servers through a central mirafleet server. This file
is everything an AI agent needs to install it as an MCP server in Cline. Follow the steps in
order. Do not skip the verification at the end.

## 0. Prerequisites (ask the user)

You need three things from the user. Ask for them before you start; never guess them.

1. **A mirafleet central server the user can reach**, as `HOST:PORT` (default port 2222), for
   example `central.example.com:2222`. If the user has none, stop and point them to
   <https://github.com/mirastacklabs-ai/mirafleet/blob/main/docs/QUICKSTART.md> (section 1,
   Central) and continue once they have one.
2. **A one-time registration code.** The user gets it on the central host with
   `sudo -u mirafleet mirafleet central operator code` (or from the Admin GUI, Operators page).
   It rotates every 60 seconds, so ask for it just before step 2.
3. **A name for this machine**, such as `durga` (used as `-name`).

## 1. Install the binary

Pick the first that fits. The `mirafleet` binary must end up on `PATH`.

| System | Command |
|---|---|
| macOS, Linux (Homebrew) | `brew install mirastacklabs-ai/tap/mirafleet` |
| macOS, Linux (script) | `curl -fsSL https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.sh \| sh` (installs to `~/.local/bin`; add that to `PATH` if needed) |
| Windows (PowerShell 5.1 or 7) | `irm https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.ps1 \| iex` (add `-AddToPath` to update `PATH`) |
| Any OS with Node 18+ and no binary | use the npx form in step 3; step 2 then runs as `npx -y @mirastacklabs/mirafleet mcp login ...` |

Check: `mirafleet version` prints a version.

## 2. Log in once

Send the registration code on stdin, never in the command line:

```bash
echo 'PASTE-THE-CODE' | mirafleet mcp login HOST:PORT -name NAME -code -
```

```powershell
# Windows PowerShell
'PASTE-THE-CODE' | mirafleet mcp login HOST:PORT -name NAME -code -
```

Replace `HOST:PORT`, `NAME` and the code with the user's values. The code carries the hash of
central's host key, so this machine pins central. If it fails, the code probably expired: ask the
user for a fresh one.

## 3. Add the MCP server to Cline

Open Cline's MCP Servers panel, **Configure**, **Configure MCP Servers**, which opens
`cline_mcp_settings.json` (CLI: `~/.cline/data/settings/cline_mcp_settings.json`). Add the entry
inside `mcpServers`, keeping any servers already there:

```json
{
  "mcpServers": {
    "mirafleet": {
      "command": "mirafleet",
      "args": ["mcp", "-project-dir", "."],
      "disabled": false,
      "autoApprove": []
    }
  }
}
```

No binary installed (npx form):

```json
{
  "mcpServers": {
    "mirafleet": {
      "command": "npx",
      "args": ["-y", "@mirastacklabs/mirafleet", "mcp", "-project-dir", "."],
      "disabled": false,
      "autoApprove": []
    }
  }
}
```

Keep `autoApprove` empty so Cline asks the user before each mirafleet tool call.

## 4. Verify (read-only)

In a terminal run:

```bash
mirafleet mcp api fleets.list
```

It must list the fleets on central (a fresh quickstart shows `dev`) and change nothing. Then, in
Cline, confirm the `mirafleet` server shows as connected in the MCP Servers panel and call its
fleets-listing tool (read-only). If both work, the install is done.

## Troubleshooting

- `command not found`: the binary is not on `PATH`; reopen the terminal or fix `PATH`, or use the
  npx form.
- Login fails: fresh registration code (they rotate every 60 seconds); check `HOST:PORT` is
  reachable (central's port 2222 must be open to this machine).
- Guarded fleets ask for `confirm`: that is central enforcing approval; relay it to the user.

More: <https://github.com/mirastacklabs-ai/mirafleet> and
[integrations/cline/README.md](integrations/cline/README.md).
