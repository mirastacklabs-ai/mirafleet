# Releasing mirafleet

For maintainers. The design is in [M6.md](M6.md) (sections 2.16, 7, 8, 9); this page is the
checklist. The first release is `v0.6.0`, rehearsed as `v0.6.0-rc.1`.

**Rule for everyone, agents included:** a tag push, an approval in the `release` environment,
a token, a registry change and a wiki push each need the maintainer's explicit yes at that
moment. Never `--force` a tag. Nothing here runs by itself except `release.yml` on a pushed tag.

## 1. What a release is

One git tag `vX.Y.Z` (or `vX.Y.Z-rc.N`) on `main` produces every channel from the same build:

| Channel | Produced by job | Notes |
|---|---|---|
| GitHub release: archives (6 targets), `.deb` `.rpm` `.apk`, `checksums.txt` (+ `checksums.txt.sigstore.json`), SBOMs, `install.sh`, `install.ps1`, `THIRD_PARTY_LICENSES.txt` | `build`, `draft`, `publish-release` | cosign keyless signature of `checksums.txt`; build provenance for every archive and package |
| Gemini CLI extension: `darwin.mirafleet.tar.gz`, `linux.mirafleet.tar.gz`, `win32.mirafleet.tar.gz` (identical; no binary inside) | `build` (goreleaser) | listed in `checksums.txt`, so signed and attested with the rest; one per platform because Gemini uses a generic name only for a single-asset release |
| OCI image `docker.io/mirastack/mirafleet` and `ghcr.io/mirastacklabs-ai/mirafleet` | `image` | one multi-arch build, same digest in both; cosign + provenance for both. Tags `X.Y.Z`, `X.Y`, `latest`; a pre-release gets only `X.Y.Z-rc.N` |
| npm `@mirastacklabs/mirafleet` + six platform packages | `npm` | stages the 7 packages (`npm stage publish`); the maintainer approves them on npmjs.com with 2FA; provenance; pre-releases go to dist-tag `next` |
| Homebrew tap `mirastacklabs-ai/homebrew-tap` | `brew` | not for pre-releases |
| MCP Registry `io.github.mirastacklabs-ai/mirafleet` | `mcp-registry` | after `npm`; not for pre-releases |

Job order: `verify` -> `build` -> `draft` -> `publish-release` -> (`image`, `npm`, `brew`) ->
`mcp-registry`. `verify` and `build` hold no credentials. `draft`, `publish-release`, `image`,
`npm`, `brew` and `mcp-registry` all run in the protected `release` environment, so each waits for
the reviewer to approve it in the GitHub UI. Each credential is used by one job only.
Environment approvals are made by the user in the GitHub UI; an agent never approves a
deployment, not even after a yes.

## 2. Accounts and secrets to set up first (M6.md section 9, step P1)

Names only; never put a value in a file or a chat. Nothing local needs a secret; if a rehearsal
ever does, use the git-ignored `.env.local`.

