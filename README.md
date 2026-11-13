# mirafleet

Let an AI coding agent drive fleets of servers through one central server, the way a CI server
drives GitLab runners. Works with Claude Code first, and with any MCP-capable harness.

> **Status:** early development (milestone M1 of 6). Not ready for production use.

- Design: [docs/ARCHITECTURE.md](docs/ARCHITECTURE.md)
- Plan: [docs/PLAN.md](docs/PLAN.md)

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
