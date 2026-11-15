#!/bin/sh
# Builds the OCI image locally from a binary (no push) and checks it (docs/M6.md sections 2.7, 2.14).
# Everything runs in containers; the host only needs the Docker CLI (colima's runtime) and tar.
#
# Usage: scripts/release/imagetest.sh [test|build]
#   build   build the image and print its tag (labtest.sh uses this)
#   test    build, then check the image (default)
# Environment (trusted caller input; CTR is word-split and executed):
#   MIRAFLEET_CTR         container CLI (default docker)
#   MIRAFLEET_BIN         an existing linux binary for the daemon's architecture (default: built
#                         with scripts/dev.sh into dist/imagetest, which is gitignored)
#   MIRAFLEET_TEST_IMAGE  tag to build (default mirafleet-imagetest:local)
#   MIRAFLEET_GO_IMAGE    tool image with bash, openssl and curl (default: dev.sh's Go image)
# The GUI checks (TLS on 8443, fingerprint equals the log) run when the binary has -gui-listen.
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CTR="${MIRAFLEET_CTR:-docker}"
TOOL_IMAGE="${MIRAFLEET_GO_IMAGE:?run through scripts/release.sh (it pins the Go image)}"
IMG="${MIRAFLEET_TEST_IMAGE:-mirafleet-imagetest:local}"
LABEL=org.mirastacklabs.mirafleet.release=1
WORK="$ROOT/dist/imagetest"

arch() {
	case "$($CTR info --format '{{.Architecture}}')" in
	aarch64 | arm64) echo arm64 ;;
	x86_64 | amd64) echo amd64 ;;
	*) echo "unsupported daemon architecture" >&2; return 1 ;;
	esac
}

build() {
	a=$(arch)
	ctx="$WORK/ctx"
	rm -rf "$ctx"
	mkdir -p "$ctx/linux/$a"
	if [ -n "${MIRAFLEET_BIN:-}" ]; then
		cp "$MIRAFLEET_BIN" "$ctx/linux/$a/mirafleet"
	else
		# Native build in the pinned Go container; the output lands in the gitignored dist/.
		MIRAFLEET_CGO=0 "$ROOT/scripts/dev.sh" go build -trimpath -o "dist/imagetest/ctx/linux/$a/mirafleet" ./cmd/mirafleet
	fi
	cp "$ROOT/LICENSE" "$ROOT/NOTICE" "$ctx/"
	if [ -f "$ROOT/dist/THIRD_PARTY_LICENSES.txt" ]; then
		cp "$ROOT/dist/THIRD_PARTY_LICENSES.txt" "$ctx/"
	else
		echo "test placeholder: run scripts/release.sh licenses-report for the real file" >"$ctx/THIRD_PARTY_LICENSES.txt"
	fi
	$CTR build -q --platform "linux/$a" --label "$LABEL" \
		--build-arg VERSION=0.0.0-test --build-arg REVISION=imagetest \
		-f "$ROOT/packaging/oci/Dockerfile" -t "$IMG" "$ctx" >/dev/null
}

case "${1:-test}" in
build)
	build
	echo "$IMG"
	exit 0
	;;
test) ;;
*)
	echo "usage: scripts/release/imagetest.sh [test|build]" >&2
	exit 2
	;;
esac

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	echo "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)" >&2
	exit 1
fi

fail() {
	echo "FAIL: $*" >&2
	exit 1
}
ok() { echo "ok: $*"; }

