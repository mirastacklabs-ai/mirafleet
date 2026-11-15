#!/bin/sh
# pkgtest: install the snapshot .deb/.rpm/.apk in digest-pinned distro containers and check the
# system user, files, modes, `systemd-analyze verify` on the units, and remove/purge/reinstall
# behaviour (data and user are never deleted). docs/M6.md section 2.14.
#
# Usage: scripts/release/pkgtest.sh [DIST_DIR]      (default: <repo>/dist)
# Always runs the static checks first (packaging/ files + maintainer scripts run directly in a
# debian and an alpine container). Then every package found in DIST_DIR is tested on its
# distro; a missing format is reported as SKIP. Last line: OK ... or FAIL ...
# Env: MIRAFLEET_CTR (container CLI, default docker). Trusted caller environment.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
DIST="${1:-$ROOT/dist}"
CTR="${MIRAFLEET_CTR:-docker}"
LABEL=org.mirastacklabs.mirafleet.release=1

# Pinned by index digest as pulled on 14 Nov 2026 (tags kept for readers).
DEBIAN_IMAGE=docker.io/library/debian:12@sha256:268fb24c39bca92d326dd83ea3abebe4a3f7751c804ffef23bb6d857dd343ff8
UBUNTU_IMAGE=docker.io/library/ubuntu:24.04@sha256:2198e4c65076e9cb0b3d695219df667c0e3b0072da64ca3c2648aca17dcecaf5
ROCKY_IMAGE=docker.io/library/rockylinux:9@sha256:d7be1c094cc5845ee815d4632fe377514ee6ebcf8efaed6892889657e5ddaaa6
ALPINE_IMAGE=docker.io/library/alpine:3@sha256:f59abe656b692d901e184be3445e084786602108599d2a51d229c0f5a48c71bf

# emit_check: the checker that runs inside each container (sh -s MODE PKGFILE).
emit_check() {
	cat <<'CHECK'
set -eu
mode=$1
pkg=${2:-}
fail() { echo "FAIL: $*" >&2; exit 1; }
say() { echo "  ok: $*"; }
# want PATH MODE OWNER:GROUP
want() {
	[ -e "$1" ] || fail "$1 missing"
	got=$(stat -c '%a %U:%G' "$1")
	[ "$got" = "$2 $3" ] || fail "$1 is '$got', want '$2 $3'"
}
absent() { [ ! -e "$1" ] || fail "$1 must not exist"; }

if command -v apt-get >/dev/null 2>&1; then fam=deb
elif command -v dnf >/dev/null 2>&1; then fam=rpm
else fam=apk; fi

if [ "$fam" = deb ]; then
	export DEBIAN_FRONTEND=noninteractive
	# Minimal images (ubuntu) tell dpkg to skip /usr/share/doc; a real host installs the docs.
	rm -f /etc/dpkg/dpkg.cfg.d/excludes
	apt-get update -qq >/dev/null
	apt-get install -y -qq systemd >/dev/null 2>&1
elif [ "$fam" = rpm ]; then
	dnf install -y -q systemd >/dev/null 2>&1
fi

S=/usr/lib/systemd/system
U=/usr/lib/systemd/user
case $mode in
static)
	[ -d /src/packaging ] || fail "packaging dir not mounted"
	P=/src/packaging
	printf '#!/bin/sh\nexit 0\n' >/tmp/stub
	install -D -m 0755 /tmp/stub /usr/bin/mirafleet
	if [ "$fam" != apk ]; then
		for u in mirafleet-central mirafleet-runner mirafleet-runner-workspace; do
			install -D -m 0644 "$P/systemd/$u.service" "$S/$u.service"
		done
		install -D -m 0644 "$P/systemd/mirafleet-runner@.service" "$S/mirafleet-runner@.service"
		install -D -m 0644 "$P/systemd/user/mirafleet-runner.service" "$U/mirafleet-runner.service"
	fi
	install -D -m 0600 "$P/env/central.env" /etc/mirafleet/central.env
	install -D -m 0644 "$P/env/runner.env" /etc/mirafleet/runner.env
	install -d -m 0755 /usr/share/doc/mirafleet/examples
	install -m 0644 "$P/examples/actions.yaml" "$P/examples/workspace.yaml" "$P/examples/workspace-host.yaml" /usr/share/doc/mirafleet/examples/
	sh "$P/scripts/preinstall.sh"
	out=$(sh "$P/scripts/postinstall.sh") || fail "postinstall failed"
	echo "$out" | grep -q 8443 || fail "postinstall prints no firewall note"
	# idempotent: run both again
	sh "$P/scripts/preinstall.sh"
	sh "$P/scripts/postinstall.sh" >/dev/null || fail "postinstall not idempotent"
	sh "$P/scripts/postinstall.sh" 1 >/dev/null || fail "postinstall rpm-install arg"
	sh "$P/scripts/postinstall.sh" 2 >/dev/null || fail "postinstall rpm-upgrade arg"
	sh "$P/scripts/postinstall.sh" configure 0.5.0 >/dev/null || fail "postinstall deb-upgrade arg"
	;;
