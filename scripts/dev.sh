#!/bin/sh
# Run every Go toolchain step inside a container; nothing is installed on the host.
# Default runtime: the Docker CLI against colima's Docker runtime (`colima start`).
# Override with MIRAFLEET_CTR, e.g. MIRAFLEET_CTR=nerdctl on a Linux host with containerd.
# MIRAFLEET_CTR and MIRAFLEET_GO_IMAGE are trusted caller environment (the CTR value is
# word-split and executed); never derive them from untrusted input.
#
# Usage: scripts/dev.sh build|test|vet|cross|fmt|licenses|integration|integration-host|shell|go <args...>
set -eu

# Pinned by tag and by multi-arch index digest.
GO_IMAGE="${MIRAFLEET_GO_IMAGE:-docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea}"
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
		-e GOPATH=/gocache/gopath \
		-e GOMODCACHE=/go/pkg/mod \
		-e GOFLAGS="-buildvcs=false -modcacherw" \
		${MIRAFLEET_CGO:+-e CGO_ENABLED=$MIRAFLEET_CGO} \
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
licenses)
	# Linked code only (what go-licenses checks), for every target in $TARGETS so darwin/windows-only
	# modules are checked too; allowlist per CLAUDE.md. go-licenses is built into the
	# container's /tmp (host-arch, before GOOS is exported per target) and never installed on the host.
	# NOTE: go-licenses classifies each module's top-level LICENSE only. modernc.org/libc bundles
	# musl-derived MIT code that it does not see (reviewed manually, see docs/M2.md §6.3).
	# --ignore modernc.org/mathutil: its LICENSE is verbatim BSD-3-Clause text (reviewed by hand)
	# but go-licenses' classifier cannot match it ("The mathutil Authors" wording). The ignore is
	# valid only for the reviewed version, pinned below; any bump fails until re-reviewed.
	# --ignore github.com/segmentio/asm: its LICENSE is verbatim MIT-0 (MIT No Attribution,
	# approved and on the CLAUDE.md allowlist), reviewed by hand, but go-licenses v1.6.0 has no
	# MIT-0 classifier and reports it as unknown. MIT-0 stays in --allowed_licenses for when it does.
	# Pinned to the reviewed version like mathutil. Linked via go-sdk -> segmentio/encoding (M5 §10).
	# github.com/modelcontextprotocol/go-sdk (Apache-2.0 + MIT combined file) needs no ignore:
	# go-licenses classifies it as an allowed licence.
	run ro "v=\$(go list -m -f '{{.Version}}' modernc.org/mathutil)
		[ \"\$v\" = v1.7.1 ] || { echo 're-review modernc.org/mathutil licence' >&2; exit 1; }
		v=\$(go list -m -f '{{.Version}}' github.com/segmentio/asm)
		[ \"\$v\" = v1.2.1 ] || { echo 're-review github.com/segmentio/asm licence' >&2; exit 1; }
		GOBIN=/tmp/bin go install github.com/google/go-licenses@v1.6.0
		for t in $TARGETS; do
			export GOOS=\${t%/*} GOARCH=\${t#*/} CGO_ENABLED=0
			echo \"== \$t\"
			/tmp/bin/go-licenses check ./... \\
				--ignore modernc.org/mathutil \\
				--ignore github.com/segmentio/asm \\
				--allowed_licenses=Apache-2.0,MIT,MIT-0,BSD-2-Clause,BSD-3-Clause,ISC,0BSD
		done"
	;;
integration)
	# containerd integration tests. (1) Build `-tags integration` test binaries statically in the
	# pinned golang image into the mirafleet-itest-bin volume. (2) Run them as root inside a
	# throwaway docker:dind container that starts containerd directly (not dockerd).
	# --privileged is for dev/CI only: it applies to this --rm container (inside the container
	# runtime's VM), never touches the host, and all state (containerd root, sockets) lives inside
	# the container (tmpfs) and vanishes with it. Tests use no network; images are built at test time.
	# NOTE: --privileged only confines itself to a VM-backed runtime (colima, Docker Desktop). On a
	# bare Linux host with a native docker daemon it grants the container full host privileges, so
	# do not run this target there (use a throwaway VM).
	# Test binaries are mounted read-only. Exit status is the tests' exit status.
	# Add packages that carry integration tests to ITEST_PKGS (space separated).
	# ./cmd/mirafleet carries the built-binary end-to-end test (TestIntegration*): the mirafleet
	# binary itself is built below into /out/mirafleet and handed to it as MIRAFLEET_TEST_BIN.
	ITEST_PKGS="${MIRAFLEET_ITEST_PKGS:-./internal/workspace/ctrd ./cmd/mirafleet ./internal/central}"
	# docker:28-dind supplies runc 1.3.3 and the userland (its bundled containerd 1.7.28 is NOT
	# used: the harness runs containerd 2.x built from go.mod, with its matching shim, from
	# /itest). Pinned by tag and multi-arch index digest.
	DIND_IMAGE="${MIRAFLEET_DIND_IMAGE:-docker.io/library/docker:28-dind@sha256:2a232a42256f70d78e3cc5d2b5d6b3276710a0de0596c145f627ecfae90282ac}"
	$CTR volume create mirafleet-itest-bin >/dev/null
	# shellcheck disable=SC2086
	run ro "" # ensure the cache volumes exist and are owned by the host user
	# shellcheck disable=SC2086
	$CTR run -q --rm --user 0:0 -v mirafleet-itest-bin:/out "$GO_IMAGE" sh -c 'rm -rf /out/* && chown "$1:$2" /out' sh "$(id -u)" "$(id -g)"
	# shellcheck disable=SC2086
	$CTR run -q --rm --user "$(id -u):$(id -g)" \
		-v "$ROOT":/src:ro -v mirafleet-itest-bin:/out \
		-v mirafleet-gomod:/go/pkg/mod -v mirafleet-gocache:/gocache \
		-w /src -e HOME=/tmp -e GOCACHE=/gocache -e GOPATH=/gocache/gopath -e GOMODCACHE=/go/pkg/mod \
		-e GOFLAGS="-buildvcs=false -modcacherw" -e CGO_ENABLED=0 -e GOOS=linux \
		"$GO_IMAGE" sh -ec 'for p in "$@"; do
			n=$(echo "$p" | sed "s|^\./||; s|/|_|g")
			go test -c -tags integration -o "/out/$n.test" "$p"
		done
		# the real, static mirafleet binary for the built-binary end-to-end test
		go build -o /out/mirafleet ./cmd/mirafleet
		# containerd daemon + runc shim, built statically from the SAME module version as go.mod
		# (the version is read from it). The daemon needs many modules that the repo go.mod does not
		# list, so it is built in a scratch module under /tmp; go.mod/go.sum are never touched and
		# nothing here is linked into mirafleet. Needs the Go module proxy for the extra modules
		# (cached in the mirafleet-gomod volume afterwards). Selected versions follow containerd'"'"'s
		# own go.mod requirements (MVS), checked against the Go checksum database.
		v=$(go list -m -f "{{.Version}}" github.com/containerd/containerd/v2)
		mkdir /tmp/b && cd /tmp/b && go mod init itest-ctrd >/dev/null 2>&1
		M=github.com/containerd/containerd/v2
		go get "$M/cmd/containerd@$v" "$M/cmd/containerd-shim-runc-v2@$v" >/dev/null 2>&1
		go build -tags no_btrfs,no_devmapper,no_zfs,no_aufs -o /out/containerd "$M/cmd/containerd"
		go build -o /out/containerd-shim-runc-v2 "$M/cmd/containerd-shim-runc-v2"
		ls /out' sh $ITEST_PKGS
	# shellcheck disable=SC2016
	$CTR run --rm --privileged --entrypoint sh \
		-v mirafleet-itest-bin:/itest:ro \
		--tmpfs /var/lib/containerd:rw,exec,dev,suid \
		--tmpfs /run/containerd:rw,exec \
		"$DIND_IMAGE" -ec '
		# cgroup v2 nesting, as in the dind entrypoint.
		if [ -f /sys/fs/cgroup/cgroup.controllers ]; then
			mkdir -p /sys/fs/cgroup/init
			# Bounded: the loop retries while processes are still moving into init, but gives up
			# after 50 tries instead of spinning forever on a cgroup tree that cannot be nested.
			n=0
			while ! {
				xargs -rn1 < /sys/fs/cgroup/cgroup.procs > /sys/fs/cgroup/init/cgroup.procs || :
				sed -e "s/ / +/g" -e "s/^/+/" < /sys/fs/cgroup/cgroup.controllers > /sys/fs/cgroup/cgroup.subtree_control
			}; do
				n=$((n+1))
				[ $n -le 50 ] || { echo "cgroup v2 nesting did not settle after 50 tries" >&2; exit 1; }
				sleep 0.1
			done
		fi
		mount --make-rshared /
		mkdir -p /etc/containerd
		cat > /etc/containerd/config.toml <<CFG
version = 3
root = "/var/lib/containerd"
state = "/run/containerd"
[grpc]
  address = "/run/containerd/containerd.sock"
CFG
		export PATH=/itest:$PATH # our containerd 2.x and shim win over the dind-bundled ones
		containerd --config /etc/containerd/config.toml >/tmp/containerd.log 2>&1 &
		cpid=$!
		i=0
		until [ -S /run/containerd/containerd.sock ] && ctr --address /run/containerd/containerd.sock version >/dev/null 2>&1; do
			i=$((i+1))
			if [ $i -gt 100 ] || ! kill -0 $cpid 2>/dev/null; then echo "containerd did not start:" >&2; cat /tmp/containerd.log >&2; exit 1; fi
			sleep 0.3
		done
		export MIRAFLEET_TEST_CONTAINERD_SOCK=/run/containerd/containerd.sock
		export MIRAFLEET_TEST_BIN=/itest/mirafleet
		rc=0
		for b in /itest/*.test; do
			echo "== $b"
			# cmd_mirafleet.test also holds the (non-integration) unit tests, which the normal
			# gate already runs; here only the built-binary end-to-end tests.
			run=""
			case "$b" in
			*/cmd_mirafleet.test) run="-test.run ^TestIntegration" ;;
			*/internal_central.test) run="-test.run ^TestM3DoneIntegration" ;; # the rest runs in the normal gate
			esac
			"$b" -test.v -test.timeout 10m $run || rc=$?
		done
		[ $rc -eq 0 ] || { echo "--- containerd log tail" >&2; tail -n 30 /tmp/containerd.log >&2; }
		kill $cpid 2>/dev/null || :
		exit $rc'
	;;