| What | Where | Used by |
|---|---|---|
| Ruleset on `main`: PR required, required checks (`gate`, `licenses`, `lint`, `dco`, and the two orchestrator checks `windows-latest` and `macos-latest`), no direct push; allow auto-merge, delete head branches, squash only | repo settings | all changes (section 4) |
| Ruleset on tags `v*`: only admins create, update or delete | repo settings | keeps releases deliberate |
| Environment `release`: required reviewer = the maintainer; deployment tags limited to `v*` | repo settings | every publishing job |
| Immutable releases (if offered), private vulnerability reporting, wiki enabled | repo settings | |
| Repo `mirastacklabs-ai/homebrew-tap` (public, folder `Formula/`) | GitHub | `brew` |
| Secret `HOMEBREW_TAP_TOKEN`: fine-grained PAT, contents read/write on the tap only, with an expiry | `release` environment secret | `brew` only |
| Docker Hub namespace `mirastack`, public repo `mirafleet` | hub.docker.com | `image` |
| Variable `DOCKERHUB_USERNAME` and secret `DOCKERHUB_TOKEN`: a personal access token with Read & Write scope (works on the free plan; an Organization Access Token is an option only if the org's plan has it, never a requirement) | `release` environment | `image` only |
| npm account with 2FA and org `mirastacklabs`. After the first publish, a trusted publisher for each of the 7 packages (repository `mirastacklabs-ai/mirafleet`, workflow `release.yml`, environment `release`, allowed action `npm stage publish` only), then package publishing access "Require 2FA and disallow tokens" | npmjs.com | `npm` |
| Secret `NPM_TOKEN` (first publish only, packages do not exist yet so no trusted publisher can be set): a granular token with "Read and write (stage only)", no 2FA bypass. Delete it once the trusted publishers exist | `release` environment secret | `npm` |
| ghcr, cosign, provenance, MCP Registry | none | GitHub OIDC and the job's `GITHUB_TOKEN`; no secret |
| Anthropic plugin directory, galleries, Docker-Sponsored OSS | each site | steps P9 to P11 |

After the first image push, make the ghcr package public and link it to the repo (P6). No Apple
or Windows code-signing account is used: the docs give quarantine one-liners instead.

## 3. Runbook

Check the preconditions, then go through the steps. Each step marked **yes** needs the
maintainer's explicit go-ahead.

**Before any tag**
1. `main` is green: required checks, and the slow CI jobs (`integration`, `dist`, `e2e-systemd`)
   on the commit you will tag. `release.yml` re-checks this and stops if any is missing or red.
2. `scripts/release.sh m6check` is green locally (or its steps ran in CI).
3. The version bump to the exact tag is merged: `scripts/dev.sh go run ./tools/relgen bump X.Y.Z`
   (`X.Y.Z-rc.N` for a rehearsal tag), committed on a branch, merged by PR (section 4).
   `relgen check vX.Y.Z[-rc.N]` must pass; the workflow's `verify` job runs it again and stops on a
   mismatch. A tag that failed `verify` is never moved: bump to the next rc and tag that.
   The bump also rewrites the marketplace entry's `source.path` to `integrations/claude-code/plugin`
   together with `source.ref`: the path exists only from the first tag after v0.6.0, so until that
   bump `main` keeps the old path with `ref: v0.6.0`, which existing installs rely on.
4. The accounts in section 2 exist (P1).

**Rehearsal: `v0.6.0-rc.1`**
5. **yes** Tag the merge commit on `main` and push the tag (P2):
   `git tag -a v0.6.0-rc.1 -m "v0.6.0-rc.1" <sha> && git push origin v0.6.0-rc.1`.
6. `verify` and `build` run by themselves. When `draft` is waiting, **yes** approve it (P3): it
   signs `checksums.txt` and creates the draft release.
7. Check the draft: assets present, `checksums.txt` signature and provenance:
   `gh release download v0.6.0-rc.1 -D /tmp/rc` (in a scratch directory), then
   `sha256sum -c checksums.txt`, `gh attestation verify <file> --repo mirastacklabs-ai/mirafleet`
   and `install.sh -v v0.6.0-rc.1 --verify-signature` (it runs `cosign verify-blob` with the exact identity).
8. **yes** Approve `publish-release`, then `image` and `npm`. `npm` only stages the 7 packages.
   **yes** Approve the staged versions at <https://www.npmjs.com/settings/mirastacklabs/staged-packages>
   (batch approve, one 2FA), or `npm stage approve <id>`. npm versions are burned once published
   (unpublish works only within 72 hours), so check the draft first. A package that did not exist
   gets a public placeholder `0.0.0-stage` on its first stage. `brew` and `mcp-registry` are
   skipped for a pre-release.
9. Verify: `install.sh -v v0.6.0-rc.1 --verify-signature` in a fresh container;
   `cosign verify` on both image names and compare the digests; `npx -y @mirastacklabs/mirafleet@0.6.0-rc.1 version`.

**Release: `v0.6.0`**
10. Fix anything the rehearsal found, merge it, and merge `relgen bump X.Y.Z` (step 3).
11. **yes** Tag and push `v0.6.0` (P4), same commands as step 5.
12. **yes** Approve the jobs in order (P5): `draft`, `publish-release`, `image`, `npm`, `brew`.
    Then **yes** approve the staged npm packages on npmjs.com (step 8). Only after that, approve
    `mcp-registry`: it fails with a clear message if the npm version is not live yet, and you
    re-run it. The registry version is effectively permanent.
    After the first publish, add a trusted publisher to each of the 7 packages (allowed action
    `npm stage publish` only), delete `NPM_TOKEN`, and set each package's publishing access to
    "Require 2FA and disallow tokens".
13. Post-publish checks: `install.sh --verify-signature`; `gh attestation verify`;
    `cosign verify` on both registries with the same digest; `brew install mirastacklabs-ai/tap/mirafleet`;
    `npx` on three operating systems; `docker compose` from the published image;
    `go install github.com/mirastacklabs-ai/mirafleet/cmd/mirafleet@v0.6.0`; the registry lists
    the server; the marketplace install works.

**After**
14. Docker Hub repo description and README, ghcr package public and linked (P6).
15. Wiki: you create the first page, then **yes** to publishing `docs/guide` and the quickstart (P7, P8).
16. Submissions, one at a time: Anthropic plugin directory (P9; submit `integrations/claude-code/directory`,
    the slim `mirafleet-setup` plugin, not the full one: see docs/M6.md section 2.18), galleries (P10), Docker-Sponsored
    OSS (section 7; P11), mirastacklabs.ai one-pager and announcement (P12).
17. `scripts/release.sh clean`.

If a job fails, fix the cause and re-run the failed job from the Actions page. A re-run of `npm`
skips versions that are already published (staged but unapproved versions are not published yet,
so approve or reject them on npmjs.com first); the other publishing jobs are safe to repeat only
before they have pushed anything, so read the log first.

## Every release

After the tag is pushed (each item needs the maintainer; an agent never approves a deployment):

1. Approve the release jobs in the GitHub UI, in order: `draft`, `publish-release`, `image`, `npm`, `brew`, then `mcp-registry`.
2. Approve the staged npm packages with 2FA at <https://www.npmjs.com/settings/mirastacklabs/staged-packages>.
   `mcp-registry` fails until npm is live; re-run that job after the approval.
3. Verify: GitHub release (assets, `checksums.txt` signature, attestations), Docker Hub and GHCR
   (same digest), npm (`npx -y @mirastacklabs/mirafleet@X.Y.Z version`), Homebrew, MCP Registry.
   Then replace the release body ("Draft. Verify...") with notes written by hand
   (`gh release edit vX.Y.Z --notes-file FILE`): goreleaser's changelog is off.
4. Claude Code marketplace: nothing. `relgen bump` already set `source.path` and `source.ref` to the tag.
5. Claude directory: portal, then each submission (`mirafleet-setup`; `mirafleet` once Verified), Settings,
   "Tracked branch or tag" = `vX.Y.Z`, Save, "Check for new commits" (else Publish says
   "Version isn't ready"), wait for the scan, Publish. The ref cannot be changed while a
   version is with a reviewer, and changing it cancels a waiting publish request.
6. Gemini: nothing. The release ships `darwin|linux|win32.mirafleet.tar.gz` (built by goreleaser, signed via
   `checksums.txt`, version checked by `relgen gemini-check`); `gemini extensions install` takes the Latest release.
7. Kilo: PR to the Kilo marketplace repo updating the pinned npx version in `mcps/mirafleet/MCP.yaml`;
   regenerate `marketplace.yaml` and run their tests in a node container.
8. Wiki: regenerate with `relgen wiki -out DIR -commit SHA` (see M6.md), then push it (**yes**).
9. Update `docs/HANDOFF.md`.
10. Now and then, re-check the directories that feed themselves from the registry or GitHub (Glama and similar).

Calendar: `HOMEBREW_TAP_TOKEN` expires on 15 Dec 2026; renew it before then or `brew` fails.

## 4. Pull requests and auto-merge

Once the ruleset is on, nobody pushes to `main`. Every change, by a person or an agent:

1. `git switch -c m6/<step>` (a short-lived branch).
2. Commit with `-s` (section 5).
3. `git push -u origin m6/<step>`
4. `gh pr create`
5. `gh pr merge --auto --squash --delete-branch`

The PR merges by itself when the required checks are green; the branch is deleted. This is a
routine push to our own repo. Tags are never created by this flow.

## 5. DCO

Every commit needs a `Signed-off-by:` line that matches its author: `git commit -s`. The `dco`
check (`.github/workflows/dco.yml`, a script over `git log`) fails the PR otherwise, merge commits
included. Fix a missing sign-off with `git commit --amend -s --no-edit` (last commit) or
`git rebase --signoff main` (a branch you own). Squash merges keep the sign-off of the PR's
commits in the message. See [CONTRIBUTING.md](../CONTRIBUTING.md).

## 6. Rollback

**A bad release** (published artifacts cannot be recalled; make the next version good). Every
action below needs the maintainer's explicit **yes**:
- **yes** GitHub release: edit it back to a draft, or delete it, and say why in the notes. With
  immutable releases, leave it published, note it in the release notes, and ship Z+1. Delete the
  tag only if no registry has been published from it; never force-move a tag. Ship `vX.Y.(Z+1)`.
- **yes** npm: `npm deprecate @mirastacklabs/mirafleet@X.Y.Z "use X.Y.(Z+1)"` (and the platform
  packages). `npm unpublish` works only within 72 hours and burns the version.
- **yes** Image: move `latest` by pushing the fixed version; old digests stay. Document the bad tag.
- **yes** Homebrew: revert the commit in the tap. **yes** MCP Registry: publish the fixed version.

**A bad upgrade on a server** (central backs up its SQLite database before every migration and
keeps the 3 newest in `<data>/backups/`):
1. Stop central.
2. Reinstall the previous version.
3. Move `mirafleet.db`, `mirafleet.db-wal` and `mirafleet.db-shm` aside.
4. Copy the newest `backups/mirafleet-schema<N>-<time>.db` back as `mirafleet.db`.
5. Start central.

Writes made after the upgrade are lost. A restore needs the same `MIRAFLEET_SECRETS_KEY` as the
backup. Upgrade order is central first, then runners, then operators.

## 7. Docker-Sponsored Open Source application (P11)

Apply after `v0.6.0` is live, from the Docker Hub organization `mirastack`, at
<https://www.docker.com/community/open-source/application/>. The form changes; the text below
answers its usual questions. Fill the contact fields yourself.

> **Project name:** mirafleet
>
> **Short description:** mirafleet lets Claude Code (and other MCP agents) operate fleets of
> servers through a central server. Runners on each server connect out to central over SSH;
> every action runs on the runner through an allowlist guard (argv only, no shell,
> regex-validated parameters), and every call is audited.
>
> **Repository:** https://github.com/mirastacklabs-ai/mirafleet
>
> **Licence:** Apache-2.0, with a permissive-only dependency policy (Apache-2.0, MIT, BSD, ISC).
>
> **Docker Hub:** docker.io/mirastack/mirafleet (public, multi-arch: linux/amd64 and linux/arm64).
> The same image digest is also published to ghcr.io/mirastacklabs-ai/mirafleet.
>
> **How we use Docker:** the official image is a static binary on `FROM scratch`, runs as a
> non-root user (65532), and is signed with cosign (keyless) with build provenance attached.
> Docker Compose files for central, runners and a one-machine lab are first-class installs. Our
> CI and release builds run in digest-pinned containers.
>
> **Why the sponsorship:** the project is community-driven and non-commercial. Free, unthrottled
> pulls let users and CI systems install central and runners from Docker Hub without rate
> limits, and the Docker-Sponsored Open Source badge helps users find an image that is signed,
> built from tagged source with published provenance.
>
> **Commercial use:** none. There is no paid tier or hosted service; the project does not
> collect telemetry. Enterprise features are out of scope for the open-source edition.
>
> **Contact / maintainers:** (fill in)

## 8. mirastacklabs.ai one-pager copy (P12)

The maintainer builds the page elsewhere. This is the copy; style follows [BRAND.md](BRAND.md)
(Oswald headings in upper case, JetBrains Mono for labels and the command, teal accent, orange
only for emphasis rules). Use the three-bar mark and the `MIRAFLEET BY MIRASTACK LABS` wordmark.

**Heading:** MIRAFLEET

**Tagline:** Let Claude Code operate your fleet of servers, safely.

**What it is**
mirafleet connects Claude Code (and other MCP agents) to your servers through one central server
you run yourself. A small runner on each server dials out to central over SSH, so no server opens
an inbound port. Every action runs on the runner through an allowlist guard: argv only, no shell,
parameters checked by regex. Every call is audited. One static binary, Apache-2.0, no telemetry.

**Who it is for**
- Platform and ops teams who want an agent to check, restart and fix things across servers
  without handing it a shell.
- Developers who want Claude Code to work in a remote workspace with the same guard and audit.
- Anyone who needs to see and revoke what an agent can reach: there is an admin GUI for it.

**Install in one line** (laptop, macOS or Linux)

```
curl -fsSL https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.sh | sh
```

Also: `brew install mirastacklabs-ai/tap/mirafleet` · `npx -y @mirastacklabs/mirafleet version` ·
Docker `docker.io/mirastack/mirafleet`. Windows: `install.ps1` from the release page.

**Links**
- Source and releases: https://github.com/mirastacklabs-ai/mirafleet
- Quickstart: link the published wiki page (from [QUICKSTART.md](QUICKSTART.md))
- Security and threat model: link the wiki or repo copy of [THREAT-MODEL.md](THREAT-MODEL.md)
- Image: https://hub.docker.com/r/mirastack/mirafleet and ghcr.io/mirastacklabs-ai/mirafleet

Releases are signed and carry build provenance; the quickstart shows how to verify a download.

## 9. Where things live

- Workflow: `.github/workflows/release.yml`; CI: `ci.yml`, `orchestrator.yml`, `dco.yml`.
- Config: `.goreleaser.yaml`, `scripts/release/tools.Dockerfile` (pinned tool versions),
  `packaging/oci/Dockerfile`, `tools/relgen` (bump, check, npm, brew, serverjson, wiki).
- Local checks: `scripts/release.sh m6check` (everything, in order, then cleans up);
  `MIRAFLEET_M6CHECK_SKIP="cross licenses"` skips named steps.
- Cosign identity that `install.sh` and `install.ps1` require: the `release.yml` workflow of this
  repository at `refs/tags/v*`, issuer `https://token.actions.githubusercontent.com`. Renaming the
  workflow file breaks verification for existing installers.