pkg)
	case $fam in
	deb) apt-get install -y -qq "$pkg" >/dev/null ;;
	rpm) dnf install -y -q "$pkg" >/dev/null ;;
	apk) apk add -q --allow-untrusted "$pkg" ;;
	esac
	/usr/bin/mirafleet version >/dev/null || fail "mirafleet version failed"
	say "mirafleet version runs"
	;;
esac

# --- shared checks -------------------------------------------------------------------------
getent group mirafleet >/dev/null || fail "group mirafleet missing"
getent passwd mirafleet >/dev/null || fail "user mirafleet missing"
ent=$(getent passwd mirafleet)
case $ent in *:/var/lib/mirafleet:*) ;; *) fail "home is not /var/lib/mirafleet: $ent" ;; esac
case $ent in *nologin | */false) ;; *) fail "shell is not nologin: $ent" ;; esac
[ "$(id -u mirafleet)" -lt 1000 ] || [ "$fam" = apk ] || fail "mirafleet is not a system uid"
say "system user and group"
want /usr/bin/mirafleet 755 root:root
want /var/lib/mirafleet/central 700 mirafleet:mirafleet
want /var/lib/mirafleet/runner 700 mirafleet:mirafleet
want /var/lib/mirafleet/runner-ws 700 root:root
want /etc/mirafleet/central.env 600 root:root
want /etc/mirafleet/runner.env 644 root:root
want /usr/share/doc/mirafleet/examples/actions.yaml 644 root:root
want /usr/share/doc/mirafleet/examples/workspace.yaml 644 root:root
want /usr/share/doc/mirafleet/examples/workspace-host.yaml 644 root:root
grep -q '^executor: host' /usr/share/doc/mirafleet/examples/workspace-host.yaml || fail "workspace-host.yaml is not a host config"
absent /etc/mirafleet/actions.yaml
absent /etc/mirafleet/workspace.yaml
say "files, directories, modes; no live actions.yaml/workspace.yaml"
grep -q '^MIRAFLEET_LISTEN=0.0.0.0:2222' /etc/mirafleet/central.env || fail "central.env listen default"
grep -q -- '-gui-listen 127.0.0.1:8443' /etc/mirafleet/central.env || fail "central.env gui-listen note"
grep -q -- '-gui-cert' /etc/mirafleet/central.env || fail "central.env gui-cert note"
grep -q '8443' /etc/mirafleet/central.env || fail "central.env firewall note"
grep -q '^#MIRAFLEET_SECRETS_KEY=$' /etc/mirafleet/central.env || fail "central.env has no commented secrets-key line"
if grep -q '^MIRAFLEET_SECRETS_KEY' /etc/mirafleet/central.env; then fail "the package must not ship a key (generate with central keygen)"; fi
grep -q 'central keygen' /etc/mirafleet/central.env || fail "central.env does not name central keygen"
say "central.env GUI defaults, firewall note, secrets-key placeholder (no key shipped)"

if [ "$fam" = apk ]; then
	for u in central runner runner-workspace; do absent "$S/mirafleet-$u.service"; done
	absent "$S/mirafleet-runner@.service"
	absent "$U/mirafleet-runner.service"
	say "apk ships no units"
