#!/bin/sh
# Brings up deploy/compose/lab with a locally built image, joins and approves the runner, waits
# for it to be online, runs an action as an operator, checks the GUI login page, then tears
# everything down with `compose down -v` (docs/M6.md sections 2.8, 2.14). Nothing is pushed.
#
# Usage: scripts/release/labtest.sh
# Environment (trusted caller input):
#   MIRAFLEET_CTR        container CLI (default docker; Compose is used as `$MIRAFLEET_CTR compose`)
#   MIRAFLEET_IMAGE      image to test (default: built by imagetest.sh build and removed after)
#   MIRAFLEET_GO_IMAGE   tool image with curl (default: dev.sh's Go image)
# The GUI check runs when the binary has -gui-listen.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CTR="${MIRAFLEET_CTR:-docker}"
TOOL_IMAGE="${MIRAFLEET_GO_IMAGE:-docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea}"
LAB="$ROOT/deploy/compose/lab"

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	echo "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)" >&2
	exit 1
fi

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
ok() { echo "ok: $*"; }

BUILT=""
if [ -z "${MIRAFLEET_IMAGE:-}" ]; then
	MIRAFLEET_IMAGE=$(sh "$ROOT/scripts/release/imagetest.sh" build)
	BUILT=1
fi
export MIRAFLEET_IMAGE
# Set but empty: Compose picks random free host ports on 127.0.0.1.
export MIRAFLEET_SSH_PORT="" MIRAFLEET_GUI_PORT=""

# A throwaway project name keeps this away from a lab someone runs by hand.
export COMPOSE_PROJECT_NAME="mflabtest$$"
dc() { $CTR compose -f "$LAB/compose.yaml" "$@"; }
M="mirafleet"

cleanup() {
	dc --profile runner --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
	[ -z "$BUILT" ] || $CTR rmi -f "$MIRAFLEET_IMAGE" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

dc config -q || fail "compose file does not validate"
dc up -d --wait central >/dev/null 2>&1 || { dc logs >&2; fail "central did not start"; }

# Published on loopback only.
for p in 2222 8443; do
	addr=$(dc port central "$p")
	case "$addr" in 127.0.0.1:*) ;; *) fail "port $p is published on $addr, not 127.0.0.1" ;; esac
done
ok "central is up; ports published on 127.0.0.1 only"

dc exec -T central $M central fleet create prod >/dev/null
tok=$(dc exec -T central $M central token create -fleet prod 2>/dev/null)
[ -n "$tok" ] || fail "no join token"
printf '%s\n' "$tok" | dc run --rm -T runner runner join central:2222 -name lab-1 -token - >/dev/null || fail "runner join failed"
dc exec -T central $M central runner approve prod/lab-1 >/dev/null
dc up -d runner >/dev/null 2>&1 || fail "runner did not start"

n=0
until dc exec -T central $M central runner list 2>/dev/null | grep lab-1 | grep -q active; do
	n=$((n + 1))
	[ "$n" -le 60 ] || { dc logs >&2; fail "runner is not active"; }
	sleep 1
done
# Online = the runner's connection is up (listed by `runners.list`, field online).
code=$(dc exec -T central $M central operator code | sed -n 's/^code: *//p')
[ -n "$code" ] || fail "no operator code"
printf '%s\n' "$code" | dc run --rm -T operator mcp login central:2222 -name lab -code - >/dev/null || fail "mcp login failed"
n=0
until dc run --rm -T operator mcp api runners.list '{"fleet":"prod"}' 2>/dev/null | tr -d ' \n' | grep -q '"name":"lab-1"[^}]*"online":true'; do
	n=$((n + 1))
	[ "$n" -le 60 ] || { dc logs >&2; fail "runner lab-1 did not come online"; }
	sleep 1
done
ok "runner lab-1 joined, approved and online"

out=$(dc run --rm -T operator mcp api action.run '{"fleet":"prod","runner":"lab-1","action":"version"}') || fail "action.run failed"
printf '%s' "$out" | tr -d ' \n' | grep -q '"exit_code":0' || fail "action.run did not exit 0: $out"
ok "mcp api action.run version -> exit 0"

if dc run --rm -T central central run -h 2>&1 | grep -q -- '-gui-listen'; then
	gp=$(dc port central 8443)
	gp=${gp##*:}
	n=0
	until page=$($CTR run --rm --network host "$TOOL_IMAGE" curl -sk "https://127.0.0.1:$gp/" -L 2>/dev/null) && [ -n "$page" ]; do
		n=$((n + 1))
		[ "$n" -le 20 ] || fail "GUI is not reachable on 127.0.0.1:$gp"
		sleep 1
	done
	printf '%s' "$page" | grep -qiE 'login|setup' || fail "GUI page is neither login nor setup"
	ok "GUI answers over TLS on 127.0.0.1:$gp"
else
	echo "skip: this binary has no -gui-listen; the GUI check runs once M4 is merged"
fi

dc --profile runner --profile tools down -v --remove-orphans >/dev/null 2>&1 || fail "compose down -v failed"
echo "OK labtest"
