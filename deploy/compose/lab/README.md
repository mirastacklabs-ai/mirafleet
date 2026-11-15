# One-machine lab

Central, an actions runner and an operator on one machine. Ports are published on `127.0.0.1`
only (SSH 2222, GUI 8443). Needs Docker with Compose.

```sh
docker compose up -d                      # central (init runs first)
docker compose logs central               # the GUI certificate fingerprint
docker compose cp central:/var/lib/mirafleet/central/gui/setup-token -   # one-time GUI setup token

M="docker compose exec -T central mirafleet"
$M central fleet create prod
$M central token create -fleet prod 2>/dev/null |
  docker compose run --rm -T runner runner join central:2222 -name lab-1 -data /var/lib/mirafleet/runner -token -
$M central runner approve prod/lab-1
docker compose up -d runner               # the runner stays offline until it has joined

# the operator side: register this lab's operator, then run an action
$M central operator code | sed -n 's/^code: *//p' |
  docker compose run --rm -T operator mcp login central:2222 -name lab -code -
docker compose run --rm operator mcp api action.run '{"fleet":"prod","runner":"lab-1","action":"version"}'
```

Clean up, including all data: `docker compose down -v`.
