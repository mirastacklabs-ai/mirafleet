#!/bin/sh
# Checks the Claude Code plugin launcher (integrations/claude-code/plugin/bin/mirafleet.js) in a
# digest-pinned Node container: nothing is installed on the host. Asserts that it
#   - exits 127 with a message naming the install channels when `mirafleet` is not on PATH,
#   - passes argv through to the binary and returns the binary's exit code and stdio.
set -eu

NODE_IMAGE="${MIRAFLEET_NODE_IMAGE:-docker.io/library/node:24.21-bookworm-slim@sha256:d6aa754f16b3197301076f047b5def2f02ea1dbbc2ca920407d46d7ec7f87b20}"
CTR="${MIRAFLEET_CTR:-docker}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

if [ -z "${MIRAFLEET_CTR:-}" ] && ! docker info >/dev/null 2>&1; then
	echo "Docker runtime not reachable: start it with \`colima start\` (Docker runtime)" >&2
	exit 1
fi

# shellcheck disable=SC2016
$CTR run -q --rm --user "$(id -u):$(id -g)" -e HOME=/tmp \
	-v "$ROOT/integrations/claude-code/plugin/bin":/launcher:ro \
	"$NODE_IMAGE" sh -ec '
L=/launcher/mirafleet.js
N=$(command -v node)
$N --check "$L"

# 1. binary missing
out=$(PATH=/nonexistent $N "$L" mcp 2>&1) && { echo "FAIL: expected non-zero when mirafleet is missing"; exit 1; } || code=$?
[ "$code" = 127 ] || { echo "FAIL: missing binary exit $code, want 127"; exit 1; }
for s in "not found on PATH" Homebrew install.sh install.ps1 "release archive"; do
	echo "$out" | grep -q "$s" || { echo "FAIL: message lacks $s: $out"; exit 1; }
done

# 2. argv and exit code pass through (fake binary on PATH)
mkdir /tmp/bin
printf "#!/bin/sh\necho \"args=\$*\"\nexit 7\n" > /tmp/bin/mirafleet
chmod +x /tmp/bin/mirafleet
out=$(PATH=/tmp/bin $N "$L" mcp hook claude-code session-start) && { echo "FAIL: expected exit 7"; exit 1; } || code=$?
[ "$code" = 7 ] || { echo "FAIL: exit $code, want 7"; exit 1; }
[ "$out" = "args=mcp hook claude-code session-start" ] || { echo "FAIL: stdout $out"; exit 1; }

# 3. stdin is inherited
printf "#!/bin/sh\nexec /bin/cat\n" > /tmp/bin/mirafleet
out=$(echo hello | PATH=/tmp/bin $N "$L" mcp)
[ "$out" = hello ] || { echo "FAIL: stdin not passed: $out"; exit 1; }

# 5. relative PATH entries are skipped: a ./mirafleet planted in the project never runs
mkdir /tmp/proj
printf "#!/bin/sh\necho PLANTED\n" > /tmp/proj/mirafleet
chmod +x /tmp/proj/mirafleet
out=$(cd /tmp/proj && PATH=.:bin $N "$L" mcp 2>&1) && { echo "FAIL: ran a relative PATH entry: $out"; exit 1; } || code=$?
[ "$code" = 127 ] || { echo "FAIL: relative PATH exit $code, want 127: $out"; exit 1; }

# 6. a child killed by a signal never yields exit 0
printf "#!/bin/sh\nkill -TERM \$\$\n" > /tmp/bin/mirafleet
PATH=/tmp/bin $N "$L" mcp && { echo "FAIL: exit 0 after signal death"; exit 1; } || code=$?
[ "$code" -ge 128 ] || { echo "FAIL: signal exit $code, want >=128"; exit 1; }

# 4. the harness PID (the launcher parent) is handed to the binary
printf "#!/bin/sh\necho \"pid=\$MIRAFLEET_HARNESS_PID\"\n" > /tmp/bin/mirafleet
out=$(PATH=/tmp/bin /bin/sh -c "$N \"$L\" mcp; true" )
case "$out" in pid=[1-9]*) ;; *) echo "FAIL: MIRAFLEET_HARNESS_PID not set: $out"; exit 1;; esac
echo OK
'