integration-host)
	# M7 host-mode integration tests (docs/M7.md §7.2). No containerd, no --privileged. (1) Build
	# `-tags integration` test binaries and the mirafleet binary statically, as in `integration`, into
	# the separate volume mirafleet-itest-host-bin. (2) Run them as root in a throwaway container of
	# the digest-pinned Go image (Debian bookworm, which ships git and openssh-client), after
	# creating the unprivileged users `dev` and `qa`; the tests start one runner as each (setuid to
	# the user, so no runner runs as root). The container's own IP is mapped to gitlab.test and
	# registry.gitlab.test in /etc/hosts, so the fake GitLab (internal/testfake/gitlab) listens on a
	# non-loopback address. The tests get:
	#   MIRAFLEET_TEST_BIN=/itest/mirafleet  MIRAFLEET_TEST_RUNNER_USERS="dev qa"
	#   MIRAFLEET_TEST_FAKE_IP=<container ip>  (gitlab.test and registry.gitlab.test resolve to it)
	# Everything is inside the --rm container and vanishes with it. Override the package list with
	# MIRAFLEET_ITEST_HOST_PKGS (space separated).
	HOST_PKGS="${MIRAFLEET_ITEST_HOST_PKGS:-./internal/testfake/gitlab ./internal/central}"
	$CTR volume create mirafleet-itest-host-bin >/dev/null
	# shellcheck disable=SC2086
	run ro "" # ensure the cache volumes exist and are owned by the host user
	# shellcheck disable=SC2086
	$CTR run -q --rm --user 0:0 -v mirafleet-itest-host-bin:/out "$GO_IMAGE" sh -c 'rm -rf /out/* && chown "$1:$2" /out' sh "$(id -u)" "$(id -g)"
	# shellcheck disable=SC2086
	$CTR run -q --rm --user "$(id -u):$(id -g)" \
		-v "$ROOT":/src:ro -v mirafleet-itest-host-bin:/out \
		-v mirafleet-gomod:/go/pkg/mod -v mirafleet-gocache:/gocache \
		-w /src -e HOME=/tmp -e GOCACHE=/gocache -e GOPATH=/gocache/gopath -e GOMODCACHE=/go/pkg/mod \
		-e GOFLAGS="-buildvcs=false -modcacherw" -e CGO_ENABLED=0 -e GOOS=linux \
		"$GO_IMAGE" sh -ec 'for p in "$@"; do
			n=$(echo "$p" | sed "s|^\./||; s|/|_|g")
			go test -c -tags integration -o "/out/$n.test" "$p"
		done
		go build -o /out/mirafleet ./cmd/mirafleet
		ls /out' sh $HOST_PKGS
	# shellcheck disable=SC2016
	$CTR run --rm --user 0:0 --entrypoint sh \
		-v mirafleet-itest-host-bin:/itest:ro \
		"$GO_IMAGE" -ec '
		command -v git >/dev/null && command -v ssh >/dev/null || { echo "image lacks git or openssh-client" >&2; exit 1; }
		for u in dev qa; do
			useradd -m -s /bin/bash "$u"
			chmod 700 "/home/$u"
		done
		ip=$(hostname -i | tr " " "\n" | grep -v "^127\." | grep -m1 "\.") || { echo "no non-loopback address" >&2; exit 1; }
		echo "$ip gitlab.test registry.gitlab.test" >> /etc/hosts
		export MIRAFLEET_TEST_BIN=/itest/mirafleet MIRAFLEET_TEST_RUNNER_USERS="dev qa" MIRAFLEET_TEST_FAKE_IP="$ip"
		cd /tmp
		rc=0
		for b in /itest/*.test; do
			echo "== $b"
			run=""
			case "$b" in
			*/internal_central.test) run="-test.run ^TestM7DoneIntegration" ;; # the rest runs in the normal gate
			esac
			"$b" -test.v -test.timeout 20m $run || rc=$?
		done
		exit $rc'
	;;
go) run rw 'exec go "$@"' "$@" ;;
*)
	echo "usage: scripts/dev.sh build|test|vet|cross|fmt|licenses|integration|integration-host|shell|go <args...>" >&2
	exit 2
	;;
esac
