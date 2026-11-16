#!/bin/sh
# Release tooling. Every step runs in a digest-pinned container; nothing is
# installed on the host. Release targets live here, not in dev.sh; add new ones
# below the marked line. ONE target per call.
# Default runtime: the Docker CLI against colima's Docker runtime (`colima start`).
# Override with MIRAFLEET_CTR. MIRAFLEET_CTR and the *_IMAGE variables are trusted caller
# environment (CTR is word-split and executed); never derive them from untrusted input.
#
# Usage: scripts/release.sh licenses-report|vuln|lint-sh|lint-actions|stage|snapshot|pkgtest|installtest|image|lab|wslab|npmtest|harnesscheck|m6check|clean
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

# Same targets as dev.sh.
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


# Release tools image (goreleaser, syft, cosign, nfpm; each pinned in tools.Dockerfile).
# Not named *_IMAGE: it is built locally from pinned bases, not pulled.
TOOLS_TAG=mirafleet-release-tools:local
tools_build() {
	$CTR build -q --label "$LABEL" -t "$TOOLS_TAG" \
		-f "$ROOT/scripts/release/tools.Dockerfile" "$ROOT/scripts/release" >/dev/null
	$CTR run --rm "$TOOLS_TAG" sh -c 'goreleaser --version | grep -m1 -i version; go version; syft version | grep -m1 -i "^version"; cosign version 2>&1 | grep -m1 -i gitversion; nfpm --version'
}

