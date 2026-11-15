#!/bin/sh
# Install the mirafleet binary from a GitHub release.
#
#   curl -fsSL https://github.com/mirastacklabs-ai/mirafleet/releases/latest/download/install.sh | sh
#   sh install.sh [-v vX.Y.Z | vX.Y.Z] [-b DIR] [--sudo] [--verify-signature]
#
# linux/darwin, amd64/arm64. HTTPS only. The SHA-256 of the archive is always verified against
# checksums.txt; the cosign signature of checksums.txt is verified when cosign is installed, and
# required with --verify-signature. Installs to ~/.local/bin unless -b is given; sudo is used only
# with --sudo (then the default directory is /usr/local/bin).
set -eu

REPO="mirastacklabs-ai/mirafleet"
# Exact keyless identity: the release workflow of this repository, run for a tag.
IDENTITY_RE='^https://github\.com/mirastacklabs-ai/mirafleet/\.github/workflows/release\.yml@refs/tags/v[0-9A-Za-z.+-]+$'
OIDC_ISSUER="https://token.actions.githubusercontent.com"

die() {
	echo "install.sh: $*" >&2
	exit 1
}

usage() {
	cat <<'USAGE'
usage: install.sh [-v vX.Y.Z | vX.Y.Z] [-b DIR] [--sudo] [--verify-signature]
  -v VERSION          release tag to install (default: latest)
  -b DIR              install directory (default: ~/.local/bin, or /usr/local/bin with --sudo)
  --sudo              run mkdir/install through sudo (only when you ask for it)
  --verify-signature  require a valid cosign signature on checksums.txt (needs cosign)
Environment: MIRAFLEET_INSTALL_BASE_URL (https only; the directory holding the release files,
requires an explicit version) is for tests and mirrors.
USAGE
}

version=""
bindir=""
use_sudo=0
need_sig=0
while [ $# -gt 0 ]; do
	case "$1" in
	-v)
		[ $# -ge 2 ] || die "-v needs a value"
		version=$2
		shift 2
		;;
	-b)
		[ $# -ge 2 ] || die "-b needs a value"
		bindir=$2
		shift 2
		;;
	--sudo)
		use_sudo=1
		shift
		;;
	--verify-signature)
		need_sig=1
		shift
		;;
	-h | --help)
		usage
		exit 0
		;;
	v[0-9]*)
		version=$1
		shift
		;;
	*)
		usage >&2
		exit 2
		;;
	esac
done

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) die "unsupported OS $(uname -s); on Windows use install.ps1" ;;
esac
case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) die "unsupported architecture $(uname -m)" ;;
esac

command -v curl >/dev/null 2>&1 || die "curl is required"
if command -v sha256sum >/dev/null 2>&1; then
	sha256() { sha256sum "$1" | awk '{print $1}'; }
elif command -v shasum >/dev/null 2>&1; then
	sha256() { shasum -a 256 "$1" | awk '{print $1}'; }
elif command -v openssl >/dev/null 2>&1; then
	sha256() { openssl dgst -sha256 "$1" | awk '{print $NF}'; }
else
	die "no sha256 tool (sha256sum, shasum or openssl): refusing to install unverified"
fi

fetch() { # URL DEST
	case "$1" in
	https://*) ;;
	*) die "refusing non-HTTPS URL: $1" ;;
	esac
	curl --proto '=https' --tlsv1.2 -fsSL --retry 2 -o "$2" "$1" || die "download failed: $1"
}

base="${MIRAFLEET_INSTALL_BASE_URL:-}"
case "$base" in
"") ;;
https://*) base=${base%/} ;;
*) die "MIRAFLEET_INSTALL_BASE_URL must be https://, got: $base" ;;
esac

if [ -z "$version" ]; then
	[ -z "$base" ] || die "MIRAFLEET_INSTALL_BASE_URL needs an explicit version"
	final=$(curl --proto '=https' --tlsv1.2 -fsSL -o /dev/null -w '%{url_effective}' \
		"https://github.com/$REPO/releases/latest") || die "cannot resolve the latest release"
	version=${final##*/}
fi
case "$version" in *[!0-9A-Za-z.+-]*) die "invalid version" ;; esac
printf '%s\n' "$version" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+([-+.][0-9A-Za-z.-]+)?$' ||
	die "invalid version: $version"
[ -n "$base" ] || base="https://github.com/$REPO/releases/download/$version"

archive="mirafleet_${version#v}_${os}_${arch}.tar.gz"

if [ -z "$bindir" ]; then
	if [ "$use_sudo" -eq 1 ]; then bindir=/usr/local/bin; else bindir="${HOME:?HOME is not set}/.local/bin"; fi
fi

tmp=$(mktemp -d "${TMPDIR:-/tmp}/mirafleet-install.XXXXXX") || die "cannot create a temp dir"
trap 'rm -rf "$tmp"' EXIT INT TERM HUP

echo "Installing mirafleet $version ($os/$arch) to $bindir"
fetch "$base/checksums.txt" "$tmp/checksums.txt"

if [ "$need_sig" -eq 1 ] || command -v cosign >/dev/null 2>&1; then
	command -v cosign >/dev/null 2>&1 || die "--verify-signature needs cosign on PATH"
	fetch "$base/checksums.txt.sigstore.json" "$tmp/checksums.txt.sigstore.json"
	cosign verify-blob \
		--bundle "$tmp/checksums.txt.sigstore.json" \
		--certificate-identity-regexp "$IDENTITY_RE" \
		--certificate-oidc-issuer "$OIDC_ISSUER" \
		"$tmp/checksums.txt" >/dev/null 2>&1 || die "cosign signature verification of checksums.txt FAILED"
	echo "cosign signature of checksums.txt verified"
else
	echo "cosign not found: skipping signature check (checksum is still verified)"
fi

want=$(awk -v f="$archive" '$2 == f || $2 == "*" f {print $1; exit}' "$tmp/checksums.txt")
[ -n "$want" ] || die "$archive is not listed in checksums.txt"
printf '%s\n' "$want" | grep -Eq '^[0-9a-fA-F]{64}$' || die "malformed checksum for $archive"

fetch "$base/$archive" "$tmp/$archive"
got=$(sha256 "$tmp/$archive")
[ "$got" = "$want" ] || die "checksum mismatch for $archive (want $want, got $got)"
echo "sha256 verified"

mkdir "$tmp/x"
tar -xzf "$tmp/$archive" -C "$tmp/x" mirafleet || die "$archive does not contain a mirafleet binary"
if [ ! -f "$tmp/x/mirafleet" ] || [ -L "$tmp/x/mirafleet" ]; then die "unexpected archive content"; fi

if [ "$use_sudo" -eq 1 ]; then run="sudo"; else run=""; fi
# shellcheck disable=SC2086
$run mkdir -p "$bindir"
# shellcheck disable=SC2086
$run install -m 0755 "$tmp/x/mirafleet" "$bindir/mirafleet"
echo "Installed $bindir/mirafleet"
case ":$PATH:" in
*":$bindir:"*) ;;
*) echo "Note: $bindir is not on your PATH; add it to your shell profile." ;;
esac
