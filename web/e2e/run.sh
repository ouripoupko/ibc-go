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

(cd "$here" && [ -d node_modules/puppeteer-core ] || npm install --silent)
(cd "$root" && go build -o "$work/ibc" ./cmd/ibc)
# The nodes compile apps' Solidity source: solc if installed, else solc-js.
if [ -z "${IBC_SOLC:-}" ]; then
    if command -v solc >/dev/null; then IBC_SOLC=solc; else IBC_SOLC="npx -y -p solc@0.8.38 solcjs"; fi
fi
export IBC_SOLC
for port in 2731 2732; do
    "$work/ibc" -addr "localhost:$port" -data "$work/data-$port" >"$work/node-$port.log" 2>&1 &
    pids+=($!)
done
sleep 1
# Passkeys need a domain name, not an IP address: the nodes are at localhost.
node "$here/run.mjs" http://localhost:2731 http://localhost:2732
