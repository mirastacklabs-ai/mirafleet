#!/bin/sh
# npmtest (docs/M6.md section 2.14): generate the 7 npm packages, `npm pack` them, install them
# offline into a temp project from the tarballs, run the launcher, and assert that no install
# scripts exist or ran. Everything runs in digest-pinned containers; nothing is installed on the
# host and nothing is published.
#
# Usage: scripts/release/npmtest.sh [DIST VERSION]
#   no arguments: build a stand-in release (the native linux binary in all six archives).
#   DIST VERSION: use real archives + checksums.txt (e.g. a goreleaser snapshot); VERSION must be
#                 X.Y.Z or X.Y.Z-rc.N and the host/container arch must be linux amd64 or arm64.
set -eu

GO_IMAGE="${MIRAFLEET_GO_IMAGE:-docker.io/library/golang:1.27.4-bookworm@sha256:4fa7c968ba93f1635670522dbb01a2c78150443e1385746924566079f36570ea}"
NODE_IMAGE="${MIRAFLEET_NODE_IMAGE:-docker.io/library/node:24.21-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20}"
CTR="${MIRAFLEET_CTR:-docker}"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
LABEL=org.mirastacklabs.mirafleet.release=1
VERSION_DEFAULT=0.0.1-rc.1

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	echo "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)" >&2
	exit 1
fi

# Work dir inside the repo (colima mounts only $HOME); dist/ is gitignored.
mkdir -p "$ROOT/dist"
WORK="$(mktemp -d "$ROOT/dist/npmtest-XXXXXX")"
trap 'rm -rf "$WORK"' EXIT INT TERM
WORKNAME="$(basename "$WORK")"

