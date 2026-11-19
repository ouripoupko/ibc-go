#!/usr/bin/env bash
# Writes the package's copyright file to OUT, from packaging/copyright, with
# the versions this release holds: ibc's (VERSION), go-ethereum's (go.mod), and
# solc's (compiler.PinnedVersion).
#
#   VERSION=v0.1.0 packaging/copyright.sh dist/stage/copyright
set -euo pipefail
out=${1:?usage: VERSION=v0.1.0 $0 OUT}
version=${VERSION:?set VERSION to the release tag}
root=$(cd "$(dirname "$0")/.." && pwd)
solc=$(sed -n 's/^const PinnedVersion = "\(.*\)"$/\1/p' "$root/compiler/compiler.go")
geth=$(awk '$1 == "github.com/ethereum/go-ethereum" { print $2 }' "$root/go.mod")
[ -n "$solc" ] && [ -n "$geth" ] || { echo "cannot find the solc or go-ethereum version" >&2; exit 1; }
mkdir -p "$(dirname "$out")"
sed -e "s/@IBC_VERSION@/$version/g" -e "s/@GETH_VERSION@/$geth/g" -e "s/@SOLC_VERSION@/$solc/g" \
    "$root/packaging/copyright" > "$out"