# toolsrun SCRIPT: run SCRIPT under sh -ec in the tools image with the source writable at /src.
# A git worktree's .git file points outside the tree, so the common git dir is mounted at its
# own path (read-only). No network credentials, no Docker socket.
# shellcheck disable=SC2086
toolsrun() {
	gitmount=""
	common=$(git -C "$ROOT" rev-parse --path-format=absolute --git-common-dir 2>/dev/null || true)
	case "$common" in "" | "$ROOT"/*) ;; *) gitmount="-v $common:$common:ro" ;; esac
	$CTR run -q --rm --user 0:0 -v mirafleet-gomod:/go/pkg/mod -v mirafleet-gocache:/gocache \
		"$TOOLS_TAG" sh -c 'chown -R "$1:$2" /go/pkg/mod /gocache' sh "$(id -u)" "$(id -g)"
	$CTR run -q --rm \
		--user "$(id -u):$(id -g)" \
		-v "$ROOT":/src:rw $gitmount \
		-v mirafleet-gomod:/go/pkg/mod \
		-v mirafleet-gocache:/gocache \
		-w /src \
		-e HOME=/tmp \
		-e GOCACHE=/gocache \
		-e GOPATH=/gocache/gopath \
		-e GOMODCACHE=/go/pkg/mod \
		-e GOFLAGS="-buildvcs=false -modcacherw" \
		-e GIT_CONFIG_COUNT=1 -e GIT_CONFIG_KEY_0=safe.directory -e GIT_CONFIG_VALUE_0='*' \
		"$TOOLS_TAG" sh -ec "$1"
}

# runscript NAME [ARG...]: run scripts/release/NAME (a separate script), or fail clearly.
runscript() {
	name=$1
	shift
	if [ ! -f "$ROOT/scripts/release/$name" ]; then
		echo "release.sh: not yet implemented: scripts/release/$name does not exist" >&2
		exit 1
	fi
	MIRAFLEET_GO_IMAGE="$GO_IMAGE" MIRAFLEET_CTR="$CTR" MIRAFLEET_RELEASE_ROOT="$ROOT" MIRAFLEET_RELEASE_LABEL="$LABEL" \
		exec sh "$ROOT/scripts/release/$name" "$@"
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
	# Every tracked-style shell script: scripts/*.sh and scripts/release/*.sh.
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
# --- release targets go below this line (snapshot, pkgtest, ..., m6check) ---
stage)
	# dist/stage/: the documents goreleaser puts in archives and packages. THIRD_PARTY_LICENSES.txt
	# comes from `licenses-report`; docs/QUICKSTART.md is a late-lane file, so without it this
	# fails unless MIRAFLEET_STAGE_PLACEHOLDER=1 (snapshot sets it; a real release never does).
	[ -f "$ROOT/dist/THIRD_PARTY_LICENSES.txt" ] || sh "$0" licenses-report
	rm -rf "$ROOT/dist/stage"
	mkdir -p "$ROOT/dist/stage"
	for f in LICENSE NOTICE README.md; do cp "$ROOT/$f" "$ROOT/dist/stage/$f"; done
	cp "$ROOT/dist/THIRD_PARTY_LICENSES.txt" "$ROOT/dist/stage/THIRD_PARTY_LICENSES.txt"
	if [ -f "$ROOT/docs/QUICKSTART.md" ]; then
		cp "$ROOT/docs/QUICKSTART.md" "$ROOT/dist/stage/QUICKSTART.md"
	elif [ "${MIRAFLEET_STAGE_PLACEHOLDER:-}" = 1 ]; then
		echo "stage: docs/QUICKSTART.md missing (late lane): using a placeholder" >&2
		echo "QUICKSTART placeholder: docs/QUICKSTART.md is not written yet." >"$ROOT/dist/stage/QUICKSTART.md"
	else
		echo "stage: docs/QUICKSTART.md is missing; a release needs it" >&2
		exit 1
	fi
	;;
snapshot)
	# Proof that .goreleaser.yaml builds every asset: no publish, no signing. Output in
	# dist/goreleaser/ (archives, packages, checksums.txt, SBOMs); other release tests read it.
	MIRAFLEET_STAGE_PLACEHOLDER=1 sh "$0" stage
	tools_build
	toolsrun "goreleaser release --snapshot --clean --skip=publish,sign"
	echo "snapshot: assets in dist/goreleaser/"
	;;
pkgtest)
	runscript pkgtest.sh "${1:-$ROOT/dist/goreleaser}"
	;;
installtest)
	runscript installtest.sh "$@"
	;;
image)
	runscript imagetest.sh "$@"
	;;
lab)
	runscript labtest.sh "$@"
	;;
wslab)
	# Not in m6check: it needs a Linux Docker host with containerd and creates /var/lib/mirafleet/ws
	# there. CI runs it as the compose-workspace job.
	runscript wslabtest.sh "$@"
	;;
npmtest)
	runscript npmtest.sh "$@"
	;;
harnesscheck)
	runscript harnesscheck.sh "$@"
	;;
m6check)
	# Every check in order, then `relgen check`; stops at the first failure,
	# prints one summary line, and always cleans up (dist/ and labelled test resources).
	# MIRAFLEET_M6CHECK_SKIP="cross licenses" skips named steps (for example when CI already ran the
	# full gate); a skipped step is listed in the summary. The slow privileged `integration` run is
	# not part of m6check (CI runs it).
	steps="fmt vet build test cross licenses vuln lint-sh lint-actions snapshot relgen pkgtest installtest image lab npmtest harnesscheck"
	skip=" ${MIRAFLEET_M6CHECK_SKIP:-} "
	mkdir -p "$ROOT/dist/m6check"
	ran=0
	skipped=""
	failed=""
	for s in $steps; do
		case "$skip" in *" $s "*)
			skipped="$skipped $s"
			continue
			;;
		esac
		log="$ROOT/dist/m6check/$s.log"
		mkdir -p "$ROOT/dist/m6check" # a step such as snapshot may clear dist/
		echo "== $s"
		rc=0
		case "$s" in
		fmt | vet | build | test | cross | licenses) "$ROOT/scripts/dev.sh" "$s" >"$log" 2>&1 || rc=$? ;;
		relgen)
			# the version every versioned file must agree on: server.json's own
			v=$(sed -n 's/^  "version": "\([^"]*\)".*/\1/p' "$ROOT/server.json" | head -n 1)
			if [ -n "$v" ]; then
				"$ROOT/scripts/dev.sh" go run ./tools/relgen check "v$v" >"$log" 2>&1 || rc=$?
			else
				echo "no version in server.json" >"$log"
				rc=1
			fi
			;;
		*) sh "$0" "$s" >"$log" 2>&1 || rc=$? ;;
		esac
		if [ "$rc" -ne 0 ]; then
			failed=$s
			tail -n 25 "$log"
			break
		fi
		ran=$((ran + 1))
	done
	sh "$0" clean >/dev/null 2>&1 || echo "m6check: clean failed (run scripts/release.sh clean)" >&2
	if [ -n "$failed" ]; then
		echo "m6check: FAIL at $failed ($ran steps passed)"
		exit 1
	fi
	echo "m6check: OK ($ran steps passed${skipped:+; skipped:$skipped})"
	;;
*)
	echo "usage: scripts/release.sh licenses-report|vuln|lint-sh|lint-actions|stage|snapshot|pkgtest|installtest|image|lab|wslab|npmtest|harnesscheck|m6check|clean" >&2
	exit 2
	;;
esac
