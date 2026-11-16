#!/bin/sh
# End-to-end test of deploy/compose/runner-workspace: a workspace-mode runner (container executor)
# that runs from the OCI image. Builds the image from the current tree, starts central and an
# operator with deploy/compose/lab, starts the workspace runner with its own compose file, joins
# and approves it, then as an operator: opens a workspace, runs a command, writes and reads a
# file, checks the egress policy (allowed, denied, no route around the proxy) and the exec
# timeout, closes the workspace and tears everything down.
#
# Usage: scripts/release/wslabtest.sh   (through scripts/release.sh wslab)
# Needs a Linux Docker daemon that uses a host containerd 2.x at /run/containerd/containerd.sock
# (docker-ce does), cgroup2 or cgroupfs-driver runc, and permission to create /var/lib/mirafleet/ws
# on the daemon's host (done in a throwaway container). On colima: the VM is the host.
# Refuses to run when /var/lib/mirafleet/ws already exists (it may belong to a real runner).
# Environment (trusted caller input):
#   MIRAFLEET_CTR        container CLI (default docker; Compose is used as `$MIRAFLEET_CTR compose`)
#   MIRAFLEET_IMAGE      image to test (default: built by imagetest.sh build and removed after)
#   MIRAFLEET_GO_IMAGE   tool image (default: dev.sh's Go image; set by release.sh)
# Left behind on purpose: the pulled busybox image in the containerd namespace `mirafleet`
# (a cache; remove with `ctr -n mirafleet images rm`).
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CTR="${MIRAFLEET_CTR:-docker}"
TOOL_IMAGE="${MIRAFLEET_GO_IMAGE:?run through scripts/release.sh (it pins the Go image)}"
LAB="$ROOT/deploy/compose/lab"
WSR="$ROOT/deploy/compose/runner-workspace"
LABEL=org.mirastacklabs.mirafleet.release=1
# The workspace image: busybox by index digest (the shipped workspace.yaml allows this).
BUSYBOX=docker.io/library/busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec
WSROOT=/var/lib/mirafleet/ws

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	echo "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)" >&2
	exit 1
fi

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
ok() { echo "ok: $*"; }

# hostsh SCRIPT: run SCRIPT under sh in a throwaway container with the daemon host's /var/lib at
# /hostvar and /run at /hostrun (the paths below are the daemon's, not the CLI's).
hostsh() {
	$CTR run --rm --label "$LABEL" -v /var/lib:/hostvar -v /run:/hostrun "$TOOL_IMAGE" sh -ec "$1"
}

hostsh 'test -S /hostrun/containerd/containerd.sock' || fail "no containerd socket at /run/containerd/containerd.sock on the Docker host"
hostsh '! test -e /hostvar/mirafleet/ws' || fail "$WSROOT exists on the Docker host: remove it (it may belong to a real runner)"

BUILT=""
if [ -z "${MIRAFLEET_IMAGE:-}" ]; then
	MIRAFLEET_IMAGE=$(sh "$ROOT/scripts/release/imagetest.sh" build)
	BUILT=1
fi
export MIRAFLEET_IMAGE
# Set but empty: Compose picks random free host ports on 127.0.0.1.
export MIRAFLEET_SSH_PORT="" MIRAFLEET_GUI_PORT=""

SUFFIX=$$
LABPROJ="mfwslab$SUFFIX"
RUNPROJ="mfwsrun$SUFFIX"
HTTPD="mfwshttpd$SUFFIX"
TMP="$ROOT/dist/wslab" # under the repo (gitignored): a colima VM mounts $HOME, not /var/folders
rm -rf "$TMP"
mkdir -p "$TMP"
CREATED_VARLIB=""
hostsh 'test -d /hostvar/mirafleet' || CREATED_VARLIB=1

dc() { COMPOSE_PROJECT_NAME="$LABPROJ" $CTR compose -f "$LAB/compose.yaml" "$@"; }
rc() { COMPOSE_PROJECT_NAME="$RUNPROJ" $CTR compose -f "$WSR/compose.yaml" -f "$TMP/override.yaml" "$@"; }
M="mirafleet"
# op METHOD JSON [ARG...]: one operator API call (output has its spaces and newlines removed).
op() {
	m=$1
	p=$2
	shift 2
	dc run --rm -T operator mcp api "$m" "$p" "$@" 2>/dev/null | tr -d ' \n'
}

