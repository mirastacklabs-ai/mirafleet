#!/bin/sh
# e2e-systemd (docs/M6.md section 2.15): install the snapshot .deb in a real systemd (PID 1 in a
# throwaway privileged debian container on the Docker runtime), then run central and an actions
# runner as the shipped units: join, approve, `mcp login`, `mcp api action.run`, one MCP
# `tools/call`, and check that removing and purging the package keeps the data and the user.
# Nothing is pushed or published; the container and image are removed on exit.
#
# Usage: scripts/release/e2e-systemd.sh [DIST_DIR]   (default: <repo>/dist/goreleaser)
# Needs a snapshot first (scripts/release.sh snapshot). Last line: OK e2e-systemd or FAIL ...
# Env: MIRAFLEET_CTR (container CLI, default docker; trusted caller environment).
# Also checks the Admin GUI (TLS, fingerprint, setup token, Host, setup, session across restart).
# Also: host-mode runners `dev` and `qa` (separate OS users with linger, the packaged user unit),
# and the pre-migration backup on upgrade (a schema-3 database is rebuilt, the package is
# reinstalled, central migrates to 4 and keeps a schema-3 backup). sqlite3, libpam-systemd and
# dbus are dev-only tools of this throwaway test image; they are not part of the package.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DIST="${1:-$ROOT/dist/goreleaser}"
CTR="${MIRAFLEET_CTR:-docker}"
LABEL=org.mirastacklabs.mirafleet.release=1
# Pinned by index digest (same as pkgtest.sh).
DEBIAN_IMAGE=docker.io/library/debian:12@sha256:268fb24c39bca92d326dd83ea3abebe4a3f7751c804ffef23bb6d857dd343ff8
IMG="mirafleet-e2e-systemd:$$"
NAME="mirafleet-e2e-systemd-$$"

fail() {
	echo "FAIL e2e-systemd: $*" >&2
	exit 1
}

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	fail "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)"
fi
[ -d "$DIST" ] || fail "$DIST does not exist: run scripts/release.sh snapshot first"
DIST_ABS="$(cd "$DIST" && pwd)"

