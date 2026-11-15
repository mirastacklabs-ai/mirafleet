# mirafleet quickstart

From nothing to an agent working on your servers. You need:

- one Linux machine for **central** (a small VM is enough), with root or `sudo`;
- one or more Linux machines for **runners** (they can be the same machine for a first try);
- your **laptop** (macOS, Linux or Windows) for the operator side.

Examples use `central.example.com` for central and `0.6.0` for the version. Replace them. The
packages are for Debian/Ubuntu (`apt`) and Fedora/RHEL (`dnf`); Docker Compose and the plain
binary work everywhere Linux does. mirafleet sends no telemetry; the only connections it makes
are the ones in this guide.

Every part ends with **Check it worked**. Do not go on until the check passes.

Want to see it first on one machine, with Docker only? Skip to
[7. Try it on one machine](#7-try-it-on-one-machine).

## 1. Central (Linux)

### Firewall first

Open these before you start anything:

| Port | Who must reach it | Rule |
|---|---|---|
| 2222 | runners and operators (laptops) | allow from their networks |
| 8443 (Admin GUI) | admins only | allow only from admins' networks, **or** bind the GUI to loopback and tunnel (below) |

Central needs no outbound access, except HTTPS to your GitLab if you turn on token self-rotation
later (section 4). The packages do not touch your firewall. The GUI listens on **all interfaces**
by default, so do not leave 8443 open to the world.

### Install

Pick one.

**Package (deb or rpm).** Use `linux_arm64` instead of `linux_amd64` on ARM.

```bash
V=0.6.0
BASE=https://github.com/mirastacklabs-ai/mirafleet/releases/download/v$V
curl -fLO $BASE/mirafleet_${V}_linux_amd64.deb      # or .rpm
curl -fLO $BASE/checksums.txt
sha256sum -c --ignore-missing checksums.txt          # must print: OK
sudo apt install ./mirafleet_${V}_linux_amd64.deb    # or: sudo dnf install ./mirafleet_${V}_linux_amd64.rpm
```

The package creates the `mirafleet` user, installs the systemd units and `/etc/mirafleet/`, and
starts nothing. (For a cosign check of `checksums.txt` see [Verify](#verify-a-download).)

**Docker Compose.** Needs Docker with the Compose plugin. Get `deploy/compose/central/` from the
repository or the release archive.

```bash
cd deploy/compose/central
docker compose up -d      # a one-shot service creates the keys and database first
```

**Plain container.**

```bash
docker run --rm -v mirafleet-central:/var/lib/mirafleet mirastack/mirafleet:0.6.0 central init -data /var/lib/mirafleet/central
docker run -d --name mirafleet-central --restart unless-stopped \
  -v mirafleet-central:/var/lib/mirafleet -p 2222:2222 -p 127.0.0.1:8443:8443 \
  mirastack/mirafleet:0.6.0
```

(`ghcr.io/mirastacklabs-ai/mirafleet:0.6.0` is the same image.) In a container, run admin
commands as `docker exec mirafleet-central mirafleet central ...` instead of the `sudo -u
mirafleet` form used below; with Compose, `docker compose exec central mirafleet central ...`.

### Make the secrets key (host mode and stored credentials)

Central stores credentials for host-mode runners (section 4) encrypted. The key lives in an
environment file. Create it now, even if you only use actions mode, so you never run with
credentials unencrypted:

```bash
# package install
sudo mirafleet central keygen | sudo tee -a /etc/mirafleet/central.env >/dev/null
sudo ls -l /etc/mirafleet/central.env        # must be -rw------- root root
```

`/etc/mirafleet/central.env` must stay mode `0600`, owner root (systemd reads it before dropping
to the `mirafleet` user). With Compose, append the printed `MIRAFLEET_SECRETS_KEY=...` line to
`deploy/compose/central/central.env` (mode 0600). Plain container: pass it with `--env-file`.
<!-- verify: `central keygen` prints `MIRAFLEET_SECRETS_KEY=<base64>` on stdout; central.env has the commented placeholder line; the Compose central.env carries the same line (L3) -->

**Back this key up somewhere apart from the data directory** (a password manager). If you lose
it you lose every stored credential, and central will refuse to start with credentials present
and the wrong key.

Optional but common: tell central how admins and runners reach it. Edit
`/etc/mirafleet/central.env`:

```bash
# one MIRAFLEET_CENTRAL_ARGS line only
MIRAFLEET_CENTRAL_ARGS=-advertise central.example.com:2222 -gui-host central.example.com
# keep the GUI off the network and tunnel instead:
# MIRAFLEET_CENTRAL_ARGS=-gui-listen 127.0.0.1:8443
```

`-gui-host` lists the names admins type in the browser (anything else gets a 421);
`-advertise` is the SSH address shown in the copy-paste command hints.

### Start

```bash
sudo systemctl enable --now mirafleet-central
sudo journalctl -u mirafleet-central -n 40 --no-pager
```

**Check it worked.** The journal shows, among other lines:

- the SSH **host key fingerprint** (a `SHA256:...` value);
- the GUI URL(s) and the GUI **certificate fingerprint** (SHA-256);
- a notice that the GUI is reachable on all interfaces (if you did not restrict it);
- "GUI setup: no admin account yet. The one-time setup token ... is in
  `/var/lib/mirafleet/central/gui/setup-token`" (the path, never the token).
<!-- verify: exact journal wording and the setup-token path match M4 as merged (central_gui_linux.go) -->

Admin commands run as the service user because the admin socket checks the caller's UID:
`sudo -u mirafleet mirafleet central fleet list` should print an empty list.

## 2. The Admin GUI, first run

1. Open `https://central.example.com:8443`. To avoid exposing 8443, tunnel from your laptop and
   open `https://127.0.0.1:8443` instead:
   `ssh -L 8443:127.0.0.1:8443 you@central.example.com`.
2. The certificate is **self-signed**, so the browser warns. Before you accept, open the
   certificate details and compare its **SHA-256 fingerprint** with the one in the journal. They
   must match. If they do not, stop: someone is in the middle. (Over the tunnel, you are already
   talking to central through SSH.)
3. Read the one-time setup token on central. It is valid for 24 hours and is a file, not a log
   line:

   ```bash
   sudo -u mirafleet cat /var/lib/mirafleet/central/gui/setup-token
   # Compose: docker compose cp central:/var/lib/mirafleet/central/gui/setup-token -   (a tar stream; the token is the text)
   # container: docker cp mirafleet-central:/var/lib/mirafleet/central/gui/setup-token -
   ```

4. Paste the token on the setup page, choose a username and a password of at least 12
   characters. You are logged in; the token file is deleted and `/setup` stops existing. The
   session survives a central restart (30 minutes idle, 12 hours at most).
5. Create the fleet: **Fleets**, then new, name `dev`, mode **actions**. Then **Join tokens**:
   create a token for `dev` and copy it (it is shown once). The CLI equivalent:
   `sudo -u mirafleet mirafleet central fleet create dev -mode actions`, then
   `sudo -u mirafleet mirafleet central token create -fleet dev`.

Lost the admin password? `sudo -u mirafleet mirafleet central gui reset-admin` prints a fresh
setup token and ends all GUI sessions.

**Check it worked.** The Overview page loads after login, and a second browser tab with no
session is sent to `/login`.

## 3. A runner (Linux)

### Actions runner (the safe default)

A runner executes only the named actions in its `actions.yaml`. On the runner machine, install
the same package as in section 1 (no need to start central). Then:

```bash
sudo cp /usr/share/doc/mirafleet/examples/actions.yaml /etc/mirafleet/actions.yaml   # edit it: this file is the security boundary
echo 'PASTE-THE-JOIN-TOKEN' | sudo -u mirafleet mirafleet runner join central.example.com:2222 -name web-1 -token -
```

The token carries a hash of central's host key, so the runner pins central before sending
anything. Then, on central (or in the GUI, **Runners**, then approve):

```bash
sudo -u mirafleet mirafleet central runner approve dev/web-1
```

Back on the runner:

```bash
sudo systemctl enable --now mirafleet-runner
```

**Check it worked.** `sudo -u mirafleet mirafleet central runner list` on central shows
`dev/web-1` as active and online, and the GUI Runners page shows it with a recent heartbeat.

### Compose runner

```bash
cd deploy/compose/runner        # edit actions.yaml here
echo 'PASTE-THE-JOIN-TOKEN' | docker compose run --rm -T runner runner join central.example.com:2222 -name web-1 -data /var/lib/mirafleet/runner -token -
# approve on central as above, then:
docker compose up -d
```

### Container-workspace runner (root, needs containerd)

For fleets in `workspace` mode (private containers per agent session). This runner is
root-equivalent on its machine; use a dedicated machine. Install and start containerd (Docker
Engine includes it), then:

```bash
sudo cp /usr/share/doc/mirafleet/examples/workspace.yaml /etc/mirafleet/workspace.yaml   # edit images, limits, network
echo 'PASTE-THE-JOIN-TOKEN' | sudo mirafleet runner join central.example.com:2222 -name ws-1 -data /var/lib/mirafleet/runner-ws -token -
# approve on central (fleet created with: central fleet create ws -mode workspace), then:
sudo systemctl enable --now mirafleet-runner-workspace
```

Host mode is not available in the container image; use the packages.

## 4. Remote development on a VM (host mode)

The agent works as an ordinary Linux user on a dedicated VM, over SSH and HTTPS to your GitLab,
without ever seeing the keys and tokens. The VM firewall decides what it can reach. Do the steps
in order.

1. **A user for the runner.** As root on the VM:

   ```bash
   sudo useradd -m dev
   sudo loginctl enable-linger dev       # lets dev's services run without a login
   ```

2. **Create a host fleet on central** and a join token for it:

   ```bash
   sudo -u mirafleet mirafleet central fleet create vms -mode workspace -executor host
   sudo -u mirafleet mirafleet central token create -fleet vms
   ```
   <!-- verify: `fleet create -mode workspace -executor host` is the accepted form (admin_workspace.go flag "executor": containerd|opensandbox|host) -->

3. **Join as `dev`** (no root needed from here):

   ```bash
   sudo -iu dev
   echo 'PASTE-THE-JOIN-TOKEN' | mirafleet runner join central.example.com:2222 -name vm1-dev -token -
   ```

   The runner's state and config live under `dev`'s home (XDG directories). Approve it on
   central: `sudo -u mirafleet mirafleet central runner approve vms/vm1-dev`.

4. **Its workspace file.** Still as `dev`:

   ```bash
   mkdir -p ~/.config/mirafleet
   cp /usr/share/doc/mirafleet/examples/workspace-host.yaml ~/.config/mirafleet/workspace.yaml
   ```
   <!-- verify: examples/workspace-host.yaml is packaged (L3) -->

   It looks like this; edit `root` and `job_max` to taste:

   ```yaml
   executor: host
   concurrent: 8
   host:
     root: ~/work                  # session and workdir directories live here (0700)
     cleanup: {delete_after: 720h} # unused directories are deleted after 30 days; 0 = never
     job_max: 12h                  # longest background job (24h at most)
   env_allow: [LANG, LC_ALL, TERM, CI, GOFLAGS, NODE_ENV]
   ```
   <!-- verify: host: block field names and defaults (root, cleanup.delete_after, job_max) against internal/workspace/wsconfig; M7 layout is <root>/sessions/<slug> and <root>/work/<name> -->

5. **Start it as a systemd user unit** (shown first because it needs no root):

   ```bash
   systemctl --user enable --now mirafleet-runner     # the package installs the unit in /usr/lib/systemd/user/
   ```

   Alternative, as root: the system unit with `User=`, one instance per user:
   `sudo systemctl enable --now mirafleet-runner@dev`.
   <!-- verify: unit paths /usr/lib/systemd/user/mirafleet-runner.service and mirafleet-runner@.service (L3); user unit reads %h/.config/mirafleet/workspace.yaml -->
   Background shells survive a runner restart (`KillMode=process`).

6. **The VM firewall is the egress control.** mirafleet adds no egress allowlist in host mode.
   Allow the VM to reach your GitLab (SSH 22 or your port, and HTTPS 443) and whatever builds
   need; block the rest.

7. **Credentials** (Admin GUI, **Credentials** page, or the CLI). Create the least-privileged
   ones GitLab allows, then assign them to the runner:

   | What | GitLab object | Scope |
   |---|---|---|
   | git over SSH | deploy key, write access, on the project | one project |
   | git over HTTPS | project access token | `write_repository` only |
   | container registry | deploy token or project token | `read_registry` / `write_registry` as needed |

   In the GUI, secret values are write-only: they are masked and never shown again, and there is
   no file upload in the browser. From the CLI the value comes from stdin, never from a flag:

   ```bash
   sudo -u mirafleet mirafleet central credential set gitlab-ssh -kind ssh-key \
        -hosts gitlab.example.com -known-hosts ./gitlab_known_hosts < ./deploy_key
   sudo -u mirafleet mirafleet central credential set gitlab-https -kind https-token -auth basic \
        -user mirafleet -hosts gitlab.example.com -expires 2027-01-31 < ./project_token
   sudo -u mirafleet mirafleet central credential assign gitlab-ssh -runner vms/vm1-dev
   sudo -u mirafleet mirafleet central credential assign gitlab-https -runner vms/vm1-dev
   ```
   <!-- verify: credential set/assign flags (-kind -hosts -known-hosts -auth -user -expires; assign NAME -runner fleet/runner | -fleet F) against merged admin_credentials.go; GUI /credentials page layout -->

   Central pushes them to the runner over the existing SSH connection; the runner keeps them in
   memory only. Optional: `-rotate gitlab_self -rotate-project PATH` lets central rotate a
   project access token before it expires. That needs outbound HTTPS from central to GitLab (and
   `MIRAFLEET_CORPORATE_CA_FILE` pointing at your CA file if GitLab uses a private CA).

8. **In the project on your laptop**, add `.mirafleet.yaml` at the root:

   ```yaml
   fleet: vms
   remote: true
   ```

   Open the project in Claude Code (section 5). Claude opens a workspace on `vm1-dev`, clones
   over SSH or HTTPS, edits with the remote `edit` tool, runs long builds as background shells,
   and commits and pushes, with nothing from the repository stored on your laptop.

9. **A second runner.** A runner serves **one operator at a time**, and any registered operator
   may use any registered runner. So give each developer (or each isolated job) their own
   runner: a second user on the same VM, or another VM.

   ```bash
   sudo useradd -m qa && sudo loginctl enable-linger qa
   sudo -u mirafleet mirafleet central token create -fleet vms
   # then repeat steps 3 to 5 as qa with -name vm1-qa; assign credentials to vms/vm1-qa
   ```

   Users on one VM are separated only by normal Unix permissions (homes are 0700); root on the VM
   sees everything. Read the [threat model](THREAT-MODEL.md) before you share a VM.

**Check it worked.** In the GUI, the **Credentials** page shows each credential delivered to
`vms/vm1-dev` with status `ok`. From the laptop:
`mirafleet mcp api runners.list` shows `vm1-dev` online. In Claude Code, ask it to run `git
ls-remote` on the project's GitLab remote through the mirafleet tools; it should work, and
`env`, `ls ~/.ssh` and `cat /proc/self/environ` on the VM should show no secret.

## 5. Operator: your laptop (macOS, Linux, Windows)

### Install the binary

| OS | Command |
|---|---|
| macOS, Linux (Homebrew) | `brew install mirastacklabs-ai/tap/mirafleet` |
| macOS, Linux (script) | `curl -fsSL https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.sh \| sh` (installs to `~/.local/bin`; add `-b DIR` to change; system-wide: `sudo sh install.sh -b /usr/local/bin`) |
| Windows (PowerShell 5.1 or 7) | `irm https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.ps1 \| iex` (user install, add `-AddToPath` to change your PATH) |
| Any OS, Node 18+ | `npx -y @mirastacklabs/mirafleet@0.6.0 version` (for MCP configs; not for the Claude Code plugin on Windows) |
| Archive | download `mirafleet_0.6.0_<os>_<arch>.tar.gz` (`.zip` on Windows) from the release page, check it against `checksums.txt`, put the binary on `PATH` |

An archive downloaded in a browser is quarantined by the OS. After checking the checksum:
macOS `xattr -d com.apple.quarantine mirafleet`; Windows `Unblock-File .\mirafleet.exe`.
Homebrew, npm and the two install scripts are not affected.

### Log in

On central, get a registration code (rotates every 60 seconds), or copy it from the GUI
**Operators** page:

```bash
sudo -u mirafleet mirafleet central operator code
```

On your laptop, send it on stdin (never in argv):

```bash
echo 'PASTE-THE-CODE' | mirafleet mcp login central.example.com:2222 -name durga -code -
```

```powershell
# Windows PowerShell
'PASTE-THE-CODE' | mirafleet mcp login central.example.com:2222 -name durga -code -
```

The code carries the hash of central's host key, so your laptop pins central.

**Check it worked.** `mirafleet mcp api fleets.list` lists `dev` (and `vms`), and
`mirafleet mcp api action.run '{"fleet":"dev","runner":"web-1","action":"uptime"}'` returns
output.

### Connect your agent

**Claude Code** (plugin, needs the binary on `PATH`):

```text
/plugin marketplace add mirastacklabs-ai/mirafleet
/plugin install mirafleet@mirastacklabs
```

Then `/mirafleet list my fleets`. The plugin adds session handoff, workspace close at session end
and the confirm prompt. **Other harnesses** (Codex CLI, Gemini CLI, Cursor, VS Code Copilot,
Cline, Kilo Code, opencode, any stdio MCP client): copy the config from
[integrations/](../integrations/README.md). They work without hooks.

Production fleets: keep them in `actions` mode and mark them guarded
(`central fleet set prod -confirm=true`) so a call needs the fleet name typed as `confirm`.

**Check it worked.** In your harness, ask it to list the fleets; it calls the mirafleet tools and
shows `dev`. The audit log (GUI **Audit**, or `mirafleet central audit`) shows `fleets.list`
from `durga`.

## 6. Upgrade, roll back, uninstall

### Upgrade

Order: **central first, then runners, then operators.** Central at protocol level N accepts
runners and operators at N and N-1 (in 0.x the level is the minor version: v0.6.x is 6); anything
else is refused with a message that says who must upgrade.

```bash
sudo apt install ./mirafleet_0.7.0_linux_amd64.deb      # or dnf; Compose: change the image tag and `docker compose up -d`
```

The package restarts running mirafleet units. Before central migrates its database it makes an
online backup in `/var/lib/mirafleet/central/backups/mirafleet-schema<N>-<time>.db` and keeps the
three newest. If the backup fails, central does not migrate and does not start.
Operators: `brew upgrade mirafleet`, or re-run `install.sh` / `install.ps1`, and update the
plugin. Check with `mirafleet version`.

### Roll back central

Writes made after the upgrade are lost.

```bash
sudo systemctl stop mirafleet-central
sudo apt install --allow-downgrades ./mirafleet_0.6.0_linux_amd64.deb      # the previous version
cd /var/lib/mirafleet/central
sudo -u mirafleet mkdir -p aside && sudo -u mirafleet mv mirafleet.db mirafleet.db-wal mirafleet.db-shm aside/ 2>/dev/null
sudo -u mirafleet cp backups/mirafleet-schema<N>-<time>.db mirafleet.db        # the newest backup
sudo systemctl start mirafleet-central
```

Backups are as encrypted as the database: they need `MIRAFLEET_SECRETS_KEY` to be useful, so back
that key up separately.

### Uninstall

`sudo apt remove mirafleet` (or `dnf remove`) stops and disables the units but **keeps all data**
(`/var/lib/mirafleet`, `/etc/mirafleet`) and the `mirafleet` user, also with `purge`. To wipe a
machine, stop the units and delete those yourself. Compose: `docker compose down` keeps the
volume; `docker compose down -v` deletes it. Homebrew: `brew uninstall mirafleet`. Operators also
remove `mirafleet mcp context remove NAME`.

### Verify a download

`install.sh` and `install.ps1` always verify the SHA-256. For the cosign signature of
`checksums.txt` use `install.sh --verify-signature` (needs `cosign`), or:

```bash
cosign verify-blob --bundle checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github\.com/mirastacklabs-ai/mirafleet/\.github/workflows/release\.yml@refs/tags/v' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
gh attestation verify FILE --repo mirastacklabs-ai/mirafleet
```

## 7. Try it on one machine

With Docker (Compose plugin) only, nothing else installed. Central, an actions runner and an
operator run together; ports are bound to `127.0.0.1`.

```bash
cd deploy/compose/lab
docker compose up -d
docker compose logs central                                              # the GUI fingerprint
docker compose cp central:/var/lib/mirafleet/central/gui/setup-token -   # setup token (tar stream)
M="docker compose exec -T central mirafleet"
$M central fleet create prod
$M central token create -fleet prod 2>/dev/null |
  docker compose run --rm -T runner runner join central:2222 -name lab-1 -token -
$M central runner approve prod/lab-1
docker compose up -d runner
$M central operator code | sed -n 's/^code: *//p' |
  docker compose run --rm -T operator mcp login central:2222 -name lab -code -
docker compose run --rm operator mcp api action.run '{"fleet":"prod","runner":"lab-1","action":"version"}'
docker compose down -v          # remove everything, data included
```

Open `https://127.0.0.1:8443` for the GUI, as in section 2. If you test release builds in a
VM, use a separate Colima profile (for example `colima start --profile mirafleet-lab`), never
your day-to-day Docker profile.

## Where to go next

- What is and is not protected: [THREAT-MODEL.md](THREAT-MODEL.md).
- How it works, with diagrams: the [developer guide](guide/README.md); the full reference for
  every command: guide chapter 10.
- Security reports: [SECURITY.md](../SECURITY.md). Licence and disclaimer: [README.md](../README.md).
