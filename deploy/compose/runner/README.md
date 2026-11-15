# Actions runner with Docker Compose

A runner runs only the named actions in `actions.yaml` (mounted read-only). The image has no shell,
so the example action just runs `mirafleet version`. Edit `actions.yaml` for your own.

1. On central, create a join token: `mirafleet central token create -fleet prod`
   (Compose: `docker compose exec central mirafleet central token create -fleet prod`). It prints
   once.
2. Join once. The token goes in on standard input, never on the command line:

   ```sh
   echo 'PASTE-THE-TOKEN' | docker compose run --rm -T runner runner join CENTRAL_HOST:2222 -name web-1 -data /var/lib/mirafleet/runner -token -
   ```

3. An admin approves it on central (or the GUI): `mirafleet central runner approve prod/web-1`.
4. Start it: `docker compose up -d`

The runner key and pin live in the `runner-data` volume. To join again, use a new volume.
Container workspaces and host mode are not supported in the image; use the packages for those.
