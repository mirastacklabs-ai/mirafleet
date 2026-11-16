# Contributing to mirafleet

Thanks for helping. This page covers what you need to send a change. For how the code is
organised, read the [developer guide](docs/guide/README.md) and
[docs/ARCHITECTURE.md](docs/ARCHITECTURE.md). Please follow the
[Code of Conduct](CODE_OF_CONDUCT.md). Security problems go to private reporting, see
[SECURITY.md](SECURITY.md).

## Ways to help

Code is one way. You can also [test an early build](docs/TESTING.md) and file a test report,
improve the docs, answer questions in
[Discussions](https://github.com/mirastacklabs-ai/mirafleet/discussions), or report bugs.
Where to ask what: [SUPPORT.md](SUPPORT.md).

## Before you start

- Small fixes: open a pull request directly.
- Anything bigger (a feature, a new dependency, a protocol change): open an issue first so we can
  agree on the approach. Milestones are in [docs/PLAN.md](docs/PLAN.md). Enterprise features
  (RBAC, SSO, HA) are out of scope for the open source tree.
- New dependencies need a maintainer's yes before you add them (see docs/PLAN.md).

## Sign off every commit (DCO)

mirafleet uses the [Developer Certificate of Origin](https://developercertificate.org/) (DCO).
It is a statement that you wrote the change, or have the right to submit it under the project's
licence (Apache-2.0). Add a `Signed-off-by` line with your real name and the email you commit
with:

```bash
git commit -s -m "fix: reject empty runner name"
```

That adds `Signed-off-by: Your Name <you@example.com>`. Forgot? Fix the last commit with
`git commit --amend -s --no-edit`, or a whole branch with `git rebase --signoff main`.

A CI check (`dco`) fails the pull request if any commit lacks a sign-off that matches its
author (same name and email as the commit's author). Every commit between `main` and your branch
is checked, merge commits included, and nobody is exempt, bots included: keep your branch rebased
instead of merging `main` in.

## Develop in containers only

Nothing is installed on your machine for this repo: no host `go`, npm or pip. Every step runs in a
digest-pinned container through `scripts/dev.sh`, using the Docker CLI (on macOS, Colima's
Docker runtime: `colima start`). Docker Engine and Compose are fine; do not add a hard
dependency on Docker's commercial products.

| Task | Command |
|---|---|
| Build | `scripts/dev.sh build` |
| Test (`go test -race`) | `scripts/dev.sh test` |
| Vet / format | `scripts/dev.sh vet`, `scripts/dev.sh fmt` |
| Cross-compile for all six OS/arch targets | `scripts/dev.sh cross` |
| Anything else | `scripts/dev.sh go <args>`, or `scripts/dev.sh shell` |

New tools (linters, scanners) run as pinned container images, never host installs.

## Rules for code

- One static Go binary; `CGO_ENABLED=0` must keep building.
- Central and runner are Linux only. OS-specific code goes behind build tags and fails closed
  elsewhere, never a silent no-op on a security check.
- The runner-side actions guard is the security boundary: argv only, no shell, regex-validated
  parameters.
- No telemetry and no unconfigured outbound connections, ever.
- Keep the harness-agnostic core clean: harness-specific code lives in `integrations/<harness>/`.
- Follow [docs/BRAND.md](docs/BRAND.md) for the GUI, CLI colours and docs.

## Tests

- Every change that alters behaviour comes with a test.
- Tests touch no real hosts, ports or data: use ephemeral ports, `t.TempDir()` and keys
  generated at test time. Override the database dependency; never write to a real store.
- containerd tests use the build tags `linux && integration` and run in a container.
- Run `scripts/dev.sh test`, `vet` and `fmt` before you push.

## Licences

All dependencies, transitive ones included, and any bundled binary or asset must be permissive:
Apache-2.0, MIT, MIT-0, BSD-2-Clause, BSD-3-Clause, ISC or 0BSD. Never GPL, LGPL, AGPL, MPL, SSPL,
BUSL or other copyleft or source-available terms. The only exception is font files under SIL
OFL-1.1, shipped with their licence text. `scripts/dev.sh licenses` checks this.

## Documentation

Update the docs in the same pull request. User-visible behaviour changes go in the README or
the relevant doc; design changes go in docs/ARCHITECTURE.md; the developer guide
([docs/guide/](docs/guide/README.md)) follows [docs/guide/STYLE.md](docs/guide/STYLE.md). Use the
MIRASTACK LABS running example (aarvee, raj, durga, kalyan, sid; `central.example.com`), never
real hostnames, people or secrets.

## How pull requests land

1. Fork, branch from `main`, make focused commits (each signed off).
2. Open a pull request and fill in the template.
3. Required checks must pass: `gate`, `licenses`, `lint`, `dco` and the `orchestrator` jobs.
4. A maintainer reviews. Expect questions on security and tests first.
5. When approved and green, a maintainer merges it (squash or rebase; the sign-off is kept).
   Releases are cut by maintainers, see [docs/RELEASING.md](docs/RELEASING.md).

By contributing you agree your work is licensed under the [Apache License 2.0](LICENSE).