case "$(uname -m)" in
arm64 | aarch64) ARCHES="arm64 aarch64" ;;
*) ARCHES="amd64 x86_64" ;;
esac
deb=""
for a in $ARCHES; do
	for f in "$DIST_ABS"/*"$a"*.deb; do
		[ -f "$f" ] && deb=$(basename "$f") && break 2
	done
done
[ -n "$deb" ] || fail "no .deb for this architecture in $DIST_ABS"

cleanup() {
	$CTR rm -f "$NAME" >/dev/null 2>&1 || true
	$CTR rmi -f "$IMG" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# systemd as PID 1 needs a privileged container with the host cgroup namespace; the container
# is disposable and has no published ports and no Docker socket.
$CTR build -q --label "$LABEL" -t "$IMG" - >/dev/null <<EOF || fail "image build failed"
FROM $DEBIAN_IMAGE
RUN apt-get update -qq && DEBIAN_FRONTEND=noninteractive apt-get install -y -qq --no-install-recommends \
    systemd systemd-sysv libpam-systemd dbus procps util-linux ca-certificates curl openssl sqlite3 >/dev/null && rm -rf /var/lib/apt/lists/*
STOPSIGNAL SIGRTMIN+3
CMD ["/sbin/init"]
EOF
$CTR run -d --name "$NAME" --label "$LABEL" --privileged --cgroupns=host \
	--tmpfs /run --tmpfs /run/lock --tmpfs /tmp \
	-v /sys/fs/cgroup:/sys/fs/cgroup:rw -v "$DIST_ABS":/dist:ro "$IMG" >/dev/null ||
	fail "cannot start a systemd container (needs --privileged and the host cgroup namespace)"

x() { $CTR exec "$NAME" "$@"; }
# xs SCRIPT [ARG...]: run SCRIPT under sh -ec in the container.
xs() { $CTR exec -i "$NAME" sh -ec "$@"; }
M=/usr/bin/mirafleet
# Admin commands run as the unit user (the admin socket checks the peer UID).
adm() { x runuser -u mirafleet -- $M "$@"; }
say() { echo "ok: $*"; }
# wait_for DESC SECONDS CMD...: poll until CMD succeeds, else dump the unit logs and fail.
wait_for() {
	desc=$1
	n=$2
	shift 2
	while ! "$@" >/dev/null 2>&1; do
		n=$((n - 1))
		if [ "$n" -le 0 ]; then
			x journalctl --no-pager -n 40 -u 'mirafleet-*' >&2 || true
			fail "timed out: $desc"
		fi
		sleep 1
	done
}

wait_for "systemd to boot" 60 x sh -c 'systemctl is-system-running --wait 2>/dev/null | grep -qE "running|degraded"'
say "systemd is PID 1 in the container"

x apt-get install -y -qq "/dist/$deb" >/dev/null 2>&1 || fail "apt-get install /dist/$deb"
x $M version >/dev/null || fail "mirafleet version"
x systemctl is-enabled mirafleet-central.service >/dev/null 2>&1 && fail "central unit enabled by install"
say "installed $deb; units are not enabled by the package"

x systemctl enable --now mirafleet-central.service >/dev/null 2>&1 || fail "central enable --now"
wait_for "central to answer admin commands" 60 adm central fleet list
x systemctl is-active mirafleet-central.service >/dev/null || fail "central not active"
say "mirafleet-central.service active, admin socket answers"

# --- Admin GUI (docs/M4.md 2.2, 2.3): TLS, logged fingerprint, setup token file, Host, setup ---
GUI=https://127.0.0.1:8443
gcode() { x curl -sk -o /dev/null -w '%{http_code}' "$@"; }
wait_for "GUI to answer on 8443" 30 x curl -sk -o /dev/null "$GUI/"
served=$(xs "openssl s_client -connect 127.0.0.1:8443 -servername localhost </dev/null 2>/dev/null | openssl x509 -outform der | openssl dgst -sha256 -binary | od -An -v -tx1 | tr -d ' \n'")
[ -n "$served" ] || fail "no certificate served on 8443"
x journalctl --no-pager -u mirafleet-central.service | tr 'A-F' 'a-f' | tr -d ':' | grep -qF "$served" ||
	fail "GUI certificate fingerprint $served is not in the journal"
say "GUI over TLS on 8443; certificate fingerprint equals the journal"

[ "$(x stat -c '%a %U' /var/lib/mirafleet/central/gui/setup-token)" = "600 mirafleet" ] ||
	fail "setup-token file is not 0600 mirafleet at /var/lib/mirafleet/central/gui/setup-token"
x journalctl --no-pager -u mirafleet-central.service | grep -qF '/var/lib/mirafleet/central/gui/setup-token' ||
	fail "journal does not name the setup-token file"
[ "$(gcode -H 'Host: evil.example' "$GUI/")" = 421 ] || fail "foreign Host did not get 421"
say "setup-token file is 0600 mirafleet and named in the journal; foreign Host -> 421"

xs "
	jar=/tmp/gui.jar
	csrf=\$(curl -sk -c \$jar -b \$jar $GUI/setup | grep -o 'name=\"_csrf\" value=\"[^\"]*\"' | head -1 | sed 's/.*value=\"//;s/\"\$//')
	test -n \"\$csrf\"
	c=\$(curl -sk -c \$jar -b \$jar -o /dev/null -w '%{http_code}' --data-urlencode \"_csrf=\$csrf\" \
		--data-urlencode \"token=\$(cat /var/lib/mirafleet/central/gui/setup-token)\" --data-urlencode username=admin \
		--data-urlencode password=e2e-systemd-password --data-urlencode password2=e2e-systemd-password $GUI/setup)
	test \"\$c\" = 303" || fail "GUI setup with the token from the file"
x systemctl restart mirafleet-central.service || fail "central restart"
wait_for "central after restart" 60 adm central fleet list
wait_for "GUI after restart" 30 x curl -sk -o /dev/null "$GUI/"
[ "$(gcode -b /tmp/gui.jar "$GUI/")" = 200 ] || fail "GUI session did not survive a restart"
[ "$(gcode "$GUI/setup")" = 404 ] || fail "/setup still reachable after setup"
say "GUI setup with the token; session survives a restart; /setup is 404"

adm central fleet create prod >/dev/null || fail "fleet create"
tok=$(adm central token create -fleet prod 2>/dev/null)
[ -n "$tok" ] || fail "no join token"
x cp /usr/share/doc/mirafleet/examples/actions.yaml /etc/mirafleet/actions.yaml
printf '%s\n' "$tok" | $CTR exec -i "$NAME" runuser -u mirafleet -- $M runner join 127.0.0.1:2222 -name e2e-1 -data /var/lib/mirafleet/runner -token - >/dev/null ||
	fail "runner join"
adm central runner approve prod/e2e-1 >/dev/null || fail "runner approve"
x systemctl enable --now mirafleet-runner.service >/dev/null 2>&1 || fail "runner enable --now"
wait_for "runner e2e-1 to be active" 60 sh -c "$CTR exec $NAME runuser -u mirafleet -- $M central runner list | grep e2e-1 | grep -q active"
say "runner e2e-1 joined, approved, mirafleet-runner.service active"

# The operator side runs as root with its own HOME.
code=$(adm central operator code | sed -n 's/^code: *//p')
[ -n "$code" ] || fail "no operator code"
printf '%s\n' "$code" | $CTR exec -i -e HOME=/root "$NAME" $M mcp login 127.0.0.1:2222 -name e2e -code - >/dev/null || fail "mcp login"
online() { $CTR exec -e HOME=/root "$NAME" $M mcp api runners.list '{"fleet":"prod"}' | tr -d ' \n' | grep -q '"name":"e2e-1"[^}]*"online":true'; }
wait_for "runner e2e-1 online" 60 online
out=$($CTR exec -e HOME=/root "$NAME" $M mcp api action.run '{"fleet":"prod","runner":"e2e-1","action":"uptime"}') || fail "action.run"
printf '%s' "$out" | tr -d ' \n' | grep -q '"exit_code":0' || fail "action.run did not exit 0: $out"
say "mcp login; mcp api action.run uptime -> exit 0"

