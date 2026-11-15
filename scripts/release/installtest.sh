#!/bin/sh
# Test scripts/install.sh against a local HTTPS server serving fake release files, inside a
# digest-pinned container (no network, nothing installed on the host).
#   scripts/release/installtest.sh
# The cosign cases use a stand-in `cosign` that accepts only a signature made for the expected
# identity and issuer, so they prove install.sh passes and enforces them (real keyless signing needs
# Sigstore and is covered by the release smoke test).
set -eu

IMAGE="${MIRAFLEET_GO_IMAGE:-docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea}"
CTR="${MIRAFLEET_CTR:-docker}"

if [ "${1:-}" != "--inside" ]; then
	ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
	exec $CTR run -q --rm --network none -v "$ROOT/scripts":/scripts:ro "$IMAGE" \
		sh /scripts/release/installtest.sh --inside
fi

# ---- inside the container ----
INSTALL=/scripts/install.sh
W=$(mktemp -d)
export HOME="$W/home" GOCACHE="$W/gocache" GOPATH="$W/gopath" GOFLAGS=-mod=mod
mkdir -p "$HOME" "$W/www" "$W/bin"
fail=0
ok() { echo "ok   $*"; }
bad() { echo "FAIL $*"; fail=1; }

# TLS server (self-signed for localhost).
openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=localhost \
	-addext subjectAltName=DNS:localhost -keyout "$W/key.pem" -out "$W/cert.pem" 2>/dev/null
cat >"$W/srv.go" <<'GO'
package main

import (
	"net"
	"net/http"
	"os"
)

func main() {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	os.WriteFile(os.Args[4], []byte(l.Addr().(*net.TCPAddr).AddrPort().Port()), 0o600)
	panic(http.ServeTLS(l, http.FileServer(http.Dir(os.Args[1])), os.Args[2], os.Args[3]))
}
GO
(cd "$W" && CGO_ENABLED=0 go build -o srv srv.go)
"$W/srv" "$W/www" "$W/cert.pem" "$W/key.pem" "$W/port" &
srv=$!
trap 'kill $srv 2>/dev/null; rm -rf "$W"' EXIT
i=0
while [ ! -s "$W/port" ] && [ $i -lt 50 ]; do sleep 0.1; i=$((i + 1)); done
[ -s "$W/port" ] || { echo "server did not start"; exit 1; }
BASE="https://localhost:$(cat "$W/port")"
export CURL_CA_BUNDLE="$W/cert.pem"

case "$(uname -m)" in aarch64) A=arm64 ;; *) A=amd64 ;; esac
V=v9.9.9
ARCHIVE="mirafleet_9.9.9_linux_$A.tar.gz"
printf '#!/bin/sh\necho mirafleet fake 9.9.9\n' >"$W/mirafleet"
chmod +x "$W/mirafleet"
tar -czf "$W/www/$ARCHIVE" -C "$W" mirafleet
good=$(sha256sum "$W/www/$ARCHIVE" | awk '{print $1}')
printf '%s  %s\n' "$good" "$ARCHIVE" >"$W/www/checksums.txt"
printf 'sig-by: https://github.com/mirastacklabs-ai/mirafleet/.github/workflows/release.yml@refs/tags/v9.9.9\n' >"$W/www/checksums.txt.sig"
echo cert >"$W/www/checksums.txt.pem"

run_install() { # DIR [ARGS...] ; env MIRAFLEET_INSTALL_BASE_URL set by caller
	d=$1
	shift
	sh "$INSTALL" -v "$V" -b "$d" "$@" >"$W/out" 2>&1
}

# 1. good checksum installs and runs
export MIRAFLEET_INSTALL_BASE_URL="$BASE"
if run_install "$W/d1" && [ "$("$W/d1/mirafleet")" = "mirafleet fake 9.9.9" ]; then ok "good checksum installs"; else bad "good checksum"; cat "$W/out"; fi

# 2. bad checksum refuses
printf '%s  %s\n' "$(printf x | sha256sum | awk '{print $1}')" "$ARCHIVE" >"$W/www/checksums.txt"
if run_install "$W/d2"; then bad "bad checksum installed"; elif [ ! -e "$W/d2/mirafleet" ] && grep -q mismatch "$W/out"; then ok "bad checksum refused"; else bad "bad checksum wrong failure"; cat "$W/out"; fi

# 3. archive not listed refuses
printf '%s  other.tar.gz\n' "$good" >"$W/www/checksums.txt"
if run_install "$W/d3"; then bad "unlisted installed"; elif [ ! -e "$W/d3/mirafleet" ] && grep -q "not listed" "$W/out"; then ok "unlisted archive refused"; else bad "unlisted wrong failure"; cat "$W/out"; fi
printf '%s  %s\n' "$good" "$ARCHIVE" >"$W/www/checksums.txt"

# 4. plain HTTP refused
if MIRAFLEET_INSTALL_BASE_URL="http://localhost:1" run_install "$W/d4"; then bad "http installed"; elif [ ! -e "$W/d4/mirafleet" ] && grep -q "https" "$W/out"; then ok "http refused"; else bad "http wrong failure"; cat "$W/out"; fi

# 5. --verify-signature without cosign refuses
if run_install "$W/d5" --verify-signature; then bad "verify-signature without cosign installed"; elif [ ! -e "$W/d5/mirafleet" ] && grep -q cosign "$W/out"; then ok "--verify-signature without cosign refused"; else bad "no-cosign wrong failure"; cat "$W/out"; fi

# 6-7. stand-in cosign: accepts only the exact identity regexp and issuer given by install.sh
cat >"$W/bin/cosign" <<'CS'
#!/bin/sh
[ "$1" = verify-blob ] || exit 2
shift
sig="" re="" iss=""
while [ $# -gt 0 ]; do
	case "$1" in
	--signature) sig=$2; shift 2 ;;
	--certificate-identity-regexp) re=$2; shift 2 ;;
	--certificate-oidc-issuer) iss=$2; shift 2 ;;
	*) shift ;;
	esac
done
[ "$iss" = "https://token.actions.githubusercontent.com" ] || exit 1
ident=$(sed -n 's/^sig-by: //p' "$sig")
printf '%s\n' "$ident" | grep -Eq "$re"
CS
chmod +x "$W/bin/cosign"
OLDPATH=$PATH
PATH="$W/bin:$PATH"
if run_install "$W/d6"; then ok "cosign present, right identity installs"; else bad "right identity"; cat "$W/out"; fi
printf 'sig-by: https://github.com/evil/mirafleet/.github/workflows/release.yml@refs/tags/v9.9.9\n' >"$W/www/checksums.txt.sig"
if run_install "$W/d7"; then bad "wrong identity installed"; elif [ ! -e "$W/d7/mirafleet" ] && grep -q FAILED "$W/out"; then ok "wrong cosign identity refused"; else bad "wrong identity wrong failure"; cat "$W/out"; fi
printf 'sig-by: https://github.com/mirastacklabs-ai/mirafleet/.github/workflows/other.yml@refs/heads/main\n' >"$W/www/checksums.txt.sig"
if run_install "$W/d8"; then bad "wrong workflow installed"; else ok "wrong workflow refused"; fi
PATH=$OLDPATH

if [ $fail -eq 0 ]; then echo "installtest: OK"; else echo "installtest: FAIL"; exit 1; fi
