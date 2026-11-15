# Central with Docker Compose

```sh
docker compose up -d
```

A one-shot `central-init` service creates the host key, database and registration secret in the
`central-data` volume (safe to repeat), then `central` starts. It restarts unless stopped.

## First start

The GUI is on `https://127.0.0.1:8443` only (the port is bound to `127.0.0.1` in `compose.yaml`).
Its certificate is self-signed and kept in the volume, so its fingerprint survives restarts.

1. Read the certificate fingerprint from the log, and check that your browser shows the same one
   before you type anything: `docker compose logs central`.
2. Read the one-time setup token:
   `docker compose cp central:/var/lib/mirafleet/central/gui/setup-token -`
   (the output is a tar stream; the token is the file's text).
3. Open `https://127.0.0.1:8443`, accept the certificate after the fingerprint matches, and create
   the admin account.

Token lost or expired (24 h), or password lost: `docker compose exec central mirafleet central gui reset-admin`
prints a fresh setup token once and ends all GUI sessions.

To reach the GUI from other machines, change the port line to `"8443:8443"` and allow port 8443
only from your admins' networks, or keep it local and use `ssh -L 8443:127.0.0.1:8443 HOST`.
To use your own certificate, mount it read-only and add `-gui-cert` and `-gui-key` to the
`central run` command.

## Admin commands

```sh
docker compose exec central mirafleet central fleet create prod
docker compose exec central mirafleet central token create -fleet prod
docker compose exec central mirafleet central runner list
docker compose exec central mirafleet central operator code
```

SSH (runners and operators) is on port 2222 of this host. `central.env.local` is read as an env
file (optional, untracked and gitignored). Create it private, with `umask 077` or `chmod 600`;
the secrets key goes there (`(umask 077; docker run --rm docker.io/mirastack/mirafleet:0.6.0
central keygen >> central.env.local)`). `central.env` is only a tracked template of the settings.

## Back up and stop

Back up the `central-data` volume. `docker compose down` keeps it; `docker compose down -v`
deletes it.