init='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"e2e-systemd","version":"0"}}}'
note='{"jsonrpc":"2.0","method":"notifications/initialized"}'
call='{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"run_action","arguments":{"fleet":"prod","runner":"e2e-1","action":"uptime"}}}'
resp=$( (printf '%s\n%s\n%s\n' "$init" "$note" "$call"; sleep 5) | $CTR exec -i -e HOME=/root "$NAME" $M mcp -project-dir /tmp 2>/dev/null) || true
printf '%s' "$resp" | grep -q '"id":2' || fail "no tools/call response: [$resp]"
printf '%s' "$resp" | tr -d ' ' | grep -q '"isError":true' && fail "tools/call run_action returned an error: [$resp]"
printf '%s' "$resp" | tr -d ' \n' | grep -q 'exit_code' || fail "tools/call run_action has no exit_code: [$resp]"
say "MCP tools/call run_action ok"

# --- Host mode (docs/M7.md section 3.3): runners `dev` and `qa`, two OS users, user unit, linger ---
x test -f /usr/lib/systemd/user/mirafleet-runner.service || fail "user unit not installed"
x test -f /usr/lib/systemd/system/mirafleet-runner@.service || fail "system template not installed"
x test -f /usr/share/doc/mirafleet/examples/workspace-host.yaml || fail "workspace-host.yaml not installed"
adm central fleet create vms -mode workspace -executor host >/dev/null || fail "host fleet create"
for u in dev qa; do
	x useradd -m -s /bin/bash "$u" || fail "useradd $u"
	x loginctl enable-linger "$u" || fail "enable-linger $u"
	uid=$(x id -u "$u")
	wait_for "user manager of $u" 60 x test -S "/run/user/$uid/systemd/private"
	[ "$(x loginctl show-user "$u" -p Linger --value)" = yes ] || fail "linger of $u is not on"
	# Runs as the runner's own user, never root: join (state in the XDG dirs), workspace file, unit.
	utok=$(adm central token create -fleet vms 2>/dev/null)
	[ -n "$utok" ] || fail "no join token for $u"
	printf '%s\n' "$utok" | $CTR exec -i -e HOME=/home/$u "$NAME" runuser -u "$u" -- $M runner join 127.0.0.1:2222 -name "$u" -token - >/dev/null ||
		fail "runner join as $u"
	adm central runner approve "vms/$u" >/dev/null || fail "approve vms/$u"
	x runuser -u "$u" -- sh -ec 'mkdir -p ~/.config/mirafleet && cp /usr/share/doc/mirafleet/examples/workspace-host.yaml ~/.config/mirafleet/workspace.yaml' ||
		fail "workspace.yaml for $u"
	x runuser -u "$u" -- env "HOME=/home/$u" "XDG_RUNTIME_DIR=/run/user/$uid" systemctl --user enable --now mirafleet-runner >/dev/null 2>&1 ||
		fail "systemctl --user enable --now as $u"
