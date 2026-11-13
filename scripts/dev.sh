#!/bin/sh
# Run every Go toolchain step inside a container; nothing is installed on the host.
# Default runtime: a colima VM with the containerd runtime, driven by nerdctl
#   colima start -p mirafleet --runtime containerd --cpu 4 --memory 6 --disk 30
# Override with MIRAFLEET_CTR, e.g. MIRAFLEET_CTR=nerdctl on a Linux host with containerd.
#
# Usage: scripts/dev.sh build|test|vet|cross|fmt|shell|go <args...>
set -eu

GO_IMAGE="${MIRAFLEET_GO_IMAGE:-docker.io/library/golang:1.27.3-bookworm}"
CTR="${MIRAFLEET_CTR:-colima nerdctl -p mirafleet --}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

# Supported targets for every binary we ship. Keep in sync with docs/PLAN.md (M6).
TARGETS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

# run SCRIPT [ARG...]: SCRIPT runs under sh -ec in the container; ARGs become $1, $2, ...
# Volume warnings ("already exists") come from nerdctl itself and are harmless.
# Source is read-only unless MODE=rw (go, shell). -q suppresses image pull output.
# shellcheck disable=SC2086
run() {
	script=$1
	shift
	$CTR run -q --rm -i \
		-v "$ROOT":/src:"${MODE:-ro}" \
		-v mirafleet-gomod:/go/pkg/mod \
		-v mirafleet-gocache:/root/.cache/go-build \
		-w /src \
		-e GOFLAGS=-buildvcs=false \
		"$GO_IMAGE" sh -ec "$script" sh "$@"
}

cmd="${1:-}"
[ $# -gt 0 ] && shift
case "$cmd" in
build) run 'CGO_ENABLED=0 go build ./... && CGO_ENABLED=0 go build -o /dev/null ./cmd/mirafleet' ;;
test)  run 'go test -race ./...' ;; # -race needs cgo; only tests use it, shipped builds stay CGO_ENABLED=0
vet)   run 'go vet ./...' ;;
fmt)   run 'out=$(gofmt -l .); [ -z "$out" ] || { echo "gofmt needed:"; echo "$out"; exit 1; }' ;;
cross)
	run "for t in $TARGETS; do
		export GOOS=\${t%/*} GOARCH=\${t#*/} CGO_ENABLED=0
		echo \"== \$t\"
		go build -o /dev/null ./cmd/mirafleet
		go vet ./...
	done"
	;;
shell)
	# shellcheck disable=SC2086
	$CTR run -q --rm -it -v "$ROOT":/src -v mirafleet-gomod:/go/pkg/mod -w /src "$GO_IMAGE" bash
	;;
go) MODE=rw run 'exec go "$@"' "$@" ;;
*)
	echo "usage: scripts/dev.sh build|test|vet|cross|fmt|shell|go <args...>" >&2
	exit 2
	;;
esac