else
	for u in central runner runner-workspace; do want "$S/mirafleet-$u.service" 644 root:root; done
	want "$S/mirafleet-runner@.service" 644 root:root
	want "$U/mirafleet-runner.service" 644 root:root
	for f in "$S/mirafleet-runner@.service" "$U/mirafleet-runner.service"; do
		grep -q '^KillMode=process' "$f" || fail "host-mode unit $f needs KillMode=process"
	done
	# verify wants every referenced unit to exist: stand in for containerd when absent
	if [ ! -e "$S/containerd.service" ] && [ ! -e /etc/systemd/system/containerd.service ]; then
		mkdir -p /run/systemd/system
		printf '[Service]\nExecStart=/bin/true\n' >/run/systemd/system/containerd.service
	fi
	systemd-analyze verify "$S/mirafleet-central.service" "$S/mirafleet-runner.service" \
		"$S/mirafleet-runner-workspace.service" || fail "systemd-analyze verify"
	# The template needs an instance name; the user unit is verified against the user manager's
	# unit set. systemd-analyze is a dev-only tool here: it runs in the test container, never ships.
	systemd-analyze verify "$S/mirafleet-runner@.service" || fail "systemd-analyze verify (template)"
	# --user needs a runtime dir to start its offline manager.
	mkdir -m 0700 -p /tmp/xdg-verify
	XDG_RUNTIME_DIR=/tmp/xdg-verify systemd-analyze --user verify "$U/mirafleet-runner.service" ||
		fail "systemd-analyze --user verify"
	say "systemd-analyze verify (five units; user unit with --user)"
	if ls /etc/systemd/system/*.wants/mirafleet-* >/dev/null 2>&1; then fail "a unit was enabled on install"; fi
	say "units not enabled"
fi

# --- data survives upgrade, remove and purge ------------------------------------------------
echo keep >/var/lib/mirafleet/central/marker
echo keep >/var/lib/mirafleet/runner-ws/marker
alive() {
	[ "$(cat /var/lib/mirafleet/central/marker)" = keep ] || fail "$1: central data lost"
	[ "$(cat /var/lib/mirafleet/runner-ws/marker)" = keep ] || fail "$1: runner-ws data lost"
	getent passwd mirafleet >/dev/null || fail "$1: user deleted"
	getent group mirafleet >/dev/null || fail "$1: group deleted"
}
if [ "$mode" = static ]; then
	P=/src/packaging
	sh "$P/scripts/preremove.sh" upgrade
	sh "$P/scripts/preremove.sh" 1
	sh "$P/scripts/preremove.sh" remove
	sh "$P/scripts/preremove.sh" 0
	sh "$P/scripts/postremove.sh" remove
	sh "$P/scripts/postremove.sh" purge
	alive "maintainer scripts"
else
	case $fam in
	deb)
		dpkg -i "$pkg" >/dev/null 2>&1 || fail "reinstall (upgrade path)"
		alive "reinstall"
		apt-get remove -y -qq mirafleet >/dev/null 2>&1
		[ ! -e /usr/bin/mirafleet ] || fail "binary left after remove"
		absent "$S/mirafleet-central.service"
		alive "remove"
		[ -e /etc/mirafleet/central.env ] || fail "conffile deleted on remove"
		apt-get purge -y -qq mirafleet >/dev/null 2>&1
		alive "purge"
		;;
	rpm)
		dnf reinstall -y -q "$pkg" >/dev/null 2>&1 || rpm -U --force "$pkg" >/dev/null 2>&1 || fail "reinstall"
		alive "reinstall"
		dnf remove -y -q mirafleet >/dev/null 2>&1
		[ ! -e /usr/bin/mirafleet ] || fail "binary left after remove"
		absent "$S/mirafleet-central.service"
		alive "remove"
		;;
	apk)
		# apk has no --force-reinstall for a local file: remove, then add again over the kept data.
		apk del -q mirafleet && apk add -q --allow-untrusted "$pkg" || fail "reinstall"
		alive "reinstall"
		apk del -q mirafleet
		[ ! -e /usr/bin/mirafleet ] || fail "binary left after remove"
		alive "remove"
		;;
	esac
fi
say "upgrade/remove/purge keep /var/lib/mirafleet and the user"
echo "PASS $mode $fam"
CHECK
}

# run_in LABEL IMAGE MODE [PKG]: run the checker; the dist dir and repo are mounted read-only.
run_in() {
	label=$1
	image=$2
	mode=$3
	pkg=${4:-}
	echo "== $label"
	emit_check | $CTR run -q --rm -i --label "$LABEL" -v "$ROOT":/src:ro -v "$DIST_ABS":/dist:ro \
		"$image" sh -s "$mode" "$pkg" || {
		echo "FAIL pkgtest: $label"
		exit 1
	}
}

# find_pkg EXT ARCHNAMES...: first file in DIST matching *<arch>*.<ext>
find_pkg() {
	ext=$1
	shift
	for a in "$@"; do
		for f in "$DIST"/*"$a"*."$ext"; do
			if [ -f "$f" ]; then
				basename "$f"
				return 0
			fi
		done
	done
	return 1
}

if ! $CTR info >/dev/null 2>&1; then
	echo "FAIL pkgtest: container runtime not reachable (colima start)"
	exit 1
fi
mkdir -p "$DIST"
DIST_ABS="$(cd "$DIST" && pwd)"

case "$(uname -m)" in
arm64 | aarch64) A_DEB="arm64 aarch64" A_RPM="aarch64 arm64" ;;
*) A_DEB="amd64 x86_64" A_RPM="x86_64 amd64" ;;
esac

run_in "static debian" "$DEBIAN_IMAGE" static
run_in "static alpine" "$ALPINE_IMAGE" static

n=0
# shellcheck disable=SC2086
if f=$(find_pkg deb $A_DEB); then
	run_in "deb on debian:12" "$DEBIAN_IMAGE" pkg "/dist/$f"
	run_in "deb on ubuntu:24.04" "$UBUNTU_IMAGE" pkg "/dist/$f"
	n=$((n + 2))
else echo "SKIP deb: none in $DIST"; fi
# shellcheck disable=SC2086
if f=$(find_pkg rpm $A_RPM); then
	run_in "rpm on rockylinux:9" "$ROCKY_IMAGE" pkg "/dist/$f"
	n=$((n + 1))
else echo "SKIP rpm: none in $DIST"; fi
# shellcheck disable=SC2086
if f=$(find_pkg apk $A_RPM); then
	run_in "apk on alpine:3" "$ALPINE_IMAGE" pkg "/dist/$f"
	n=$((n + 1))
else echo "SKIP apk: none in $DIST"; fi

echo "OK pkgtest: static checks passed, $n package run(s)"
