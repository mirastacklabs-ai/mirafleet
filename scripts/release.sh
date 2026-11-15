#!/bin/sh
# M6 release tooling (docs/M6.md). Every step runs in a digest-pinned container; nothing is
# installed on the host. dev.sh is not touched by M6: release targets live here, and N2/N3 add
# theirs below the marked line. ONE target per call.
# Default runtime: the Docker CLI against colima's Docker runtime (`colima start`).
# Override with MIRAFLEET_CTR. MIRAFLEET_CTR and the *_IMAGE variables are trusted caller
# environment (CTR is word-split and executed); never derive them from untrusted input.
#
# Usage: scripts/release.sh licenses-report|vuln|lint-sh|lint-actions|clean
set -eu

# Pinned by tag and multi-arch index digest. GO_IMAGE must equal the one in dev.sh
# (a relgen test enforces it).
GO_IMAGE="${MIRAFLEET_GO_IMAGE:-docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea}"
SHELLCHECK_IMAGE="${MIRAFLEET_SHELLCHECK_IMAGE:-docker.io/koalaman/shellcheck:v0.10.0@sha256:2097951f02e735b613f4a34de20c40f937a6c8f18ecb170612c88c34517221fb}"
ACTIONLINT_IMAGE="${MIRAFLEET_ACTIONLINT_IMAGE:-docker.io/rhysd/actionlint:1.7.7@sha256:887a259a5a534f3c4f36cb02dca341673c6089431057242cdc931e9f133147e9}"
CTR="${MIRAFLEET_CTR:-docker}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Tools built into the Go container's /tmp (never the host), pinned by version.
GO_LICENSES_VERSION=v1.6.0
GOVULNCHECK_VERSION=v1.9.0 # v1.1.x crashes (old x/tools) on Go 1.27

# Label on every image, container and volume a release test creates, so `clean` finds them.
LABEL=org.mirastacklabs.mirafleet.release=1

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	echo "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)" >&2
	exit 1
fi

# Same targets as dev.sh (docs/PLAN.md M6).
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

# gorun MODE SCRIPT [ARG...]: like dev.sh run. MODE is ro|rw (source mount); SCRIPT runs under
# sh -ec in the Go container; ARGs become $1, $2, ... Shares dev.sh's cache volumes.
# EXTRA holds extra `-v` mounts (word-split, trusted). -q suppresses pull output.
# shellcheck disable=SC2086
gorun() {
	mode=$1
	script=$2
	shift 2
	$CTR run -q --rm -v mirafleet-gomod:/go/pkg/mod -v mirafleet-gocache:/gocache \
		"$GO_IMAGE" sh -c '[ "$(stat -c %u /gocache)" = "$1" ] && [ "$(stat -c %u /go/pkg/mod)" = "$1" ] || chown -R "$1:$2" /go/pkg/mod /gocache' sh "$(id -u)" "$(id -g)"
	$CTR run -q --rm -i \
		--user "$(id -u):$(id -g)" \
		-v "$ROOT":/src:"$mode" ${EXTRA:-} \
		-v mirafleet-gomod:/go/pkg/mod \
		-v mirafleet-gocache:/gocache \
		-w /src \
		-e HOME=/tmp \
		-e GOCACHE=/gocache \
		-e GOPATH=/gocache/gopath \
		-e GOMODCACHE=/go/pkg/mod \
		-e GOFLAGS="-buildvcs=false -modcacherw" \
		"$GO_IMAGE" sh -ec "[ -f /src/go.mod ] || { echo 'source not visible in the VM: colima only mounts \$HOME by default' >&2; exit 1; }; $script" sh "$@"
}

cmd="${1:-}"
[ $# -gt 0 ] && shift
case "$cmd" in
licenses-report)
	# THIRD_PARTY_LICENSES.txt: go-licenses save per target (so darwin/windows-only modules are
	# included), merged by `relgen licenses` with the hand-reviewed licenses/extra. The --ignores
	# are the same as dev.sh licenses and are covered by licenses/extra (relgen checks the tie).
	# Output: dist/THIRD_PARTY_LICENSES.txt (dist/ is gitignored); the source mount stays read-only.
	mkdir -p "$ROOT/dist"
	EXTRA="-v $ROOT/dist:/src/dist:rw" gorun ro "rm -rf /src/dist/licenses-raw
		GOBIN=/tmp/bin go install github.com/google/go-licenses@$GO_LICENSES_VERSION
		for t in $TARGETS; do
			export GOOS=\${t%/*} GOARCH=\${t#*/} CGO_ENABLED=0
			echo \"== \$t\"
			/tmp/bin/go-licenses check ./... \\
				--ignore modernc.org/mathutil \\
				--ignore github.com/segmentio/asm \\
				--allowed_licenses=Apache-2.0,MIT,MIT-0,BSD-2-Clause,BSD-3-Clause,ISC,0BSD
			/tmp/bin/go-licenses save ./... --save_path \"/src/dist/licenses-raw/\$(echo \$t | tr / _)\" \\
				--ignore modernc.org/mathutil \\
				--ignore github.com/segmentio/asm
		done
		unset GOOS GOARCH CGO_ENABLED
		go run ./tools/relgen licenses -o /src/dist/THIRD_PARTY_LICENSES.txt /src/dist/licenses-raw/*
		rm -rf /src/dist/licenses-raw"
	;;
vuln)
	gorun ro "GOBIN=/tmp/bin go install golang.org/x/vuln/cmd/govulncheck@$GOVULNCHECK_VERSION
		/tmp/bin/govulncheck ./..."
	;;
lint-sh)
	# Every tracked-style shell script: scripts/*.sh and scripts/release/*.sh (N2 adds the latter).
	set -- "$ROOT"/scripts/*.sh
	[ ! -d "$ROOT/scripts/release" ] || set -- "$@" "$ROOT"/scripts/release/*.sh
	files=""
	for f in "$@"; do
		[ -f "$f" ] && files="$files ${f#"$ROOT"/}"
	done
	# shellcheck disable=SC2086
	$CTR run -q --rm --user "$(id -u):$(id -g)" -v "$ROOT":/src:ro -w /src \
		"$SHELLCHECK_IMAGE" -x --severity=warning $files
	;;
lint-actions)
	$CTR run -q --rm --user "$(id -u):$(id -g)" -v "$ROOT":/src:ro -w /src \
		"$ACTIONLINT_IMAGE" -color
	;;
clean)
	# dist/ plus labelled release-test containers, images and volumes. The Go module and build
	# cache volumes (mirafleet-gomod, mirafleet-gocache) are kept.
	rm -rf "$ROOT/dist"
	# shellcheck disable=SC2046,SC2086
	{
		ids=$($CTR ps -aq --filter "label=$LABEL")
		[ -z "$ids" ] || $CTR rm -f $ids >/dev/null
		ids=$($CTR images -q --filter "label=$LABEL")
		[ -z "$ids" ] || $CTR rmi -f $ids >/dev/null
		ids=$($CTR volume ls -q --filter "label=$LABEL")
		[ -z "$ids" ] || $CTR volume rm -f $ids >/dev/null
	}
	echo "clean: dist/ and labelled release-test resources removed"
	;;
# --- N2/N3 targets go below this line (snapshot, pkgtest, ..., m6check) ---
*)
	echo "usage: scripts/release.sh licenses-report|vuln|lint-sh|lint-actions|clean" >&2
	exit 2
	;;
esac
