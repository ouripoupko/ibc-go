#!/usr/bin/env bash
# Downloads the official native solc of the version the node pins
# (compiler.PinnedVersion), for linux-$GOARCH, to OUT, and checks it against
# the hash in Solidity's release list, as the node checks the compilers it
# downloads.
#
#   GOARCH=amd64 packaging/fetch-solc.sh dist/stage/solc
set -euo pipefail
out=${1:?usage: GOARCH=amd64|arm64 $0 OUT}
arch=${GOARCH:?set GOARCH to amd64 or arm64}
root=$(cd "$(dirname "$0")/.." && pwd)
version=$(sed -n 's/^const PinnedVersion = "\(.*\)"$/\1/p' "$root/compiler/compiler.go")
[ -n "$version" ] || { echo "no PinnedVersion in compiler/compiler.go" >&2; exit 1; }
base=https://binaries.soliditylang.org/linux-$arch

read -r path sha < <(curl -fsSL "$base/list.json" | python3 -c '
import json, sys
v = sys.argv[1]
b = [b for b in json.load(sys.stdin)["builds"] if b["version"] == v and "prerelease" not in b]
if not b: sys.exit("solc " + v + " is not released for this architecture")
print(b[0]["path"], b[0]["sha256"].removeprefix("0x"))
' "$version")

mkdir -p "$(dirname "$out")"
curl -fsSL -o "$out" "$base/$path"
echo "$sha  $out" | sha256sum --check --quiet
chmod 755 "$out"
echo "solc $version for linux-$arch: $path"
