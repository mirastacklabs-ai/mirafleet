# mirafleet

Let an AI coding agent drive fleets of servers through one central server, the way a CI server
drives GitLab runners. Works with Claude Code first, and with any MCP-capable harness.

> **Status:** early development (milestone M2 of 6 built). Not ready for production use.

- Design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- Plan: [docs/PLAN.md](docs/PLAN.md)

## Quickstart (current state)

Central and runner run on Linux; `mirafleet mcp` also builds for macOS and Windows. Tokens and
codes are read from stdin (`-`), never passed in argv.

```bash
# On the central host
mirafleet central init -listen 0.0.0.0:2222
mirafleet central run &                          # or run it under your service manager
mirafleet central fleet create uat -mode actions
mirafleet central token create -fleet uat        # prints a join token once

# On each runner
echo "$JOIN_TOKEN" | mirafleet runner join central.example.com:2222 -name jump1 -token -
mirafleet runner run -actions /etc/mirafleet/actions.yaml

# Back on central
mirafleet central runner approve uat/jump1
mirafleet central operator code                  # rotating code, valid for 60 seconds

# On the operator's machine
echo "$REGISTRATION_CODE" | mirafleet mcp login central.example.com:2222 -name alice -code -
mirafleet mcp api fleets.list
mirafleet mcp api action.run '{"fleet":"uat","runner":"jump1","action":"hello"}'
```

Runner lifecycle (`approve`, `disable`, `enable`, `revoke`) and all other admin commands exist
only on central. See [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md#enrollment).

## License

Licensed under the [Apache License, Version 2.0](LICENSE). Copyright 2026 mirastacklabs.
See [NOTICE](NOTICE).

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
