#!/bin/sh
# Check the six M6 harness integrations (snippets parse, commands equal the shared body, LF).
# Runs in the dev container; nothing is installed on the host.
set -eu
cd "$(dirname "$0")/../.."
exec scripts/dev.sh go test -count=1 -run 'TestM6|TestNoCRLFInIntegrations' ./integrations/
