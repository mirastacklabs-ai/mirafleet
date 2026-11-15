# mirafleet

Let an AI coding agent drive fleets of servers through one central server, the way a CI server
drives GitLab runners. Works with Claude Code first, and with any MCP-capable harness.

> **Status:** v0.6.0, the first release (milestones M1 to M7 are built). Pre-1.0: the protocol and CLI can still change between minor versions. Read the
> [threat model](docs/THREAT-MODEL.md) before you point it at anything important.

- Quickstart (a fresh Linux VM plus your laptop): [docs/QUICKSTART.md](docs/QUICKSTART.md)
- Design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md) · Plan: [docs/PLAN.md](docs/PLAN.md)
- Threat model: [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md) · Security policy: [SECURITY.md](SECURITY.md)
- Developer guide: [docs/guide/](docs/guide/README.md) · also on the [wiki](https://github.com/mirastacklabs-ai/mirafleet/wiki)

## What it is

- **Central** is one Linux server. Runners dial out to it; the agent talks to it.
- **Runners** run on the servers you want to manage. In *actions* mode a runner executes only the
  named, argv-only, regex-checked actions in its own `actions.yaml`. In *workspace* mode the
  agent gets a private container. In *host* mode it works as an ordinary user on a dedicated
  VM, with credentials that stay on central and never reach the agent.
- **The orchestrator** (`mirafleet mcp`) is an MCP server on your laptop (macOS, Linux or
  Windows). Every request is audited; `confirm` guards protect production fleets.
- An **Admin GUI** (HTTPS, served by central) manages fleets, runners, tokens, operators,
  credentials and the audit log.

## No telemetry

mirafleet has no telemetry: no usage data, crash reports or update checks are sent anywhere. Its
only network connections are the ones you configure. Central makes one optional outbound
connection, HTTPS to your GitLab, only if you turn on GitLab token self-rotation for a credential.

## Install

One binary, `mirafleet`, runs as `central`, `runner` or `mcp`. Central and runners are Linux only;
the orchestrator side also runs on macOS and Windows (amd64 and arm64). Pick one channel per
machine. Versions below are `0.6.0`; replace them with the release you want.
Pre-releases (`-rc.N`) are not published to Homebrew or the MCP Registry, and go to npm dist-tag `next`.

| Where | Channel | Command |
|---|---|---|
| Central or runner (Linux) | Package (deb, rpm, apk) from the [release page](https://github.com/mirastacklabs-ai/mirafleet/releases) | `sudo apt install ./mirafleet_0.6.0_linux_amd64.deb` · `sudo dnf install ./mirafleet_0.6.0_linux_amd64.rpm` · `sudo apk add --allow-untrusted ./mirafleet_0.6.0_linux_amd64.apk` |
| Central or actions runner (Linux) | Docker Compose (`deploy/compose/`) | `docker compose up -d` in `deploy/compose/central` or `deploy/compose/runner` |
| Central or actions runner (Linux) | OCI image | `docker pull mirastack/mirafleet:0.6.0` (also `ghcr.io/mirastacklabs-ai/mirafleet:0.6.0`, same digest) |
| macOS, Linux | Homebrew | `brew install mirastacklabs-ai/tap/mirafleet` |
| macOS, Linux | Script | `curl -fsSL https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.sh \| sh` |
| Windows | Script (PowerShell 5.1 or 7, no admin) | `irm https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.ps1 \| iex` |
| Any OS with Node 18+ | npm / npx (`@mirastacklabs/mirafleet`) | `npx -y @mirastacklabs/mirafleet@0.6.0 mcp -project-dir .` |
| Claude Code | Marketplace plugin (the binary must be on `PATH` too) | `/plugin marketplace add mirastacklabs-ai/mirafleet`, then `/plugin install mirafleet@mirastacklabs` |
| Anywhere | Archive | `mirafleet_0.6.0_<os>_<arch>.tar.gz` (`.zip` on Windows) |
| Go developers | `go install` | `go install github.com/mirastacklabs-ai/mirafleet/cmd/mirafleet@v0.6.0` |

Notes:

- **Verify what you download.** Every release has `checksums.txt` with a keyless cosign signature
  and GitHub build provenance. `install.sh` and `install.ps1` always check the SHA-256;
  `install.sh --verify-signature` also requires the cosign signature. For other downloads run
  `sha256sum -c checksums.txt` and, if you use GitHub's tooling,
  `gh attestation verify FILE --repo mirastacklabs-ai/mirafleet`.
- **Packages** install `/usr/bin/mirafleet`, systemd units (deb and rpm), `/etc/mirafleet/*.env`
  and the docs in `/usr/share/doc/mirafleet/`. They create the `mirafleet` system user, never
  enable a service on their own, never delete data on removal, and do not change your firewall.
  apk (Alpine) ships the binary and config but no service files.
- **No Apple or Microsoft code signing.** Homebrew, npm, `install.sh` and `install.ps1` are not
  affected by quarantine. An archive downloaded in a browser is: after checking its checksum run
  `xattr -d com.apple.quarantine mirafleet` (macOS) or `Unblock-File .\mirafleet.exe` (Windows).
- **Windows and the Claude Code plugin:** its hooks need a real `mirafleet.exe` on `PATH` (use
  `install.ps1` or the zip). The npm launcher is for plain MCP configs.
- Other harnesses (Codex CLI, Gemini CLI, Cursor, VS Code Copilot, Cline, Kilo Code, opencode,
  anything that speaks stdio MCP): see [integrations/](integrations/README.md).

## Quickstart in ten lines

Central and runner run on Linux; tokens and codes are read from stdin (`-`), never passed in
argv. The [full quickstart](docs/QUICKSTART.md) covers firewalls, the GUI first-run setup,
runners, host mode and upgrades.

```bash
# On the central host (package installed)
sudo systemctl enable --now mirafleet-central       # the journal shows the GUI fingerprint and setup-token path
sudo -u mirafleet mirafleet central fleet create dev -mode actions
sudo -u mirafleet mirafleet central token create -fleet dev          # join token, shown once
# On a runner (after: sudo cp /usr/share/doc/mirafleet/examples/actions.yaml /etc/mirafleet/)
echo "$JOIN_TOKEN" | sudo -u mirafleet mirafleet runner join central.example.com:2222 -name web-1 -data /var/lib/mirafleet/runner -token -
sudo systemctl enable --now mirafleet-runner
# Back on central
sudo -u mirafleet mirafleet central runner approve dev/web-1
sudo -u mirafleet mirafleet central operator code                    # valid for 60 seconds
# On your laptop
echo "$REGISTRATION_CODE" | mirafleet mcp login central.example.com:2222 -name durga -code -
mirafleet mcp api fleets.list
mirafleet mcp api action.run '{"fleet":"dev","runner":"web-1","action":"uptime"}'
```

Then open the Admin GUI at `https://central.example.com:8443` (check the certificate
fingerprint against the central log first) and add the plugin or MCP config for your harness.

## Safety in one paragraph

The runner re-checks every request against its own allowlist, so even a compromised central
cannot make it run anything else. Workspaces and host mode give an agent room to work and still
keep it away from your other machines and, in host mode, away from your secrets. Read what is
**not** protected in [docs/THREAT-MODEL.md](docs/THREAT-MODEL.md): for example root on a runner
VM, a compromised central, and (in host mode) a determined agent that reads its own runner's
enrolment key.

## License

Licensed under the [Apache License, Version 2.0](LICENSE). Copyright 2026 mirastacklabs.
See [NOTICE](NOTICE). Third-party licences are in `THIRD_PARTY_LICENSES.txt` in every archive,
package and image.

## Disclaimer

mirafleet is software that runs commands on servers, and it can be driven by AI agents. Read this
before you use it.

- **No warranty.** mirafleet is provided "AS IS", without warranties or conditions of any kind,
  express or implied, as set out in Section 7 of the License.
- **No liability.** In no event shall mirastacklabs, the authors or the contributors be liable
  for any damages arising from the use, misuse or inability to use mirafleet. This includes,
  without limitation, outages, data loss or corruption, security incidents, unauthorized access,
  and any action taken by a person or an AI agent through mirafleet, as set out in Section 8 of
  the License.
- **Misuse is the user's responsibility.** Using mirafleet against systems you do not own or are
  not authorized to operate, or for any unlawful purpose, is prohibited. Any such use is solely
  the responsibility of the person or organization doing it.
- **You control what it can do.** You are responsible for how you deploy and configure mirafleet:
  who can reach central, which operators and runners you approve, the actions you allow, the
  container images you use, and the permissions you grant to AI agents.
- **AI agents can make mistakes.** An agent may misread instructions or be manipulated by
  content it reads, such as command output (prompt injection). Keep production fleets in
  `actions` mode, use the `confirm` guards, and review what an agent is about to run.
- **Your obligations stay yours.** You are responsible for complying with applicable laws,
  your organization's policies, and the terms of any AI provider you use with mirafleet.

If you do not accept these terms, do not use mirafleet.