cleanup() {
	status=$?
	if [ "$status" -ne 0 ]; then
		echo "--- runner logs" >&2
		rc logs --no-color 2>&1 | tail -n 40 >&2 || true
	fi
	rc down -v --remove-orphans >/dev/null 2>&1 || true
	$CTR rm -f "$HTTPD" >/dev/null 2>&1 || true
	dc --profile runner --profile tools down -v --remove-orphans >/dev/null 2>&1 || true
	# Workspace directories are root-owned: remove them (and the staged binary) from a container.
	hostsh "rm -rf /hostvar/mirafleet/ws; if [ -n '$CREATED_VARLIB' ]; then rmdir /hostvar/mirafleet 2>/dev/null || true; fi" >/dev/null 2>&1 || true
	[ -z "$BUILT" ] || $CTR rmi -f "$MIRAFLEET_IMAGE" >/dev/null 2>&1 || true
	rm -rf "$TMP"
}
trap cleanup EXIT INT TERM

# --- static checks on the shipped files ---
! grep -qE '^[[:space:]]*(privileged|network_mode)[[:space:]]*:' "$WSR/compose.yaml" || fail "compose.yaml uses privileged or network_mode"
grep -q 'create_host_path: false' "$WSR/compose.yaml" || fail "compose.yaml must not let Docker create the workspace root"
ok "compose.yaml: not privileged, workspace root must pre-exist"

# The workspace root, as the packages make it: root-owned, 0700.
hostsh 'install -d -m 0700 -o 0 -g 0 /hostvar/mirafleet/ws'

# The lab network is where the runner finds central, and where the egress targets live.
cat >"$TMP/override.yaml" <<EOF
services:
  runner:
    networks: [default, lab]
    volumes:
      - $TMP/workspace.yaml:/etc/mirafleet/workspace.yaml:ro
networks:
  lab:
    external: true
    name: ${LABPROJ}_default
EOF
# The shipped example, plus IP-literal egress rules (the targets below are private addresses).
{
	cat "$WSR/workspace.yaml"
	echo "egress_cidr_rules: true"
} >"$TMP/workspace.yaml"
chmod 0644 "$TMP/workspace.yaml"

dc config -q || fail "lab compose file does not validate"
dc up -d --wait central >/dev/null 2>&1 || { dc logs >&2; fail "central did not start"; }
rc config -q || fail "runner-workspace compose file does not validate"
ok "central is up"

# Two HTTP servers on the lab network: 8080 is on the fleet's egress list, 8081 is not.
$CTR pull -q "$BUSYBOX" >/dev/null
$CTR run -d --name "$HTTPD" --label "$LABEL" --network "${LABPROJ}_default" "$BUSYBOX" \
	sh -c 'mkdir /www && echo egress-ok >/www/index.html && httpd -p 8081 -h /www && exec httpd -f -p 8080 -h /www' >/dev/null ||
	fail "cannot start the test HTTP servers"
HIP=$($CTR inspect -f "{{(index .NetworkSettings.Networks \"${LABPROJ}_default\").IPAddress}}" "$HTTPD")
[ -n "$HIP" ] || fail "no address for the test HTTP servers"

dc exec -T central $M central fleet create ws -mode workspace -image "$BUSYBOX" -network egress -egress "$HIP/32:8080" >/dev/null || fail "fleet create failed"
tok=$(dc exec -T central $M central token create -fleet ws 2>/dev/null)
[ -n "$tok" ] || fail "no join token"
printf '%s\n' "$tok" | rc run --rm -T runner runner join central:2222 -name ws-1 -data /var/lib/mirafleet/runner -token - >/dev/null 2>&1 || fail "runner join failed"
dc exec -T central $M central runner approve ws/ws-1 >/dev/null
rc up -d runner >/dev/null 2>&1 || fail "runner did not start"

code=$(dc exec -T central $M central operator code | sed -n 's/^code: *//p')
[ -n "$code" ] || fail "no operator code"
printf '%s\n' "$code" | dc run --rm -T operator mcp login central:2222 -name lab -code - >/dev/null 2>&1 || fail "mcp login failed"
n=0
until op runners.list '{"fleet":"ws"}' | grep -q '"name":"ws-1"[^}]*"online":true'; do
	n=$((n + 1))
	[ "$n" -le 60 ] || fail "workspace runner ws-1 did not come online"
	sleep 1
done
ok "workspace runner ws-1 joined, approved and online (executor containerd)"

# --- the container itself: not privileged, one added capability, no Docker socket ---
cid=$(rc ps -q runner)
[ "$($CTR inspect -f '{{.HostConfig.Privileged}}' "$cid")" = false ] || fail "runner container is privileged"
case "$($CTR inspect -f '{{.HostConfig.CapAdd}}' "$cid")" in
"[SYS_ADMIN]" | "[CAP_SYS_ADMIN]") ;;
*) fail "runner capabilities are not exactly SYS_ADMIN: $($CTR inspect -f '{{.HostConfig.CapAdd}}' "$cid")" ;;
esac
case "$($CTR inspect -f '{{range .Mounts}}{{.Source}} {{end}}' "$cid")" in *docker.sock*) fail "runner has the Docker socket" ;; esac
ok "runner container: not privileged, cap_add [SYS_ADMIN] only, no Docker socket"

