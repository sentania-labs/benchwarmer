#!/usr/bin/env bash
# Fetch the llama.cpp runtime pinned in packaging/llama-cpp.json, verify it
# by SHA-256, and unpack it into <stage>/runtime/vulkan/. The llama.cpp
# license (not shipped in the release zip) is fetched from the same tag and
# also verified; licenses go to <stage>/licenses/.
#
# Usage: packaging/fetch-llama-cpp.sh <stage-dir>
#
# Downloads are cached in .cache/llama-cpp/ and re-verified on every use, so
# a corrupted or tampered cache fails the build the same way a bad download
# does. Any mismatch is fatal.
set -euo pipefail

stage=${1:?usage: fetch-llama-cpp.sh <stage-dir>}
here=$(cd "$(dirname "$0")" && pwd)
pin="$here/llama-cpp.json"
cache="${LLAMA_CPP_CACHE:-$here/../.cache/llama-cpp}"

field() { jq -er ".$1" "$pin"; }
tag=$(field tag)
asset=$(field asset)
url=$(field url)
sum=$(field sha256)
lic_url=$(field license_url)
lic_sum=$(field license_sha256)

# fetch URL SHA256 DEST: download unless a cached copy already verifies.
fetch() {
	local url=$1 want=$2 dest=$3
	if [[ -f $dest ]] && echo "$want  $dest" | sha256sum -c --status; then
		return 0
	fi
	rm -f "$dest"
	echo "fetch: $url" >&2
	curl -fsSL --retry 3 -o "$dest.part" "$url"
	if ! echo "$want  $dest.part" | sha256sum -c --status; then
		echo "SHA-256 mismatch for $url" >&2
		echo "  want $want" >&2
		echo "  got  $(sha256sum "$dest.part" | cut -d' ' -f1)" >&2
		rm -f "$dest.part"
		exit 1
	fi
	mv "$dest.part" "$dest"
}

mkdir -p "$cache"
fetch "$url" "$sum" "$cache/$asset"
fetch "$lic_url" "$lic_sum" "$cache/LICENSE-llama.cpp-$tag"

rt="$stage/runtime/vulkan"
rm -rf "$rt"
mkdir -p "$rt" "$stage/licenses"
unzip -q "$cache/$asset" -d "$rt"
if [[ ! -f $rt/llama-server.exe ]]; then
	echo "llama-server.exe not found in $asset" >&2
	exit 1
fi

cp "$cache/LICENSE-llama.cpp-$tag" "$stage/licenses/LICENSE-llama.cpp"
# The zip bundles the LLVM OpenMP runtime (libomp.dll) with its license.
if [[ -f $rt/LICENSE-LLVM-OpenMP ]]; then
	cp "$rt/LICENSE-LLVM-OpenMP" "$stage/licenses/LICENSE-LLVM-OpenMP"
fi
echo "llama.cpp $tag unpacked to $rt" >&2
