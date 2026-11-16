# Privacy

mirafleet is open source (Apache-2.0) and self-hosted. MIRASTACK LABS operates no mirafleet
service and receives no data from it.

## What mirafleet sends

Nothing to us. mirafleet has no telemetry: no usage data, crash reports or update checks are sent
anywhere. Its only network connections are the ones you configure:

- The orchestrator (`mirafleet mcp`, on your machine) connects to the central you logged in to.
- Runners connect to your central over SSH.
- Central makes one optional outbound connection, HTTPS to your GitLab, only if you turn on
  GitLab token self-rotation for a credential.

## The Claude Code plugin

The plugin sends nothing anywhere. It runs the `mirafleet` binary on your machine, which talks only
to the central you logged in to.

## Where your data lives

Everything mirafleet stores stays on machines you run: central's data directory (database, host
key, GUI certificate), the audit log (on central, with a local copy on each runner), and your
stored credentials. Credentials are encrypted on central, are write-only (never returned by any
method, never in an audit row), and are kept in memory only on the runner a credential is assigned
to. See the [threat model](docs/THREAT-MODEL.md).

## Third-party channels

Downloading mirafleet from GitHub, npm, Docker Hub or Homebrew is governed by those services'
own privacy policies. Your AI harness (for example Claude Code) has its own policy for what it
sends to its provider.

## Questions

Ask in [Discussions](https://github.com/mirastacklabs-ai/mirafleet/discussions) (see
[SUPPORT.md](SUPPORT.md)). Security issues go through the private process in
[SECURITY.md](SECURITY.md), never a public issue.