done
for u in dev qa; do
	uid=$(x id -u "$u")
	userctl() { x runuser -u "$u" -- env "HOME=/home/$u" "XDG_RUNTIME_DIR=/run/user/$uid" systemctl --user "$@"; }
	wait_for "user unit of $u active" 60 userctl is-active mirafleet-runner
	x pgrep -u "$uid" -f 'mirafleet runner run' >/dev/null || fail "no runner process owned by $u"
	wait_for "runner vms/$u active on central" 60 sh -c "$CTR exec $NAME runuser -u mirafleet -- $M central runner list | grep -w $u | grep -q active"
done
x pgrep -u 0 -f 'mirafleet runner run' >/dev/null 2>&1 && fail "a host-mode runner is running as root"
say "host-mode runners dev and qa: separate users with linger, packaged user unit, active on central"

# --- Backup on upgrade (docs/M6.md section 2.3) ---
# Only one snapshot exists, so rebuild a schema-3 database (undo migration 0004), reinstall the
# package (the upgrade), and start central: it must back the schema-3 database up, then migrate.
db=/var/lib/mirafleet/central/mirafleet.db
x systemctl stop mirafleet-central.service || fail "central stop"
x test ! -e /var/lib/mirafleet/central/backups || fail "a fresh install already has backups"
x runuser -u mirafleet -- sqlite3 "$db" 'DROP TABLE credential_assignments; DROP TABLE credentials; DROP TABLE secrets_meta; PRAGMA user_version=3;' ||
	fail "cannot rebuild a schema-3 database"
before=$(x runuser -u mirafleet -- sqlite3 "$db" 'select count(*) from runners')
[ "$before" -ge 3 ] || fail "expected the e2e-1, dev and qa runners in the database, found $before"
x apt-get install -y -qq --reinstall "/dist/$deb" >/dev/null 2>&1 || fail "package upgrade (reinstall)"
x systemctl start mirafleet-central.service || fail "central start after upgrade"
wait_for "central after upgrade" 60 adm central fleet list
bak=$(x sh -c 'ls /var/lib/mirafleet/central/backups/mirafleet-schema3-*.db' 2>/dev/null | head -1)
[ -n "$bak" ] || fail "no pre-migration backup mirafleet-schema3-*.db after the upgrade"
[ "$(x stat -c '%a %U' "$bak")" = "600 mirafleet" ] || fail "backup $bak is not 0600 mirafleet"
[ "$(x stat -c '%a' /var/lib/mirafleet/central/backups)" = 700 ] || fail "backups dir is not 0700"
[ "$(x runuser -u mirafleet -- sqlite3 "$bak" 'PRAGMA user_version')" = 3 ] || fail "backup is not schema 3"
[ "$(x runuser -u mirafleet -- sqlite3 "$bak" 'select count(*) from runners')" = "$before" ] || fail "backup lost runner rows"
[ "$(x runuser -u mirafleet -- sqlite3 "$db" 'PRAGMA user_version')" = 4 ] || fail "database was not migrated to schema 4"
[ "$(x runuser -u mirafleet -- sqlite3 "$db" 'select count(*) from runners')" = "$before" ] || fail "runner rows lost by the upgrade"
adm central fleet list | grep -qw prod || fail "fleet prod lost by the upgrade"
x systemctl is-active mirafleet-central.service >/dev/null || fail "central not active after upgrade"
say "upgrade: schema-3 backup (0600) kept, database migrated to 4, data survives"

# Uninstall keeps data, user and configuration (docs/M6.md section 2.5).
x sh -c 'echo keep >/var/lib/mirafleet/central/e2e-marker; chown mirafleet:mirafleet /var/lib/mirafleet/central/e2e-marker'
alive() {
	xs 'test "$(cat /var/lib/mirafleet/central/e2e-marker)" = keep && test -f /var/lib/mirafleet/runner/runner.yaml && getent passwd mirafleet >/dev/null' ||
		fail "$1: data or user lost"
}
x apt-get remove -y -qq mirafleet >/dev/null 2>&1 || fail "apt-get remove"
x test ! -e /usr/bin/mirafleet || fail "binary left after remove"
x systemctl is-active mirafleet-central.service >/dev/null 2>&1 && fail "central still active after remove"
alive remove
x apt-get purge -y -qq mirafleet >/dev/null 2>&1 || fail "apt-get purge"
alive purge
say "remove and purge keep /var/lib/mirafleet and the user; units stopped"

echo "OK e2e-systemd"