C=""
V="mfimgtest-$$"
cleanup() {
	[ -z "$C" ] || $CTR rm -f "$C" >/dev/null 2>&1 || true
	$CTR volume rm -f "$V" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

# --- Dockerfile shape: FROM scratch and nothing but COPY (no RUN: builds without QEMU) ---
DF="$ROOT/packaging/oci/Dockerfile"
[ "$(grep -c '^FROM ' "$DF")" = 1 ] && grep -q '^FROM scratch$' "$DF" || fail "Dockerfile is not a single FROM scratch"
! grep -qE '^(RUN|ADD)[[:space:]]' "$DF" || fail "Dockerfile has RUN/ADD lines"
ok "Dockerfile: FROM scratch, only COPY"

build
ok "image built: $IMG"

# --- image config ---
[ "$($CTR inspect -f '{{.Config.User}}' "$IMG")" = "65532:65532" ] || fail "image user is not 65532:65532"
ports=$($CTR inspect -f '{{json .Config.ExposedPorts}}' "$IMG")
case "$ports" in *'"2222/tcp"'*) ;; *) fail "2222 not exposed: $ports" ;; esac
case "$ports" in *'"8443/tcp"'*) ;; *) fail "8443 not exposed: $ports" ;; esac
for l in title description source licenses version revision; do
	[ -n "$($CTR inspect -f "{{index .Config.Labels \"org.opencontainers.image.$l\"}}" "$IMG")" ] || fail "label $l missing"
done
ok "non-root user 65532, EXPOSE 2222 8443, OCI labels"

for sh in /bin/sh sh /bin/bash /busybox/sh; do
	if $CTR run --rm --entrypoint "$sh" "$IMG" -c true >/dev/null 2>&1; then
		fail "the image has a shell ($sh)"
	fi
done
ok "no shell in the image"

# --- no certificate, key or anything else but the expected files in any layer ---
rm -rf "$WORK/save"
mkdir -p "$WORK/save"
$CTR save "$IMG" -o "$WORK/img.tar"
tar -xf "$WORK/img.tar" -C "$WORK/save"
: >"$WORK/files.txt"
find "$WORK/save" -type f | while IFS= read -r f; do
	tar -tf "$f" 2>/dev/null || true # blobs that are not tars (config, manifests) are skipped
done | sed 's|^\./||; s|^/||' | grep -v '/$' | sort -u >"$WORK/files.txt"
grep -qx 'usr/bin/mirafleet' "$WORK/files.txt" || fail "layer scan found no binary (scan is not reading the layers)"
extra=$(grep -vxE 'usr/bin/mirafleet|usr/share/doc/mirafleet/(LICENSE|NOTICE|THIRD_PARTY_LICENSES\.txt)|var/lib/mirafleet/\.keep' "$WORK/files.txt" || true)
[ -z "$extra" ] || fail "unexpected files in the image layers: $extra"
ok "layers hold only the binary, licence files and the data dir marker (no cert, key or secret)"

# --- init, then run, on random ports ---
$CTR volume create --label "$LABEL" "$V" >/dev/null
$CTR run --rm -v "$V:/var/lib/mirafleet" "$IMG" central init -data /var/lib/mirafleet/central >"$WORK/init.log"
FP=$(sed -n 's/^fingerprint: *//p' "$WORK/init.log")
[ -n "$FP" ] || fail "central init printed no fingerprint"
$CTR run --rm -v "$V:/var/lib/mirafleet" "$IMG" central init -data /var/lib/mirafleet/central >/dev/null || fail "central init is not repeatable"
C=$($CTR run -d --label "$LABEL" -v "$V:/var/lib/mirafleet" -p 127.0.0.1::2222 -p 127.0.0.1::8443 "$IMG")

n=0
until $CTR exec "$C" mirafleet central fleet list >/dev/null 2>&1; do
	n=$((n + 1))
	[ "$n" -le 40 ] || { $CTR logs "$C" >&2; fail "central did not come up"; }
	[ "$($CTR inspect -f '{{.State.Running}}' "$C")" = true ] || { $CTR logs "$C" >&2; fail "central exited"; }
	sleep 1
done
$CTR exec "$C" mirafleet central fleet create smoke >/dev/null
$CTR exec "$C" mirafleet central fleet list | grep -q smoke || fail "fleet list does not show the new fleet"
ok "central init + run; docker exec ... fleet create/list"

code_fp=$($CTR exec "$C" mirafleet central operator code | sed -n 's/^host key: *//p')
[ "$code_fp" = "$FP" ] || fail "init fingerprint ($FP) differs from the running host key ($code_fp)"
ok "SSH host key fingerprint in the init output matches the running central"

# Runs as UID 65532: the files central made in the volume belong to it.
uid=$($CTR run --rm -v "$V:/v:ro" "$TOOL_IMAGE" stat -c %u /v/central/host_ed25519)
[ "$uid" = 65532 ] || fail "host key is owned by uid $uid, not 65532"
ok "data written as uid 65532"

# SSH reachable on the published port (the server speaks first).
$CTR port "$C" 2222/tcp >/dev/null || fail "2222 is not published"
banner=$($CTR run --rm --network "container:$C" "$TOOL_IMAGE" bash -c 'exec 3<>/dev/tcp/127.0.0.1/2222; head -c 4 <&3')
[ "$banner" = "SSH-" ] || fail "no SSH banner on 2222 (got: $banner)"
ok "SSH banner on 2222"

# --- GUI (needs the M4 flags in the binary) ---
if $CTR run --rm "$IMG" central run -h 2>&1 | grep -q -- '-gui-listen'; then
	gui_fp() { # prints "hex b64" of the certificate served on 8443
		$CTR run --rm --network "container:$C" "$TOOL_IMAGE" bash -c '
			der=$(mktemp)
			openssl s_client -connect 127.0.0.1:8443 -servername localhost </dev/null 2>/dev/null | openssl x509 -outform der >"$der" || exit 1
			[ -s "$der" ] || exit 1
			echo "$(openssl dgst -sha256 -binary "$der" | od -An -v -tx1 | tr -d " \n") $(openssl dgst -sha256 -binary "$der" | base64 | tr -d "=\n")"'
	}
	n=0
	until served=$(gui_fp 2>/dev/null); do
		n=$((n + 1))
		[ "$n" -le 20 ] || { $CTR logs "$C" >&2; fail "no TLS answer on 8443"; }
		sleep 1
	done
	hex=${served% *}
	b64=${served#* }
	log_has() {
		$CTR logs "$C" 2>&1 | tr 'A-F' 'a-f' | tr -d ':' | grep -qF "$hex" ||
			$CTR logs "$C" 2>&1 | tr -d '=' | grep -qF "$b64"
	}
	log_has || { $CTR logs "$C" >&2; fail "GUI certificate fingerprint ($hex) is not in the log"; }
	code=$($CTR run --rm --network "container:$C" "$TOOL_IMAGE" curl -sk -o /dev/null -w '%{http_code}' https://127.0.0.1:8443/)
	case "$code" in 200 | 303) ;; *) fail "GUI answered HTTP $code" ;; esac
	$CTR run --rm -v "$V:/v:ro" "$TOOL_IMAGE" test -s /v/central/gui/tls.crt || fail "GUI certificate is not in the data volume"
	$CTR restart -t 10 "$C" >/dev/null
	n=0
	until again=$(gui_fp 2>/dev/null); do
		n=$((n + 1))
		[ "$n" -le 30 ] || fail "no TLS answer on 8443 after restart"
		sleep 1
	done
	[ "${again% *}" = "$hex" ] || fail "GUI certificate changed across a restart"
	ok "GUI TLS on 8443, fingerprint equals the log, certificate kept in the volume across a restart"
else
	echo "skip: this binary has no -gui-listen; GUI checks run once M4 is merged"
fi

echo "OK imagetest"
