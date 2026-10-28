#!/usr/bin/env bash
# Fetch the Ollama Windows release pinned in packaging/ollama-runtime.json,
# verify the whole zip by SHA-256, and unpack only the llama-server runtime
# (about 75 MB of the 1.6 GB zip: no CUDA, ROCm or MLX) into
# <stage>/runtime/vulkan/, keeping Ollama's lib/ollama layout:
#
#   runtime/vulkan/llama-server.exe        22 KB stub, loads libllama-server-impl.dll
#   runtime/vulkan/libllama*.dll, ggml*.dll, libmtmd/libomp/libc++/...
#   runtime/vulkan/vulkan/ggml-vulkan.dll  the Vulkan backend ggml loads from here
#
# Why Ollama's build: it is Authenticode signed by Ollama Inc., which the
# upstream llama.cpp Windows builds are not. Windows security policy may
# still block a signed binary. The signatures are checked
# on a Windows machine by packaging/verify-runtime.ps1.
#
# Licenses (Ollama's, llama.cpp's, and the files the zip carries) go to
# <stage>/licenses/.
#
# Usage: packaging/fetch-ollama-runtime.sh <stage-dir>
#
# The full zip is cached in .cache/ollama-runtime/ (CI caches that directory
# keyed by the pinned hash) and re-verified on every use, so a corrupted or
# tampered cache fails the build the same way a bad download does. Any
# mismatch is fatal.
set -euo pipefail

stage=${1:?usage: fetch-ollama-runtime.sh <stage-dir>}
here=$(cd "$(dirname "$0")" && pwd)
pin="$here/ollama-runtime.json"
cache="${OLLAMA_RUNTIME_CACHE:-$here/../.cache/ollama-runtime}"

field() { jq -er ".$1" "$pin"; }
version=$(field ollama_version)
llama_tag=$(field llama_cpp_tag)
asset=$(field asset)
url=$(field url)
sum=$(field sha256)

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
fetch "$(field ollama_license_url)" "$(field ollama_license_sha256)" "$cache/LICENSE-ollama-$version"
fetch "$(field llama_cpp_license_url)" "$(field llama_cpp_license_sha256)" "$cache/LICENSE-llama.cpp-$llama_tag"

# The members to keep: lib/ollama/ top-level exe and dll files, the Vulkan
# backend folder, and the license files. llama-quantize is not used.
# Everything else (cuda_*, rocm, mlx, ollama.exe) stays in the zip.
mapfile -t members < <(
	unzip -Z1 "$cache/$asset" | grep -E \
		'^lib/ollama/([^/]+\.(exe|dll)|[^/]*LICENSE[^/]*|vulkan/[^/]+\.dll)$' |
		grep -vE '^lib/ollama/(llama-quantize\.exe|libllama-quantize-impl\.dll)$'
)
need=(llama-server.exe libllama-server-impl.dll libllama.dll ggml.dll ggml-base.dll
	ggml-cpu-x64.dll libmtmd.dll libomp.dll libc++.dll libunwind.dll libwinpthread-1.dll
	vulkan/ggml-vulkan.dll)
for n in "${need[@]}"; do
	printf '%s\n' "${members[@]}" | grep -qxF "lib/ollama/$n" || {
		echo "$n not found in $asset" >&2
		exit 1
	}
done

rt="$stage/runtime/vulkan"
rm -rf "$rt"
mkdir -p "$rt" "$stage/licenses"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
unzip -q "$cache/$asset" "${members[@]}" -d "$tmp"
cp -r "$tmp/lib/ollama/." "$rt/"

# The archive hash binds these file hashes to the pin. Windows CI separately
# checks every executable signature before the package can ship.
manifest_files=$(find "$rt" -type f \( -iname '*.exe' -o -iname '*.dll' \) -print0 | sort -z | while IFS= read -r -d '' f; do
    printf '%s\t%s\n' "${f#"$rt"/}" "$(sha256sum "$f" | cut -d' ' -f1)"
done | jq -Rn '[inputs | split("\t") | {path: .[0], sha256: .[1]}]')
jq -n --arg build "$llama_tag" --arg version "$version" --argjson files "$manifest_files" \
    '{llama_cpp_tag: $build, ollama_version: $version, files: $files}' > "$rt/benchwarmer-runtime.json"

cp "$cache/LICENSE-ollama-$version" "$stage/licenses/LICENSE-ollama"
cp "$cache/LICENSE-llama.cpp-$llama_tag" "$stage/licenses/LICENSE-llama.cpp"
# Third-party notices the zip carries (llama.cpp vendors, cpp-httplib, Go).
for f in "$rt"/*LICENSE*; do
	case $(basename "$f") in
	LLAMA_CPP_LICENSE) ;; # same text as LICENSE-llama.cpp
	*) cp "$f" "$stage/licenses/$(basename "$f")" ;;
	esac
done
echo "Ollama $version runtime (llama.cpp $llama_tag) unpacked to $rt ($(du -sh "$rt" | cut -f1))" >&2