# --- open, exec, files ---
out=$(op workspace.open '{"fleet":"ws","session":"e2e"}') || true
WS=$(printf '%s' "$out" | sed -n 's/.*"workspace":{"id":"\([0-9a-f]*\)".*/\1/p')
[ -n "$WS" ] || fail "workspace.open failed: $out"
printf '%s' "$out" | grep -q '"state":"open"' || fail "workspace is not open: $out"
ok "workspace.open -> $WS"

out=$(op workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"sh\",\"-c\",\"echo ok; id -u\"]}")
printf '%s' "$out" | grep -q '"exit_code":0' || fail "exec failed: $out"
printf '%s' "$out" | grep -q '"stdout":"ok\\n[1-9][0-9]*\\n"' || fail "exec output is not 'ok' then a non-root uid: $out"
ok "workspace.exec echo ok -> exit 0, runs as a non-root uid"

printf 'hello-e2e' | dc run --rm -T operator mcp api workspace.write_file "{\"workspace\":\"$WS\",\"path\":\"/workspace/a.txt\"}" -in - >/dev/null 2>&1 || fail "write_file failed"
out=$(op workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"sh\",\"-c\",\"cat /workspace/a.txt; echo from-exec >/workspace/b.txt\"]}")
printf '%s' "$out" | grep -q '"stdout":"hello-e2e"' || fail "exec does not see the written file: $out"
# read_file writes its body to a file in the (kept) operator container.
RD="mfwsrd$SUFFIX"
dc run --name "$RD" -T operator mcp api workspace.read_file "{\"workspace\":\"$WS\",\"path\":\"/workspace/b.txt\"}" -out /var/lib/mirafleet/b.txt >/dev/null 2>&1 || fail "read_file failed"
body=$($CTR cp "$RD:/var/lib/mirafleet/b.txt" - | tar -xO)
$CTR rm -f "$RD" >/dev/null
[ "$body" = "from-exec" ] || fail "read_file returned '$body', want 'from-exec'"
ok "write_file then exec reads it; exec writes, read_file returns it"

out=$(op workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"sh\",\"-c\",\"sleep 60\"],\"timeout_s\":2}")
printf '%s' "$out" | grep -q '"timed_out":true' || fail "exec timeout did not fire: $out"
ok "exec timeout kills the command"

# --- egress policy ---
get() { # get PORT: wget through the proxy the runner provides
	op workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"wget\",\"-T\",\"5\",\"-qO-\",\"http://$HIP:$1/\"]}"
}
out=$(get 8080)
printf '%s' "$out" | grep -q '"stdout":"egress-ok\\n"' || fail "allowed egress ($HIP:8080) did not work: $out"
out=$(get 8081)
printf '%s' "$out" | grep -q '"exit_code":0' && fail "egress to $HIP:8081 (not on the list) worked: $out"
out=$(op workspace.exec "{\"workspace\":\"$WS\",\"argv\":[\"sh\",\"-c\",\"unset http_proxy HTTP_PROXY; wget -T 3 -qO- http://$HIP:8080/\"]}")
printf '%s' "$out" | grep -q '"exit_code":0' && fail "a workspace reached $HIP:8080 without the proxy: $out"
ok "egress: $HIP:8080 allowed through the proxy; :8081 refused; no route around the proxy"

# --- close ---
[ "$(op workspace.close "{\"workspace\":\"$WS\"}")" = "{}" ] || fail "workspace.close did not answer {}"
left=$($CTR exec "$cid" /usr/bin/mirafleet runner workspace list -data /var/lib/mirafleet/runner -workspace /etc/mirafleet/workspace.yaml 2>&1) || fail "runner workspace list failed: $left"
case "$left" in *"$WS"*) fail "workspace $WS is still on the runner after close: $left" ;; esac
hostsh "! test -e /hostvar/mirafleet/ws/$WS" || fail "workspace directory $WS is still on the host after close"
ok "workspace.close: gone from the runner and from the host"

rc down -v --remove-orphans >/dev/null 2>&1 || fail "runner compose down -v failed"
$CTR rm -f "$HTTPD" >/dev/null
dc --profile runner --profile tools down -v --remove-orphans >/dev/null 2>&1 || fail "lab compose down -v failed"
echo "OK wslabtest"
