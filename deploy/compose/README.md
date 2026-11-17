# Docker Compose

Four ready-to-use setups for the mirafleet image (`docker.io/mirastack/mirafleet`, also on
`ghcr.io/mirastacklabs-ai/mirafleet`). Central and runners run on Linux hosts.

| Directory | What it is |
|---|---|
| [central/](central/README.md) | Central on its own: SSH on 2222 and the admin GUI on 8443. |
| [runner/](runner/README.md) | One actions-mode runner, for a server you want to manage. |
| [runner-workspace/](runner-workspace/README.md) | One workspace-mode runner (container executor) for a Docker host. Root-equivalent on the host. |
| [lab/](lab/README.md) | Central, a runner and an operator on one machine, to try everything. |

To use another image or tag, set `MIRAFLEET_IMAGE` (for example a local build).
The image has no shell. Run admin commands with `docker compose exec central mirafleet central ...`.