if [ $# -eq 2 ]; then
	DISTDIR="$1"
	VERSION="$2"
	mkdir -p "$WORK/dist"
	cp "$DISTDIR"/mirafleet_"$VERSION"_* "$DISTDIR"/checksums.txt "$WORK/dist/"
elif [ $# -eq 0 ]; then
	VERSION="$VERSION_DEFAULT"
else
	echo "usage: scripts/release/npmtest.sh [DIST VERSION]" >&2
	exit 2
fi

# Step 1 (Go container): the binary (stand-in mode), the packages from the archives.
# shellcheck disable=SC2016
$CTR run -q --rm --label "$LABEL" --user "$(id -u):$(id -g)" \
	-v "$ROOT":/src:ro -v "$WORK":/work:rw \
	-v mirafleet-gomod:/go/pkg/mod -v mirafleet-gocache:/gocache \
	-w /src -e HOME=/tmp -e GOCACHE=/gocache -e GOPATH=/gocache/gopath -e GOMODCACHE=/go/pkg/mod \
	-e GOFLAGS="-buildvcs=false -modcacherw" -e CGO_ENABLED=0 \
	"$GO_IMAGE" sh -ec '
		version=$1
		go build -o /tmp/relgen ./tools/relgen
		if [ ! -d /work/dist ]; then
			go build -trimpath -ldflags "-s -w -X main.version=$version" -o /tmp/mirafleet ./cmd/mirafleet
			/tmp/relgen -root /src npmfixture -bin /tmp/mirafleet -dist /work/dist "$version"
		fi
		/tmp/relgen -root /src npm -dist /work/dist -out /work/pkgs "$version"
	' sh "$VERSION"

# Step 2 (node container, no network): pack, inspect, install from the tarballs, run.
$CTR run -q --rm --label "$LABEL" --network none --user "$(id -u):$(id -g)" \
	-v "$ROOT":/src:ro -v "$WORK":/work:rw -e HOME=/tmp -e NPM_CONFIG_CACHE=/tmp/npm-cache \
	"$NODE_IMAGE" sh -ec '
		version=$1
		fail() { echo "npmtest FAIL: $*" >&2; exit 1; }

		echo "== launcher unit tests"
		cp -R /src/npm/mirafleet /tmp/launcher
		node --test /tmp/launcher/test

		echo "== pack"
		mkdir /work/tarballs
		for d in /work/pkgs/*; do
			(cd "$d" && npm pack --ignore-scripts --pack-destination /work/tarballs >/dev/null)
		done
		[ "$(ls /work/tarballs | wc -l)" = 7 ] || fail "want 7 tarballs"

		echo "== manifests"
		node -e '"'"'
			const fs = require("fs");
			const version = process.argv[1];
			const server = JSON.parse(fs.readFileSync("/src/server.json", "utf8"));
			const plat = ["linux-x64","linux-arm64","darwin-x64","darwin-arm64","win32-x64","win32-arm64"];
			const bad = (m) => { console.error("npmtest FAIL: " + m); process.exit(1); };
			for (const d of fs.readdirSync("/work/pkgs")) {
				const p = JSON.parse(fs.readFileSync("/work/pkgs/" + d + "/package.json", "utf8"));
				if (p.version !== version) bad(d + " version " + p.version);
				if (p.scripts && Object.keys(p.scripts).length) bad(d + " has scripts");
				if (p.dependencies) bad(d + " has dependencies");
				if (d === "mirafleet") {
					if (p.name !== "@mirastacklabs/mirafleet") bad("wrapper name");
					if (p.mcpName !== server.name) bad("mcpName != server.json name");
					if (!server.packages.some((x) => x.registryType === "npm" && x.identifier === p.name)) bad("server.json package");
					if (p.engines.node !== ">=18") bad("engines");
					const o = p.optionalDependencies || {};
					if (Object.keys(o).length !== 6) bad("optionalDependencies count");
					for (const k of plat) if (o["@mirastacklabs/mirafleet-" + k] !== version) bad("optional dep " + k + " not exact");
				} else {
					const k = d.replace("mirafleet-", "");
					if (!plat.includes(k)) bad("unexpected dir " + d);
					const [os, cpu] = k.split("-");
					if (JSON.stringify(p.os) !== JSON.stringify([os]) || JSON.stringify(p.cpu) !== JSON.stringify([cpu])) bad(d + " os/cpu");
				}
			}
		'"'"' "$version"

		echo "== tarball contents"
		for t in /work/tarballs/*.tgz; do
			tar -tzf "$t" | grep -v "^package/" && fail "$t: entry outside package/"
			case "$t" in
			*mirafleet-mirafleet-[0-9]*)
				tar -tzf "$t" | grep -q "^package/bin/mirafleet.js$" || fail "launcher missing"
				tar -tzf "$t" | grep -q "test/" && fail "tests shipped in the wrapper"
				tar -tzf "$t" | grep -q "^package/LICENSE$" || fail "wrapper LICENSE"
				;;
			*)
				tar -tzf "$t" | grep -qE "^package/bin/mirafleet(\.exe)?$" || fail "$t: binary missing"
				tar -tzf "$t" | grep -q "^package/LICENSE$" || fail "$t: LICENSE missing"
				tar -tzf "$t" | grep -q "^package/THIRD_PARTY_LICENSES.txt$" || fail "$t: THIRD_PARTY_LICENSES missing"
				;;
			esac
		done
		arch=$(node -p "process.arch")
		nt=$(ls /work/tarballs/*mirafleet-linux-$arch-*.tgz)
		tar -tvzf "$nt" | grep "bin/mirafleet$" | grep -q "^-rwx" || fail "binary not executable in tarball"

		echo "== offline install"
		mkdir /tmp/proj && cd /tmp/proj
		wrapper=$(ls /work/tarballs/mirastacklabs-mirafleet-[0-9]*.tgz)
		node -e '"'"'
			const fs = require("fs");
			const ov = {};
			for (const f of fs.readdirSync("/work/tarballs")) {
				const m = /^mirastacklabs-(mirafleet-(?:linux|darwin|win32)-[a-z0-9]+)-\d.*\.tgz$/.exec(f);
				if (m) ov["@mirastacklabs/" + m[1]] = "file:/work/tarballs/" + f;
			}
			if (Object.keys(ov).length !== 6) { console.error("npmtest FAIL: overrides " + Object.keys(ov).length); process.exit(1); }
			fs.writeFileSync("package.json", JSON.stringify({ name: "npmtest", version: "1.0.0", private: true, overrides: ov }));
		'"'"'
		npm install --offline --no-audit --no-fund --loglevel=verbose "$wrapper" >/tmp/install.log 2>&1 || { cat /tmp/install.log; fail "npm install"; }
		grep -qiE "(pre|post)?install script|lifecycle|^> " /tmp/install.log && { cat /tmp/install.log; fail "an install script ran"; }
		# Only the native platform package is installed (npm skips the others by os/cpu).
		got=$(ls node_modules/@mirastacklabs | tr "\n" " ")
		[ "$got" = "mirafleet mirafleet-linux-$arch " ] || fail "installed packages: $got"
		for m in node_modules/@mirastacklabs/*/package.json node_modules/.package-lock.json; do
			grep -q "hasInstallScript" "$m" && fail "$m: hasInstallScript"
		done
		true

		echo "== run through the launcher"
		out=$(npx --offline mirafleet version) || fail "npx mirafleet version"
		[ "$out" = "$version" ] || fail "version output [$out], want [$version]"
		out=$(./node_modules/.bin/mirafleet version) || fail "bin shim"
		[ "$out" = "$version" ] || fail "shim output [$out]"
		npx --offline mirafleet bogus-command >/dev/null 2>&1 && fail "exit code not propagated"
		init="{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\",\"params\":{\"protocolVersion\":\"2025-06-18\",\"capabilities\":{},\"clientInfo\":{\"name\":\"npmtest\",\"version\":\"0\"}}}"
		resp=$(printf "%s\n" "$init" | npx --offline mirafleet mcp -project-dir /tmp/proj 2>/dev/null) || true
		echo "$resp" | grep -q "\"serverInfo\"" || fail "MCP initialize through the launcher: [$resp]"
		echo "npmtest OK"
	' sh "$VERSION"
