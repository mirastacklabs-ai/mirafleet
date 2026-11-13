#!/bin/sh
# Run every Go toolchain step inside a container; nothing is installed on the host.
# Default runtime: the Docker CLI against colima's Docker runtime (`colima start`).
# Override with MIRAFLEET_CTR, e.g. MIRAFLEET_CTR=nerdctl on a Linux host with containerd.
# MIRAFLEET_CTR and MIRAFLEET_GO_IMAGE are trusted caller environment (the CTR value is
# word-split and executed); never derive them from untrusted input.
#
# Usage: scripts/dev.sh build|test|vet|cross|fmt|shell|go <args...>
set -eu

# Pinned by tag and by multi-arch index digest.
GO_IMAGE="${MIRAFLEET_GO_IMAGE:-docker.io/library/golang:1.27.3-bookworm@sha256:dc0963f32053e4bc451d24d87e3892d42d01021b61f093e98eb95c0352bb84b0}"
CTR="${MIRAFLEET_CTR:-docker}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	echo "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)" >&2
	exit 1
fi

# Supported targets for every binary we ship. Keep in sync with docs/PLAN.md (M6).
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

# run MODE SCRIPT [ARG...]: MODE is ro|rw (source mount); SCRIPT runs under sh -ec in the
# container; ARGs become $1, $2, ... Set TTY=-t for an interactive shell.
# Named volumes mirafleet-gomod and mirafleet-gocache hold the module and build caches.
# Everything runs as the host user so files written to the source mount are never root-owned.
# -q suppresses pull output.
# shellcheck disable=SC2086
run() {
	mode=$1
	script=$2
	shift 2
	# Named volumes are created root-owned; hand them to the host user once (idempotent).
	$CTR run -q --rm -v mirafleet-gomod:/go/pkg/mod -v mirafleet-gocache:/gocache \
		"$GO_IMAGE" sh -c '[ "$(stat -c %u /gocache)" = "$1" ] && [ "$(stat -c %u /go/pkg/mod)" = "$1" ] || chown -R "$1:$2" /go/pkg/mod /gocache' sh "$(id -u)" "$(id -g)"
	$CTR run -q --rm -i ${TTY:-} \
		--user "$(id -u):$(id -g)" \
		-v "$ROOT":/src:"$mode" \
		-v mirafleet-gomod:/go/pkg/mod \
		-v mirafleet-gocache:/gocache \
		-w /src \
		-e HOME=/tmp \
		-e GOCACHE=/gocache \
		-e GOFLAGS="-buildvcs=false -modcacherw" \
		"$GO_IMAGE" sh -ec "$PRECHECK$script" sh "$@"
}

PRECHECK='[ -f /src/go.mod ] || { echo "source not visible in the VM: colima only mounts \$HOME by default" >&2; exit 1; }; '

cmd="${1:-}"
[ $# -gt 0 ] && shift
case "$cmd" in
build) run ro 'CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go build -o /dev/null ./cmd/mirafleet' ;;
test)  run ro 'go test -race ./... && CGO_ENABLED=0 go test ./...' ;; # -race needs cgo; shipped builds stay CGO_ENABLED=0
vet)   run ro 'go vet ./...' ;;
fmt)   run ro 'out=$(gofmt -l .); [ -z "$out" ] || { echo "gofmt needed:"; echo "$out"; exit 1; }' ;;
cross)
	run ro "for t in $TARGETS; do
		export GOOS=\${t%/*} GOARCH=\${t#*/} CGO_ENABLED=0
		echo \"== \$t\"
		go build -o /dev/null ./cmd/mirafleet
		go vet ./...
	done"
	;;
shell)
	TTY=
	[ -t 0 ] && TTY=-t
	run rw 'exec bash'
	;;
go) run rw 'exec go "$@"' "$@" ;;
*)
	echo "usage: scripts/dev.sh build|test|vet|cross|fmt|shell|go <args...>" >&2
	exit 2
	;;
esac
