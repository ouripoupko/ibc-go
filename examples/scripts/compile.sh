#!/usr/bin/env bash
# Compiles every contract in examples/contracts into examples/build/<Name>.bin
# and <Name>.abi. Needs Node.js (npx); downloads solc-js on first use.
#
#   examples/scripts/compile.sh            # all contracts
#   examples/scripts/compile.sh Poll.sol   # one contract
set -euo pipefail

SOLC_VERSION=${SOLC_VERSION:-0.8.38}
here=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

cd "$here/contracts"
sources=("$@")
[ ${#sources[@]} -eq 0 ] && sources=(*.sol)

for src in "${sources[@]}"; do
    name=$(basename "$src" .sol)
    npx -y -p "solc@$SOLC_VERSION" solcjs --bin --abi --optimize -o "$tmp/$name" "$src" >/dev/null
    # solcjs names its output <source>_<contract>; keep the contract named like the file.
    cp "$tmp/$name/${name}_sol_${name}.bin" "$here/build/$name.bin"
    cp "$tmp/$name/${name}_sol_${name}.abi" "$here/build/$name.abi"
    echo "compiled $src -> build/$name.bin, build/$name.abi"
done
