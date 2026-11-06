#!/usr/bin/env bash
# Builds ibc, starts two nodes on temporary data directories, and runs the
# browser test of the web interface against them. Optional: it needs Node.js
# and Chrome (set CHROME to its path if it is not /usr/bin/google-chrome).
#
#   web/e2e/run.sh            # SHOTS=dir/ also saves screenshots
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
pids=()
cleanup() { kill "${pids[@]}" 2>/dev/null || true; rm -rf "$work"; }
trap cleanup EXIT

(cd "$here" && [ -d node_modules ] || npm install --silent)
(cd "$root" && go build -o "$work/ibc" ./cmd/ibc)
for port in 2731 2732; do
    "$work/ibc" -addr "127.0.0.1:$port" -data "$work/data-$port" >"$work/node-$port.log" 2>&1 &
    pids+=($!)
done
sleep 1
node "$here/run.mjs" http://127.0.0.1:2731 http://127.0.0.1:2732
